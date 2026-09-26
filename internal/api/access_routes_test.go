package api

import (
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const testProxy = "192.168.1.5"

// reqOpt changes a test request.
type reqOpt func(*http.Request)

func from(addr string) reqOpt { return func(r *http.Request) { r.RemoteAddr = addr } }

func header(k, v string) reqOpt { return func(r *http.Request) { r.Header.Add(k, v) } }

func overTLS(version uint16) reqOpt {
	return func(r *http.Request) { r.TLS = &tls.ConnectionState{Version: version, HandshakeComplete: true} }
}

// req sends a request with a credential (as do) and options.
func (e *coreEnv) req(method, target, body, cred string, opts ...reqOpt) *httptest.ResponseRecorder {
	r := coreRequest(method, target, body)
	switch {
	case strings.HasPrefix(cred, "pc_"):
		r.Header.Set("Authorization", "Bearer "+cred)
	case cred != "":
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: cred})
	}
	for _, o := range opts {
		o(r)
	}
	return e.serve(r)
}

func (e *coreEnv) web(t *testing.T, fn func(w *settings.Web)) {
	t.Helper()
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { fn(&a.Web); return nil }); err != nil {
		t.Fatal(err)
	}
}

// Stage 2: every request of an address outside the web ACL is refused with
// 403 (a text page for browsers), with the security headers; loopback,
// the private ranges and the allowed networks pass; the refusals count.
func TestWebAccessStage2(t *testing.T) {
	e := newCoreEnv(t)
	e.web(t, func(w *settings.Web) { w.RestrictToNetworks = true })
	w := e.req("GET", "/api/v1/auth/status", "", "", from("192.0.2.1:4000"))
	coreWantError(t, w, http.StatusForbidden, "forbidden", "")
	if !strings.Contains(w.Body.String(), "this address may not use the web UI") || w.Header().Get("Content-Security-Policy") != csp ||
		w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("refusal %s %v", w.Body, w.Header())
	}
	w = e.req("GET", "/", "", "", from("192.0.2.1:4000"), header("Accept", "text/html,application/xhtml+xml"))
	if w.Code != http.StatusForbidden || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") ||
		!strings.Contains(w.Body.String(), "PiCache does not allow the web UI from 192.0.2.1.") ||
		!strings.Contains(w.Body.String(), "picache web-access --reset") {
		t.Fatalf("html refusal %d %q", w.Code, w.Body)
	}
	for _, path := range []string{"/healthz", "/metrics"} {
		if w := e.req("GET", path, "", "", from("192.0.2.1:4000")); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	for _, addr := range []string{"127.0.0.1:1", "[::1]:1", "192.168.7.7:1", "10.1.1.1:1", "[fd00::1]:1", "[::ffff:192.168.1.9]:1"} {
		if w := e.req("GET", "/api/v1/auth/status", "", "", from(addr)); w.Code != http.StatusOK {
			t.Errorf("%s refused: %d", addr, w.Code)
		}
	}
	e.web(t, func(w *settings.Web) { w.AllowedNetworks = []string{"192.0.2.0/24"} })
	if w := e.req("GET", "/api/v1/auth/status", "", "", from("192.0.2.1:4000")); w.Code != http.StatusOK {
		t.Fatalf("allowed network refused: %d", w.Code)
	}
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error {
		a.Web.AllowedNetworks, a.DNS.AllowedNetworks = nil, []string{"192.0.2.0/24"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := e.req("GET", "/api/v1/auth/status", "", "", from("192.0.2.1:4000")); w.Code != http.StatusOK {
		t.Fatalf("dns.allowedNetworks must allow the web UI too: %d", w.Code)
	}
	session := e.provisionAndLogin(t)
	var info struct {
		WebRefused    int    `json:"webRefused"`
		ClientAddress string `json:"clientAddress"`
		PeerAddress   string `json:"peerAddress"`
	}
	coreDecode(t, e.req("GET", "/api/v1/system/info", "", session), &info)
	if info.WebRefused != 4 || info.ClientAddress != "192.0.2.1" || info.PeerAddress != "192.0.2.1" {
		t.Fatalf("system info %+v", info)
	}
}

// A keep-alive connection from an address that was just removed is
// refused from its next request.
func TestWebAccessKeepAliveRefusedAfterRemoval(t *testing.T) {
	e := newCoreEnv(t)
	e.web(t, func(w *settings.Web) { w.RestrictToNetworks, w.AllowedNetworks = true, []string{"198.51.100.0/24"} })
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "198.51.100.7:4711" // the peer of this connection
		r.Host = coreHost
		e.srv.Handler().ServeHTTP(w, r)
	}))
	defer ts.Close()
	client := ts.Client()
	reused := false
	get := func() int {
		trace := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) { reused = i.Reused }}
		r, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), "GET", ts.URL+"/api/v1/auth/status", nil)
		resp, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		// Read to EOF: only then does the transport reuse the connection.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get(); code != http.StatusOK {
		t.Fatalf("first request %d", code)
	}
	e.web(t, func(w *settings.Web) { w.AllowedNetworks = nil })
	if code := get(); code != http.StatusForbidden || !reused {
		t.Fatalf("second request on the same connection: %d (reused %v)", code, reused)
	}
}

