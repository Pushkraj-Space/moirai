package cloud

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBrowserJourney(t *testing.T) {
	if os.Getenv("MOIRAI_BROWSER_TESTS") != "1" {
		t.Skip("set MOIRAI_BROWSER_TESTS=1 after installing Playwright Chromium")
	}
	s, h, token, _ := setup(t)
	server := httptest.NewUnstartedServer(h)
	s.Config.Origin = "http://" + server.Listener.Addr().String()
	server.Start()
	defer server.Close()
	command := exec.Command("node", "tests/browser/e2e.mjs")
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	command.Dir = root
	command.Env = append(os.Environ(), "MOIRAI_TEST_ORIGIN="+server.URL, "MOIRAI_TEST_TOKEN="+token)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	} else {
		t.Log(string(output))
	}
}
