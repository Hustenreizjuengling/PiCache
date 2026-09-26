package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
)

const viewerPassword = "viewer password 1"

// withViewer creates the viewer "watcher" through the API and signs in.
func (e *coreEnv) withViewer(t *testing.T, adminSession string) (id int64, session string) {
	t.Helper()
	w := e.do("POST", "/api/v1/system/users", `{"username":"watcher","password":"`+viewerPassword+`","role":"viewer","currentPassword":"`+corePassword+`"}`, adminSession)
	if w.Code != http.StatusCreated {
		t.Fatalf("create viewer: %d %s", w.Code, w.Body)
	}
	var u auth.User
	coreDecode(t, w, &u)
	if u.Role != auth.RoleViewer || u.Username != "watcher" {
		t.Fatalf("created %+v", u)
	}
	w = e.do("POST", "/api/v1/auth/login", `{"username":"watcher","password":"`+viewerPassword+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("viewer login: %d %s", w.Code, w.Body)
	}
	return u.ID, coreSessionCookie(t, w).Value
}

func concretePath(pattern string) (string, string) {
	method, path, _ := strings.Cut(pattern, " ")
	return method, strings.NewReplacer("{id}", "7", "{service}", "steam", "{name}", "picache-backup-x-20260101T000000Z.db",
		"{mac}", "02:00:00:00:00:01", "{host}", "cdn.example", "{section}", "web").Replace(path)
}

// Every role × route pair over the registry: a viewer gets 403 on every A
// and S route and is let through on R and U routes; an admin token gets
// 403 on every U and S route.
func TestRolesOverRoutes(t *testing.T) {
	e := newCoreEnv(t)
	admin := e.provisionAndLogin(t)
	adminTok := e.createToken(t, admin, "admin")
	_, viewer := e.withViewer(t, admin)
	for _, r := range e.srv.routes {
		if strings.Contains(r.Pattern, "/stream/") || r.Perm == permPublic ||
			r.Pattern == "POST /api/v1/auth/logout" { // would end the session used for the next routes
			continue
		}
		method, path := concretePath(r.Pattern)
		w := e.do(method, path, "", viewer)
		admins := r.Perm == permAdmin || r.Perm == permSession
		switch {
		case admins && (w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "this action requires admin rights")):
			t.Errorf("viewer on %s: %d %s", r.Pattern, w.Code, w.Body)
		case !admins && (w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized):
			t.Errorf("viewer on %s: %d %s", r.Pattern, w.Code, w.Body)
		}
		w = e.do(method, path, "", adminTok)
		interactive := r.Perm == permSelf || r.Perm == permSession
		if interactive && (w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "interactive login")) {
			t.Errorf("admin token on %s: %d %s", r.Pattern, w.Code, w.Body)
		}
	}
}

