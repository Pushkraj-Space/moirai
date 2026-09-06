package cloud

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	moirai "github.com/october-dev/moirai"
)

const MaxArchive int64 = 32 << 20
const MaxUserBytes int64 = 256 << 20
const MaxPublications = 100

type Config struct {
	Origin             string
	GitHubClientID     string
	GitHubClientSecret string
	Development        bool
	SupportEmail       string
	TrustedProxyCIDRs  []string
	MetricsToken       string
}
type Server struct {
	DB         *sql.DB
	Blobs      Blobs
	Config     Config
	HTTP       *http.Client
	Log        *slog.Logger
	expensive  chan struct{}
	trusted    []*net.IPNet
	mu         sync.Mutex
	rates      map[string]rate
	requests   atomic.Uint64
	failures   atomic.Uint64
	duration   atomic.Uint64
	published  atomic.Uint64
	downloaded atomic.Uint64
}
type rate struct {
	Window int64
	Count  int
}
type user struct {
	ID    string `json:"id"`
	Login string `json:"login"`
}
type Publication struct {
	ID         string `json:"id"`
	Owner      string `json:"owner"`
	Visibility string `json:"visibility"`
	Expires    int64  `json:"expires"`
	Created    int64  `json:"created"`
	Size       int64  `json:"size"`
	Parent     string `json:"parent,omitempty"`
	Revoked    bool   `json:"revoked"`
	Deleted    bool   `json:"deleted"`
	URL        string `json:"url"`
	CanManage  bool   `json:"can_manage"`
}
type PublishRequest struct {
	Archive    json.RawMessage `json:"archive"`
	Visibility string          `json:"visibility"`
	Expires    int64           `json:"expires"`
	Parent     string          `json:"parent,omitempty"`
	Team       string          `json:"team,omitempty"`
}

func New(db *sql.DB, blobs Blobs, cfg Config) (*Server, error) {
	u, err := url.Parse(cfg.Origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("origin must be an absolute origin without a path")
	}
	if u.Scheme != "https" && !(cfg.Development && u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")) {
		return nil, errors.New("production requires HTTPS; HTTP development requires loopback")
	}
	if !cfg.Development && (cfg.GitHubClientID == "" || cfg.GitHubClientSecret == "") {
		return nil, errors.New("GitHub OAuth credentials are required")
	}
	if cfg.MetricsToken != "" && len(cfg.MetricsToken) < 32 {
		return nil, errors.New("metrics token must contain at least 32 characters")
	}
	cfg.Origin = strings.TrimRight(cfg.Origin, "/")
	trusted := []*net.IPNet{}
	for _, cidr := range cfg.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy CIDR: %w", err)
		}
		trusted = append(trusted, network)
	}
	return &Server{DB: db, Blobs: blobs, Config: cfg, HTTP: &http.Client{Timeout: 15 * time.Second}, Log: slog.Default(), rates: map[string]rate{}, expensive: make(chan struct{}, 2), trusted: trusted}, nil
}

