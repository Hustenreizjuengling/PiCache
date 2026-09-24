package api

import (
	"net/http"
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

func TestAuthRoutesSecureCookieOverTLS(t *testing.T) {
	e := newCoreEnv(t)
	if err := e.auth.Provision(t.Context(), "admin", corePassword); err != nil {
		t.Fatal(err)
	}
	r := coreRequest("POST", "https://"+coreHost+"/api/v1/auth/login", `{"username":"admin","password":"`+corePassword+`"}`)
	w := e.serve(r)
	if w.Code != http.StatusOK || !coreSessionCookie(t, w).Secure {
		t.Fatalf("login over TLS: %d, cookie %+v", w.Code, coreSessionCookie(t, w))
	}
}

func TestAuthRoutesAccountSecurity(t *testing.T) {
	e := newCoreEnv(t)
	session := e.provisionAndLogin(t)

	w := e.do("POST", "/api/v1/auth/password", `{"currentPassword":"wrong password","newPassword":"a new password"}`, session)
	coreWantError(t, w, http.StatusBadRequest, "invalid", "currentPassword")
	w = e.do("POST", "/api/v1/auth/password", `{"currentPassword":"`+corePassword+`","newPassword":"a new password"}`, session)
	if w.Code != http.StatusNoContent {
		t.Fatalf("change password: %d %s", w.Code, w.Body)
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
	coreDecode(t, e.do("POST", "/api/v1/auth/totp/begin", "", session), &begin)
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

	coreWantError(t, e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"root"}`, session), http.StatusBadRequest, "invalid", "scope")
	coreWantError(t, e.do("POST", "/api/v1/tokens", `{"name":"x","scope":"read","expiresInDays":-1}`, session),
		http.StatusBadRequest, "invalid", "expiresInDays")

	w := e.do("POST", "/api/v1/tokens", `{"name":"grafana","scope":"admin","expiresInDays":30}`, session)
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
