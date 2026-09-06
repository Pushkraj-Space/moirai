package cloud

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (s *Server) setCookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: seconds, HttpOnly: true, Secure: !s.Config.Development, SameSite: http.SameSiteLaxMode})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	if _, err := s.DB.ExecContext(r.Context(), `DELETE FROM tokens WHERE hash=$1`, hash(s.token(r))); err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.setCookie(w, "moirai_session", "", -1)
	w.WriteHeader(204)
}
func (s *Server) deviceStart(w http.ResponseWriter, r *http.Request) {
	id, secret := randomID(), randomID()
	_, err := s.DB.ExecContext(r.Context(), `INSERT INTO devices(id,secret_hash,expires) VALUES($1,$2,$3)`, id, hash(secret), time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.json(w, 201, map[string]any{"id": id, "token": secret, "verification_uri": s.Config.Origin + "/auth/start?device=" + id, "expires_in": 600, "interval": 5})
}
func (s *Server) devicePoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	secret := s.token(r)
	var stored, uid string
	var expires int64
	err := s.DB.QueryRowContext(r.Context(), `SELECT secret_hash,user_id,expires FROM devices WHERE id=$1`, id).Scan(&stored, &uid, &expires)
	if err != nil || expires <= time.Now().Unix() || subtle.ConstantTimeCompare([]byte(hash(secret)), []byte(stored)) != 1 {
		s.fail(w, 404, "device_expired")
		return
	}
	if uid == "" {
		s.json(w, 202, map[string]string{"status": "pending"})
		return
	}
	s.json(w, 200, map[string]string{"status": "approved"})
}
func (s *Server) authStart(w http.ResponseWriter, r *http.Request) {
	device := r.URL.Query().Get("device")
	if device != "" && !validID.MatchString(device) {
		s.fail(w, 400, "invalid_device")
		return
	}
	if s.Config.GitHubClientID == "" {
		s.fail(w, 503, "oauth_not_configured")
		return
	}
	state, verifier := randomID(), randomID()
	next := r.URL.Query().Get("next")
	if next != "" && next != "/app" && !(strings.HasPrefix(next, "/s/") && validID.MatchString(strings.TrimPrefix(next, "/s/"))) {
		s.fail(w, 400, "invalid_return_path")
		return
	}
	_, err := s.DB.ExecContext(r.Context(), `INSERT INTO oauth_states(hash,verifier,device,next_path,expires) VALUES($1,$2,$3,$4,$5)`, hash(state), verifier, device, next, time.Now().Add(10*time.Minute).Unix())
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.setCookie(w, "moirai_oauth", state, 600)
	challenge := sha256.Sum256([]byte(verifier))
	query := url.Values{"client_id": {s.Config.GitHubClientID}, "redirect_uri": {s.Config.Origin + "/auth/callback"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, "https://github.com/login/oauth/authorize?"+query.Encode(), http.StatusSeeOther)
}
func (s *Server) authCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	cookie, err := r.Cookie("moirai_oauth")
	if err != nil || !validID.MatchString(state) || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		s.fail(w, 400, "invalid_oauth_state")
		return
	}
	var verifier, device, next string
	var expires int64
	err = s.DB.QueryRowContext(r.Context(), `DELETE FROM oauth_states WHERE hash=$1 RETURNING verifier,device,next_path,expires`, hash(state)).Scan(&verifier, &device, &next, &expires)
	s.setCookie(w, "moirai_oauth", "", -1)
	if err != nil || expires <= time.Now().Unix() {
		s.fail(w, 400, "expired_oauth_state")
		return
	}
	values := url.Values{"client_id": {s.Config.GitHubClientID}, "client_secret": {s.Config.GitHubClientSecret}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {s.Config.Origin + "/auth/callback"}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(r.Context(), "POST", "https://github.com/login/oauth/access_token", strings.NewReader(values.Encode()))
	if err != nil {
		s.fail(w, 500, "internal_error")
		return
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := s.HTTP.Do(req)
	if err != nil {
		s.fail(w, 502, "identity_provider_unavailable")
		return
	}
	data, readErr := boundedRead(response.Body, 64<<10)
	response.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if readErr != nil || response.StatusCode != 200 || json.Unmarshal(data, &token) != nil || token.AccessToken == "" {
		s.fail(w, 401, "oauth_failed")
		return
	}
	req, err = http.NewRequestWithContext(r.Context(), "GET", "https://api.github.com/user", nil)
	if err != nil {
		s.fail(w, 500, "internal_error")
		return
	}
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	response, err = s.HTTP.Do(req)
	if err != nil {
		s.fail(w, 502, "identity_provider_unavailable")
		return
	}
	data, readErr = boundedRead(response.Body, 64<<10)
	response.Body.Close()
	var account struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if readErr != nil || response.StatusCode != 200 || json.Unmarshal(data, &account) != nil || account.ID <= 0 || account.Login == "" {
		s.fail(w, 401, "oauth_failed")
		return
	}
	uid := strconv.FormatInt(account.ID, 10)
	session := randomID()
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	// A GitHub handle may have moved to this stable account ID since the old
	// holder last signed in. Keep identity and existing grants keyed by ID.
	_, err = tx.ExecContext(r.Context(), `UPDATE users SET login='former-account-' || id WHERE login=$1 AND id<>$2`, strings.ToLower(account.Login), uid)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO users(id,login) VALUES($1,$2) ON CONFLICT(id) DO UPDATE SET login=excluded.login`, uid, strings.ToLower(account.Login))
	if err == nil {
		_, err = tx.ExecContext(r.Context(), `INSERT INTO tokens(hash,user_id,expires) VALUES($1,$2,$3)`, hash(session), uid, time.Now().Add(30*24*time.Hour).Unix())
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.setCookie(w, "moirai_session", session, 30*24*3600)
	destination := "/app"
	if next != "" {
		destination = next
	}
	if device != "" {
		destination = "/connect?id=" + device
	}
	http.Redirect(w, r, destination, http.StatusSeeOther)
}
func (s *Server) connectPage(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("id")
	if !validID.MatchString(id) {
		s.fail(w, 400, "invalid_device")
		return
	}
	s.page(w, "Connect your terminal", fmt.Sprintf("Approve only if this code matches the code printed by your Moirai CLI: %s", id), connectTemplate, map[string]string{"ID": id, "Login": u.Login})
}
func (s *Server) connectApprove(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.ParseForm() != nil {
		s.fail(w, 400, "invalid_request")
		return
	}
	id := r.Form.Get("id")
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	var secret string
	err = tx.QueryRowContext(r.Context(), `UPDATE devices SET user_id=$1 WHERE id=$2 AND user_id='' AND expires>$3 RETURNING secret_hash`, u.ID, id, time.Now().Unix()).Scan(&secret)
	if err != nil {
		s.fail(w, 400, "device_unavailable")
		return
	}
	_, err = tx.ExecContext(r.Context(), `INSERT INTO tokens(hash,user_id,expires) VALUES($1,$2,$3)`, secret, u.ID, time.Now().Add(30*24*time.Hour).Unix())
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.page(w, "Terminal connected", "Return to your terminal to continue.", "", nil)
}