// Only a trusted proxy's X-Forwarded-For is read (never Forwarded or
// X-Real-IP), and every consumer uses the effective client: RemoteAddr,
// the login throttle, the session IP, the audit IP and the password
// confirmation throttle.
func TestForwardedClientConsumers(t *testing.T) {
	e := newCoreEnv(t)
	e.web(t, func(w *settings.Web) { w.TrustedProxies = []string{testProxy} })
	e.srv.mux.HandleFunc("GET /api/v1/test-client", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", r.RemoteAddr, clientIP(r))
	})
	viaProxy := func(client string) []reqOpt {
		return []reqOpt{from(testProxy + ":40000"), header("X-Forwarded-For", client)}
	}
	for _, tc := range []struct {
		name string
		opts []reqOpt
		want string
	}{
		{"trusted proxy", viaProxy("203.0.113.9"), "203.0.113.9:0 203.0.113.9"},
		{"spoofed Forwarded and X-Real-IP ignored", append(viaProxy("203.0.113.9"), header("Forwarded", "for=6.6.6.6"),
			header("X-Real-IP", "7.7.7.7"), header("True-Client-IP", "8.8.8.8")), "203.0.113.9:0 203.0.113.9"},
		{"several lines", append(viaProxy("1.1.1.1"), header("X-Forwarded-For", "203.0.113.10, "+testProxy)), "203.0.113.10:0 203.0.113.10"},
		{"untrusted peer", []reqOpt{from("192.168.1.6:1"), header("X-Forwarded-For", "203.0.113.9")}, "192.168.1.6:0 192.168.1.6"},
		{"no header", []reqOpt{from(testProxy + ":1")}, testProxy + ":0 " + testProxy},
		{"ipv6 with port", viaProxy("[2001:db8::9]:443"), "[2001:db8::9]:0 2001:db8::9"},
	} {
		w := e.req("GET", "/api/v1/test-client", "", "", tc.opts...)
		if w.Code != http.StatusOK || w.Body.String() != tc.want {
			t.Errorf("%s: %d %q, want %q", tc.name, w.Code, w.Body, tc.want)
		}
	}
	w := e.req("GET", "/api/v1/test-client", "", "", viaProxy("203.0.113.9, unknown")...)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "")
	if !strings.Contains(w.Body.String(), "malformed X-Forwarded-For header") || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("malformed: %s %v", w.Body, w.Header())
	}

	if err := e.auth.Provision(context.Background(), "admin", corePassword); err != nil {
		t.Fatal(err)
	}
	// Login throttle per effective client: client A is locked after 5
	// failures, client B behind the same proxy is not.
	for i := range 5 {
		e.req("POST", "/api/v1/auth/login", fmt.Sprintf(`{"username":"u%d","password":"wrong password"}`, i), "", viaProxy("203.0.113.20")...)
	}
	coreWantError(t, e.req("POST", "/api/v1/auth/login", `{"username":"u9","password":"wrong password"}`, "", viaProxy("203.0.113.20")...),
		http.StatusTooManyRequests, "too_many_requests", "")
	coreWantError(t, e.req("POST", "/api/v1/auth/login", `{"username":"u9","password":"wrong password"}`, "", viaProxy("203.0.113.21")...),
		http.StatusUnauthorized, "unauthorized", "")
	login := func(client string) string {
		w := e.req("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+corePassword+`"}`, "", viaProxy(client)...)
		if w.Code != http.StatusOK {
			t.Fatalf("login from %s: %d %s", client, w.Code, w.Body)
		}
		return coreSessionCookie(t, w).Value
	}
	s1, s2 := login("203.0.113.30"), login("203.0.113.31")
	var sessions []auth.SessionInfo
	coreDecode(t, e.req("GET", "/api/v1/auth/sessions", "", s1, viaProxy("203.0.113.30")...), &sessions)
	ips := map[string]bool{}
	for _, s := range sessions {
		ips[s.IP] = true
	}
	if !ips["203.0.113.30"] || !ips["203.0.113.31"] || ips[testProxy] {
		t.Fatalf("session IPs %v", ips)
	}
	entries, _, err := e.auth.AuditLog(context.Background(), auth.AuditQuery{Search: "auth.login", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, en := range entries {
		if en.Action == "auth.login" && en.IP != "203.0.113.30" && en.IP != "203.0.113.31" {
			t.Fatalf("audit IP %q", en.IP)
		}
	}
	// Password confirmations are throttled by the effective client:
	// session 1 fails 5 times from client C; session 2 is then refused from
	// C but not from D.
	bad := `{"name":"x","scope":"read","currentPassword":"wrong password"}`
	time.Sleep(1100 * time.Millisecond) // the global attempt limit (10/s) refills
	for range 5 {
		e.req("POST", "/api/v1/tokens", bad, s1, viaProxy("203.0.113.40")...)
	}
	coreWantError(t, e.req("POST", "/api/v1/tokens", bad, s2, viaProxy("203.0.113.40")...), http.StatusTooManyRequests, "too_many_requests", "")
	coreWantError(t, e.req("POST", "/api/v1/tokens", bad, s2, viaProxy("203.0.113.41")...), http.StatusBadRequest, "invalid", "currentPassword")
}

// X-Forwarded-Proto: https of a trusted proxy is the effective scheme
// (__Host- cookie, no redirect loop, HSTS); from an untrusted peer it
// changes nothing.
func TestForwardedScheme(t *testing.T) {
	e := newCoreEnv(t)
	e.web(t, func(w *settings.Web) { w.TrustedProxies, w.RedirectToHTTPS = []string{testProxy}, true })
	if err := e.auth.Provision(context.Background(), "admin", corePassword); err != nil {
		t.Fatal(err)
	}
	login := `{"username":"admin","password":"` + corePassword + `"}`
	w := e.req("POST", "/api/v1/auth/login", login, "", from(testProxy+":1"), header("X-Forwarded-Proto", "https"))
	if c := coreSessionCookie(t, w); w.Code != http.StatusOK || c.Name != auth.SecureSessionCookie || !c.Secure {
		t.Fatalf("login via TLS proxy: %d %+v", w.Code, c)
	}
	if hsts := w.Header().Get("Strict-Transport-Security"); !strings.HasPrefix(hsts, "max-age=") {
		t.Fatalf("HSTS %q", hsts)
	}
	if w := e.req("GET", "/api/v1/auth/status", "", "", from(testProxy+":1"), header("X-Forwarded-Proto", "http, HTTPS")); w.Code != http.StatusOK {
		t.Fatalf("no redirect loop: %d", w.Code)
	}
	w = e.req("GET", "/api/v1/auth/status", "", "", from("192.168.1.6:1"), header("X-Forwarded-Proto", "https"))
	if w.Code != http.StatusTemporaryRedirect || w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatalf("untrusted peer: %d %v", w.Code, w.Header())
	}
}

// The lock-out check of settings changes (PATCH and PUT, sessions and
// admin tokens): the field names the change, the message the address.
func TestWebLockoutCheck(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	adminTok := e.createToken(t, session, "admin")
	remote := from("192.0.2.1:4000") // outside the private ranges
	patch := func(body, cred string, opts ...reqOpt) *httptest.ResponseRecorder {
		return e.req("PATCH", "/api/v1/settings/web", body, cred, opts...)
	}
	w := patch(`{"restrictToNetworks":true}`, session, remote)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "web.restrictToNetworks")
	if !strings.Contains(w.Body.String(), "this change would lock out your address 192.0.2.1; allow it first") {
		t.Fatalf("message %s", w.Body)
	}
	coreWantError(t, patch(`{"restrictToNetworks":true}`, adminTok, remote), http.StatusBadRequest, "invalid", "web.restrictToNetworks")
	full := e.set.Get().Clone()
	full.Web.RestrictToNetworks = true
	body, _ := jsonMarshal(full)
	coreWantError(t, e.req("PUT", "/api/v1/settings", body, session, remote), http.StatusBadRequest, "invalid", "web.restrictToNetworks")
	// From this machine it always passes; with the address allowed too.
	if w := patch(`{"restrictToNetworks":true}`, session, from("127.0.0.1:1")); w.Code != http.StatusOK {
		t.Fatalf("loopback: %d %s", w.Code, w.Body)
	}
	e.web(t, func(w *settings.Web) { w.RestrictToNetworks = false })
	if w := patch(`{"restrictToNetworks":true,"allowedNetworks":["192.0.2.0/24"]}`, session, remote); w.Code != http.StatusOK {
		t.Fatalf("with the address allowed: %d %s", w.Code, w.Body)
	}
	// Removing the entry that allows the requester.
	coreWantError(t, patch(`{"allowedNetworks":[]}`, session, remote), http.StatusBadRequest, "invalid", "web.allowedNetworks")
	// A validation error wins over the lock-out check.
	coreWantError(t, patch(`{"allowedNetworks":["garbage"]}`, session, remote), http.StatusBadRequest, "invalid", "web.allowedNetworks[0]")
	// Trusting a proxy changes who the requester is: 192.0.2.1 forwards for
	// 198.51.100.9, which is not allowed.
	xff := header("X-Forwarded-For", "198.51.100.9")
	w = patch(`{"trustedProxies":["192.0.2.1"]}`, session, remote, xff)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "web.trustedProxies")
	if !strings.Contains(w.Body.String(), "lock out your address 198.51.100.9") {
		t.Fatalf("message %s", w.Body)
	}
	w = patch(`{"trustedProxies":["192.0.2.1"]}`, session, remote, header("X-Forwarded-For", "unknown"))
	coreWantError(t, w, http.StatusBadRequest, "invalid", "web.trustedProxies")
	if !strings.Contains(w.Body.String(), "this change would lock you out: the X-Forwarded-For header of 192.0.2.1 cannot be read") {
		t.Fatalf("message %s", w.Body)
	}
	if w := patch(`{"trustedProxies":["192.0.2.1"],"allowedNetworks":["192.0.2.0/24","198.51.100.9"]}`, session, remote, xff); w.Code != http.StatusOK {
		t.Fatalf("proxy with its client allowed: %d %s", w.Code, w.Body)
	}
	// No longer trusting the (public) proxy the request came through
	// refuses the connection itself: the message names the proxy.
	e.web(t, func(w *settings.Web) { w.AllowedNetworks = []string{"198.51.100.9"} })
	w = patch(`{"trustedProxies":[]}`, session, remote, xff)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "web.trustedProxies")
	if !strings.Contains(w.Body.String(), "this change would lock out your connection from 192.0.2.1; allow it first") {
		t.Fatalf("message %s", w.Body)
	}
	// dns.allowedNetworks allows the web UI too, so removing it is checked.
	e.web(t, func(w *settings.Web) { w.TrustedProxies, w.AllowedNetworks = nil, nil })
	if _, err := e.set.Update(context.Background(), func(a *settings.All) error { a.DNS.AllowedNetworks = []string{"192.0.2.0/24"}; return nil }); err != nil {
		t.Fatal(err)
	}
	coreWantError(t, e.req("PATCH", "/api/v1/settings/dns", `{"allowedNetworks":[]}`, session, remote),
		http.StatusBadRequest, "invalid", "dns.allowedNetworks")
}

// web.tlsMinVersion 1.3 is refused from a request over TLS 1.2.
func TestTLSMinVersionGuard(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	w := e.req("PATCH", "/api/v1/settings/web", `{"tlsMinVersion":"1.3"}`, session, overTLS(tls.VersionTLS12))
	coreWantError(t, w, http.StatusBadRequest, "invalid", "web.tlsMinVersion")
	if !strings.Contains(w.Body.String(), "your browser is connected with TLS 1.2") {
		t.Fatalf("message %s", w.Body)
	}
	coreWantError(t, e.req("PATCH", "/api/v1/settings/web", `{"tlsMinVersion":"1.1"}`, session), http.StatusBadRequest, "invalid", "web.tlsMinVersion")
	if w := e.req("PATCH", "/api/v1/settings/web", `{"tlsMinVersion":"1.3"}`, session, overTLS(tls.VersionTLS13)); w.Code != http.StatusOK {
		t.Fatalf("over TLS 1.3: %d %s", w.Code, w.Body)
	}
	if w := e.req("PATCH", "/api/v1/settings/web", `{"tlsMinVersion":"1.2"}`, session); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	if w := e.req("PATCH", "/api/v1/settings/web", `{"tlsMinVersion":"1.3"}`, session); w.Code != http.StatusOK {
		t.Fatalf("over plain HTTP (unaffected): %d %s", w.Code, w.Body)
	}
}

// A restore is never refused; settings that would lock the requester out
// (or need TLS 1.3 from a TLS 1.2 connection) add webAccessWarning.
func TestRestoreWebAccessWarning(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)
	restore := func(opts ...reqOpt) map[string]any {
		t.Helper()
		r := coreRequest("POST", "/api/v1/system/restore", "")
		r.Body = http.NoBody
		r.Header.Set("Content-Type", "application/octet-stream")
		r.Header.Set(restorePasswordHeader, corePassword)
		r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: session})
		for _, o := range opts {
			o(r)
		}
		w := e.serve(r)
		if w.Code != http.StatusAccepted {
			t.Fatalf("restore %d %s", w.Code, w.Body)
		}
		var out map[string]any
		coreDecode(t, w, &out)
		return out
	}
	staged := settings.Defaults() // restricted
	e.rt.staged = &staged
	out := restore(from("192.0.2.1:4000"))
	if msg, _ := out["webAccessWarning"].(string); !strings.HasPrefix(msg, "After the restart the restored settings will not allow your address 192.0.2.1") {
		t.Fatalf("warning %v", out)
	}
	if _, ok := restore(from("192.168.1.20:4000"))["webAccessWarning"]; ok {
		t.Fatal("no warning for an allowed address")
	}
	staged.Web.TLSMinVersion = "1.3"
	if msg, _ := restore(from("192.168.1.20:4000"), overTLS(tls.VersionTLS12))["webAccessWarning"].(string); !strings.Contains(msg, "require TLS 1.3") {
		t.Fatalf("TLS warning %q", msg)
	}
	e.rt.staged = nil
	if _, ok := restore(from("192.0.2.1:4000"))["webAccessWarning"]; ok {
		t.Fatal("no warning without staged settings")
	}
}

func jsonMarshal(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// The host allowlist logs the effective client, never the proxy.
func TestHostAllowlistLogsEffectiveClient(t *testing.T) {
	e := newCoreEnv(t)
	e.web(t, func(w *settings.Web) { w.TrustedProxies = []string{testProxy} })
	var buf strings.Builder
	var mu sync.Mutex
	d := e.srv.d
	d.Log = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	srv := New(d)
	r := coreRequest("GET", "/api/v1/auth/status", "")
	r.Host, r.RemoteAddr = "evil.example", testProxy+":1"
	r.Header.Set("X-Forwarded-For", "203.0.113.50")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	mu.Lock()
	defer mu.Unlock()
	if w.Code != http.StatusMisdirectedRequest || !strings.Contains(buf.String(), "client=203.0.113.50") {
		t.Fatalf("%d, log %s", w.Code, buf.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