// The account routes: create, update, delete with their errors, audits and
// cookies; the last admin stays.
func TestUserRoutes(t *testing.T) {
	e := newCoreEnv(t)
	admin := e.provisionAndLogin(t)
	pw := `"currentPassword":"` + corePassword + `"`
	coreWantError(t, e.do("POST", "/api/v1/system/users", `{"username":"-x","password":"long enough 1","role":"viewer",`+pw+`}`, admin),
		http.StatusBadRequest, "invalid", "username")
	coreWantError(t, e.do("POST", "/api/v1/system/users", `{"username":"x","password":"short","role":"viewer",`+pw+`}`, admin),
		http.StatusBadRequest, "invalid", "password")
	coreWantError(t, e.do("POST", "/api/v1/system/users", `{"username":"x","password":"long enough 1","role":"root",`+pw+`}`, admin),
		http.StatusBadRequest, "invalid", "role")
	coreWantError(t, e.do("POST", "/api/v1/system/users", `{"username":"x","password":"long enough 1","role":"viewer","currentPassword":"nope"}`, admin),
		http.StatusBadRequest, "invalid", "currentPassword")
	id, viewer := e.withViewer(t, admin)
	w := e.do("POST", "/api/v1/system/users", `{"username":"WATCHER","password":"long enough 1","role":"viewer",`+pw+`}`, admin)
	coreWantError(t, w, http.StatusConflict, "conflict", "")
	if !strings.Contains(w.Body.String(), "an account named WATCHER already exists") {
		t.Fatalf("duplicate %s", w.Body)
	}
	var list []auth.User
	coreDecode(t, e.do("GET", "/api/v1/system/users", "", admin), &list)
	if len(list) != 2 || list[0].Username != "admin" || list[0].Role != auth.RoleAdmin || list[1].Role != auth.RoleViewer {
		t.Fatalf("list %+v", list)
	}
	path := fmt.Sprintf("/api/v1/system/users/%d", id)
	coreWantError(t, e.do("PUT", path, `{`+pw+`}`, admin), http.StatusBadRequest, "invalid", "body")
	coreWantError(t, e.do("PUT", path, `{"disableTotp":false,`+pw+`}`, admin), http.StatusBadRequest, "invalid", "body")
	coreWantError(t, e.do("PUT", "/api/v1/system/users/99", `{"role":"admin",`+pw+`}`, admin), http.StatusNotFound, "not_found", "")
	coreWantError(t, e.do("PUT", "/api/v1/system/users/x", `{"role":"admin",`+pw+`}`, admin), http.StatusBadRequest, "invalid", "id")
	var me auth.User
	coreDecode(t, e.do("GET", "/api/v1/auth/me", "", admin), &me)
	selfPath := fmt.Sprintf("/api/v1/system/users/%d", me.ID)
	coreWantError(t, e.do("PUT", selfPath, `{"password":"new password 1",`+pw+`}`, admin), http.StatusBadRequest, "invalid", "password")
	coreWantError(t, e.do("PUT", selfPath, `{"disableTotp":true,`+pw+`}`, admin), http.StatusBadRequest, "invalid", "disableTotp")
	coreWantError(t, e.do("PUT", selfPath, `{"role":"viewer",`+pw+`}`, admin), http.StatusConflict, "conflict", "")
	coreWantError(t, e.do("DELETE", selfPath, `{`+pw+`}`, admin), http.StatusConflict, "conflict", "")

	// Promote the viewer: their session ends, the admin keeps theirs.
	if w := e.do("PUT", path, `{"role":"admin",`+pw+`}`, admin); w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
		t.Fatalf("promote: %d %s %v", w.Code, w.Body, w.Result().Cookies())
	}
	if w := e.do("GET", "/api/v1/auth/me", "", viewer); w.Code != http.StatusUnauthorized {
		t.Fatalf("a role change ends the user's sessions: %d", w.Code)
	}
	// Self-demotion with another admin left clears the cookies.
	w = e.do("PUT", selfPath, `{"role":"viewer",`+pw+`}`, admin)
	if w.Code != http.StatusOK || !clearsSession(w) {
		t.Fatalf("self-demotion: %d %v", w.Code, w.Result().Cookies())
	}
	// Sign in again as the viewer, then the new admin deletes the old one.
	time.Sleep(1100 * time.Millisecond) // the global attempt limit (10/s) refills
	w = e.do("POST", "/api/v1/auth/login", `{"username":"watcher","password":"`+viewerPassword+`"}`, "")
	other := coreSessionCookie(t, w).Value
	otherPw := `"currentPassword":"` + viewerPassword + `"`
	if w := e.do("DELETE", selfPath, `{`+otherPw+`}`, other); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	// Self-delete of the last admin is refused; with a second admin allowed.
	coreWantError(t, e.do("DELETE", path, `{`+otherPw+`}`, other), http.StatusConflict, "conflict", "")
	if w := e.do("POST", "/api/v1/system/users", `{"username":"second","password":"second password","role":"admin",`+otherPw+`}`, other); w.Code != http.StatusCreated {
		t.Fatalf("second admin: %d %s", w.Code, w.Body)
	}
	w = e.do("DELETE", path, `{`+otherPw+`}`, other)
	if w.Code != http.StatusNoContent || !clearsSession(w) {
		t.Fatalf("self-delete: %d %v", w.Code, w.Result().Cookies())
	}
	actions := strings.Join(e.auditActions(t), ",")
	for _, a := range []string{"auth.user.create", "auth.user.update", "auth.user.delete"} {
		if !strings.Contains(actions, a) {
			t.Errorf("audit lacks %s: %s", a, actions)
		}
	}
	entries, _, _ := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "auth.user.update"})
	if len(entries) == 0 || !strings.Contains(entries[len(entries)-1].Details, `"role":"admin"`) ||
		!strings.Contains(entries[len(entries)-1].Details, `"sessionsRevoked":1`) {
		t.Fatalf("update audit %+v", entries)
	}
}

// clearsSession reports whether a response deletes the session cookie.
func clearsSession(w interface{ Result() *http.Response }) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookie && c.MaxAge < 0 {
			return true
		}
	}
	return false
}

