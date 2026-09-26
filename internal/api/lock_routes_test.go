package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/config"
)

// routeClass is the expected classification of a non-GET route
// (docs/ARCHITECTURE.md 6.1, the configuration lock and the destructive
// switch).
type routeClass struct {
	lock        lockClass
	destructive bool
}

// expectedClasses lists every non-GET route. A new route must be added
// here; a route that is missing or classified differently fails the test.
var expectedClasses = func() map[string]routeClass {
	m := map[string]routeClass{}
	add := func(c routeClass, patterns ...string) {
		for _, p := range patterns {
			m[p] = c
		}
	}
	none, locked, exempt := routeClass{lock: lockNone}, routeClass{lock: lockLocked}, routeClass{lock: lockExempt}
	destroy := routeClass{lock: lockLocked, destructive: true}
	add(exempt,
		"POST /api/v1/system/restart", "POST /api/v1/system/update/check", "POST /api/v1/system/backups/scheduled/run",
		"PUT /api/v1/parental/groups/{id}/override", "DELETE /api/v1/parental/groups/{id}/override",
		"PUT /api/v1/parental/groups/{id}/pause", "DELETE /api/v1/parental/groups/{id}/pause",
		"POST /api/v1/dns/cache/flush", "POST /api/v1/dns/upstreams/test",
		"POST /api/v1/filter/lists/refresh", "POST /api/v1/filter/lists/{id}/refresh", "POST /api/v1/download-cache/source/refresh",
		"POST /api/v1/network/scan", "POST /api/v1/dhcp/probe", "POST /api/v1/notifications/channels/{id}/test",
		"POST /api/v1/storage/targets/{id}/test", "POST /api/v1/storage/targets/{id}/benchmark", "DELETE /api/v1/storage/benchmark",
		"POST /api/v1/cache/verify")
	add(routeClass{lock: lockPause}, "POST /api/v1/dns/blocking")
	add(none,
		"POST /api/v1/auth/setup", "POST /api/v1/auth/login", "POST /api/v1/auth/logout", "POST /api/v1/dns/lookup",
		"POST /api/v1/filter/explain", "POST /api/v1/auth/password", "DELETE /api/v1/auth/sessions/{id}",
		"POST /api/v1/auth/totp/begin", "POST /api/v1/auth/totp/confirm", "POST /api/v1/auth/totp/disable",
		"POST /api/v1/tokens", "DELETE /api/v1/tokens/{id}")
	add(locked,
		"PUT /api/v1/settings", "PATCH /api/v1/settings/{section}",
		"POST /api/v1/dns/records", "PUT /api/v1/dns/records/{id}", "DELETE /api/v1/dns/records/{id}",
		"POST /api/v1/dns/forwarders", "POST /api/v1/dns/forwarders/import", "PUT /api/v1/dns/forwarders/{id}",
		"DELETE /api/v1/dns/forwarders/{id}", "POST /api/v1/dns/blocked-clients", "DELETE /api/v1/dns/blocked-clients",
		"POST /api/v1/clients", "PUT /api/v1/clients/{id}", "DELETE /api/v1/clients/{id}",
		"POST /api/v1/groups", "PUT /api/v1/groups/{id}", "DELETE /api/v1/groups/{id}",
		"POST /api/v1/filter/lists", "PUT /api/v1/filter/lists/{id}", "DELETE /api/v1/filter/lists/{id}",
		"POST /api/v1/filter/rules", "PUT /api/v1/filter/rules/{id}", "DELETE /api/v1/filter/rules/{id}",
		"PUT /api/v1/parental/groups/{id}",
		"PUT /api/v1/download-cache/services/{id}/enabled", "PUT /api/v1/download-cache/services/{id}/domains",
		"POST /api/v1/download-cache/services", "PUT /api/v1/download-cache/services/{id}",
		"DELETE /api/v1/download-cache/services/{id}", "PUT /api/v1/download-cache/labels",
		"DELETE /api/v1/cache/objects/{id}", "POST /api/v1/cache/objects/{id}/pin", "POST /api/v1/cache/groups/pin",
		"POST /api/v1/cache/evict", "DELETE /api/v1/cache/noslice/{host}",
		"DELETE /api/v1/dhcp/leases/{mac}", "POST /api/v1/dhcp/static", "POST /api/v1/dhcp/static/import",
		"PUT /api/v1/dhcp/static/{mac}", "DELETE /api/v1/dhcp/static/{mac}",
		"POST /api/v1/storage/targets", "PUT /api/v1/storage/targets/{id}",
		"POST /api/v1/storage/targets/{id}/apply", "POST /api/v1/storage/targets/{id}/activate",
		"POST /api/v1/notifications/channels", "PUT /api/v1/notifications/channels/{id}", "DELETE /api/v1/notifications/channels/{id}",
		"POST /api/v1/system/update/apply",
		"POST /api/v1/system/users", "PUT /api/v1/system/users/{id}", "PUT /api/v1/system/tls", "POST /api/v1/system/tls/local-ca")
	add(destroy,
		"POST /api/v1/system/restore", "POST /api/v1/dhcp/reset", "DELETE /api/v1/dhcp/leases",
		"POST /api/v1/cache/services/{service}/purge", "POST /api/v1/cache/groups/delete", "POST /api/v1/storage/targets/{id}/init",
		"DELETE /api/v1/storage/targets/{id}", "DELETE /api/v1/system/backups/scheduled/files/{name}",
		"DELETE /api/v1/system/users/{id}", "DELETE /api/v1/system/tls")
	return m
}()

