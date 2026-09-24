package api

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// middleware wraps h: recover → security headers → host allowlist →
// HTTPS redirect → cross-origin protection → handler.
func (s *Server) middleware(h http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, s.log, apperr.Forbidden("cross-origin request rejected"))
	}))
	h = cop.Handler(h)
	h = s.httpsRedirect(h)
	h = s.hosts.middleware(h, s.log)
	h = s.securityHeaders(h)
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

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), interest-cohort=()")
		if r.TLS != nil && s.d.Settings.Get().Web.RedirectToHTTPS {
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
			http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusTemporaryRedirect)
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

	mu       sync.Mutex
	lastWarn map[string]time.Time // rate-limits the rejection log (bounded)
	rejected atomic.Uint64
}

func newHostAllowlist(cfg *config.Config, set *settings.Store) *hostAllowlist {
	h := &hostAllowlist{cfg: cfg}
	h.rebuild(set.Get())
	set.Subscribe(func(_, n *settings.All) { h.rebuild(n) })
	return h
}

func (h *hostAllowlist) rebuild(s *settings.All) {
	names := []string{"localhost"}
	domains := append([]string{s.DNS.LocalDomain}, netutil.ResolvConfSearch()...)
	add := func(n string) {
		n = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(n), "."))
		if n == "" || slices.Contains(names, n) {
			return
		}
		names = append(names, n)
		if !strings.Contains(n, ".") {
			for _, d := range domains {
				if d != "" && !slices.Contains(names, n+"."+d) {
					names = append(names, n+"."+d)
				}
			}
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
			h.rejected.Add(1)
			if h.shouldWarn(r.Host) {
				log.Warn("rejected request with unknown Host header (add it under Settings → Web → Allowed hosts or PICACHE_WEB_HOSTS)",
					slog.String("host", r.Host), slog.String("client", clientIP(r)))
			}
			if strings.Contains(r.Header.Get("Accept"), "text/html") {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusMisdirectedRequest)
				_, _ = io.WriteString(w, "PiCache does not know the host name \""+sanitizeHost(r.Host)+"\".\n\n"+
					"Open PiCache by its IP address and add this name under Settings > Web > Allowed hosts,\n"+
					"or set PICACHE_WEB_HOSTS. This protects against DNS rebinding attacks.\n")
				return
			}
			var body errorBody
			body.Error.Code = "misdirected"
			body.Error.Message = "unknown host name; allow it in the web settings or via PICACHE_WEB_HOSTS"
			_ = writeJSON(w, http.StatusMisdirectedRequest, body)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// shouldWarn rate-limits the rejection log to once per host per 10 minutes
// (at most 256 tracked hosts).
func (h *hostAllowlist) shouldWarn(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastWarn == nil {
		h.lastWarn = map[string]time.Time{}
	}
	now := time.Now()
	if t, ok := h.lastWarn[host]; ok && now.Sub(t) < 10*time.Minute {
		return false
	}
	if len(h.lastWarn) >= 256 {
		for k, t := range h.lastWarn {
			if now.Sub(t) >= 10*time.Minute {
				delete(h.lastWarn, k)
			}
		}
		if len(h.lastWarn) >= 256 {
			return false
		}
	}
	h.lastWarn[host] = now
	return true
}

// Rejected returns the number of requests rejected by the host allowlist.
func (h *hostAllowlist) Rejected() uint64 { return h.rejected.Load() }

// sanitizeHost makes a Host header safe to echo in a plain-text response.
func sanitizeHost(s string) string {
	out := make([]byte, 0, 100)
	for i := 0; i < len(s) && len(out) < 100; i++ {
		c := s[i]
		if c >= 0x20 && c < 0x7f && c != '"' && c != '\\' {
			out = append(out, c)
		}
	}
	return string(out)
}
