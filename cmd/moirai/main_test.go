package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	moirai "github.com/october-dev/moirai"
)

func TestFormatsAndInterspersedFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := app{out: &stdout, err: &stderr}
	if err := a.run(context.Background(), []string{"formats", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"claude_code"`) || !strings.Contains(stdout.String(), `"chatgpt"`) {
		t.Fatalf("unexpected formats: %s", stdout.String())
	}

	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(`{"id":"test","messages":[{"role":"user","content":"hello"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := a.run(context.Background(), []string{"inspect", path, "--from", "simple", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"messages": 1`) {
		t.Fatalf("unexpected inspect result: %s", stdout.String())
	}
}

func TestImportRehomesMissingWorkingDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	input := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(input, []byte(`{"id":"source","cwd":"/definitely/missing/moirai-project","messages":[{"role":"user","content":"hello"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := app{out: &stdout, err: &stderr}
	if err := a.run(context.Background(), []string{"import", input, "--to", "claude_code"}); err != nil {
		t.Fatal(err)
	}
	var saved moirai.SavedSession
	if err := json.Unmarshal(stdout.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if saved.Ref.CWD != cwd || !strings.Contains(stderr.String(), "cwd_rehomed") {
		t.Fatalf("saved=%#v stderr=%q", saved, stderr.String())
	}
}

func TestContinueClaudeUsesNativeProjectLayout(t *testing.T) {
	home := t.TempDir()
	claudeConfig := filepath.Join(home, "claude-config")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfig)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "share"))
	project := filepath.Join(t.TempDir(), "e2e.dot_proj")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "session.json")
	source := map[string]any{
		"id":        "source",
		"timestamp": "2026-09-02T00:00:00Z",
		"cwd":       project,
		"messages":  []any{map[string]any{"role": "user", "content": "continue this session"}},
	}
	data, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	a := app{out: &stdout, err: &stderr}
	if err := a.run(context.Background(), []string{"continue", input, "--from", "simple", "--with", "claude_code", "--no-launch"}); err != nil {
		t.Fatal(err)
	}
	var saved moirai.SavedSession
	if err := json.Unmarshal(stdout.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	encodedProject := strings.Map(func(char rune) rune {
		if char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			return char
		}
		return '-'
	}, project)
	wantedLocation := filepath.Join(encodedProject, saved.Ref.ID+".jsonl")
	if saved.Ref.Location != wantedLocation {
		t.Fatalf("location = %q, want %q", saved.Ref.Location, wantedLocation)
	}
	if _, err := os.Stat(filepath.Join(claudeConfig, "projects", wantedLocation)); err != nil {
		t.Fatalf("saved Claude session: %v", err)
	}
	command, err := moirai.CommandFor(moirai.FormatClaudeCode, saved.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if command.Program != "claude" || len(command.Args) != 2 || command.Args[0] != "--resume" || command.Args[1] != saved.Ref.ID || command.Dir != project {
		t.Fatalf("launch command = %#v", command)
	}
}

func TestHumanOutputScrubsTerminalControls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	root := filepath.Join(home, ".pi", "agent", "sessions", "--tmp-project--")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	data := strings.Join([]string{
		`{"type":"session","version":3,"id":"terminal-safe","timestamp":"2026-01-01T00:00:00Z","cwd":"/tmp/project"}`,
		`{"type":"message","id":"m1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"needle\u001b]52;c;cGF5bG9hZA==\u0007"}]}}`,
		`{"type":"session_info","name":"title\u001b[2J"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"list", "--format", "pi"}, {"show", "terminal-safe", "--format", "pi"}, {"search", "needle", "--format", "pi"}} {
		var stdout, stderr bytes.Buffer
		a := app{out: &stdout, err: &stderr}
		if err := a.run(context.Background(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if strings.ContainsRune(stdout.String(), '\x1b') || strings.ContainsRune(stdout.String(), '\a') {
			t.Fatalf("%v emitted terminal control bytes: %q", args, stdout.String())
		}
	}
}

func TestArchiveCreateAndVerify(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "session.json")
	archive := filepath.Join(dir, "session.moirai")
	if err := os.WriteFile(input, []byte(`{"id":"test","messages":[{"role":"user","content":"hello"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := app{out: &stdout, err: &stderr}
	if err := a.run(context.Background(), []string{"archive", "create", input, "--out", archive}); err != nil {
		t.Fatal(err)
	}
	if err := a.run(context.Background(), []string{"archive", "verify", archive}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"valid": true`) {
		t.Fatalf("unexpected verify result: %s", stdout.String())
	}
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	err := (app{out: &stdout, err: io.Discard}).run(context.Background(), args)
	return stdout.String(), err
}

func assertLines(t *testing.T, output string, lines []string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(output, line+"\n") {
			t.Errorf("missing line %q in %q", line, output)
		}
	}
}

func assertAbsent(t *testing.T, output string, values []string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(output, value) {
			t.Errorf("output contains %q: %q", value, output)
		}
	}
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestArchiveInspectFixture(t *testing.T) {
	const fixture = "../../testdata/archive-v1.moirai"
	want := archiveSummary{
		Format:        "moirai.session",
		Version:       "1",
		SchemaVersion: "1.0",
		CreatedAt:     "2026-09-01T23:12:51.344477Z",
		SHA256:        "61d8d16451a0aa06cfa6598e65ca6e79aa8385018ec4054ef09525710fe0e5e8",
		Valid:         true,
		ID:            "interop",
		Title:         "<portable> & verified",
		Timestamp:     "2026-01-01T00:00:00Z",
		CWD:           "/tmp/éxample",
		Messages:      3,
		Blocks:        map[moirai.BlockType]int{"text": 1, "thinking": 0, "tool_use": 1, "tool_result": 1, "image": 0, "artifact": 0, "unknown": 0},
	}
	// The fixture's message text, tool input keys, tool name, and tool result.
	content := []string{"café", "nested", "symbols", "calculate", `"ok"`}

	human, err := runCLI(t, "archive", "inspect", fixture)
	if err != nil {
		t.Fatal(err)
	}
	assertLines(t, human, []string{
		"Format: moirai.session 1 (schema 1.0)",
		"Transcript digest: sha256 " + want.SHA256 + " verified",
		"Created: " + want.CreatedAt,
		"ID: interop",
		"Title: <portable> & verified",
		"Timestamp: " + want.Timestamp,
		"Working directory: /tmp/éxample",
		"Messages: 3", "Blocks: 3",
		"  text: 1", "  thinking: 0", "  tool_use: 1", "  tool_result: 1", "  image: 0", "  artifact: 0", "  unknown: 0",
		"Warnings: 0",
	})
	assertAbsent(t, human, content)

	raw, err := runCLI(t, "archive", "inspect", fixture, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got archiveSummary
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("summary = %#v, want %#v", got, want)
	}
	assertAbsent(t, raw, content)
}

// archiveSentinelTranscript puts the token SENTINEL in every location that
// archive inspect must never print and keeps it out of every allowed field, so
// one substring check covers the whole exclusion list. The title carries a
// terminal control sequence and a newline that could forge an ID line.
func archiveSentinelTranscript() *moirai.Transcript {
	return &moirai.Transcript{
		SchemaVersion: moirai.SchemaVersion,
		Meta: moirai.Metadata{
			ID:            "allowlist",
			Timestamp:     "2026-01-01T00:00:00Z",
			UpdatedAt:     "2026-01-02T00:00:00Z",
			CWD:           "/work/project",
			GitBranch:     "BRANCH-SENTINEL",
			Title:         "Title\x1b[2J\nID: spoof",
			Model:         "model-x",
			ModelProvider: "PROVIDER-SENTINEL",
			CLIVersion:    "CLI-SENTINEL",
			Provenance: &moirai.Provenance{
				SourceFormat:     moirai.FormatClaudeCode,
				SourceSessionID:  "source-session",
				ImportedAt:       "2026-01-03T00:00:00Z",
				ParentSessionID:  "parent-session",
				ParentCheckpoint: "parent-checkpoint",
				SourceCWD:        "/source/cwd",
			},
			Extra: json.RawMessage(`{"meta":"META-EXTRA-SENTINEL"}`),
		},
		Messages: []moirai.Message{
			{
				ID:   "MESSAGE-ID-SENTINEL",
				Role: moirai.RoleUser,
				Content: []moirai.Block{
					{Type: moirai.BlockText, Text: "TEXT-SENTINEL"},
					{Type: moirai.BlockImage, Source: &moirai.MediaSource{Type: "base64", MediaType: "image/png", Data: "TUVESUEtU0VOVElORUw="}},
					{Type: moirai.BlockImage, Source: &moirai.MediaSource{Type: "url", URL: "https://example.invalid/URL-SENTINEL"}},
				},
				Extra: json.RawMessage(`{"message":"MESSAGE-EXTRA-SENTINEL"}`),
			},
			{
				Role:       moirai.RoleAssistant,
				StopReason: "STOP-SENTINEL",
				Usage:      &moirai.Usage{InputTokens: 987654321},
				Content: []moirai.Block{
					{Type: moirai.BlockThinking, Text: "THINKING-SENTINEL", Signature: "SIGNATURE-SENTINEL"},
					{Type: moirai.BlockThinking, Encrypted: "ENCRYPTED-SENTINEL"},
					{Type: moirai.BlockToolUse, ID: "call-1", Name: "TOOL-NAME-SENTINEL", Input: json.RawMessage(`{"arg":"TOOL-INPUT-SENTINEL"}`)},
					{Type: moirai.BlockArtifact, Artifact: &moirai.Artifact{Name: "ARTIFACT-NAME-SENTINEL", Description: "ARTIFACT-DESCRIPTION-SENTINEL", Source: &moirai.MediaSource{Type: "text", Text: "ARTIFACT-TEXT-SENTINEL"}}},
					{Type: moirai.BlockArtifact, Artifact: &moirai.Artifact{Name: "ARTIFACT-PATH-NAME-SENTINEL", Source: &moirai.MediaSource{Type: "path", Path: "/ARTIFACT-PATH-SENTINEL"}}},
				},
			},
			{
				Role: moirai.RoleUser,
				Content: []moirai.Block{
					{Type: moirai.BlockToolResult, ToolUseID: "call-1", Content: json.RawMessage(`"TOOL-RESULT-SENTINEL"`)},
					{Type: moirai.BlockUnknown, Data: json.RawMessage(`{"unknown":"UNKNOWN-SENTINEL"}`)},
				},
			},
		},
		Extra: json.RawMessage(`{"transcript":"TRANSCRIPT-EXTRA-SENTINEL"}`),
	}
}

func encodeSentinelArchive(t *testing.T) []byte {
	t.Helper()
	data, err := moirai.EncodeArchive(archiveSentinelTranscript(), moirai.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestArchiveInspectNeverPrintsContent(t *testing.T) {
	path := writeTempFile(t, "session.moirai", encodeSentinelArchive(t))
	// The image data is MEDIA-SENTINEL in base64; the usage count is a bare number.
	content := []string{"SENTINEL", "TUVESUEtU0VOVElORUw=", "987654321"}
	blocks := map[moirai.BlockType]int{"text": 1, "thinking": 2, "tool_use": 1, "tool_result": 1, "image": 2, "artifact": 2, "unknown": 1}

	human, err := runCLI(t, "archive", "inspect", path)
	if err != nil {
		t.Fatal(err)
	}
	assertLines(t, human, []string{
		"ID: allowlist",
		"Title: Title [2J ID: spoof",
		"Timestamp: 2026-01-01T00:00:00Z",
		"Updated: 2026-01-02T00:00:00Z",
		"Working directory: /work/project",
		"Model: model-x",
		"Source format: claude_code",
		"Source session: source-session",
		"Imported: 2026-01-03T00:00:00Z",
		"Parent session: parent-session",
		"Parent checkpoint: parent-checkpoint",
		"Source working directory: /source/cwd",
		"Messages: 3", "Blocks: 10",
		"  text: 1", "  thinking: 2", "  tool_use: 1", "  tool_result: 1", "  image: 2", "  artifact: 2", "  unknown: 1",
		"Warnings: 0",
	})
	if !strings.Contains(human, "Created: ") {
		t.Errorf("missing Created line in %q", human)
	}
	assertAbsent(t, human, append(content, "\x1b", "\nID: spoof"))

	raw, err := runCLI(t, "archive", "inspect", path, "--json")
	if err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, raw, content)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatal(err)
	}
	wantKeys := []string{"blocks", "created_at", "cwd", "format", "id", "messages", "model", "provenance", "schema_version", "sha256", "timestamp", "title", "updated_at", "valid", "version", "warnings"}
	if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, wantKeys) {
		t.Errorf("keys = %v, want %v", keys, wantKeys)
	}
	var got archiveSummary
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Blocks, blocks) || got.Messages != 3 || !got.Valid || got.CreatedAt == "" || len(got.SHA256) != 64 {
		t.Errorf("summary = %#v", got)
	}
	if !reflect.DeepEqual(got.Provenance, archiveSentinelTranscript().Meta.Provenance) {
		t.Errorf("provenance = %#v", got.Provenance)
	}
}

func TestArchiveInspectRejects(t *testing.T) {
	encoded := encodeSentinelArchive(t)
	tampered := bytes.Replace(encoded, []byte("TEXT-SENTINEL"), []byte("TEXT-ALTERED"), 1)
	versioned := bytes.Replace(encoded, []byte(`"version": "1"`), []byte(`"version": "2"`), 1)
	if bytes.Equal(tampered, encoded) || bytes.Equal(versioned, encoded) {
		t.Fatal("archive edits did not apply")
	}
	tests := []struct {
		name string
		data []byte
		args []string
		want error
	}{
		{name: "tampered", data: tampered, want: moirai.ErrIntegrity},
		{name: "version", data: versioned, want: moirai.ErrUnsupportedVersion},
		{name: "oversized", data: encoded, args: []string{"--max-input-bytes", "16"}, want: moirai.ErrLimitExceeded},
		{name: "session", data: []byte(`{"id":"x","messages":[]}`), want: moirai.ErrUnsupportedVersion},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempFile(t, tt.name+".moirai", tt.data)
			for _, output := range [][]string{nil, {"--json"}} {
				args := append([]string{"archive", "inspect", path}, tt.args...)
				stdout, err := runCLI(t, append(args, output...)...)
				if !errors.Is(err, tt.want) {
					t.Fatalf("%v: error %v, want %v", args, err, tt.want)
				}
				if stdout != "" {
					t.Fatalf("%v: stdout %q", args, stdout)
				}
			}
		})
	}
}

func TestListFilterApply(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "app")
	elsewhere := filepath.Join(root, "other")
	refs := []moirai.SessionRef{
		{ID: "a", CWD: project, ModifiedAt: "2026-09-08T12:00:00Z"},
		{ID: "b", CWD: filepath.Join(project, "pkg", "sub"), ModifiedAt: "2026-09-07T12:00:00Z"},
		{ID: "c", CWD: project + "2", ModifiedAt: "2026-09-06T12:00:00Z"},
		{ID: "d", ModifiedAt: "2026-09-05T12:00:00Z"},
		{ID: "e", CWD: elsewhere, Timestamp: "2026-09-04T12:00:00Z"},
		{ID: "f", CWD: elsewhere, ModifiedAt: "not-a-time", Timestamp: "2026-09-03T12:00:00Z"},
		{ID: "g", CWD: elsewhere, ModifiedAt: "2026-09-02T12:00:00Z", Timestamp: "2026-01-01T00:00:00Z"},
		{ID: "h", CWD: elsewhere, ModifiedAt: "2026-09-01T12:00:00.123456789Z"},
		{ID: "i", CWD: elsewhere, ModifiedAt: "2026-08-31T17:30:00+05:30"},
		{ID: "j", CWD: elsewhere},
	}
	all := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	cases := []struct {
		name                     string
		cwd, since, until, limit string
		want                     []string
	}{
		{name: "no filter preserves order", want: all},
		{name: "cwd matches the exact directory", cwd: filepath.Join(project, "pkg", "sub"), want: []string{"b"}},
		{name: "cwd includes descendants", cwd: project, want: []string{"a", "b"}},
		{name: "cwd accepts a trailing separator", cwd: project + string(filepath.Separator), want: []string{"a", "b"}},
		{name: "cwd excludes a sibling sharing the prefix", cwd: project + "2", want: []string{"c"}},
		{name: "cwd excludes sessions without a working directory", cwd: root, want: []string{"a", "b", "c", "e", "f", "g", "h", "i", "j"}},
		{name: "since is inclusive", since: "2026-09-07T12:00:00Z", want: []string{"a", "b"}},
		{name: "until is inclusive", until: "2026-09-02T12:00:00Z", want: []string{"g", "h", "i"}},
		{name: "since and until form a window", since: "2026-09-03T00:00:00Z", until: "2026-09-06T00:00:00Z", want: []string{"d", "e"}},
		{name: "until at the zero instant is an active bound", until: "0001-01-01T00:00:00Z"},
		{name: "bounds fall back to timestamp and drop malformed or missing times", since: "0001-01-01T00:00:00Z", want: []string{"a", "b", "c", "d", "e", "g", "h", "i"}},
		{name: "modified_at wins over a conflicting timestamp", since: "2026-09-02T12:00:00Z", until: "2026-09-02T12:00:00Z", want: []string{"g"}},
		{name: "fractional seconds parse", since: "2026-09-01T12:00:00Z", until: "2026-09-01T13:00:00Z", want: []string{"h"}},
		{name: "offsets compare as instants", since: "2026-08-31T12:00:00Z", until: "2026-08-31T12:00:00Z", want: []string{"i"}},
		{name: "limit keeps the first entries in order", limit: "3", want: []string{"a", "b", "c"}},
		{name: "limit above the result count keeps everything", limit: "99", want: all},
		{name: "limit applies after the other filters", cwd: elsewhere, since: "2026-09-01T00:00:00Z", limit: "2", want: []string{"e", "g"}},
		{name: "nothing matching returns nil", cwd: filepath.Join(root, "missing")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			filter, err := parseListFilter(tc.cwd, tc.since, tc.until, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			got := filter.apply(refs)
			if tc.want == nil && got != nil {
				t.Fatalf("got %#v, want nil", got)
			}
			ids := make([]string, len(got))
			for i, ref := range got {
				ids[i] = ref.ID
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("got %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestListFilterParse(t *testing.T) {
	failures := []struct {
		name                     string
		cwd, since, until, limit string
		want                     string
	}{
		{name: "malformed since", since: "yesterday", want: "--since"},
		{name: "date-only until", until: "2026-09-08", want: "--until"},
		{name: "since after until", since: "2026-09-08T00:00:00Z", until: "2026-09-01T00:00:00Z", want: "--since must not be after --until"},
		{name: "since after the zero instant", since: "0001-01-01T00:00:01Z", until: "0001-01-01T00:00:00Z", want: "--since must not be after --until"},
		{name: "zero limit", limit: "0", want: "--limit"},
		{name: "negative limit", limit: "-1", want: "--limit"},
		{name: "non-numeric limit", limit: "abc", want: "--limit"},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseListFilter(tc.cwd, tc.since, tc.until, tc.limit)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	filter, err := parseListFilter("", "", "", "")
	if err != nil || filter.cwd != "" || filter.since != nil || filter.until != nil || filter.limit != 0 {
		t.Fatalf("empty flags: filter = %#v, err = %v", filter, err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relative := strings.Join([]string{"sub", "..", "x"}, string(filepath.Separator))
	filter, err = parseListFilter(relative, "2026-09-01T00:00:00Z", "2026-09-01T00:00:00Z", "5")
	if err != nil {
		t.Fatal(err)
	}
	if filter.cwd != filepath.Join(wd, "x") || filter.since == nil || filter.until == nil || !filter.since.Equal(*filter.until) || filter.limit != 5 {
		t.Fatalf("filter = %#v", filter)
	}
}

func TestListFiltersOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	sessions := filepath.Join(home, "pi-sessions")
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", sessions)
	work := t.TempDir()
	project := filepath.Join(work, "app")
	elsewhere := filepath.Join(work, "other")
	// Write order, lexical order, and modification order all differ so the
	// assertions below can only pass through the registry's newest-first sort.
	for _, s := range []struct {
		id, cwd  string
		modified time.Time
	}{
		{"gamma", project, time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)},
		{"beta", elsewhere, time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},
		{"alpha", filepath.Join(project, "sub"), time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC)},
	} {
		header, err := json.Marshal(map[string]any{"type": "session", "version": 3, "id": s.id, "timestamp": "2026-09-01T00:00:00Z", "cwd": s.cwd})
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(sessions, "--"+s.id+"--")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, s.id+".jsonl")
		data := string(header) + "\n" + `{"type":"message","id":"m1","timestamp":"2026-09-01T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}` + "\n"
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, s.modified, s.modified); err != nil {
			t.Fatal(err)
		}
	}
	run := func(t *testing.T, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		a := app{out: &stdout, err: &stderr}
		if err := a.run(context.Background(), append([]string{"list", "--format", "pi"}, args...)); err != nil {
			t.Fatalf("%v: %v (stderr: %s)", args, err, stderr.String())
		}
		return stdout.String()
	}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"cwd", []string{"--cwd", project}, []string{"gamma", "alpha"}},
		{"window", []string{"--since", "2026-09-04T00:00:00Z", "--until", "2026-09-06T00:00:00Z"}, []string{"gamma"}},
		{"limit follows discovery order", []string{"--limit", "1"}, []string{"beta"}},
		{"combined", []string{"--cwd", project, "--since", "2026-09-02T00:00:00Z", "--limit", "1"}, []string{"gamma"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result struct {
				Sessions []moirai.SessionRef `json:"sessions"`
			}
			if err := json.Unmarshal([]byte(run(t, append(tc.args, "--json")...)), &result); err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, ref := range result.Sessions {
				ids = append(ids, ref.ID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Fatalf("got %v, want %v", ids, tc.want)
			}
		})
	}
	if out := run(t, "--cwd", filepath.Join(elsewhere, "nothing"), "--json"); !strings.Contains(out, `"sessions": null`) {
		t.Fatalf("zero-match JSON = %s", out)
	}
	lines := strings.Split(strings.TrimSpace(run(t, "--cwd", elsewhere)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "beta") {
		t.Fatalf("human output = %q", lines)
	}
	var stdout, stderr bytes.Buffer
	a := app{out: &stdout, err: &stderr}
	err := a.run(context.Background(), []string{"list", "--format", "pi", "--limit", "0"})
	if err == nil || !strings.Contains(err.Error(), "--limit") || stdout.Len() != 0 {
		t.Fatalf("err = %v, stdout = %q", err, stdout.String())
	}
}
