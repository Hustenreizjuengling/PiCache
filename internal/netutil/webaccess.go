package netutil

import (
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// WebACL decides which addresses may use the web UI and API
// (docs/ARCHITECTURE.md 6.1). This machine (loopback and its own
// addresses) is always allowed. While web.restrictToNetworks is on, so are
// the private ranges (PrivateLANPrefixes), the networks this machine is
// connected to (ConnectedSubnets, public ones included), dns.allowedNetworks,
// web.allowedNetworks and web.trustedProxies; with it off everyone is.
// dns.allowAllNetworks and dns.trustConnectedNetworks do not change it. It
// is immutable.
type WebACL struct {
	restrict bool
	own      []netip.Addr   // this machine's addresses (canonical)
	allowed  []netip.Prefix // while restrict
	proxies  []netip.Prefix // web.trustedProxies
}

// NewWebACL builds the web ACL of s with the current interface addresses.
// The API also builds one from candidate settings to judge a change before
// it is saved.
func NewWebACL(s *settings.All) *WebACL {
	return newWebACL(s, LocalAddrs(), ConnectedSubnets())
}

func newWebACL(s *settings.All, own []netip.Addr, connected []netip.Prefix) *WebACL {
	a := &WebACL{restrict: s.Web.RestrictToNetworks, proxies: settings.WebPrefixes(s.Web.TrustedProxies)}
	for _, ip := range own {
		if ip = Canon(ip); ip.IsValid() && !slices.Contains(a.own, ip) {
			a.own = append(a.own, ip)
		}
	}
	if a.restrict {
		a.allowed = slices.Concat(PrivateLANPrefixes, connected, settings.ParsePrefixes(s.DNS.AllowedNetworks),
			settings.WebPrefixes(s.Web.AllowedNetworks), a.proxies)
	}
	return a
}

// Allowed reports whether ip may use the web UI and API.
func (a *WebACL) Allowed(ip netip.Addr) bool {
	if a == nil {
		return false
	}
	if !a.restrict {
		return true
	}
	ip = Canon(ip)
	if !ip.IsValid() {
		return false
	}
	return ip.IsLoopback() || slices.Contains(a.own, ip) || inAny(ip, a.allowed)
}

// TrustedProxy reports whether ip is in web.trustedProxies: its
// X-Forwarded-For and X-Forwarded-Proto headers are read.
func (a *WebACL) TrustedProxy(ip netip.Addr) bool {
	return a != nil && inAny(ip, a.proxies)
}

// Restricted reports whether web.restrictToNetworks is on.
func (a *WebACL) Restricted() bool { return a != nil && a.restrict }

// WebAccess keeps the WebACL current (rebuilt on every settings change and
// by Refresh, which the app calls every 60 s and on SIGHUP, so renumbered
// interfaces are followed within a minute) and counts and logs refused
// connections and requests of both enforcement stages: Listener (at
// accept) and the API middleware (every request).
type WebAccess struct {
	cur     atomic.Pointer[WebACL]
	set     *settings.Store
	log     *slog.Logger
	build   func(*settings.All) *WebACL // NewWebACL (tests replace it)
	refused atomic.Uint64

	// rebuildMu serialises the rebuilds of Refresh and the settings
	// subscriber; each builds from the latest snapshot under it, so a slow
	// Refresh that started before a settings change cannot store an ACL of
	// the previous settings after the subscriber stored the new one.
	rebuildMu sync.Mutex

	mu       sync.Mutex
	lastWarn map[netip.Addr]time.Time // rate-limits the refusal log (bounded)
}

// Refusal log limits: one line per address per webWarnEvery, at most
// webWarnTracked addresses remembered (beyond that: counted, not logged).
const (
	webWarnEvery   = 10 * time.Minute
	webWarnTracked = 256
)

// NewWebAccess creates the holder and subscribes to settings changes.
func NewWebAccess(set *settings.Store, log *slog.Logger) *WebAccess {
	return newWebAccess(set, log, NewWebACL)
}

func newWebAccess(set *settings.Store, log *slog.Logger, build func(*settings.All) *WebACL) *WebAccess {
	w := &WebAccess{set: set, log: log.With(slog.String("component", "web-access")), build: build}
	w.cur.Store(build(set.Get()))
	// The store holds the new snapshot before it calls its listeners.
	set.Subscribe(func(_, _ *settings.All) { w.Refresh() })
	return w
}

// Get returns the current ACL.
func (w *WebAccess) Get() *WebACL { return w.cur.Load() }

// Refresh rebuilds the ACL from the current settings and interfaces.
func (w *WebAccess) Refresh() {
	w.rebuildMu.Lock()
	defer w.rebuildMu.Unlock()
	w.cur.Store(w.build(w.set.Get()))
}

// Refused returns the number of connections and requests refused since
// the start.
func (w *WebAccess) Refused() uint64 { return w.refused.Load() }

// Refuse counts a refused connection (peer invalid: stage 1, at accept)
// or request (stage 2; client is the effective client, peer the TCP peer)
// and logs it at most once per address per 10 minutes.
func (w *WebAccess) Refuse(client, peer netip.Addr) {
	w.refused.Add(1)
	if !w.shouldWarn(client) {
		return
	}
	attrs := []any{slog.String("client", client.String())}
	if peer.IsValid() {
		attrs = append(attrs, slog.String("peer", peer.String()))
	}
	w.log.Warn("refused web UI access (not in the allowed networks; see Users & security > Web access or run `picache web-access --reset`)", attrs...)
}

func (w *WebAccess) shouldWarn(ip netip.Addr) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lastWarn == nil {
		w.lastWarn = map[netip.Addr]time.Time{}
	}
	now := time.Now()
	if t, ok := w.lastWarn[ip]; ok && now.Sub(t) < webWarnEvery {
		return false
	}
	if _, ok := w.lastWarn[ip]; !ok && len(w.lastWarn) >= webWarnTracked {
		for k, t := range w.lastWarn {
			if now.Sub(t) >= webWarnEvery {
				delete(w.lastWarn, k)
			}
		}
		if len(w.lastWarn) >= webWarnTracked {
			return false
		}
	}
	w.lastWarn[ip] = now
	return true
}

// Listener wraps a web listener (stage 1): a connection whose peer the
// current ACL does not allow is closed right after accept, before TLS and
// HTTP. No connection cap is added (a cap per address would throttle
// everyone behind a reverse proxy); the HTTP server timeouts bound them.
func (w *WebAccess) Listener(ln net.Listener) net.Listener { return &webListener{Listener: ln, w: w} }

type webListener struct {
	net.Listener
	w *WebAccess
}

func (l *webListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := AddrFromNet(c.RemoteAddr())
		if l.w.Get().Allowed(ip) {
			return c, nil
		}
		l.w.Refuse(ip, netip.Addr{})
		_ = c.Close()
	}
}
