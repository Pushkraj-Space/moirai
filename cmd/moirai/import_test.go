package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	moirai "github.com/october-dev/moirai"
)

// Only Root and Save are used for destination stores during import. Any
// unexpected call to another embedded Store method fails the test.
type importTestStore struct {
	moirai.Store
	root string
	save func(context.Context, *moirai.Transcript, moirai.RenderOptions) (*moirai.SavedSession, error)
}

func (s importTestStore) Format() moirai.Format { return moirai.FormatClaudeCode }
func (s importTestStore) Root() string          { return s.root }
func (s importTestStore) Save(ctx context.Context, transcript *moirai.Transcript, opts moirai.RenderOptions) (*moirai.SavedSession, error) {
	return s.save(ctx, transcript, opts)
}

func isolatedImportEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("PATH", t.TempDir())
}

func decodeDryRunReport(t *testing.T, output []byte) map[string]any {
	t.Helper()
	var report map[string]any
	if err := json.Unmarshal(output, &report); err != nil {
		t.Fatalf("decode preview: %v\n%s", err, output)
	}
	return report
}

func TestDryRunDoesNotSaveOrLaunch(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
	}{
		{"import", []string{"import", "../../testdata/session-v1.json", "--to", "claude_code"}},
		{"continue", []string{"continue", "../../testdata/session-v1.json", "--with", "claude_code"}},
		{"continue without launch", []string{"continue", "../../testdata/session-v1.json", "--with", "claude_code", "--no-launch"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolatedImportEnv(t)
			store := importTestStore{
				root: filepath.Join(t.TempDir(), "destination"),
				save: func(context.Context, *moirai.Transcript, moirai.RenderOptions) (*moirai.SavedSession, error) {
					t.Fatal("dry-run called Save")
					return nil, nil
				},
			}
			registryCalls, launches := 0, 0
			var out, stderr bytes.Buffer
			a := app{
				out: &out, err: &stderr,
				newStores: func() (*moirai.StoreRegistry, error) {
					registryCalls++
					return moirai.NewStoreRegistry(store), nil
				},
				launch: func(context.Context, moirai.LaunchCommand) error {
					launches++
					return nil
				},
			}
			args := append(c.args, "--dry-run", "--json")
			if err := a.run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			report := decodeDryRunReport(t, out.Bytes())
			if report["dry_run"] != true || report["launch"] != false || report["destination_store"] != store.root {
				t.Fatalf("preview: %s", out.Bytes())
			}
			if span, ok := report["range"]; !ok || span != nil {
				t.Fatalf("file input must report range: null: %s", out.Bytes())
			}
			if report["source_format"] != "simple" || report["target_format"] != "claude_code" || report["messages"] != float64(1) {
				t.Fatalf("conversion fields: %s", out.Bytes())
			}
			if size, ok := report["rendered_bytes"].(float64); !ok || size <= 0 {
				t.Fatalf("preview must render the transcript: %s", out.Bytes())
			}
			for _, key := range []string{"warnings", "provenance"} {
				if _, ok := report[key]; !ok {
					t.Errorf("preview is missing %s", key)
				}
			}
			if registryCalls != 1 || launches != 0 {
				t.Fatalf("registry calls = %d, launches = %d; want 1, 0", registryCalls, launches)
			}
		})
	}
}

func TestContinueSavesAndLaunchesWithInjectedDependencies(t *testing.T) {
	isolatedImportEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ref := moirai.SessionRef{Format: moirai.FormatClaudeCode, ID: "saved-by-spy", CWD: t.TempDir()}
	saves, launches := 0, 0
	store := importTestStore{
		root: filepath.Join(t.TempDir(), "destination"),
		save: func(gotCtx context.Context, transcript *moirai.Transcript, opts moirai.RenderOptions) (*moirai.SavedSession, error) {
			saves++
			if gotCtx != ctx || len(transcript.Messages) != 1 || opts.ID == "" || opts.ID != transcript.Meta.ID {
				t.Fatalf("unexpected Save arguments: transcript=%+v opts=%+v", transcript, opts)
			}
			return &moirai.SavedSession{Ref: ref, Created: true}, nil
		},
	}
	var out, stderr bytes.Buffer
	a := app{
		out: &out, err: &stderr,
		newStores: func() (*moirai.StoreRegistry, error) {
			return moirai.NewStoreRegistry(store), nil
		},
		launch: func(gotCtx context.Context, command moirai.LaunchCommand) error {
			launches++
			if gotCtx != ctx || saves != 1 || command.Program != "claude" || !slices.Equal(command.Args, []string{"--resume", ref.ID}) || command.Dir != ref.CWD {
				t.Fatalf("launch must follow Save and use its ref: saves=%d command=%+v", saves, command)
			}
			return nil
		},
	}
	if err := a.run(ctx, []string{"continue", "../../testdata/session-v1.json", "--with", "claude_code"}); err != nil {
		t.Fatal(err)
	}
	if saves != 1 || launches != 1 {
		t.Fatalf("saves = %d, launches = %d; want 1, 1", saves, launches)
	}
}

