package api

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
)

// setupHints tell a new user where to find the setup token.
var setupHints = []string{
	"The setup token is printed in the PiCache log at startup (e.g. journalctl -u picache | grep -i 'setup token').",
	"On the PiCache host run: sudo picache setup-token",
	"In Docker run: docker exec -u 65532:65532 <container> /picache setup-token",
}

// registerAuthRoutes registers the auth endpoints (docs/API.md).
func (s *Server) registerAuthRoutes() {
	s.route("GET /api/v1/auth/status", permPublic, s.authStatus)
	s.route("POST /api/v1/auth/setup", permPublic, s.authSetup)
	s.route("POST /api/v1/auth/login", permPublic, s.authLogin)
	s.route("POST /api/v1/auth/logout", permRead, s.authLogout)
	s.route("GET /api/v1/auth/me", permRead, s.authMe)
	s.route("POST /api/v1/auth/password", permSession, s.authPassword)
	s.route("GET /api/v1/auth/sessions", permSession, s.authSessions)
	s.route("DELETE /api/v1/auth/sessions/{id}", permSession, s.authRevokeSession)
	s.route("POST /api/v1/auth/totp/begin", permSession, s.authTOTPBegin)
	s.route("POST /api/v1/auth/totp/confirm", permSession, s.authTOTPConfirm)
	s.route("POST /api/v1/auth/totp/disable", permSession, s.authTOTPDisable)
	s.route("GET /api/v1/tokens", permSession, s.tokensList)
	s.route("POST /api/v1/tokens", permSession, s.tokensCreate)
	s.route("DELETE /api/v1/tokens/{id}", permSession, s.tokensDelete)
}

func authReqMeta(r *http.Request) auth.ReqMeta {
	return auth.ReqMeta{IP: clientIP(r), UserAgent: r.UserAgent(), Devices: auth.DeviceCookieValues(r)}
}

type authStatusResponse struct {
	SetupRequired bool       `json:"setupRequired"`
	Authenticated bool       `json:"authenticated"`
	User          *auth.User `json:"user,omitempty"`
	Scope         auth.Scope `json:"scope,omitempty"`
	TokenAuth     bool       `json:"tokenAuth"`
	Language      string     `json:"language"`
	SetupHints    []string   `json:"setupHints,omitempty"`
	HTTPSPort     int        `json:"httpsPort"` // port of the bound HTTPS listener, 0 if none
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	required, err := s.d.Auth.SetupRequired(ctx)
	if err != nil {
		return err
	}
	out := authStatusResponse{SetupRequired: required, Language: s.d.Settings.Get().Web.Language, HTTPSPort: s.httpsPort()}
	if required {
		out.SetupHints = setupHints
	}
	p, err := s.d.Auth.Authenticate(r)
	var u auth.User
	if err == nil {
		u, err = s.d.Auth.Me(ctx, p)
	}
	switch {
	case err == nil:
		out.Authenticated, out.User, out.Scope, out.TokenAuth = true, &u, p.Scope, p.TokenID != 0
	case apperr.KindOf(err) != apperr.KindUnauthorized:
		s.log.Warn("auth status: authenticate", slog.Any("err", err))
	}
	return ok(w, out)
}

// httpsPort returns the port of the bound HTTPS listener (0 if none), so the
// sign-in and setup pages can point to it when they are opened over HTTP.
func (s *Server) httpsPort() int {
	if s.d.Runtime == nil {
		return 0
	}
	for _, addr := range s.d.Runtime.Listeners().Bound["web-tls"] {
		if _, p, err := net.SplitHostPort(addr); err == nil {
			if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 {
				return n
			}
		}
	}
	return 0
}

// cookieSecurity reports whether the request arrived over TLS (the session
// cookie is then named __Host-picache_session) and whether the cookie gets
// the Secure flag (TLS, or PICACHE_WEB_SECURE_COOKIES behind a TLS proxy).
func (s *Server) cookieSecurity(r *http.Request) (tls, secure bool) {
	tls = r.TLS != nil
	return tls, tls || (s.d.Config != nil && s.d.Config.WebSecureCookies)
}

// clearSessionCookies deletes the session cookie(s) in the browser.
func (s *Server) clearSessionCookies(w http.ResponseWriter, r *http.Request) {
	for _, c := range s.d.Auth.ClearCookies(s.cookieSecurity(r)) {
		http.SetCookie(w, c)
	}
}

