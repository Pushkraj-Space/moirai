package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	moirai "github.com/october-dev/moirai"
)

func TestDownloadNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.moirai")
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- writeNewArchive(path, []byte("complete archive")) }()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("got %d successful writers, want 1", winners)
	}
	if err := writeNewArchive(path, []byte("replacement")); err == nil {
		t.Fatal("overwrote destination")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "complete archive" {
		t.Fatalf("destination changed: %q %v", data, err)
	}
}

func TestDryRunCreatesNoStore(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	var out, stderr bytes.Buffer
	a := app{out: &out, err: &stderr}
	for _, command := range []string{"import", "continue"} {
		flag := "--to"
		if command == "continue" {
			flag = "--with"
		}
		out.Reset()
		err := a.run(context.Background(), []string{command, "../../testdata/session-v1.json", flag, "claude_code", "--dry-run", "--json"})
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if json.Unmarshal(out.Bytes(), &result) != nil || result["dry_run"] != true {
			t.Fatalf("report: %s", out.String())
		}
		if _, err = os.Stat(root); !os.IsNotExist(err) {
			t.Fatal("dry run created destination")
		}
	}
}
func TestArchiveContinueAndPublishPreview(t *testing.T) {
	root := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	var out, stderr bytes.Buffer
	a := app{out: &out, err: &stderr}
	preview := filepath.Join(t.TempDir(), "reviewed.moirai")
	if err := a.run(context.Background(), []string{"publish", "../../testdata/session-v1.json", "--preview-out", preview}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(preview)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := moirai.DecodeArchive(data, moirai.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Meta.CWD != "" {
		t.Fatal("workspace leaked")
	}
	out.Reset()
	if err = a.run(context.Background(), []string{"continue", preview, "--with", "claude_code", "--no-launch"}); err != nil {
		t.Fatal(err)
	}
	var saved moirai.SavedSession
	if err = json.Unmarshal(out.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Ref.ID == tr.Meta.ID {
		t.Fatal("source identity reused")
	}
}
func TestCloudTokensNeverFollowRedirectOrDifferentOrigin(t *testing.T) {
	t.Setenv("MOIRAI_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("MOIRAI_SERVER", "")
	t.Setenv("MOIRAI_TOKEN", "")
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	c, err := loadCloud(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	c.config.Token = strings.Repeat("a", 48)
	if _, _, err = c.request(context.Background(), "GET", "/v1/me", nil, ""); err == nil {
		t.Fatal("redirect accepted")
	}
	if reached {
		t.Fatal("followed credential redirect")
	}
	if _, err = c.publicationID(target.URL + "/s/" + strings.Repeat("a", 48)); err == nil {
		t.Fatal("other origin accepted")
	}
}
