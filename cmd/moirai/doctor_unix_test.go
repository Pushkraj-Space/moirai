//go:build unix

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	moirai "github.com/october-dev/moirai"
	"golang.org/x/sys/unix"
)

func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root can bypass ordinary mode permission checks")
	}
}

// restrict changes the mode of path and restores 0o700 on cleanup so the
// temporary directory can be removed.
func restrict(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o700) })
}

// claudeConfig creates an isolated Claude Code configuration directory with a
// projects directory inside it and returns both paths.
func claudeConfig(t *testing.T) (config, projects string) {
	t.Helper()
	home, _ := isolatedDoctorEnv(t)
	config = filepath.Join(home, "claude-config")
	projects = filepath.Join(config, "projects")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", config)
	return config, projects
}

func TestDoctorDirectoryPermissions(t *testing.T) {
	skipAsRoot(t)
	cases := []struct {
		name               string
		mode               os.FileMode
		readable, writable bool
		codes              []string
		status             string
	}{
		{"read only", 0o500, true, false, []string{"store_unwritable"}, "readable, not writable"},
		{"no search bit", 0o600, true, false, []string{"store_unwritable"}, "readable, not writable"},
		{"write only", 0o300, false, true, []string{"store_unreadable"}, "not readable, writable"},
		{"no access", 0o000, false, false, []string{"store_unreadable", "store_unwritable"}, "not readable, not writable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, projects := claudeConfig(t)
			restrict(t, projects, c.mode)
			result := runDoctor(t)
			row := result.row(t, moirai.FormatClaudeCode)
			if !isTrue(row.Exists) || row.Readable == nil || *row.Readable != c.readable || row.Writable == nil || *row.Writable != c.writable {
				t.Fatalf("row: %+v", row)
			}
			codes := warningCodes(row)
			if index := slices.Index(codes, "executable_missing"); index >= 0 {
				codes = slices.Delete(codes, index, index+1)
			}
			if !slices.Equal(codes, c.codes) {
				t.Fatalf("warnings %v, want %v", codes, c.codes)
			}
			if !strings.Contains(result.human, "  store: "+projects+" (CLAUDE_CONFIG_DIR is set)\n  status: "+c.status+"\n") {
				t.Fatalf("human output:\n%s", result.human)
			}
		})
	}
}

func TestDoctorReportsStatFailure(t *testing.T) {
	skipAsRoot(t)
	config, _ := claudeConfig(t)
	restrict(t, config, 0o000)
	result := runDoctor(t)
	row := result.row(t, moirai.FormatClaudeCode)
	failed, ok := findWarning(row, "store_stat_failed")
	if !ok || failed.Path != row.Store || !strings.Contains(failed.Message, "permission denied") {
		t.Fatalf("row: %+v", row)
	}
	for _, key := range []string{"exists", "readable", "writable"} {
		if result.hasKey(moirai.FormatClaudeCode, key) {
			t.Errorf("stat failure must omit %s: %v", key, result.raw[moirai.FormatClaudeCode])
		}
	}
	if !strings.Contains(result.human, "  status: unknown\n") {
		t.Fatalf("human output:\n%s", result.human)
	}
}

func TestDoctorNeverOpensFIFO(t *testing.T) {
	for _, c := range []struct {
		format   moirai.Format
		variable string
	}{
		{moirai.FormatPi, "PI_CODING_AGENT_SESSION_DIR"},
		{moirai.FormatOpenCode, "OPENCODE_DB"},
	} {
		for _, kind := range []string{"direct", "symlink"} {
			t.Run(string(c.format)+"/"+kind, func(t *testing.T) {
				home, _ := isolatedDoctorEnv(t)
				fifo := filepath.Join(home, "fifo")
				if err := unix.Mkfifo(fifo, 0o600); err != nil {
					t.Fatal(err)
				}
				root := fifo
				if kind == "symlink" {
					root = filepath.Join(home, "link")
					if err := os.Symlink(fifo, root); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv(c.variable, root)
				// Use a child process so a regression can be killed without
				// leaving a blocked goroutine behind during environment cleanup.
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, binary, "-test.run=^TestDoctorFIFOProcess$")
				cmd.Env = append(os.Environ(), "MOIRAI_TEST_DOCTOR_FIFO=1")
				output, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatal("doctor blocked: it must never open a FIFO store root")
				}
				if err != nil {
					t.Fatalf("doctor subprocess: %v\n%s", err, output)
				}
				var rows []doctorRow
				if err := json.Unmarshal(output, &rows); err != nil {
					t.Fatalf("doctor JSON: %v\n%s", err, output)
				}
				index := slices.IndexFunc(rows, func(row doctorRow) bool { return row.Format == c.format })
				if index < 0 {
					t.Fatalf("no %s row", c.format)
				}
				row := rows[index]
				wrong, ok := findWarning(row, "store_wrong_type")
				if !isTrue(row.Exists) || row.Readable != nil || row.Writable != nil || !ok || !strings.Contains(wrong.Message, "named pipe") {
					t.Fatalf("row: %+v", row)
				}
				var raw []map[string]json.RawMessage
				if err := json.Unmarshal(output, &raw); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{"readable", "writable"} {
					if _, ok := raw[index][key]; ok {
						t.Errorf("FIFO root must omit %s: %v", key, raw[index])
					}
				}
			})
		}
	}
}

func TestDoctorFIFOProcess(t *testing.T) {
	if os.Getenv("MOIRAI_TEST_DOCTOR_FIFO") != "1" {
		return
	}
	a := app{out: os.Stdout, err: os.Stderr}
	if err := a.run(context.Background(), []string{"doctor", "--json"}); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func TestDoctorSymlinkLoopHasUnknownStatus(t *testing.T) {
	home, _ := isolatedDoctorEnv(t)
	root := filepath.Join(home, "opencode.db")
	if err := os.Symlink(root, root); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DB", root)
	result := runDoctor(t)
	row := result.row(t, moirai.FormatOpenCode)
	if warning, ok := findWarning(row, "store_stat_failed"); !ok || warning.Path != root {
		t.Fatalf("symlink loop must report stat failure: %+v", row)
	}
	for _, key := range []string{"exists", "readable", "writable"} {
		if result.hasKey(moirai.FormatOpenCode, key) {
			t.Errorf("stat failure must omit %s: %v", key, result.raw[moirai.FormatOpenCode])
		}
	}
}

func TestDoctorHealthyDirectoryIsWritable(t *testing.T) {
	_, projects := claudeConfig(t)
	result := runDoctor(t)
	row := result.row(t, moirai.FormatClaudeCode)
	if !isTrue(row.Exists) || !isTrue(row.Readable) || !isTrue(row.Writable) {
		t.Fatalf("healthy directory: %+v", row)
	}
	if !strings.Contains(result.human, "  store: "+projects+" (CLAUDE_CONFIG_DIR is set)\n  status: readable, writable\n") {
		t.Fatalf("healthy directory status:\n%s", result.human)
	}
}
