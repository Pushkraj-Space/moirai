package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	moirai "github.com/october-dev/moirai"
)

func setup(t *testing.T) (*Server, http.Handler, string, string) {
	t.Helper()
	driver, dsn := "sqlite", filepath.Join(t.TempDir(), "test.db")
	if configured := os.Getenv("MOIRAI_TEST_POSTGRES"); configured != "" {
		driver = "pgx"
		admin, err := OpenDB(context.Background(), driver, configured)
		if err != nil {
			t.Fatal(err)
		}
		schema := "test_" + randomID()
		if _, err = admin.Exec("CREATE SCHEMA " + schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec("DROP SCHEMA " + schema + " CASCADE"); admin.Close() })
		u, err := url.Parse(configured)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	}
	db, err := OpenDB(context.Background(), driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = Migrate(context.Background(), db, driver); err != nil {
		t.Fatal(err)
	}
	blobs, err := EncryptBlobs(Files{t.TempDir()}, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(db, blobs, Config{Origin: "http://127.0.0.1:8080", Development: true, GitHubClientID: "test-client", GitHubClientSecret: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"id":2,"login":"bob"}`)), Header: make(http.Header)}, nil
	})}
	a, b := randomID(), randomID()
	for _, u := range []struct{ id, login, token string }{{"1", "alice", a}, {"2", "bob", b}} {
		if _, err = db.Exec(`INSERT INTO users(id,login) VALUES($1,$2)`, u.id, u.login); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO tokens(hash,user_id,expires) VALUES($1,$2,$3)`, hash(u.token), u.id, time.Now().Add(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	return s, s.Handler(), a, b
}
func request(t *testing.T, h http.Handler, method, path, token string, body any, key string) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.RemoteAddr = "127.0.0.1:12345"
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func archiveFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../testdata/archive-v1.moirai")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func publishFixture(t *testing.T, h http.Handler, token, visibility string) Publication {
	t.Helper()
	w := request(t, h, "POST", "/v1/publications", token, PublishRequest{Archive: archiveFixture(t), Visibility: visibility}, randomID())
	if w.Code != 201 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	var p Publication
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	return p
}
func assertCode(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status=%d want=%d body=%s", w.Code, want, w.Body.String())
	}
}

func TestAccessLifecycle(t *testing.T) {
	s, h, a, b := setup(t)
	p := publishFixture(t, h, a, "private")
	base := "/v1/publications/" + p.ID
	for _, route := range []string{base, base + "/archive", "/s/" + p.ID} {
		for _, token := range []string{"", b} {
			assertCode(t, request(t, h, "GET", route, token, nil, ""), 404)
		}
		assertCode(t, request(t, h, "GET", route, a, nil, ""), 200)
	}
	for _, route := range []string{base + "/revoke", base + "/grants"} {
		assertCode(t, request(t, h, "POST", route, b, map[string]string{"login": "bob"}, ""), 404)
	}
	assertCode(t, request(t, h, "DELETE", base, b, nil, ""), 404)
	assertCode(t, request(t, h, "POST", base+"/grants", a, map[string]string{"login": "bob"}, ""), 204)
	assertCode(t, request(t, h, "GET", base+"/archive", b, nil, ""), 200)
	assertCode(t, request(t, h, "DELETE", base+"/grants/2", a, nil, ""), 204)
	assertCode(t, request(t, h, "GET", base+"/archive", b, nil, ""), 404)
	assertCode(t, request(t, h, "POST", base+"/revoke", a, nil, ""), 204)
	assertCode(t, request(t, h, "GET", base+"/archive", a, nil, ""), 404)
	assertCode(t, request(t, h, "DELETE", base, a, nil, ""), 204)
	assertCode(t, request(t, h, "DELETE", base, a, nil, ""), 204)
	if _, err := s.Blobs.Get(context.Background(), p.ID); err == nil {
		t.Fatal("deleted blob still present")
	}
	var used, count int
	if err := s.DB.QueryRow(`SELECT bytes_used,publications FROM users WHERE id='1'`).Scan(&used, &count); err != nil {
		t.Fatal(err)
	}
	if used != 0 || count != 0 {
		t.Fatalf("quota not released: %d %d", used, count)
	}
}
func TestPublicExpiryAndFork(t *testing.T) {
	s, h, a, b := setup(t)
	for _, visibility := range []string{"public", "unlisted"} {
		p := publishFixture(t, h, a, visibility)
		assertCode(t, request(t, h, "GET", "/v1/publications/"+p.ID+"/archive", "", nil, ""), 200)
		if _, err := s.DB.Exec(`UPDATE publications SET expires=$1 WHERE id=$2`, time.Now().Unix()-1, p.ID); err != nil {
			t.Fatal(err)
		}
		assertCode(t, request(t, h, "GET", "/s/"+p.ID, "", nil, ""), 404)
	}
	parent := publishFixture(t, h, a, "private")
	body := PublishRequest{Archive: archiveFixture(t), Visibility: "public", Parent: parent.ID}
	assertCode(t, request(t, h, "POST", "/v1/publications", b, body, randomID()), 404)
	w := request(t, h, "POST", "/v1/publications", a, body, randomID())
	assertCode(t, w, 201)
	var fork Publication
	json.Unmarshal(w.Body.Bytes(), &fork)
	w = request(t, h, "GET", "/v1/publications/"+fork.ID, b, nil, "")
	assertCode(t, w, 200)
	if strings.Contains(w.Body.String(), parent.ID) {
		t.Fatal("private parent ID disclosed")
	}
}
func TestIdempotencyAndQuotas(t *testing.T) {
	s, h, a, _ := setup(t)
	body := PublishRequest{Archive: archiveFixture(t), Visibility: "private"}
	key := randomID()
	first := request(t, h, "POST", "/v1/publications", a, body, key)
	assertCode(t, first, 201)
	second := request(t, h, "POST", "/v1/publications", a, body, key)
	assertCode(t, second, 200)
	var p, q Publication
	json.Unmarshal(first.Body.Bytes(), &p)
	json.Unmarshal(second.Body.Bytes(), &q)
	if p.ID != q.ID {
		t.Fatal("retry created another publication")
	}
	body.Visibility = "public"
	assertCode(t, request(t, h, "POST", "/v1/publications", a, body, key), 409)
	if _, err := s.DB.Exec(`UPDATE users SET publications=100 WHERE id='1'`); err != nil {
		t.Fatal(err)
	}
	assertCode(t, request(t, h, "POST", "/v1/publications", a, body, randomID()), 413)
	body.Archive = json.RawMessage(`{"format":"invalid"}`)
	assertCode(t, request(t, h, "POST", "/v1/publications", a, body, randomID()), 400)
}
func TestConcurrentQuotaReservation(t *testing.T) {
	s, h, a, _ := setup(t)
	if _, err := s.DB.Exec(`UPDATE users SET publications=99 WHERE id='1'`); err != nil {
		t.Fatal(err)
	}
	body := PublishRequest{Archive: archiveFixture(t), Visibility: "private"}
	var wg sync.WaitGroup
	codes := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- request(t, h, "POST", "/v1/publications", a, body, randomID()).Code }()
	}
	wg.Wait()
	close(codes)
	successes := 0
	for code := range codes {
		if code == 201 {
			successes++
		} else if code != 413 && code != 503 {
			t.Fatalf("unexpected status %d", code)
		}
	}
	if successes != 1 {
		t.Fatalf("quota allowed %d publications", successes)
	}
}
func TestBrowserCSRFAndTokens(t *testing.T) {
	s, h, a, _ := setup(t)
	r := httptest.NewRequest("POST", "/v1/logout", nil)
	r.AddCookie(&http.Cookie{Name: "moirai_session", Value: a})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertCode(t, w, 403)
	r = httptest.NewRequest("POST", "/v1/logout", nil)
	r.AddCookie(&http.Cookie{Name: "moirai_session", Value: a})
	r.Header.Set("Origin", s.Config.Origin)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertCode(t, w, 204)
	assertCode(t, request(t, h, "GET", "/v1/me", a, nil, ""), 401)
}
func TestDeviceApproval(t *testing.T) {
	s, h, a, _ := setup(t)
	w := request(t, h, "POST", "/v1/auth/device", "", nil, "")
	assertCode(t, w, 201)
	var device struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	json.Unmarshal(w.Body.Bytes(), &device)
	assertCode(t, request(t, h, "GET", "/v1/auth/device/"+device.ID, device.Token, nil, ""), 202)
	assertCode(t, request(t, h, "GET", "/v1/auth/device/"+device.ID, a, nil, ""), 404)
	r := httptest.NewRequest("POST", "/connect", strings.NewReader(url.Values{"id": {device.ID}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", s.Config.Origin)
	r.AddCookie(&http.Cookie{Name: "moirai_session", Value: a})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertCode(t, w, 200)
	assertCode(t, request(t, h, "GET", "/v1/auth/device/"+device.ID, device.Token, nil, ""), 200)
	assertCode(t, request(t, h, "GET", "/v1/me", device.Token, nil, ""), 200)
	var count int
	s.DB.QueryRow(`SELECT COUNT(*) FROM tokens WHERE hash=$1`, device.Token).Scan(&count)
	if count != 0 {
		t.Fatal("plaintext token stored")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOAuthStatePKCEAndIdentity(t *testing.T) {
	s, h, _, _ := setup(t)
	w := request(t, h, "GET", "/auth/start", "", nil, "")
	assertCode(t, w, 303)
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := location.Query().Get("state")
	if location.Query().Get("code_challenge_method") != "S256" || len(location.Query().Get("code_challenge")) != 43 {
		t.Fatal("PKCE missing")
	}
	s.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		body := `{"id":3,"login":"charlie"}`
		if strings.Contains(r.URL.Path, "access_token") {
			r.ParseForm()
			if r.Form.Get("code_verifier") == "" {
				t.Error("no verifier")
			}
			body = `{"access_token":"synthetic-oauth-token"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	callback := "/auth/callback?state=" + state + "&code=synthetic"
	assertCode(t, request(t, h, "GET", callback, "", nil, ""), 400)
	r := httptest.NewRequest("GET", callback, nil)
	r.AddCookie(&http.Cookie{Name: "moirai_oauth", Value: state})
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertCode(t, w, 303)
	var login string
	if err = s.DB.QueryRow(`SELECT login FROM users WHERE id='3'`).Scan(&login); err != nil || login != "charlie" {
		t.Fatalf("identity: %s %v", login, err)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assertCode(t, w, 400)
}
func TestEncryptedBlobBinding(t *testing.T) {
	base := Files{t.TempDir()}
	blobs, err := EncryptBlobs(base, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	a, b := randomID(), randomID()
	ctx := context.Background()
	if err = blobs.Put(ctx, a, []byte("private text")); err != nil {
		t.Fatal(err)
	}
	ciphertext, err := base.Get(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("private text")) {
		t.Fatal("plaintext at rest")
	}
	if err = base.Put(ctx, b, ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err = blobs.Get(ctx, b); err == nil {
		t.Fatal("object swap accepted")
	}
	ciphertext[len(ciphertext)-1] ^= 1
	base.Put(ctx, a, ciphertext)
	if _, err = blobs.Get(ctx, a); err == nil {
		t.Fatal("tamper accepted")
	}
	if _, err = base.Get(ctx, "../escape"); err == nil {
		t.Fatal("unsafe path accepted")
	}
}
func TestLandingWaitlistAndEscaping(t *testing.T) {
	_, h, a, _ := setup(t)
	w := request(t, h, "GET", "/", "", nil, "")
	assertCode(t, w, 200)
	if !strings.Contains(w.Body.String(), "16 readable formats · 9 direct") {
		t.Fatal("support claims drifted")
	}
	assertCode(t, request(t, h, "POST", "/v1/waitlist", "", map[string]any{"email": "test@example.com", "consent": true}, ""), 200)
	assertCode(t, request(t, h, "POST", "/v1/waitlist", "", map[string]any{"email": "invalid", "consent": true}, ""), 400)
	tr := &moirai.Transcript{SchemaVersion: "1.0", Meta: moirai.Metadata{ID: "x", Title: `<script>alert(1)</script>`}, Messages: []moirai.Message{{Role: moirai.RoleUser, Content: []moirai.Block{{Type: moirai.BlockText, Text: `<img src=x onerror=alert(1)>`}}}}}
	data, err := moirai.EncodeArchive(tr, moirai.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	w = request(t, h, "POST", "/v1/publications", a, PublishRequest{Archive: data, Visibility: "public"}, randomID())
	assertCode(t, w, 201)
	var p Publication
	json.Unmarshal(w.Body.Bytes(), &p)
	w = request(t, h, "GET", "/s/"+p.ID, "", nil, "")
	assertCode(t, w, 200)
	if strings.Contains(w.Body.String(), "<script>alert") || strings.Contains(w.Body.String(), "<img src=x") {
		t.Fatal("stored XSS")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("private caching headers absent")
	}
}

// Runs against a disposable Postgres instance in CI, never an operator database.
func TestPostgresMigration(t *testing.T) {
	dsn := os.Getenv("MOIRAI_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set MOIRAI_TEST_POSTGRES to an isolated test database")
	}
	db, err := OpenDB(context.Background(), "pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 0; i < 2; i++ {
		if err = Migrate(context.Background(), db, "pgx"); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	if err = db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("migration version=%d error=%v", version, err)
	}
}
