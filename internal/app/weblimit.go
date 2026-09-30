package app

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// webCaps are the connection caps of the web listeners (docs/ARCHITECTURE.md
// 2 "Web access"), shared by every HTTP and HTTPS web listener.
type webCaps struct {
	// perClient caps a client key (netutil.ClientKey of the TCP peer).
	perClient int
	// perNetwork caps the IPv6 addresses of one /64 whose client keys are
	// /128 (on-link, ULA and link-local addresses) together, so a host
	// cannot multiply perClient with addresses it gives itself.
	perNetwork int
	// total caps all connections (loopback may exceed it by loopback).
	total int
	// loopback is the reserve only loopback may use beyond total: this
	// machine's health checks (the Docker HEALTHCHECK, the health wait of
	// an update) and the CLI still connect when the network filled total.
	loopback int
}

// webLimits are the caps of the web listeners (tests lower them). A browser
// keeps at most 6 HTTP/1.1 connections per host and port (its event streams
// included) or one HTTP/2 connection with up to 32 streams, so perClient
// leaves room for several browsers, tabs and scripts behind one address (a
// NAT, a Docker bridge). Loopback and web.trustedProxies have no
// per-client cap: a reverse proxy carries the connections of every client.
// At about 45 KiB per idle HTTP/2 connection, total and loopback bound them
// to about 48 MiB.
var webLimits = webCaps{perClient: 64, perNetwork: 256, total: 1024, loopback: 64}

// webLimitWarnEvery rate-limits the warning about refused connections.
const webLimitWarnEvery = time.Minute

// webLimiter caps the concurrent connections of the web listeners (webCaps).
// A connection over a cap is closed right after accept (before TLS); a
// warning is logged at most once per webLimitWarnEvery.
type webLimiter struct {
	exempt func(netip.Addr) bool // no per-client cap (web.trustedProxies)
	caps   webCaps
	log    *slog.Logger

	mu     sync.Mutex
	active int
	per    map[netip.Prefix]int // per client key
	nets   map[netip.Prefix]int // per /64 of /128 client keys
	warned time.Time            // the last warning
}

func newWebLimiter(exempt func(netip.Addr) bool, caps webCaps, log *slog.Logger) *webLimiter {
	return &webLimiter{exempt: exempt, caps: caps, log: log, per: map[netip.Prefix]int{}, nets: map[netip.Prefix]int{}}
}

// Listener wraps a web listener (behind the web ACL's listener, so refused
// addresses never take a slot).
func (l *webLimiter) Listener(ln net.Listener) net.Listener {
	return &webLimitListener{Listener: ln, l: l}
}

// webSlot is what an admitted connection counts for besides the total: its
// client key and the /64 of a /128 key (invalid: not counted).
type webSlot struct {
	key, net netip.Prefix
}

// acquire takes a slot for a connection from ip; ok is false when a cap is
// reached. Loopback may use the reserve beyond the total; loopback and
// trusted proxies count for no client key.
func (l *webLimiter) acquire(ip netip.Addr) (s webSlot, ok bool) {
	limit := l.caps.total
	switch {
	case ip.IsLoopback():
		limit += l.caps.loopback
	case l.exempt == nil || !l.exempt(ip):
		s.key = netutil.ClientKey(ip)
		if s.key.Bits() == 128 {
			s.net = netip.PrefixFrom(s.key.Addr(), 64).Masked()
		}
	}
	l.mu.Lock()
	if l.active >= limit || s.key.IsValid() && l.per[s.key] >= l.caps.perClient ||
		s.net.IsValid() && l.nets[s.net] >= l.caps.perNetwork {
		now := time.Now()
		warn := now.Sub(l.warned) >= webLimitWarnEvery
		if warn {
			l.warned = now
		}
		l.mu.Unlock()
		if warn {
			l.log.Warn("web connection limit reached: new connections are closed", slog.String("client", ip.String()),
				slog.Int("per_client", l.caps.perClient), slog.Int("per_network", l.caps.perNetwork), slog.Int("total", l.caps.total))
		}
		return webSlot{}, false
	}
	l.active++
	if s.key.IsValid() {
		l.per[s.key]++
	}
	if s.net.IsValid() {
		l.nets[s.net]++
	}
	l.mu.Unlock()
	return s, true
}

// release frees the slot of a connection acquire admitted.
func (l *webLimiter) release(s webSlot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	decrement(l.per, s.key)
	decrement(l.nets, s.net)
}

// decrement counts one connection of k less in m (an invalid k: none).
func decrement(m map[netip.Prefix]int, k netip.Prefix) {
	if !k.IsValid() {
		return
	}
	if n := m[k] - 1; n > 0 {
		m[k] = n
	} else {
		delete(m, k)
	}
}

type webLimitListener struct {
	net.Listener
	l *webLimiter
}

func (ll *webLimitListener) Accept() (net.Conn, error) {
	for {
		c, err := ll.Listener.Accept()
		if err != nil {
			return nil, err
		}
		s, ok := ll.l.acquire(netutil.AddrFromNet(c.RemoteAddr()))
		if !ok {
			_ = c.Close()
			continue
		}
		return &webLimitConn{Conn: c, l: ll.l, slot: s}, nil
	}
}

type webLimitConn struct {
	net.Conn
	l    *webLimiter
	slot webSlot
	once sync.Once
}

func (c *webLimitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.release(c.slot) })
	return err
}

// ReadFrom delegates to the underlying connection so net/http can use
// sendfile(2) for the files of the web UI on the plain HTTP listener.
func (c *webLimitConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(c.Conn, r)
}
