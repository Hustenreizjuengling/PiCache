package api

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// middleware wraps h: recover → security headers → host allowlist →
// HTTPS redirect → cross-origin protection → handler.
func (s *Server) middleware(h http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	h = cop.Handler(h)
	h = s.httpsRedirect(h)
	h = s.hosts.middleware(h, s.log)
	h = securityHeaders(h)
	h = s.recoverer(h)
	return h
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in handler", slog.String("path", r.URL.Path), slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), interest-cohort=()")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// httpsRedirect redirects plain-HTTP requests to the HTTPS listener when
// settings.Web.RedirectToHTTPS is on (never /healthz).
func (s *Server) httpsRedirect(next http.Handler) http.Handler {
	port := ""
	if len(s.d.Config.WebTLSListen) > 0 {
		if _, p, err := net.SplitHostPort(s.d.Config.WebTLSListen[0]); err == nil {
			port = p
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil && port != "" && r.URL.Path != "/healthz" && s.d.Settings.Get().Web.RedirectToHTTPS {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			target := "https://" + host
			if port != "443" {
				target += ":" + port
			}
			http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowlist defends against DNS rebinding: the Host header must be an IP
// literal, localhost, this machine's hostname, a configured server name or
// an explicitly allowed host.
type hostAllowlist struct {
	cfg     *config.Config
	allowed atomic.Pointer[[]string]
}

func newHostAllowlist(cfg *config.Config, set *settings.Store) *hostAllowlist {
	h := &hostAllowlist{cfg: cfg}
	h.rebuild(set.Get())
	set.Subscribe(func(_, n *settings.All) { h.rebuild(n) })
	return h
}

func (h *hostAllowlist) rebuild(s *settings.All) {
	names := []string{"localhost"}
	add := func(n string) {
		n = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(n), "."))
		if n == "" || slices.Contains(names, n) {
			return
		}
		names = append(names, n)
		if s.DNS.LocalDomain != "" && !strings.Contains(n, ".") {
			names = append(names, n+"."+s.DNS.LocalDomain)
		}
	}
	if hn, err := os.Hostname(); err == nil {
		add(hn)
	}
	for _, n := range s.DNS.ServerNames {
		add(n)
	}
	for _, n := range s.Web.AllowedHosts {
		add(n)
	}
	for _, n := range h.cfg.WebHosts {
		add(n)
	}
	h.allowed.Store(&names)
}

func (h *hostAllowlist) ok(hostHeader string) bool {
	host := hostHeader
	if hh, _, err := net.SplitHostPort(host); err == nil {
		host = hh
	}
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), "."))
	if _, err := netip.ParseAddr(host); err == nil {
		return true // rebinding cannot produce IP-literal Host headers
	}
	return slices.Contains(*h.allowed.Load(), host)
}

func (h *hostAllowlist) middleware(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && !h.ok(r.Host) {
			log.Warn("rejected request with unknown Host header (add it under Settings → Web → Allowed hosts or PICACHE_WEB_HOSTS)",
				slog.String("host", r.Host), slog.String("client", clientIP(r)))
			var body errorBody
			body.Error.Code = "misdirected"
			body.Error.Message = "unknown host name; allow it in the web settings or via PICACHE_WEB_HOSTS"
			_ = writeJSON(w, http.StatusMisdirectedRequest, body)
			return
		}
		next.ServeHTTP(w, r)
	})
}