// startSession sets the session cookie and the device cookie (the browser
// is then throttled by its device, not by the username delay, at its next
// sign-in) and responds with the user.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, sess *auth.Session) error {
	u, err := s.d.Auth.Me(r.Context(), &auth.Principal{UserID: sess.UserID})
	if err != nil {
		return err
	}
	tls, secure := s.cookieSecurity(r)
	http.SetCookie(w, s.d.Auth.Cookie(sess, tls, secure))
	if c := s.d.Auth.DeviceCookieFor(sess, tls, secure); c != nil {
		http.SetCookie(w, c)
	}
	return ok(w, u)
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		SetupToken string `json:"setupToken"`
		Username   string `json:"username"`
		Password   string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	sess, err := s.d.Auth.Setup(r.Context(), in.SetupToken, in.Username, in.Password, authReqMeta(r))
	if err != nil {
		return err
	}
	return s.startSession(w, r, sess)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	sess, err := s.d.Auth.Login(r.Context(), in.Username, in.Password, in.TOTP, authReqMeta(r))
	if err != nil {
		return err
	}
	return s.startSession(w, r, sess)
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) error {
	p := principal(r)
	if p.TokenID == 0 {
		err := s.d.Auth.RevokeSession(r.Context(), p, p.SessionID)
		if err != nil && apperr.KindOf(err) != apperr.KindNotFound {
			return err
		}
		s.audit(r, "auth.logout", p.Username, nil)
	}
	s.clearSessionCookies(w, r)
	return noContent(w)
}

func (s *Server) authMe(w http.ResponseWriter, r *http.Request) error {
	u, err := s.d.Auth.Me(r.Context(), principal(r))
	if err != nil {
		return err
	}
	return ok(w, u)
}

func (s *Server) authPassword(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
		KeepTokens      bool   `json:"keepTokens,omitempty"` // default: the API tokens are revoked too
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p := principal(r)
	if err := s.d.Auth.ChangePassword(r.Context(), p, in.CurrentPassword, in.NewPassword, in.KeepTokens); err != nil {
		return err
	}
	s.audit(r, "auth.password.change", p.Username, map[string]bool{"keepTokens": in.KeepTokens})
	return noContent(w)
}

func (s *Server) authSessions(w http.ResponseWriter, r *http.Request) error {
	list, err := s.d.Auth.Sessions(r.Context(), principal(r))
	if err != nil {
		return err
	}
	return ok(w, list)
}

func (s *Server) authRevokeSession(w http.ResponseWriter, r *http.Request) error {
	p, id := principal(r), r.PathValue("id")
	if err := s.d.Auth.RevokeSession(r.Context(), p, id); err != nil {
		return err
	}
	s.audit(r, "auth.session.revoke", id, nil)
	if id == p.SessionID {
		s.clearSessionCookies(w, r)
	}
	return noContent(w)
}

func (s *Server) authTOTPBegin(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		CurrentPassword string `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p := principal(r)
	secret, uri, err := s.d.Auth.TOTPBegin(r.Context(), p, in.CurrentPassword)
	if err != nil {
		return err
	}
	s.audit(r, "auth.totp.begin", p.Username, nil)
	return ok(w, map[string]string{"secret": secret, "uri": uri})
}

func (s *Server) authTOTPConfirm(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Code string `json:"code"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p := principal(r)
	if err := s.d.Auth.TOTPConfirm(r.Context(), p, in.Code); err != nil {
		return err
	}
	s.audit(r, "auth.totp.enable", p.Username, nil)
	return noContent(w)
}

func (s *Server) authTOTPDisable(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Password string `json:"password"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p := principal(r)
	if err := s.d.Auth.TOTPDisable(r.Context(), p, in.Password); err != nil {
		return err
	}
	s.audit(r, "auth.totp.disable", p.Username, nil)
	return noContent(w)
}

func (s *Server) tokensList(w http.ResponseWriter, r *http.Request) error {
	list, err := s.d.Auth.Tokens(r.Context())
	if err != nil {
		return err
	}
	return ok(w, list)
}

func (s *Server) tokensCreate(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name            string     `json:"name"`
		Scope           auth.Scope `json:"scope"`
		ExpiresInDays   int        `json:"expiresInDays,omitempty"`
		CurrentPassword string     `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.ExpiresInDays < 0 || in.ExpiresInDays > 3650 {
		return apperr.Invalid("expiresInDays", "must be between 0 (never) and 3650")
	}
	secret, info, err := s.d.Auth.CreateToken(r.Context(), principal(r), in.CurrentPassword, in.Name, in.Scope,
		time.Duration(in.ExpiresInDays)*24*time.Hour)
	if err != nil {
		return err
	}
	s.audit(r, "token.create", info.Name, info)
	return created(w, struct {
		Token string         `json:"token"`
		Info  auth.TokenInfo `json:"info"`
	}{secret, info})
}

func (s *Server) tokensDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.Auth.DeleteToken(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "token.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}