func randomID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func (s *Server) json(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) fail(w http.ResponseWriter, status int, code string) {
	if status >= 500 {
		s.failures.Add(1)
	}
	s.json(w, status, map[string]string{"error": code, "request_id": w.Header().Get("X-Request-ID")})
}
func decode(w http.ResponseWriter, r *http.Request, v any, max int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return errors.New("unexpected trailing data")
	}
	return nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.Config.MetricsToken == "" || subtle.ConstantTimeCompare([]byte(hash(provided)), []byte(hash(s.Config.MetricsToken))) != 1 {
			s.fail(w, 404, "not_found")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "moirai_http_requests_total %d\nmoirai_http_failures_total %d\nmoirai_http_duration_seconds_sum %.6f\nmoirai_publications_total %d\nmoirai_downloads_total %d\n", s.requests.Load(), s.failures.Load(), float64(s.duration.Load())/1e9, s.published.Load(), s.downloaded.Load())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { s.json(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if s.DB.PingContext(ctx) != nil {
			s.fail(w, 503, "not_ready")
			return
		}
		s.json(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /v1/formats", func(w http.ResponseWriter, r *http.Request) { s.json(w, 200, moirai.DefaultRegistry.Harnesses()) })
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		u, ok := s.requireUser(w, r)
		if ok {
			s.json(w, 200, u)
		}
	})
	mux.HandleFunc("POST /v1/logout", s.logout)
	mux.HandleFunc("GET /v1/teams", s.listTeams)
	mux.HandleFunc("POST /v1/teams", s.createTeam)
	mux.HandleFunc("GET /v1/teams/{team}/members", s.teamMembers)
	mux.HandleFunc("POST /v1/teams/{team}/members", s.addTeamMember)
	mux.HandleFunc("DELETE /v1/teams/{team}/members/{user}", s.removeTeamMember)
	mux.HandleFunc("POST /v1/auth/device", s.deviceStart)
	mux.HandleFunc("GET /v1/auth/device/{id}", s.devicePoll)
	mux.HandleFunc("GET /auth/start", s.authStart)
	mux.HandleFunc("GET /auth/callback", s.authCallback)
	mux.HandleFunc("GET /connect", s.connectPage)
	mux.HandleFunc("POST /connect", s.connectApprove)
	mux.HandleFunc("GET /v1/publications", s.list)
	mux.HandleFunc("POST /v1/publications", s.publish)
	mux.HandleFunc("POST /v1/publications/{id}/fork", s.fork)
	mux.HandleFunc("GET /v1/publications/{id}", s.metadata)
	mux.HandleFunc("GET /v1/publications/{id}/archive", s.archive)
	mux.HandleFunc("POST /v1/publications/{id}/revoke", s.revoke)
	mux.HandleFunc("DELETE /v1/publications/{id}", s.delete)
	mux.HandleFunc("POST /v1/publications/{id}/grants", s.grant)
	mux.HandleFunc("GET /v1/publications/{id}/grants", s.grants)
	mux.HandleFunc("DELETE /v1/publications/{id}/grants/{user}", s.ungrant)
	mux.HandleFunc("POST /v1/waitlist", s.waitlist)
	mux.HandleFunc("GET /s/{id}", s.viewer)
	mux.HandleFunc("GET /app", s.dashboard)
	mux.HandleFunc("GET /privacy", s.privacy)
	mux.HandleFunc("GET /", s.landing)
	mux.Handle("GET /assets/", http.FileServer(http.FS(webAssets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		s.requests.Add(1)
		w.Header().Set("X-Request-ID", randomID())
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; img-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		if !s.Config.Development {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/s/") || strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		}
		defer func() {
			s.duration.Add(uint64(time.Since(started)))
			if v := recover(); v != nil {
				s.Log.Error("request panic", "request_id", w.Header().Get("X-Request-ID"))
				s.fail(w, 500, "internal_error")
			}
			s.Log.Info("request", "request_id", w.Header().Get("X-Request-ID"), "method", r.Method, "duration_ms", time.Since(started).Milliseconds())
		}()
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			_, cookieErr := r.Cookie("moirai_session")
			if origin != "" && origin != s.Config.Origin || cookieErr == nil && origin != s.Config.Origin && !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				s.fail(w, 403, "origin_denied")
				return
			}
		}
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && !s.allow(r) {
			w.Header().Set("Retry-After", "60")
			s.fail(w, 429, "rate_limited")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/s/") || strings.HasPrefix(r.URL.Path, "/v1/publications") && (r.Method == "POST" || strings.HasSuffix(r.URL.Path, "/archive")) {
			select {
			case s.expensive <- struct{}{}:
				defer func() { <-s.expensive }()
			default:
				w.Header().Set("Retry-After", "2")
				s.fail(w, 503, "busy_retry")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) allow(r *http.Request) bool {
	// Forwarded headers are used only when the immediate peer is explicitly
	// trusted. Walk the chain from the proxy backwards, stopping at the client.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if s.trusts(ip) {
		parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(parts[i])
			if net.ParseIP(candidate) == nil {
				break
			}
			ip = candidate
			if !s.trusts(ip) {
				break
			}
		}
	}
	now := time.Now().Unix() / 60
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rates) > 10000 {
		for k, v := range s.rates {
			if v.Window != now {
				delete(s.rates, k)
			}
		}
		if len(s.rates) > 10000 {
			return false
		}
	}
	key := ip
	v := s.rates[key]
	if v.Window != now {
		v = rate{Window: now}
	}
	v.Count++
	s.rates[key] = v
	return v.Count <= 120
}
func (s *Server) trusts(address string) bool {
	ip := net.ParseIP(address)
	for _, network := range s.trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
func (s *Server) token(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if c, err := r.Cookie("moirai_session"); err == nil {
		return c.Value
	}
	return ""
}
func (s *Server) currentUser(r *http.Request) (user, error) {
	var u user
	t := s.token(r)
	if !validID.MatchString(t) {
		return u, sql.ErrNoRows
	}
	err := s.DB.QueryRowContext(r.Context(), `SELECT u.id,u.login FROM users u JOIN tokens t ON t.user_id=u.id WHERE t.hash=$1 AND t.expires>$2`, hash(t), time.Now().Unix()).Scan(&u.ID, &u.Login)
	return u, err
}
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (user, bool) {
	u, err := s.currentUser(r)
	if err != nil {
		s.fail(w, 401, "authentication_required")
		return u, false
	}
	return u, true
}
func (s *Server) publication(ctx context.Context, id string) (Publication, error) {
	var p Publication
	var revoked, deleted int
	if !validID.MatchString(id) {
		return p, sql.ErrNoRows
	}
	err := s.DB.QueryRowContext(ctx, `SELECT id,owner,visibility,expires,created,size,parent,revoked,deleted FROM publications WHERE id=$1`, id).Scan(&p.ID, &p.Owner, &p.Visibility, &p.Expires, &p.Created, &p.Size, &p.Parent, &revoked, &deleted)
	p.Revoked = revoked != 0
	p.Deleted = deleted != 0
	p.URL = s.Config.Origin + "/s/" + p.ID
	return p, err
}
func (s *Server) canRead(ctx context.Context, p Publication, u user) bool {
	if p.Deleted || p.Revoked || p.Expires != 0 && p.Expires <= time.Now().Unix() {
		return false
	}
	if p.Visibility != "private" || u.ID != "" && u.ID == p.Owner {
		return true
	}
	if u.ID != "" && s.teamRole(ctx, p.Owner, u.ID) != "" {
		return true
	}
	if u.ID == "" {
		return false
	}
	var n int
	return s.DB.QueryRowContext(ctx, `SELECT 1 FROM grants WHERE publication=$1 AND user_id=$2`, p.ID, u.ID).Scan(&n) == nil
}
func (s *Server) readable(w http.ResponseWriter, r *http.Request) (Publication, bool) {
	p, err := s.publication(r.Context(), r.PathValue("id"))
	u, _ := s.currentUser(r)
	if err != nil || !s.canRead(r.Context(), p, u) {
		s.fail(w, 404, "not_found")
		return p, false
	}
	return p, true
}
func (s *Server) owned(w http.ResponseWriter, r *http.Request) (Publication, user, bool) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return Publication{}, u, false
	}
	p, err := s.publication(r.Context(), r.PathValue("id"))
	if err != nil || !s.canManage(r.Context(), p.Owner, u.ID) {
		s.fail(w, 404, "not_found")
		return p, u, false
	}
	return p, u, true
}
func (s *Server) hideParent(ctx context.Context, p Publication, u user) Publication {
	p.CanManage = s.canManage(ctx, p.Owner, u.ID)
	if p.Parent != "" {
		parent, err := s.publication(ctx, p.Parent)
		if err != nil || !s.canRead(ctx, parent, u) {
			p.Parent = ""
		}
	}
	return p
}
func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	p, ok := s.readable(w, r)
	if !ok {
		return
	}
	u, _ := s.currentUser(r)
	s.json(w, 200, s.hideParent(r.Context(), p, u))
}
func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	p, ok := s.readable(w, r)
	if !ok {
		return
	}
	data, err := s.Blobs.Get(r.Context(), p.ID)
	if err != nil {
		s.fail(w, 503, "archive_unavailable")
		return
	}
	if _, err = moirai.DecodeArchive(data, moirai.DefaultLimits()); err != nil {
		s.fail(w, 503, "archive_integrity_error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="session.moirai"`)
	s.downloaded.Add(1)
	w.Write(data)
}
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `SELECT id FROM publications WHERE (owner=$1 OR owner IN (SELECT team_id FROM team_members WHERE user_id=$1)) AND deleted=0 ORDER BY created DESC LIMIT 100`, u.ID)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) != nil {
			rows.Close()
			s.fail(w, 500, "database_error")
			return
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	out := []Publication{}
	for _, id := range ids {
		p, err := s.publication(r.Context(), id)
		if err != nil {
			s.fail(w, 500, "database_error")
			return
		}
		out = append(out, s.hideParent(r.Context(), p, u))
	}
	s.json(w, 200, out)
}

