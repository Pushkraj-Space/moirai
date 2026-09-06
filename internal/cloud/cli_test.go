package cloud

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCLIShareJourney(t *testing.T) {
	if os.Getenv("MOIRAI_CLI_E2E") != "1" {
		t.Skip("set MOIRAI_CLI_E2E=1 to build and run the native CLI")
	}
	s, h, a, b := setup(t)
	server := httptest.NewUnstartedServer(h)
	s.Config.Origin = "http://" + server.Listener.Addr().String()
	server.Start()
	defer server.Close()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "moirai")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "./cmd/moirai")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	local := t.TempDir()
	store := filepath.Join(local, "claude")
	run := func(token string, args ...string) ([]byte, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "MOIRAI_SERVER="+server.URL, "MOIRAI_TOKEN="+token, "MOIRAI_CONFIG="+filepath.Join(local, "config.json"), "CLAUDE_CONFIG_DIR="+store)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Log(stderr.String())
		}
		return out, err
	}
	reviewed := filepath.Join(local, "reviewed.moirai")
	if _, err = run(a, "publish", "testdata/session-v1.json", "--preview-out", reviewed); err != nil {
		t.Fatal(err)
	}
	output, err := run(a, "publish", reviewed, "--yes")
	if err != nil {
		t.Fatal(err)
	}
	var publication Publication
	if err = json.Unmarshal(output, &publication); err != nil {
		t.Fatal(err)
	}
	downloaded := filepath.Join(local, "downloaded.moirai")
	if _, err = run(b, "pull", publication.URL, "--out", downloaded); err == nil {
		t.Fatal("unauthorized recipient downloaded private archive")
	}
	if _, err = run(a, "invite", publication.ID, "--login", "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err = run(b, "pull", publication.URL, "--out", downloaded); err != nil {
		t.Fatal(err)
	}
	if _, err = run(b, "continue", downloaded, "--with", "claude_code", "--dry-run", "--json"); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(store); !os.IsNotExist(err) {
		t.Fatal("dry-run touched store")
	}
	if _, err = run(b, "continue", downloaded, "--with", "claude_code", "--no-launch"); err != nil {
		t.Fatal(err)
	}
	if _, err = run(b, "fork", publication.URL, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err = run(a, "unpublish", publication.ID, "--yes"); err != nil {
		t.Fatal(err)
	}
	if _, err = run(b, "pull", publication.URL, "--out", filepath.Join(local, "revoked.moirai")); err == nil {
		t.Fatal("revoked archive downloaded")
	}
	t.Log("Native CLI journey passed: local review, private publish, access denial, invitation, pull, dry-run, native store import, fork and revoke.")
}
