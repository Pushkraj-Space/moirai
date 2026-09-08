package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	moirai "github.com/october-dev/moirai"
)

// isolatedDoctorEnv points every store at an empty home, clears every
// override variable, and replaces PATH with an empty directory. It returns the
// home and bin directories.
func isolatedDoctorEnv(t *testing.T) (home, bin string) {
	t.Helper()
	home = t.TempDir()
	bin = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range append(allOverrideNames(), "APPDATA") {
		t.Setenv(name, "")
	}
	t.Setenv("PATH", bin)
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	}
	return home, bin
}

func allOverrideNames() []string {
	var names []string
	for _, format := range moirai.Formats {
		for _, name := range storeOverrides(format) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func fakeExecutable(t *testing.T, bin, name string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

type doctorResult struct {
	rows  map[moirai.Format]doctorRow
	raw   map[moirai.Format]map[string]json.RawMessage
	human string
}

func doctorJSON() ([]doctorRow, []map[string]json.RawMessage, error) {
	var stdout, stderr bytes.Buffer
	a := app{out: &stdout, err: &stderr}
	if err := a.run(context.Background(), []string{"doctor", "--json"}); err != nil {
		return nil, nil, err
	}
	var rows []doctorRow
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return nil, nil, err
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		return nil, nil, err
	}
	return rows, raw, nil
}

func runDoctor(t *testing.T) doctorResult {
	t.Helper()
	rows, raw, err := doctorJSON()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(moirai.Formats) {
		t.Fatalf("doctor reported %d rows, want %d", len(rows), len(moirai.Formats))
	}
	result := doctorResult{rows: map[moirai.Format]doctorRow{}, raw: map[moirai.Format]map[string]json.RawMessage{}}
	for index, row := range rows {
		result.rows[row.Format] = row
		result.raw[row.Format] = raw[index]
	}
	var stdout, stderr bytes.Buffer
	a := app{out: &stdout, err: &stderr}
	if err := a.run(context.Background(), []string{"doctor"}); err != nil {
		t.Fatal(err)
	}
	result.human = stdout.String()
	return result
}

func (r doctorResult) row(t *testing.T, format moirai.Format) doctorRow {
	t.Helper()
	row, ok := r.rows[format]
	if !ok {
		t.Fatalf("no doctor row for %s", format)
	}
	return row
}

func (r doctorResult) hasKey(format moirai.Format, key string) bool {
	_, ok := r.raw[format][key]
	return ok
}

func isTrue(value *bool) bool  { return value != nil && *value }
func isFalse(value *bool) bool { return value != nil && !*value }

func warningCodes(row doctorRow) []string {
	codes := []string{}
	for _, warning := range row.Warnings {
		codes = append(codes, warning.Code)
	}
	return codes
}

func findWarning(row doctorRow, code string) (moirai.Warning, bool) {
	for _, warning := range row.Warnings {
		if warning.Code == code {
			return warning, true
		}
	}
	return moirai.Warning{}, false
}

func TestDoctorReportsStatuses(t *testing.T) {
	home, bin := isolatedDoctorEnv(t)
	fakeExecutable(t, bin, "claude")
	config := filepath.Join(home, "claude-config")
	projects := filepath.Join(config, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	codexHome := filepath.Join(home, "absent")
	t.Setenv("CODEX_HOME", codexHome)

	result := runDoctor(t)
	claude := result.row(t, moirai.FormatClaudeCode)
	if claude.DisplayName != "Claude Code" || claude.Executable != "claude" || !isTrue(claude.Installed) {
		t.Fatalf("claude executable: %+v", claude)
	}
	if claude.Store != projects || claude.ActiveOverride != "CLAUDE_CONFIG_DIR" || !slices.Equal(claude.StoreOverrides, []string{"CLAUDE_CONFIG_DIR"}) {
		t.Fatalf("claude store: %+v", claude)
	}
	if !isTrue(claude.Exists) || !isTrue(claude.Readable) || len(claude.Warnings) != 0 {
		t.Fatalf("claude status: %+v", claude)
	}
	codex := result.row(t, moirai.FormatCodex)
	codexRoot := filepath.Join(codexHome, "sessions")
	if codex.Executable != "codex" || !isFalse(codex.Installed) || codex.Store != codexRoot || !isFalse(codex.Exists) {
		t.Fatalf("codex row: %+v", codex)
	}
	if result.hasKey(moirai.FormatCodex, "readable") || result.hasKey(moirai.FormatCodex, "writable") {
		t.Fatalf("missing codex store must not report readable/writable: %v", result.raw[moirai.FormatCodex])
	}
	if !slices.Equal(warningCodes(codex), []string{"executable_missing", "store_missing"}) {
		t.Fatalf("codex warnings: %+v", codex.Warnings)
	}
	missing, _ := findWarning(codex, "store_missing")
	if missing.Path != codexRoot || !strings.Contains(missing.Message, "CODEX_HOME") {
		t.Fatalf("store_missing warning: %+v", missing)
	}

	for _, want := range []string{
		"claude_code  Claude Code  read,write,discover,continue\n  executable: claude (found)\n  store: " + projects + " (CLAUDE_CONFIG_DIR is set)\n  status: readable",
		"codex  Codex  read,write,discover,continue\n  executable: codex (not on PATH)\n  store: " + codexRoot + " (CODEX_HOME is set)\n  status: missing\n",
		"  store: " + filepath.Join(home, ".fx", "sessions") + " (set FX_HOME to override)\n  status: missing\n",
		"  warning: codex is not on PATH; install Codex or add it to PATH (executable_missing)\n",
		"  warning: " + codexRoot + ": store root does not exist; run Codex once, or set CODEX_HOME if its data lives elsewhere (store_missing)\n",
		"simple  Simple  read,write\n  store: none\n  status: none\n",
	} {
		if !strings.Contains(result.human, want) {
			t.Errorf("human output lacks %q:\n%s", want, result.human)
		}
	}
}

func TestDoctorOmitsInapplicableKeys(t *testing.T) {
	home, _ := isolatedDoctorEnv(t)
	openCodeDB := filepath.Join(home, "opencode.db")
	hermesHome := filepath.Join(home, "hermes")
	if err := os.MkdirAll(hermesHome, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{openCodeDB, filepath.Join(hermesHome, "state.db")} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENCODE_DB", openCodeDB)
	t.Setenv("HERMES_HOME", hermesHome)

	result := runDoctor(t)
	for _, format := range []moirai.Format{moirai.FormatSimple, moirai.FormatClaudeChat, moirai.FormatChatGPT} {
		for _, key := range []string{"executable", "installed", "store", "store_overrides", "active_override", "exists", "readable", "writable", "warnings"} {
			if result.hasKey(format, key) {
				t.Errorf("%s must not report %s: %s", format, key, result.raw[format][key])
			}
		}
	}
	for _, format := range []moirai.Format{moirai.FormatOpenCode, moirai.FormatHermes} {
		row := result.row(t, format)
		if !isTrue(row.Exists) || !isTrue(row.Readable) || len(row.Warnings) != 0 && row.Warnings[0].Code != "executable_missing" {
			t.Errorf("%s row: %+v", format, row)
		}
		if result.hasKey(format, "writable") {
			t.Errorf("%s is a database file and must not report writable: %s", format, result.raw[format]["writable"])
		}
	}
	if !strings.Contains(result.human, "opencode  OpenCode  read,write,discover,continue\n  executable: opencode (not on PATH)\n  store: "+openCodeDB+" (OPENCODE_DB is set)\n  status: readable\n") {
		t.Errorf("opencode block:\n%s", result.human)
	}
}

func TestDoctorIsReadOnly(t *testing.T) {
	home, _ := isolatedDoctorEnv(t)
	config := filepath.Join(home, "claude-config")
	project := filepath.Join(config, "projects", "-tmp-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "session.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)

	before := snapshotTree(t, home)
	result := runDoctor(t)
	if after := snapshotTree(t, home); !slices.Equal(before, after) {
		t.Fatalf("doctor changed the home tree:\nbefore %v\nafter  %v", before, after)
	}
	missing := 0
	for _, row := range result.rows {
		if !isFalse(row.Exists) {
			continue
		}
		missing++
		if _, err := os.Lstat(row.Store); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: missing root %s was created (err %v)", row.Format, row.Store, err)
		}
	}
	if missing == 0 {
		t.Fatal("expected at least one missing store root")
	}
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		entries = append(entries, rel+"|"+info.Mode().String()+"|"+info.ModTime().String()+"|"+string(rune(info.Size())))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestDoctorReportsWrongType(t *testing.T) {
	home, _ := isolatedDoctorEnv(t)
	config := filepath.Join(home, "claude-config")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "projects"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	dbDir := filepath.Join(home, "opencode.db")
	if err := os.MkdirAll(dbDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DB", dbDir)

	result := runDoctor(t)
	for format, fragment := range map[moirai.Format]string{moirai.FormatClaudeCode: "a regular file but a directory is expected", moirai.FormatOpenCode: "a directory but a database file is expected"} {
		row := result.row(t, format)
		wrong, ok := findWarning(row, "store_wrong_type")
		if !isTrue(row.Exists) || !ok || wrong.Path != row.Store || !strings.Contains(wrong.Message, fragment) {
			t.Errorf("%s row: %+v", format, row)
		}
		if result.hasKey(format, "readable") || result.hasKey(format, "writable") {
			t.Errorf("%s must not be opened when its type is wrong: %v", format, result.raw[format])
		}
	}
	if strings.Count(result.human, "  status: wrong type\n") != 2 {
		t.Errorf("human output:\n%s", result.human)
	}
}

func TestDoctorStoreOverrides(t *testing.T) {
	type override struct {
		format   moirai.Format
		variable string
		suffix   string
	}
	cases := []override{
		{moirai.FormatClaudeCode, "CLAUDE_CONFIG_DIR", "projects"},
		{moirai.FormatCodex, "CODEX_HOME", "sessions"},
		{moirai.FormatPi, "PI_CODING_AGENT_SESSION_DIR", ""},
		{moirai.FormatPi, "PI_CODING_AGENT_DIR", "sessions"},
		{moirai.FormatCampfire, "CAMPFIRE_CODING_AGENT_SESSION_DIR", ""},
		{moirai.FormatCampfire, "CAMPFIRE_CODING_AGENT_DIR", "sessions"},
		{moirai.FormatAmp, "AMP_THREADS_DIR", ""},
		{moirai.FormatAmp, "XDG_DATA_HOME", filepath.Join("amp", "threads")},
		{moirai.FormatGrok, "GROK_HOME", "sessions"},
		{moirai.FormatFX, "FX_HOME", "sessions"},
		{moirai.FormatOpenCode, "OPENCODE_DB", ""},
		{moirai.FormatOpenCode, "XDG_DATA_HOME", filepath.Join("opencode", "opencode.db")},
		{moirai.FormatHermes, "HERMES_HOME", "state.db"},
		{moirai.FormatCursorDesktop, "CURSOR_DESKTOP_USER_DIR", ""},
		{moirai.FormatCowork, "COWORK_SESSIONS_DIR", ""},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, override{moirai.FormatCowork, "APPDATA", filepath.Join("Claude", "local-agent-mode-sessions")})
	}
	for _, c := range cases {
		t.Run(string(c.format)+"/"+c.variable, func(t *testing.T) {
			isolatedDoctorEnv(t)
			value := filepath.Join(t.TempDir(), "override")
			if c.variable == "OPENCODE_DB" {
				value += ".db"
			}
			t.Setenv(c.variable, value)
			row := runDoctor(t).row(t, c.format)
			if want := filepath.Join(value, c.suffix); row.Store != want {
				t.Fatalf("store %q, want %q", row.Store, want)
			}
			if row.ActiveOverride != c.variable || !slices.Contains(row.StoreOverrides, c.variable) {
				t.Fatalf("override reporting: active %q candidates %v", row.ActiveOverride, row.StoreOverrides)
			}
		})
	}
}

func TestDoctorOverridePrecedence(t *testing.T) {
	type precedence struct {
		format        moirai.Format
		first, second string
	}
	cases := []precedence{
		{moirai.FormatPi, "PI_CODING_AGENT_SESSION_DIR", "PI_CODING_AGENT_DIR"},
		{moirai.FormatCampfire, "CAMPFIRE_CODING_AGENT_SESSION_DIR", "CAMPFIRE_CODING_AGENT_DIR"},
		{moirai.FormatAmp, "AMP_THREADS_DIR", "XDG_DATA_HOME"},
		{moirai.FormatOpenCode, "OPENCODE_DB", "XDG_DATA_HOME"},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases, precedence{moirai.FormatCowork, "COWORK_SESSIONS_DIR", "APPDATA"})
	}
	for _, c := range cases {
		t.Run(string(c.format), func(t *testing.T) {
			isolatedDoctorEnv(t)
			winner := filepath.Join(t.TempDir(), "winner")
			if c.first == "OPENCODE_DB" {
				winner += ".db"
			}
			t.Setenv(c.first, winner)
			t.Setenv(c.second, filepath.Join(t.TempDir(), "loser"))
			row := runDoctor(t).row(t, c.format)
			if row.Store != winner || row.ActiveOverride != c.first || !slices.Equal(row.StoreOverrides, []string{c.first, c.second}) {
				t.Fatalf("row: %+v", row)
			}
		})
	}
}

func TestDoctorOverridesCoverEveryStore(t *testing.T) {
	isolatedDoctorEnv(t)
	registry, err := moirai.DefaultStores()
	if err != nil {
		t.Fatal(err)
	}
	noOverride := map[moirai.Format]bool{moirai.FormatAntigravity: true, moirai.FormatCursor: true}
	for _, format := range moirai.Formats {
		if _, err := registry.Store(format); err != nil {
			if len(storeOverrides(format)) != 0 {
				t.Errorf("%s has no store but lists overrides", format)
			}
			continue
		}
		if listed := len(storeOverrides(format)) > 0; listed == noOverride[format] {
			t.Errorf("%s: overrides listed=%t, expected no-override=%t", format, listed, noOverride[format])
		}
	}
}

func TestDoctorScrubsHumanOutput(t *testing.T) {
	home, _ := isolatedDoctorEnv(t)
	hostile := filepath.Join(home, "abs\x1b[2Jent\a")
	t.Setenv("CODEX_HOME", hostile)
	result := runDoctor(t)
	if codex := result.row(t, moirai.FormatCodex); codex.Store != filepath.Join(hostile, "sessions") {
		t.Fatalf("JSON must keep the raw path, got %q", codex.Store)
	}
	if strings.ContainsAny(result.human, "\x1b\a") {
		t.Fatalf("human output leaks control characters:\n%q", result.human)
	}
	if !strings.Contains(result.human, "(CODEX_HOME is set)") {
		t.Fatalf("human output must name CODEX_HOME:\n%s", result.human)
	}
}

func TestDoctorRejectsArguments(t *testing.T) {
	isolatedDoctorEnv(t)
	for _, args := range [][]string{{"doctor", "extra"}, {"doctor", "--bogus"}} {
		var stdout, stderr bytes.Buffer
		a := app{out: &stdout, err: &stderr}
		if err := a.run(context.Background(), args); err == nil {
			t.Errorf("%v: expected an error", args)
		}
	}
}
