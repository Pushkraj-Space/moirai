package cloud

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"testing"
)

func TestBackupRestoreDrill(t *testing.T) {
	if os.Getenv("MOIRAI_RECOVERY_TEST") != "1" || os.Getenv("MOIRAI_TEST_POSTGRES") == "" {
		t.Skip("requires the isolated deploy/compose.test.yaml Postgres service")
	}
	s, h, a, b := setup(t)
	private := publishFixture(t, h, a, "private")
	revoked := publishFixture(t, h, a, "unlisted")
	assertCode(t, request(t, h, "POST", "/v1/publications/"+revoked.ID+"/revoke", a, nil, ""), 204)
	var schema string
	if err := s.DB.QueryRow(`SELECT current_schema()`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	dump := exec.Command("docker", "compose", "-f", "deploy/compose.test.yaml", "exec", "-T", "postgres", "pg_dump", "-U", "moirai_test", "-d", "moirai_test", "--format=custom", "--schema", schema)
	dump.Dir = "../.."
	backup, err := dump.Output()
	if err != nil {
		t.Fatal(err)
	}
	database := "restore_" + randomID()
	if _, err = s.DB.Exec("CREATE DATABASE " + database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.DB.Exec("DROP DATABASE " + database + " WITH (FORCE)") })
	restore := exec.Command("docker", "compose", "-f", "deploy/compose.test.yaml", "exec", "-T", "postgres", "pg_restore", "-U", "moirai_test", "--exit-on-error", "-d", database)
	restore.Dir = "../.."
	restore.Stdin = bytes.NewReader(backup)
	if output, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore: %v %s", err, output)
	}
	dsn, err := url.Parse(os.Getenv("MOIRAI_TEST_POSTGRES"))
	if err != nil {
		t.Fatal(err)
	}
	dsn.Path = "/" + database
	q := dsn.Query()
	q.Set("search_path", schema)
	dsn.RawQuery = q.Encode()
	db, err := OpenDB(context.Background(), "pgx", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := Files{t.TempDir()}
	for _, id := range []string{private.ID, revoked.ID} {
		data, err := s.Blobs.(*EncryptedBlobs).Base.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if err = base.Put(context.Background(), id, data); err != nil {
			t.Fatal(err)
		}
	}
	blobs, err := EncryptBlobs(base, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := New(db, blobs, s.Config)
	if err != nil {
		t.Fatal(err)
	}
	recovered.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := recovered.Handler()
	assertCode(t, request(t, handler, "GET", "/v1/publications/"+private.ID+"/archive", a, nil, ""), 200)
	assertCode(t, request(t, handler, "GET", "/v1/publications/"+private.ID+"/archive", b, nil, ""), 404)
	assertCode(t, request(t, handler, "GET", "/s/"+revoked.ID, "", nil, ""), 404)
	t.Log("Restored database and encrypted blobs into isolated storage; owner access, tenant isolation and revocation survived.")
}
