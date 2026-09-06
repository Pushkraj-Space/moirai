package cloud

import (
	"embed"
	"encoding/json"
	"html/template"
	"net/http"
	"net/mail"
	"strings"
	"time"

	moirai "github.com/october-dev/moirai"
)

//go:embed assets/*
var webAssets embed.FS

//go:embed templates/shell.html
var shell string

func (s *Server) page(w http.ResponseWriter, title, description, body string, data any) {
	s.pageStatus(w, 200, title, description, body, data)
}
func (s *Server) pageStatus(w http.ResponseWriter, status int, title, description, body string, data any) {
	t, err := template.New("page").Parse(shell + `{{define "content"}}` + body + `{{end}}`)
	if err != nil {
		s.fail(w, 500, "template_error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err = t.ExecuteTemplate(w, "page", map[string]any{"Title": title, "Description": description, "Data": data, "Indexable": body == landingTemplate, "Canonical": s.Config.Origin + "/", "Origin": s.Config.Origin}); err != nil {
		s.Log.Error("template render failed")
	}
}

//go:embed templates/landingTemplate.html
var landingTemplate string

func (s *Server) landing(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.fail(w, 404, "not_found")
		return
	}
	infos := moirai.DefaultRegistry.Harnesses()
	count := 0
	for _, i := range infos {
		if i.Capability.Continue {
			count++
		}
	}
	s.page(w, "Move the session. Keep the thread.", "Portable, inspectable agent work. Built in the open.", landingTemplate, map[string]any{"Formats": len(infos), "Continue": count, "Harnesses": infos})
}

//go:embed templates/dashboardTemplate.html
var dashboardTemplate string

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	u, err := s.currentUser(r)
	if err != nil {
		http.Redirect(w, r, "/auth/start", http.StatusSeeOther)
		return
	}
	s.page(w, "Your sessions", "Signed in as @"+u.Login, dashboardTemplate, nil)
}

//go:embed templates/viewerTemplate.html
var viewerTemplate string

func (s *Server) viewer(w http.ResponseWriter, r *http.Request) {
	p, err := s.publication(r.Context(), r.PathValue("id"))
	reader, _ := s.currentUser(r)
	if err != nil || !s.canRead(r.Context(), p, reader) {
		s.pageStatus(w, 404, "Checkpoint unavailable", "This link may be private, expired, revoked, or unavailable.", `<p>Sign in with the account invited by the owner. If you still cannot open it, ask the owner for a new link.</p><a class="button" href="/auth/start?next={{.Next}}">Sign in to open checkpoint</a>`, map[string]string{"Next": "/s/" + r.PathValue("id")})
		return
	}
	data, err := s.Blobs.Get(r.Context(), p.ID)
	if err != nil {
		s.fail(w, 503, "archive_unavailable")
		return
	}
	transcript, err := moirai.DecodeArchive(data, moirai.DefaultLimits())
	if err != nil {
		s.fail(w, 503, "archive_integrity_error")
		return
	}
	u, _ := s.currentUser(r)
	p = s.hideParent(r.Context(), p, u)
	messages := transcript.Messages
	truncated := len(messages) > 200
	if truncated {
		messages = messages[:200]
	}
	t, err := template.New("page").Funcs(template.FuncMap{"raw": func(v json.RawMessage) string { return string(v) }}).Parse(shell + `{{define "content"}}` + viewerTemplate + `{{end}}`)
	if err != nil {
		s.fail(w, 500, "template_error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err = t.ExecuteTemplate(w, "page", map[string]any{"Title": transcript.Meta.Title, "Description": "A shared Moirai checkpoint", "Origin": s.Config.Origin, "Data": map[string]any{"Publication": p, "Count": len(transcript.Messages), "Messages": messages, "Truncated": truncated}}); err != nil {
		s.Log.Error("viewer render failed")
	}
}

//go:embed templates/connectTemplate.html
var connectTemplate string

func (s *Server) privacy(w http.ResponseWriter, r *http.Request) {
	s.page(w, "Privacy & sharing", "Know what travels with your session.", `<section><h2>Your local sessions stay local</h2><p>Discovery and conversion do not upload data. Publishing sends the archive you explicitly selected, plus its visibility, expiry, and ancestry reference.</p><h2>Accounts and access</h2><p>We use your GitHub account ID and handle to identify you. Private checkpoints are readable by you and accounts you invite. Unlisted and public checkpoints can be downloaded by anyone who has the link.</p><h2>Storage and deletion</h2><p>Archives are encrypted at rest. Revocation blocks future service requests. Deletion removes the live archive; downloaded copies and independent forks remain outside the service's control. Backup retention and the production operator's terms must be published before a public launch.</p><h2>Service data</h2><p>Operational logs contain request IDs, methods, and timings, without transcript contents. Audit records track account actions. Update subscriptions store your email and signup time. Contact the operator to request account or subscription removal.</p><h2>Contact</h2><p>{{.Support}}</p></section>`, map[string]string{"Support": s.Config.SupportEmail})
}
func (s *Server) waitlist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email   string `json:"email"`
		Company string `json:"company"`
		Consent bool   `json:"consent"`
	}
	if decode(w, r, &body, 2048) != nil || !body.Consent {
		s.fail(w, 400, "invalid_request")
		return
	}
	if body.Company != "" {
		s.json(w, 200, map[string]string{"status": "subscribed"})
		return
	}
	address, err := mail.ParseAddress(body.Email)
	if err != nil || address.Address != body.Email || len(body.Email) > 254 {
		s.fail(w, 400, "invalid_email")
		return
	}
	_, err = s.DB.ExecContext(r.Context(), `INSERT INTO waitlist(email,created) VALUES($1,$2) ON CONFLICT(email) DO NOTHING`, strings.ToLower(body.Email), time.Now().Unix())
	if err != nil {
		s.fail(w, 503, "try_again")
		return
	}
	s.json(w, 200, map[string]string{"status": "subscribed"})
}