// Tokens: viewers see and delete only their own and create read tokens
// only; admins see all with their owners and delete any.
func TestTokenRoutesScoping(t *testing.T) {
	e := newCoreEnv(t)
	admin := e.provisionAndLogin(t)
	adminTokID := func() int64 {
		var list []auth.TokenInfo
		coreDecode(t, e.do("GET", "/api/v1/tokens", "", admin), &list)
		for _, tok := range list {
			if tok.Username == "admin" {
				return tok.ID
			}
		}
		t.Fatal("no admin token")
		return 0
	}
	e.createToken(t, admin, "admin")
	_, viewer := e.withViewer(t, admin)
	coreWantError(t, e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"admin","currentPassword":"`+viewerPassword+`"}`, viewer),
		http.StatusBadRequest, "invalid", "scope")
	if w := e.do("POST", "/api/v1/tokens", `{"name":"grafana","scope":"read","currentPassword":"`+viewerPassword+`"}`, viewer); w.Code != http.StatusCreated {
		t.Fatalf("viewer read token: %d %s", w.Code, w.Body)
	}
	var mine, all []auth.TokenInfo
	coreDecode(t, e.do("GET", "/api/v1/tokens", "", viewer), &mine)
	coreDecode(t, e.do("GET", "/api/v1/tokens", "", admin), &all)
	if len(mine) != 1 || mine[0].Username != "watcher" || len(all) != 2 {
		t.Fatalf("viewer sees %+v, admin sees %d", mine, len(all))
	}
	coreWantError(t, e.do("DELETE", fmt.Sprintf("/api/v1/tokens/%d", adminTokID()), "", viewer), http.StatusNotFound, "not_found", "")
	if w := e.do("DELETE", fmt.Sprintf("/api/v1/tokens/%d", mine[0].ID), "", admin); w.Code != http.StatusNoContent {
		t.Fatalf("admin deletes any token: %d", w.Code)
	}
}

// A demoted admin: through the API the sessions end and the admin tokens
// are deleted; with the role changed on disk, the open session and the
// admin token get 403 on admin routes at once.
func TestDemotionTakesEffectAtOnce(t *testing.T) {
	e := newCoreEnv(t)
	admin := e.provisionAndLogin(t)
	adminTok := e.createToken(t, admin, "admin")
	if _, err := e.auth.Users(context.Background()); err != nil {
		t.Fatal(err)
	}
	var me auth.User
	coreDecode(t, e.do("GET", "/api/v1/auth/me", "", admin), &me)
	if _, err := e.authDB(t).Exec(`UPDATE auth_users SET role = 'viewer' WHERE id = ?`, me.ID); err != nil {
		t.Fatal(err)
	}
	for _, cred := range []string{admin, adminTok} {
		coreWantError(t, e.do("PATCH", "/api/v1/settings/web", `{"language":"de"}`, cred), http.StatusForbidden, "forbidden", "")
	}
	if w := e.do("GET", "/api/v1/settings", "", admin); w.Code != http.StatusOK {
		t.Fatalf("reading stays: %d", w.Code)
	}
	var st authStatusResponse
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", admin), &st)
	if st.Scope != auth.ScopeRead || st.User.Role != auth.RoleViewer {
		t.Fatalf("status of a viewer session %+v", st)
	}
	if _, err := e.authDB(t).Exec(`UPDATE auth_users SET role = 'admin' WHERE id = ?`, me.ID); err != nil {
		t.Fatal(err)
	}
	// Through the API (a second admin demotes the first).
	time.Sleep(1100 * time.Millisecond) // the global attempt limit
	if w := e.do("POST", "/api/v1/system/users", `{"username":"boss","password":"boss password","role":"admin","currentPassword":"`+corePassword+`"}`, admin); w.Code != http.StatusCreated {
		t.Fatal(w.Body)
	}
	w := e.do("POST", "/api/v1/auth/login", `{"username":"boss","password":"boss password"}`, "")
	boss := coreSessionCookie(t, w).Value
	if w := e.do("PUT", fmt.Sprintf("/api/v1/system/users/%d", me.ID), `{"role":"viewer","currentPassword":"boss password"}`, boss); w.Code != http.StatusOK {
		t.Fatalf("demote: %d %s", w.Code, w.Body)
	}
	coreWantError(t, e.do("GET", "/api/v1/auth/me", "", admin), http.StatusUnauthorized, "unauthorized", "")
	coreWantError(t, e.do("GET", "/api/v1/auth/me", "", adminTok), http.StatusUnauthorized, "unauthorized", "")
}
