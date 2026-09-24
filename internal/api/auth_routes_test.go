package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/auth"
)

type coreStatus struct {
	SetupRequired bool       `json:"setupRequired"`
	Authenticated bool       `json:"authenticated"`
	User          *auth.User `json:"user"`
	Scope         string     `json:"scope"`
	TokenAuth     bool       `json:"tokenAuth"`
	Language      string     `json:"language"`
	SetupHints    []string   `json:"setupHints"`
}

func TestAuthRoutesSetupLoginLogout(t *testing.T) {
	e := newCoreEnv(t)

	var st coreStatus
	w := e.do("GET", "/api/v1/auth/status", "", "")
	coreDecode(t, w, &st)
	if !st.SetupRequired || st.Authenticated || len(st.SetupHints) == 0 {
		t.Fatalf("status before setup = %+v", st)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("API responses must not be cached")
	}

	w = e.do("POST", "/api/v1/auth/setup", `{"setupToken":"WRONG","username":"admin","password":"`+corePassword+`"}`, "")
	coreWantError(t, w, http.StatusForbidden, "forbidden", "")
	w = e.do("POST", "/api/v1/auth/setup", `{"setupToken":"x","username":"admin","password":"p","extra":1}`, "")
	coreWantError(t, w, http.StatusBadRequest, "invalid", "body")

	token := coreReadSetupToken(t, e.setupFile)
	w = e.do("POST", "/api/v1/auth/setup", `{"setupToken":"`+token+`","username":"admin","password":"short"}`, "")
	coreWantError(t, w, http.StatusBadRequest, "invalid", "password")
	w = e.do("POST", "/api/v1/auth/setup", `{"setupToken":"`+token+`","username":"admin","password":"`+corePassword+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	c := coreSessionCookie(t, w)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure || c.Path != "/" {
		t.Fatalf("session cookie attributes %+v", c)
	}
	var u auth.User
	coreDecode(t, w, &u)
	if u.Username != "admin" || u.ID == 0 {
		t.Fatalf("setup user = %+v", u)
	}
	w = e.do("POST", "/api/v1/auth/setup", `{"setupToken":"`+token+`","username":"x","password":"`+corePassword+`"}`, "")
	coreWantError(t, w, http.StatusForbidden, "forbidden", "")

	st = coreStatus{}
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", c.Value), &st)
	if st.SetupRequired || !st.Authenticated || st.User == nil || st.User.Username != "admin" ||
		st.Scope != "admin" || st.TokenAuth || st.SetupHints != nil {
		t.Fatalf("status after setup = %+v", st)
	}

	w = e.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"wrong password"}`, "")
	coreWantError(t, w, http.StatusUnauthorized, "unauthorized", "")
	w = e.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+corePassword+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	session := coreSessionCookie(t, w).Value

	if w := e.do("GET", "/api/v1/auth/me", "", session); w.Code != http.StatusOK {
		t.Fatalf("me: %d", w.Code)
	}
	w = e.do("POST", "/api/v1/auth/logout", "", session)
	if w.Code != http.StatusNoContent || coreSessionCookie(t, w).MaxAge >= 0 {
		t.Fatalf("logout: %d, cookie %+v", w.Code, coreSessionCookie(t, w))
	}
	coreWantError(t, e.do("GET", "/api/v1/auth/me", "", session), http.StatusUnauthorized, "unauthorized", "")
	coreWantError(t, e.do("GET", "/api/v1/auth/me", "", ""), http.StatusUnauthorized, "unauthorized", "")

	actions := e.auditActions(t)
	for _, want := range []string{"auth.setup", "auth.login", "auth.logout", "auth.login_failed"} {
		if !slices.Contains(actions, want) {
			t.Fatalf("audit %v lacks %s", actions, want)
		}
	}
}

// SEC-09: over TLS the session cookie is "__Host-picache_session" (Secure,
// Path=/, no Domain); PICACHE_WEB_SECURE_COOKIES forces Secure for plain
// HTTP behind a TLS proxy; /auth/status names the HTTPS port.
func TestAuthRoutesSecureCookieOverTLS(t *testing.T) {
	e := newCoreEnv(t)
	if err := e.auth.Provision(t.Context(), "admin", corePassword); err != nil {
		t.Fatal(err)
	}
	login := `{"username":"admin","password":"` + corePassword + `"}`
	w := e.serve(coreRequest("POST", "https://"+coreHost+"/api/v1/auth/login", login))
	c := coreSessionCookie(t, w)
	if w.Code != http.StatusOK || c.Name != auth.SecureSessionCookie || !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Fatalf("login over TLS: %d, cookie %+v", w.Code, c)
	}
	r := coreRequest("GET", "https://"+coreHost+"/api/v1/auth/me", "")
	r.AddCookie(&http.Cookie{Name: auth.SecureSessionCookie, Value: c.Value})
	if w := e.serve(r); w.Code != http.StatusOK {
		t.Fatalf("__Host- cookie: %d %s", w.Code, w.Body)
	}
	r = coreRequest("POST", "https://"+coreHost+"/api/v1/auth/logout", "")
	r.AddCookie(&http.Cookie{Name: auth.SecureSessionCookie, Value: c.Value})
	w = e.serve(r)
	var cleared []string
	for _, ck := range w.Result().Cookies() {
		if ck.MaxAge < 0 {
			cleared = append(cleared, ck.Name)
		}
	}
	if w.Code != http.StatusNoContent || !slices.Contains(cleared, auth.SecureSessionCookie) || !slices.Contains(cleared, auth.SessionCookie) {
		t.Fatalf("logout over TLS: %d, cleared %v", w.Code, cleared)
	}

	// Plain HTTP: the plain name, Secure only when forced.
	c = coreSessionCookie(t, e.do("POST", "/api/v1/auth/login", login, ""))
	if c.Name != auth.SessionCookie || c.Secure {
		t.Fatalf("login over HTTP: cookie %+v", c)
	}
	e.srv.d.Config.WebSecureCookies = true
	c = coreSessionCookie(t, e.do("POST", "/api/v1/auth/login", login, ""))
	if c.Name != auth.SessionCookie || !c.Secure {
		t.Fatalf("login over HTTP with PICACHE_WEB_SECURE_COOKIES: cookie %+v", c)
	}

	var st struct {
		HTTPSPort int `json:"httpsPort"`
	}
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", ""), &st)
	if st.HTTPSPort != 0 {
		t.Fatalf("httpsPort without an HTTPS listener = %d", st.HTTPSPort)
	}
	e.rt.tlsAddr = "[::]:8443"
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", ""), &st)
	if st.HTTPSPort != 8443 {
		t.Fatalf("httpsPort = %d, want 8443", st.HTTPSPort)
	}
}

// SEC-06: after setup, /auth/setup answers 403 at once, without using the
// global attempt budget, and locks the caller out like a password guesser.
func TestAuthRoutesSetupAfterCompletion(t *testing.T) {
	e := newCoreEnv(t)
	if err := e.auth.Provision(t.Context(), "admin", corePassword); err != nil {
		t.Fatal(err)
	}
	for range 30 {
		w := e.do("POST", "/api/v1/auth/setup", `{"setupToken":"AAAAAAAAAAAAAAAAAAAAAAAAAA","username":"x","password":"`+corePassword+`"}`, "")
		coreWantError(t, w, http.StatusForbidden, "forbidden", "")
	}
	r := coreRequest("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+corePassword+`"}`)
	r.RemoteAddr = "192.168.1.50:40000"
	if w := e.serve(r); w.Code != http.StatusOK {
		t.Fatalf("login from another client after setup spam: %d %s", w.Code, w.Body)
	}
	w := e.do("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+corePassword+`"}`, "")
	coreWantError(t, w, http.StatusTooManyRequests, "too_many_requests", "")
}

func TestAuthRoutesAccountSecurity(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)

	tok := e.createToken(t, session, "admin")
	w := e.do("POST", "/api/v1/auth/password", `{"currentPassword":"wrong password","newPassword":"a new password"}`, session)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "currentPassword")
	w = e.do("POST", "/api/v1/auth/password", `{"currentPassword":"`+corePassword+`","newPassword":"a new password"}`, session)
	if w.Code != http.StatusNoContent {
		t.Fatalf("change password: %d %s", w.Code, w.Body)
	}
	// SEC-03: the password change revokes the API tokens by default ...
	coreWantError(t, e.do("GET", "/api/v1/auth/me", "", tok), http.StatusUnauthorized, "unauthorized", "")
	// ... unless keepTokens is set.
	w = e.do("POST", "/api/v1/tokens", `{"name":"kept","scope":"read","currentPassword":"a new password"}`, session)
	var kept struct {
		Token string `json:"token"`
	}
	coreDecode(t, w, &kept)
	w = e.do("POST", "/api/v1/auth/password", `{"currentPassword":"a new password","newPassword":"`+corePassword+`","keepTokens":true}`, session)
	if w.Code != http.StatusNoContent {
		t.Fatalf("change password (keepTokens): %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/v1/auth/me", "", kept.Token); w.Code != http.StatusOK {
		t.Fatalf("keepTokens must keep the token: %d", w.Code)
	}

	var sessions []auth.SessionInfo
	coreDecode(t, e.do("GET", "/api/v1/auth/sessions", "", session), &sessions)
	if len(sessions) != 1 || !sessions[0].Current {
		t.Fatalf("sessions = %+v", sessions)
	}
	coreWantError(t, e.do("DELETE", "/api/v1/auth/sessions/nothex", "", session), http.StatusBadRequest, "invalid", "id")

	var begin struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	// SEC-03: starting TOTP enrolment needs the current password.
	coreWantError(t, e.do("POST", "/api/v1/auth/totp/begin", "", session), http.StatusBadRequest, "invalid", "body")
	coreWantError(t, e.do("POST", "/api/v1/auth/totp/begin", `{"currentPassword":"wrong password"}`, session),
		http.StatusBadRequest, "invalid", "currentPassword")
	coreDecode(t, e.do("POST", "/api/v1/auth/totp/begin", `{"currentPassword":"`+corePassword+`"}`, session), &begin)
	if begin.Secret == "" || !strings.HasPrefix(begin.URI, "otpauth://totp/") {
		t.Fatalf("totp begin = %+v", begin)
	}
	coreWantError(t, e.do("POST", "/api/v1/auth/totp/confirm", `{"code":"abc"}`, session), http.StatusBadRequest, "invalid", "code")

	w = e.do("DELETE", "/api/v1/auth/sessions/"+sessions[0].ID, "", session)
	if w.Code != http.StatusNoContent || coreSessionCookie(t, w).MaxAge >= 0 {
		t.Fatalf("revoking the current session must clear the cookie: %d", w.Code)
	}
	coreWantError(t, e.do("GET", "/api/v1/auth/me", "", session), http.StatusUnauthorized, "unauthorized", "")
}

func TestAuthRoutesTokens(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)

	pw := `,"currentPassword":"` + corePassword + `"`
	coreWantError(t, e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"root"`+pw+`}`, session), http.StatusBadRequest, "invalid", "scope")
	coreWantError(t, e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"read","expiresInDays":-1`+pw+`}`, session),
		http.StatusBadRequest, "invalid", "expiresInDays")
	// SEC-03: a session alone cannot create a token.
	coreWantError(t, e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"admin"}`, session),
		http.StatusBadRequest, "invalid", "currentPassword")
	coreWantError(t, e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"admin","currentPassword":"wrong password"}`, session),
		http.StatusBadRequest, "invalid", "currentPassword")

	w := e.do("POST", "/api/v1/tokens", `{"name":"grafana","scope":"admin","expiresInDays":30`+pw+`}`, session)
	var created struct {
		Token string         `json:"token"`
		Info  auth.TokenInfo `json:"info"`
	}
	coreDecode(t, w, &created)
	if w.Code != http.StatusCreated || !strings.HasPrefix(created.Token, "pc_") || created.Info.ExpiresAt.IsZero() {
		t.Fatalf("create: %d %+v", w.Code, created)
	}
	readTok := e.createToken(t, session, "read")

	var list []auth.TokenInfo
	coreDecode(t, e.do("GET", "/api/v1/tokens", "", session), &list)
	if len(list) != 2 || strings.Contains(e.do("GET", "/api/v1/tokens", "", session).Body.String(), created.Token) {
		t.Fatalf("list = %+v", list)
	}

	var st coreStatus
	coreDecode(t, e.do("GET", "/api/v1/auth/status", "", readTok), &st)
	if !st.Authenticated || !st.TokenAuth || st.Scope != "read" {
		t.Fatalf("status with token = %+v", st)
	}
	if w := e.do("GET", "/api/v1/auth/me", "", readTok); w.Code != http.StatusOK {
		t.Fatalf("read token on permRead: %d", w.Code)
	}
	coreWantError(t, e.do("GET", "/api/v1/system/audit", "", readTok), http.StatusForbidden, "forbidden", "")
	if w := e.do("GET", "/api/v1/system/audit", "", created.Token); w.Code != http.StatusOK {
		t.Fatalf("admin token on permAdmin: %d", w.Code)
	}
	// Tokens never manage account security.
	coreWantError(t, e.do("GET", "/api/v1/tokens", "", created.Token), http.StatusForbidden, "forbidden", "")
	// Logging out with a token ends nothing and is harmless.
	if w := e.do("POST", "/api/v1/auth/logout", "", created.Token); w.Code != http.StatusNoContent {
		t.Fatalf("token logout: %d", w.Code)
	}

	coreWantError(t, e.do("DELETE", "/api/v1/tokens/abc", "", session), http.StatusBadRequest, "invalid", "id")
	if w := e.do("DELETE", "/api/v1/tokens/"+strconv.FormatInt(created.Info.ID, 10), "", session); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	coreWantError(t, e.do("DELETE", "/api/v1/tokens/"+strconv.FormatInt(created.Info.ID, 10), "", session), http.StatusNotFound, "not_found", "")
	coreWantError(t, e.do("GET", "/api/v1/auth/me", "", created.Token), http.StatusUnauthorized, "unauthorized", "")

	entries, _, err := e.auth.AuditLog(t.Context(), auth.AuditQuery{Search: "token."})
	if err != nil || len(entries) != 3 {
		t.Fatalf("token audit = %+v, %v", entries, err)
	}
	for _, en := range entries {
		if strings.Contains(en.Details, created.Token) || strings.Contains(en.Details, readTok) {
			t.Fatalf("audit leaks a token secret: %+v", en)
		}
	}
}

// deviceCookie returns the device cookie a response sets (either name).
func deviceCookie(t *testing.T, w *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.DeviceCookie || c.Name == auth.SecureDeviceCookie {
			return c
		}
	}
	t.Fatalf("no device cookie in %v", w.Header())
	return nil
}

// SEC-05: sign-in and setup give the browser a device cookie; with it, a
// sign-in is not held up by the username delay that failures from other
// hosts keep running (without it, it is).
func TestAuthRoutesDeviceCookie(t *testing.T) {
	e := newCoreEnv(t)
	token := coreReadSetupToken(t, e.setupFile)
	w := e.do("POST", "/api/v1/auth/setup", `{"setupToken":"`+token+`","username":"admin","password":"`+corePassword+`"}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", w.Code, w.Body)
	}
	if c := deviceCookie(t, w); c.Name != auth.DeviceCookie || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge <= 0 {
		t.Fatalf("device cookie after setup: %+v", c)
	}

	login := `{"username":"admin","password":"` + corePassword + `"}`
	w = e.serve(coreRequest("POST", "https://"+coreHost+"/api/v1/auth/login", login))
	dev := deviceCookie(t, w)
	if w.Code != http.StatusOK || dev.Name != auth.SecureDeviceCookie || !dev.Secure || dev.Path != "/" || dev.Domain != "" {
		t.Fatalf("login over TLS: %d, device cookie %+v", w.Code, dev)
	}

	// Other hosts fail sign-ins for "admin" until the username is delayed
	// (over-long passwords fail at once, so the 1 s delay is still running
	// for the checks below).
	wrong := `{"username":"admin","password":"` + strings.Repeat("x", 1025) + `"}`
	for i := range 5 {
		r := coreRequest("POST", "/api/v1/auth/login", wrong)
		r.RemoteAddr = fmt.Sprintf("192.168.1.%d:40000", 200+i)
		if w := e.serve(r); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d %s", i, w.Code, w.Body)
		}
	}
	r := coreRequest("POST", "/api/v1/auth/login", login)
	coreWantError(t, e.serve(r), http.StatusTooManyRequests, "too_many_requests", "")
	// A forged value is ignored.
	r = coreRequest("POST", "/api/v1/auth/login", login)
	r.AddCookie(&http.Cookie{Name: auth.DeviceCookie, Value: "v1:" + strings.Repeat("A", 80)})
	coreWantError(t, e.serve(r), http.StatusTooManyRequests, "too_many_requests", "")

	r = coreRequest("POST", "https://"+coreHost+"/api/v1/auth/login", login)
	r.AddCookie(&http.Cookie{Name: auth.SecureDeviceCookie, Value: dev.Value})
	if w := e.serve(r); w.Code != http.StatusOK {
		t.Fatalf("login with the device cookie while the username is delayed: %d %s", w.Code, w.Body)
	}
}