func (s *Server) publish(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !validID.MatchString(key) {
		s.fail(w, 400, "idempotency_key_required")
		return
	}
	var body PublishRequest
	if decode(w, r, &body, MaxArchive+4096) != nil {
		s.fail(w, 400, "invalid_request")
		return
	}
	if body.Visibility == "" {
		body.Visibility = "private"
	}
	owner := u.ID
	if body.Team != "" {
		role := s.teamRole(r.Context(), body.Team, u.ID)
		if role != "owner" && role != "writer" {
			s.fail(w, 403, "team_publish_denied")
			return
		}
		owner = body.Team
	}
	if body.Visibility != "private" && body.Visibility != "unlisted" && body.Visibility != "public" {
		s.fail(w, 400, "invalid_visibility")
		return
	}
	if body.Expires != 0 && body.Expires <= time.Now().Unix() {
		s.fail(w, 400, "invalid_expiry")
		return
	}
	transcript, err := moirai.DecodeArchive(body.Archive, moirai.DefaultLimits())
	if err != nil {
		s.fail(w, 400, "invalid_archive")
		return
	}
	// Do not trust client-provided ancestry as an authorization decision.
	if body.Parent != "" {
		p, err := s.publication(r.Context(), body.Parent)
		if err != nil || !s.canRead(r.Context(), p, u) {
			s.fail(w, 404, "parent_not_found")
			return
		}
	}
	_ = transcript
	digestData, _ := json.Marshal(body)
	digest := hash(string(digestData))
	var oldID, oldDigest string
	err = s.DB.QueryRowContext(r.Context(), `SELECT id,digest FROM publications WHERE owner=$1 AND request_key=$2`, owner, key).Scan(&oldID, &oldDigest)
	if err == nil {
		if digest != oldDigest {
			s.fail(w, 409, "idempotency_conflict")
			return
		}
		p, err := s.publication(r.Context(), oldID)
		if err != nil {
			s.fail(w, 500, "database_error")
			return
		}
		s.json(w, 200, p)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.fail(w, 500, "database_error")
		return
	}
	id := randomID()
	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE users SET bytes_used=bytes_used+$1,publications=publications+1 WHERE id=$2 AND bytes_used+$1<=$3 AND publications<$4`, len(body.Archive), owner, MaxUserBytes, MaxPublications)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		s.fail(w, 413, "quota_exceeded")
		return
	}
	now := time.Now().Unix()
	_, err = tx.ExecContext(ctx, `INSERT INTO publications(id,owner,visibility,expires,created,size,request_key,digest,parent) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, owner, body.Visibility, body.Expires, now, len(body.Archive), key, digest, body.Parent)
	if err != nil {
		s.fail(w, 409, "publish_conflict_retry")
		return
	}
	if err = s.audit(ctx, tx, u.ID, "publish", id); err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	if err = s.Blobs.Put(ctx, id, body.Archive); err != nil {
		s.fail(w, 503, "storage_unavailable")
		return
	}
	if err = tx.Commit(); err != nil {
		// Commit outcome may be unknown. Leave the object for reconciliation rather
		// than risk deleting the blob of a successfully committed publication.
		s.fail(w, 503, "commit_unknown_retry")
		return
	}
	p := Publication{ID: id, Owner: owner, Visibility: body.Visibility, Expires: body.Expires, Created: now, Size: int64(len(body.Archive)), Parent: body.Parent, URL: s.Config.Origin + "/s/" + id}
	s.published.Add(1)
	s.json(w, 201, p)
}

