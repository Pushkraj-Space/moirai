package cloud

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type Team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

func (s *Server) teamRole(ctx context.Context, team, uid string) string {
	if !strings.HasPrefix(team, "team_") {
		return ""
	}
	var role string
	_ = s.DB.QueryRowContext(ctx, `SELECT role FROM team_members WHERE team_id=$1 AND user_id=$2`, team, uid).Scan(&role)
	return role
}
func (s *Server) canManage(ctx context.Context, owner, uid string) bool {
	return uid != "" && (owner == uid || s.teamRole(ctx, owner, uid) == "owner")
}
func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `SELECT t.id,t.name,m.role FROM teams t JOIN team_members m ON m.team_id=t.id WHERE m.user_id=$1 ORDER BY t.created`, u.ID)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var team Team
		if rows.Scan(&team.ID, &team.Name, &team.Role) != nil {
			s.fail(w, 500, "database_error")
			return
		}
		out = append(out, team)
	}
	if rows.Err() != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.json(w, 200, out)
}
func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if decode(w, r, &body, 1024) != nil || len(strings.TrimSpace(body.Name)) == 0 || len(body.Name) > 100 {
		s.fail(w, 400, "invalid_name")
		return
	}
	ctx := r.Context()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	// Serialize creation by account, so the per-account team limit is concurrent safe.
	if _, err = tx.ExecContext(ctx, `UPDATE users SET publications=publications WHERE id=$1`, u.ID); err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	var n int
	if tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM team_members WHERE user_id=$1 AND role='owner'`, u.ID).Scan(&n) != nil {
		s.fail(w, 500, "database_error")
		return
	}
	if n >= 5 {
		s.fail(w, 413, "team_limit")
		return
	}
	id := "team_" + randomID()
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,login) VALUES($1,$2)`, id, id)
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO teams(id,name,created) VALUES($1,$2,$3)`, id, body.Name, time.Now().Unix())
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO team_members(team_id,user_id,role) VALUES($1,$2,'owner')`, id, u.ID)
	}
	if err == nil {
		err = s.audit(ctx, tx, u.ID, "team_create", id)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.json(w, 201, Team{ID: id, Name: body.Name, Role: "owner"})
}
func (s *Server) teamMembers(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	team := r.PathValue("team")
	if s.teamRole(r.Context(), team, u.ID) == "" {
		s.fail(w, 404, "not_found")
		return
	}
	rows, err := s.DB.QueryContext(r.Context(), `SELECT u.id,u.login,m.role FROM users u JOIN team_members m ON m.user_id=u.id WHERE m.team_id=$1 ORDER BY u.login`, team)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id, login, role string
		if rows.Scan(&id, &login, &role) != nil {
			s.fail(w, 500, "database_error")
			return
		}
		out = append(out, map[string]string{"id": id, "login": login, "role": role})
	}
	if rows.Err() != nil {
		s.fail(w, 500, "database_error")
		return
	}
	s.json(w, 200, out)
}
func (s *Server) addTeamMember(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	team := r.PathValue("team")
	if s.teamRole(r.Context(), team, u.ID) != "owner" {
		s.fail(w, 404, "not_found")
		return
	}
	var body struct {
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if decode(w, r, &body, 1024) != nil || (body.Role != "reader" && body.Role != "writer") || body.UserID == u.ID {
		s.fail(w, 400, "invalid_member")
		return
	}
	var target string
	if s.DB.QueryRowContext(r.Context(), `SELECT id FROM users WHERE id=$1`, body.UserID).Scan(&target) != nil || strings.HasPrefix(target, "team_") {
		s.fail(w, 404, "account_not_found")
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `INSERT INTO team_members(team_id,user_id,role) VALUES($1,$2,$3) ON CONFLICT(team_id,user_id) DO UPDATE SET role=excluded.role`, team, target, body.Role)
	if err == nil {
		err = s.audit(r.Context(), tx, u.ID, "team_member_set", team)
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
func (s *Server) removeTeamMember(w http.ResponseWriter, r *http.Request) {
	u, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	team := r.PathValue("team")
	target := r.PathValue("user")
	if s.teamRole(r.Context(), team, u.ID) != "owner" {
		s.fail(w, 404, "not_found")
		return
	}
	if target == u.ID {
		s.fail(w, 409, "cannot_remove_team_owner")
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		s.fail(w, 500, "database_error")
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(r.Context(), `DELETE FROM team_members WHERE team_id=$1 AND user_id=$2`, team, target)
	if err == nil {
		err = s.audit(r.Context(), tx, u.ID, "team_member_remove", team)
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
