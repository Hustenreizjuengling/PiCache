package auth

import (
	"net/http"
	"time"
)

// Session cookie names. A session started over TLS uses the __Host- prefix:
// browsers accept such a cookie only with Secure, Path=/ and no Domain from
// a secure origin, so a plain-HTTP origin on the same host name (e.g. :8080)
// cannot plant or overwrite it. Authenticate accepts both names.
const (
	SessionCookie       = "picache_session"
	SecureSessionCookie = "__Host-picache_session"
)

// SessionCookieName returns the name of the session cookie for a request
// that arrived over TLS (tls) or plain HTTP.
func SessionCookieName(tls bool) string {
	if tls {
		return SecureSessionCookie
	}
	return SessionCookie
}

// Cookie builds the session cookie. tls: the request arrived over TLS (the
// cookie gets the __Host- name); secure: set the Secure flag (always with
// tls, and for plain-HTTP requests behind a TLS-terminating reverse proxy
// when PICACHE_WEB_SECURE_COOKIES is set).
func (a *Service) Cookie(s *Session, tls, secure bool) *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName(tls),
		Value:    s.Token,
		Path:     "/",
		Expires:  s.ExpiresAt,
		MaxAge:   max(1, int(s.ExpiresAt.Sub(a.now()).Seconds())),
		HttpOnly: true,
		Secure:   secure || tls,
		SameSite: http.SameSiteStrictMode,
	}
}

// ClearCookies builds the cookies that delete the session cookie: the plain
// name always and, over TLS, the __Host- name too (a browser that used both
// schemes may hold both).
func (a *Service) ClearCookies(tls, secure bool) []*http.Cookie {
	names := []string{SessionCookie}
	if tls {
		names = append(names, SecureSessionCookie)
	}
	out := make([]*http.Cookie, 0, len(names))
	for _, n := range names {
		out = append(out, &http.Cookie{
			Name:     n,
			Value:    "",
			Path:     "/",
			Expires:  time.Unix(0, 0),
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   secure || tls,
			SameSite: http.SameSiteStrictMode,
		})
	}
	return out
}

// maxSessionCookies bounds how many session cookie values of one request are
// looked up.
const maxSessionCookies = 4

// sessionCookies returns the session cookie values of a request, the
// __Host- one first.
func sessionCookies(r *http.Request) []string {
	var out []string
	for _, n := range []string{SecureSessionCookie, SessionCookie} {
		for _, c := range r.CookiesNamed(n) {
			if c.Value != "" && len(out) < maxSessionCookies {
				out = append(out, c.Value)
			}
		}
	}
	return out
}
