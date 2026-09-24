package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestMiddlewareHostAllowlist(t *testing.T) {
	e := newCoreEnv(t)
	for _, tc := range []struct {
		host   string
		accept string
		status int
	}{
		{"192.168.1.2:8080", "", http.StatusOK},
		{"[fd00::1]:8080", "", http.StatusOK},
		{"localhost:8080", "", http.StatusOK},
		{"picache.example", "", http.StatusOK},   // PICACHE_WEB_HOSTS
		{"PICACHE.lan.:8080", "", http.StatusOK}, // server name + local domain, case/trailing dot
		{"evil.example", "", http.StatusMisdirectedRequest},
		{"evil.example", "text/html,application/xhtml+xml", http.StatusMisdirectedRequest},
	} {
		t.Run(tc.host+" "+tc.accept, func(t *testing.T) {
			r := coreRequest("GET", "/api/v1/auth/status", "")
			r.Host = tc.host
			if tc.accept != "" {
				r.Header.Set("Accept", tc.accept)
			}
			w := e.serve(r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			if tc.status != http.StatusMisdirectedRequest {
				return
			}
			if tc.accept == "" {
				coreWantError(t, w, http.StatusMisdirectedRequest, "misdirected", "")
			} else if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") ||
				!strings.Contains(w.Body.String(), `"evil.example"`) {
				t.Fatalf("html variant: %q %q", ct, w.Body)
			}
		})
	}
	// /healthz is reachable under any name (container health checks).
	r := coreRequest("GET", "/healthz", "")
	r.Host = "evil.example"
	if w := e.serve(r); w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("healthz: %d %q", w.Code, w.Body)
	}
}

func TestMiddlewareCrossOriginProtection(t *testing.T) {
	e := newCoreEnv(t)
	login := `{"username":"admin","password":"x"}`
	for _, tc := range []struct {
		name    string
		headers map[string]string
		blocked bool
	}{
		{"cross-site fetch", map[string]string{"Sec-Fetch-Site": "cross-site"}, true},
		{"same-site fetch", map[string]string{"Sec-Fetch-Site": "same-site"}, true},
		{"foreign origin", map[string]string{"Origin": "http://evil.example"}, true},
		{"same origin", map[string]string{"Sec-Fetch-Site": "same-origin"}, false},
		{"matching origin", map[string]string{"Origin": "http://" + coreHost}, false},
		{"non-browser client", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := coreRequest("POST", "/api/v1/auth/login", login)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := e.serve(r)
			if tc.blocked {
				coreWantError(t, w, http.StatusForbidden, "forbidden", "")
			} else if w.Code != http.StatusUnauthorized { // reached the handler: wrong credentials
				t.Fatalf("status %d (%s)", w.Code, w.Body)
			}
		})
	}
}

func TestMiddlewareSecurityHeadersAndHTTPS(t *testing.T) {
	e := newCoreEnv(t)
	w := e.do("GET", "/api/v1/auth/status", "", "")
	for name, want := range map[string]string{
		"Content-Security-Policy":      csp,
		"X-Frame-Options":              "DENY",
		"X-Content-Type-Options":       "nosniff",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := w.Header().Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP too weak: %s", csp)
	}

	tlsReq := func(path string) *http.Request {
		return coreRequest("GET", "https://"+coreHost+path, "")
	}
	if hsts := e.serve(tlsReq("/api/v1/auth/status")).Header().Get("Strict-Transport-Security"); hsts != "" {
		t.Fatalf("HSTS without redirect: %q", hsts)
	}
	if _, err := e.set.Update(t.Context(), func(a *settings.All) error { a.Web.RedirectToHTTPS = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if hsts := e.serve(tlsReq("/api/v1/auth/status")).Header().Get("Strict-Transport-Security"); !strings.HasPrefix(hsts, "max-age=") {
		t.Fatalf("HSTS with redirect: %q", hsts)
	}
	w = e.do("GET", "/api/v1/auth/status?x=1", "", "")
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Location") != "https://192.168.1.2:8443/api/v1/auth/status?x=1" {
		t.Fatalf("redirect: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := e.do("GET", "/healthz", "", ""); w.Code != http.StatusOK {
		t.Fatalf("healthz must not redirect: %d", w.Code)
	}
}

func TestMiddlewarePermissions(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")

	// permSession: an admin API token is rejected, the browser session and
	// the session token as Bearer are accepted.
	coreWantError(t, e.do("GET", "/api/v1/auth/sessions", "", adminTok), http.StatusForbidden, "forbidden", "")
	coreWantError(t, e.do("POST", "/api/v1/auth/password", `{"currentPassword":"a","newPassword":"b"}`, adminTok),
		http.StatusForbidden, "forbidden", "")
	for _, cred := range []string{session, "Bearer " + session} {
		if w := e.do("GET", "/api/v1/auth/sessions", "", cred); w.Code != http.StatusOK {
			t.Fatalf("session credential %q: %d", cred[:10], w.Code)
		}
	}
	coreWantError(t, e.do("GET", "/api/v1/nonexistent", "", session), http.StatusNotFound, "not_found", "")

	e.srv.mux.HandleFunc("GET /api/v1/test-panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
	if w := e.do("GET", "/api/v1/test-panic", "", ""); w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("panic: %d %q", w.Code, w.Body)
	}
}