func TestDryRunReportsStoredRange(t *testing.T) {
	isolatedImportEnv(t)
	var out, stderr bytes.Buffer
	a := app{out: &out, err: &stderr}
	if err := a.run(context.Background(), []string{"import", "../../testdata/import-range.json", "--to", "claude_code", "--no-launch"}); err != nil {
		t.Fatal(err)
	}
	var saved moirai.SavedSession
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Ref.ID == "" {
		t.Fatal("seed import did not return a session ID")
	}
	for _, c := range []struct {
		name, suffix string
		span         *moirai.Span
		messages     int
		human        string
	}{
		{"bounded", "#3-4", &moirai.Span{Start: 3, End: 4}, 2, "Range: messages 3-4\n"},
		{"open ended", "#3-", &moirai.Span{Start: 3, End: 6}, 4, "Range: messages 3-6\n"},
		{"all messages", "", nil, 6, "Range: all 6 messages\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := []string{"continue", saved.Ref.ID + c.suffix, "--from", "claude_code", "--with", "claude_code", "--dry-run"}
			out.Reset()
			stderr.Reset()
			if err := a.run(context.Background(), append(args, "--json")); err != nil {
				t.Fatal(err)
			}
			report := decodeDryRunReport(t, out.Bytes())
			if report["messages"] != float64(c.messages) {
				t.Fatalf("message count: %s", out.Bytes())
			}
			span, ok := report["range"]
			if !ok {
				t.Fatal("preview is missing range")
			}
			if c.span == nil {
				if span != nil {
					t.Fatalf("no selector span must report range: null, got %v", span)
				}
			} else if bounds, ok := span.(map[string]any); !ok || bounds["start"] != float64(c.span.Start) || bounds["end"] != float64(c.span.End) {
				t.Fatalf("range = %v, want %+v", span, *c.span)
			}
			out.Reset()
			stderr.Reset()
			if err := a.run(context.Background(), args); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "\n"+c.human) {
				t.Fatalf("human preview is missing %q:\n%s", c.human, out.String())
			}
		})
	}
}

func TestDryRunWarningParity(t *testing.T) {
	isolatedImportEnv(t)
	var out, stderr bytes.Buffer
	a := app{out: &out, err: &stderr}
	args := []string{"import", "../../testdata/native/claude_code.jsonl", "--from", "claude_code", "--to", "codex"}
	if err := a.run(context.Background(), append(args, "--dry-run", "--json")); err != nil {
		t.Fatal(err)
	}
	var preview struct {
		Warnings []moirai.Warning `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	wantWarning := moirai.Warning{
		Code:    "unsupported_block",
		Path:    "messages[0].content[1]",
		Message: "codex cannot represent image content; block omitted",
	}
	if !slices.Contains(preview.Warnings, wantWarning) {
		t.Fatalf("dry-run is missing the image conversion warning: %+v", preview.Warnings)
	}
	out.Reset()
	stderr.Reset()
	if err := a.run(context.Background(), append(args, "--no-launch")); err != nil {
		t.Fatal(err)
	}
	var saved moirai.SavedSession
	if err := json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Created || saved.Ref.Format != moirai.FormatCodex {
		t.Fatalf("real import did not create the target session: %+v", saved)
	}
	// Omitted-record warnings come from a map. Compare multisets, retaining
	// duplicates, rather than relying on their iteration order.
	compare := func(a, b moirai.Warning) int {
		return cmp.Or(cmp.Compare(a.Code, b.Code), cmp.Compare(a.Path, b.Path), cmp.Compare(a.Message, b.Message))
	}
	slices.SortFunc(preview.Warnings, compare)
	slices.SortFunc(saved.Warnings, compare)
	if !slices.Equal(preview.Warnings, saved.Warnings) {
		t.Fatalf("warning mismatch:\ndry-run: %+v\nreal:    %+v", preview.Warnings, saved.Warnings)
	}
}

func TestDryRunScrubsDestination(t *testing.T) {
	isolatedImportEnv(t)
	store := importTestStore{
		root: "store\x1b[2J\a",
		save: func(context.Context, *moirai.Transcript, moirai.RenderOptions) (*moirai.SavedSession, error) {
			t.Fatal("dry-run called Save")
			return nil, nil
		},
	}
	var out, stderr bytes.Buffer
	a := app{
		out: &out, err: &stderr,
		newStores: func() (*moirai.StoreRegistry, error) {
			return moirai.NewStoreRegistry(store), nil
		},
		launch: func(context.Context, moirai.LaunchCommand) error {
			t.Fatal("dry-run called launcher")
			return nil
		},
	}
	if err := a.run(context.Background(), []string{"continue", "../../testdata/session-v1.json", "--with", "claude_code", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String()+stderr.String(), "\x1b\a") {
		t.Fatalf("preview leaks terminal controls: stdout=%q stderr=%q", out.String(), stderr.String())
	}
	for _, line := range []string{"Destination: " + moirai.ScrubTerminal(store.root) + "\n", "Range: all 1 messages\n", "Launch: false\n"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("human preview is missing %q:\n%s", line, out.String())
		}
	}
}

func TestDryRunDoesNotParseFilenameAsSelector(t *testing.T) {
	isolatedImportEnv(t)
	data, err := os.ReadFile("../../testdata/session-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session#not-a-range.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := app{out: &out, err: &stderr}
	if err := a.run(context.Background(), []string{"import", path, "--from", "simple", "--to", "claude_code", "--dry-run", "--json"}); err != nil {
		t.Fatal(err)
	}
	report := decodeDryRunReport(t, out.Bytes())
	if span, ok := report["range"]; !ok || span != nil || report["messages"] != float64(1) {
		t.Fatalf("filename containing # must report all messages and range: null: %s", out.Bytes())
	}
}