func (s *Server) audit(ctx context.Context, tx *sql.Tx, actor, action, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_events(id,actor,action,publication,created) VALUES($1,$2,$3,$4,$5)`, randomID(), actor, action, id, time.Now().Unix())
	return err
}
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	p, u, ok := s.owned(w, r)
	if !ok {
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `UPDATE publications SET revoked=1 WHERE id=$1`, p.ID)
	if err == nil {
		err = s.audit(r.Context(), tx, u.ID, "revoke", p.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	p, u, ok := s.owned(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// Revoke before deleting storage, so a storage failure cannot reopen access.
	if _, err := s.DB.ExecContext(ctx, `UPDATE publications SET revoked=1 WHERE id=$1`, p.ID); err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	if err := s.Blobs.Delete(ctx, p.ID); err != nil {
		s.fail(w, 503, "delete_pending_retry")
		return
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE publications SET deleted=1 WHERE id=$1 AND deleted=0`, p.ID)
	if err == nil {
		n, _ := result.RowsAffected()
		if n == 1 {
			_, err = tx.ExecContext(ctx, `UPDATE users SET bytes_used=bytes_used-$1,publications=publications-1 WHERE id=$2`, p.Size, p.Owner)
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM grants WHERE publication=$1`, p.ID)
	}
	if err == nil {
		err = s.audit(ctx, tx, u.ID, "delete", p.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	w.WriteHeader(204)
}
func (s *Server) grant(w http.ResponseWriter, r *http.Request) {
	p, u, ok := s.owned(w, r)
	if !ok {
		return
	}
	if p.Revoked || p.Deleted {
		s.fail(w, 409, "publication_unavailable")
		return
	}
	var body struct {
		Login string `json:"login"`
	}
	if decode(w, r, &body, 1024) != nil {
		s.fail(w, 400, "invalid_request")
		return
	}
	// Handles can be renamed or recycled. Resolve the current GitHub identity
	// before granting access, then bind the grant to that immutable account ID.
	if len(body.Login) < 1 || len(body.Login) > 39 || strings.Trim(body.Login, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		s.fail(w, 400, "invalid_login")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", "https://api.github.com/users/"+url.PathEscape(body.Login), nil)
	if err != nil {
		s.fail(w, 500, "internal_error")
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	response, err := s.HTTP.Do(req)
	if err != nil {
		s.fail(w, 502, "identity_provider_unavailable")
		return
	}
	data, readErr := boundedRead(response.Body, 64<<10)
	response.Body.Close()
	var identity struct {
		ID int64 `json:"id"`
	}
	if readErr != nil || response.StatusCode != 200 || json.Unmarshal(data, &identity) != nil || identity.ID <= 0 {
		s.fail(w, 404, "account_not_found")
		return
	}
	target := strconv.FormatInt(identity.ID, 10)
	var existing string
	if s.DB.QueryRowContext(r.Context(), `SELECT id FROM users WHERE id=$1`, target).Scan(&existing) != nil {
		s.fail(w, 404, "account_must_sign_in_first")
		return
	}
	s.changeGrant(w, r, p, u, target, true)
}
func (s *Server) grants(w http.ResponseWriter, r *http.Request) {
	p, _, ok := s.owned(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `SELECT u.id,u.login FROM users u JOIN grants g ON g.user_id=u.id WHERE g.publication=$1 ORDER BY u.login`, p.ID)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer rows.Close()
	out := []user{}
	for rows.Next() {
		var u user
		if err = rows.Scan(&u.ID, &u.Login); err != nil {
			s.fail(w, 500, "database_error")
			return
		}
		out = append(out, u)
	}
	if rows.Err() != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.json(w, 200, out)
}
func (s *Server) ungrant(w http.ResponseWriter, r *http.Request) {
	p, u, ok := s.owned(w, r)
	if ok {
		s.changeGrant(w, r, p, u, r.PathValue("user"), false)
	}
}
func (s *Server) changeGrant(w http.ResponseWriter, r *http.Request, p Publication, u user, target string, add bool) {
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	action := "ungrant"
	if add {
		action = "grant"
		_, err = tx.ExecContext(r.Context(), `INSERT INTO grants(publication,user_id) VALUES($1,$2) ON CONFLICT(publication,user_id) DO NOTHING`, p.ID, target)
	} else {
		_, err = tx.ExecContext(r.Context(), `DELETE FROM grants WHERE publication=$1 AND user_id=$2`, p.ID, target)
	}
	if err == nil {
		err = s.audit(r.Context(), tx, u.ID, action, p.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	w.WriteHeader(204)
}

// Stats contains only aggregate counters; expose it through operator tooling.
func (s *Server) Stats() string {
	return fmt.Sprintf("requests=%d failures=%d", s.requests.Load(), s.failures.Load())
}
