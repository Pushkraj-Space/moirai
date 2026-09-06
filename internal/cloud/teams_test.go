package cloud

import (
	"encoding/json"
	"testing"
)

func TestTeamOwnershipAndIsolation(t *testing.T) {
	s, h, a, b := setup(t)
	w := request(t, h, "POST", "/v1/teams", a, map[string]string{"name": "Research"}, "")
	assertCode(t, w, 201)
	var team Team
	json.Unmarshal(w.Body.Bytes(), &team)
	body := PublishRequest{Archive: archiveFixture(t), Visibility: "private", Team: team.ID}
	assertCode(t, request(t, h, "POST", "/v1/publications", b, body, randomID()), 403)
	path := "/v1/teams/" + team.ID + "/members"
	assertCode(t, request(t, h, "POST", path, b, map[string]string{"user_id": "2", "role": "writer"}, ""), 404)
	assertCode(t, request(t, h, "POST", path, a, map[string]string{"user_id": "2", "role": "reader"}, ""), 204)
	assertCode(t, request(t, h, "POST", "/v1/publications", b, body, randomID()), 403)
	assertCode(t, request(t, h, "POST", path, a, map[string]string{"user_id": "2", "role": "writer"}, ""), 204)
	w = request(t, h, "POST", "/v1/publications", b, body, randomID())
	assertCode(t, w, 201)
	var p Publication
	json.Unmarshal(w.Body.Bytes(), &p)
	if p.Owner != team.ID {
		t.Fatal("team ownership missing")
	}
	base := "/v1/publications/" + p.ID
	assertCode(t, request(t, h, "GET", base, a, nil, ""), 200)
	assertCode(t, request(t, h, "GET", base, b, nil, ""), 200)
	assertCode(t, request(t, h, "POST", base+"/revoke", b, nil, ""), 404)
	assertCode(t, request(t, h, "DELETE", path+"/1", a, nil, ""), 409)
	assertCode(t, request(t, h, "DELETE", path+"/2", a, nil, ""), 204)
	assertCode(t, request(t, h, "GET", base, b, nil, ""), 404)
	assertCode(t, request(t, h, "DELETE", base, a, nil, ""), 204)
	var used int
	s.DB.QueryRow(`SELECT bytes_used FROM users WHERE id=$1`, team.ID).Scan(&used)
	if used != 0 {
		t.Fatal("team quota not released")
	}
}