// Every registered route is classified exactly as the table says.
func TestRouteClassification(t *testing.T) {
	e := newCoreEnv(t)
	seen := map[string]bool{}
	for _, r := range e.srv.routes {
		method, _, _ := strings.Cut(r.Pattern, " ")
		if seen[r.Pattern] {
			t.Errorf("%s registered twice", r.Pattern)
		}
		seen[r.Pattern] = true
		if method == http.MethodGet || method == http.MethodHead {
			if r.Lock != lockNone || r.Destructive {
				t.Errorf("%s: %+v, want not locked and not destructive", r.Pattern, r)
			}
			continue
		}
		want, ok := expectedClasses[r.Pattern]
		if !ok {
			t.Errorf("%s is not classified in the test table", r.Pattern)
			continue
		}
		if r.Lock != want.lock || r.Destructive != want.destructive {
			t.Errorf("%s: lock %d destructive %v, want lock %d destructive %v", r.Pattern, r.Lock, r.Destructive, want.lock, want.destructive)
		}
	}
	for p := range expectedClasses {
		if !seen[p] {
			t.Errorf("%s is in the table but not registered", p)
		}
	}
}

// PICACHE_CONFIG_LOCKED refuses locked routes for sessions (config_locked,
// before the body is read) and lets admin tokens, exempt routes and the
// own account through; /auth/status reports it.
func TestConfigLock(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")
	e.srv.d.Config.ConfigLocked = true
	w := e.do("PATCH", "/api/v1/settings/web", `not even JSON`, session)
	coreWantError(t, w, http.StatusForbidden, "config_locked", "")
	if !strings.Contains(w.Body.String(), "the configuration is locked on this host (PICACHE_CONFIG_LOCKED); change it with an admin API token or unset the variable") {
		t.Fatalf("message %s", w.Body)
	}
	coreWantError(t, e.do("POST", "/api/v1/system/users", `{}`, session), http.StatusForbidden, "config_locked", "")
	coreWantError(t, e.do("POST", "/api/v1/dns/blocking", `{"enabled":false}`, session), http.StatusForbidden, "config_locked", "")
	if w := e.do("PATCH", "/api/v1/settings/web", `{"language":"de"}`, adminTok); w.Code != http.StatusOK {
		t.Fatalf("admin token: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/v1/system/restart", "", session); w.Code != http.StatusAccepted {
		t.Fatalf("exempt route: %d %s", w.Code, w.Body)
	}
	coreWantError(t, e.do("POST", "/api/v1/auth/password", `{"currentPassword":"x","newPassword":"y"}`, session),
		http.StatusBadRequest, "invalid", "newPassword")
	var st authStatusResponse
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", session), &st)
	if !st.ConfigLocked || !st.DestructiveAPI || st.User == nil || st.User.Role != auth.RoleAdmin || st.Scope != auth.ScopeAdmin {
		t.Fatalf("status %+v", st)
	}
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", ""), &st)
	if st.ConfigLocked || st.DestructiveAPI || st.Authenticated {
		t.Fatalf("anonymous status %+v", st)
	}
}

// PICACHE_DESTRUCTIVE_API=false refuses every destructive route for
// sessions and tokens alike.
func TestDestructiveSwitch(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")
	e.srv.d.Config.DestructiveAPI = false
	for p, c := range expectedClasses {
		if !c.destructive {
			continue
		}
		method, path, _ := strings.Cut(p, " ")
		path = strings.NewReplacer("{id}", "7", "{service}", "steam", "{name}", "picache-backup-x-20260101T000000Z.db").Replace(path)
		w := e.do(method, path, "", session)
		coreWantError(t, w, http.StatusForbidden, "forbidden", "")
		if !strings.Contains(w.Body.String(), "PICACHE_DESTRUCTIVE_API=false") {
			t.Errorf("%s (session): %s", p, w.Body)
		}
		w = e.do(method, path, "", adminTok)
		coreWantError(t, w, http.StatusForbidden, "forbidden", "")
	}
	var st authStatusResponse
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", session), &st)
	if st.DestructiveAPI {
		t.Fatal("status must report the switch")
	}
}

// Lock class pause: resuming and a timed pause pass while the
// configuration is locked, a permanent disable only for tokens.
func TestBlockingPauseClass(t *testing.T) {
	e := newDNSTestEnv(t)
	e.srv.d.Config = &config.Config{ConfigLocked: true, DestructiveAPI: true}
	call := func(body string, token int64) int {
		r := coreRequest("POST", "/api/v1/dns/blocking", body)
		p := &auth.Principal{UserID: 1, Username: "admin", SessionID: "s1", TokenID: token, Scope: auth.ScopeAdmin}
		r = r.WithContext(context.WithValue(r.Context(), principalKey, p))
		w := httptest.NewRecorder()
		if err := e.srv.dnsBlockingSet(w, r); err != nil {
			writeError(w, r, e.srv.log, err)
		}
		return w.Code
	}
	if code := call(`{"enabled":false,"pauseSeconds":600}`, 0); code != http.StatusOK {
		t.Fatalf("timed pause: %d", code)
	}
	if code := call(`{"enabled":true}`, 0); code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	if code := call(`{"enabled":false}`, 0); code != http.StatusForbidden {
		t.Fatalf("permanent disable by a session: %d", code)
	}
	// 2^55 seconds overflow a time.Duration to exactly 0: refused as out of
	// range instead of becoming a permanent disable past the lock.
	for _, body := range []string{`{"enabled":false,"pauseSeconds":36028797018963968}`, `{"enabled":false,"pauseSeconds":604801}`,
		`{"enabled":false,"pauseSeconds":-1}`} {
		if code := call(body, 0); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", body, code)
		}
	}
	if !e.srv.d.DNS.Blocking().Enabled {
		t.Fatal("blocking must stay enabled")
	}
	if code := call(`{"enabled":false}`, 3); code != http.StatusOK {
		t.Fatalf("permanent disable by a token: %d", code)
	}
}
