//go:build unix

package main

import (
	"os"
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
		t.Skip("permission checks always pass for root")
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
	if result.hasKey(moirai.FormatClaudeCode, "exists") || !ok || failed.Path != row.Store || !strings.Contains(failed.Message, "permission denied") {
		t.Fatalf("row: %+v", row)
	}
	if !strings.Contains(result.human, "  status: unknown\n") {
		t.Fatalf("human output:\n%s", result.human)
	}
}

func TestDoctorNeverOpensFIFO(t *testing.T) {
	home, _ := isolatedDoctorEnv(t)
	fifo := filepath.Join(home, "opencode.db")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_DB", fifo)
	type outcome struct {
		rows []doctorRow
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		rows, _, err := doctorJSON()
		done <- outcome{rows, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		index := slices.IndexFunc(got.rows, func(row doctorRow) bool { return row.Format == moirai.FormatOpenCode })
		if index < 0 {
			t.Fatal("no opencode row")
		}
		row := got.rows[index]
		wrong, ok := findWarning(row, "store_wrong_type")
		if !isTrue(row.Exists) || row.Readable != nil || row.Writable != nil || !ok || !strings.Contains(wrong.Message, "named pipe") {
			t.Fatalf("row: %+v", row)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("doctor blocked: it must never open a FIFO store root")
	}
}
