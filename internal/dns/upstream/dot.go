package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	dotMaxIdle     = 4                // idle connections kept per upstream
	dotIdleTimeout = 20 * time.Second // servers close idle DoT connections after ~10-30 s
)

// dotTransport is DNS over TLS (RFC 7858, ALPN "dot") with a small pool of
// idle connections. One query at a time per connection (no pipelining).
type dotTransport struct {
	host string     // TLS server name (hostname or IP literal)
	ip   netip.Addr // valid if the upstream is an IP literal
	port uint16
	boot *bootstrap
	conf *tls.Config

	mu     sync.Mutex
	idle   []idleConn
	closed bool
}

type idleConn struct {
	c     net.Conn
	since time.Time
}

func newDoT(spec settings.UpstreamSpec, boot *bootstrap, roots *x509.CertPool) *dotTransport {
	t := &dotTransport{host: spec.Host, port: uint16(spec.Port), boot: boot}
	if spec.IsIPLit {
		t.ip, _ = netip.ParseAddr(spec.Host)
	}
	t.conf = &tls.Config{
		ServerName:         spec.Host,
		NextProtos:         []string{"dot"},
		MinVersion:         tls.VersionTLS12,
		RootCAs:            roots,
		ClientSessionCache: tls.NewLRUClientSessionCache(dotMaxIdle),
	}
	return t
}

func (t *dotTransport) exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	for try := 0; ; try++ {
		c, reused, err := t.get(ctx)
		if err != nil {
			return nil, err
		}
		m, reusable, err := streamRoundTrip(ctx, c, q.Id, wire)
		if err == nil {
			if reusable {
				t.put(c)
			} else {
				_ = c.Close()
			}
			return m, nil
		}
		_ = c.Close()
		if !reused || try > 0 || ctx.Err() != nil {
			return nil, err
		}
		// The server closed the idle connection; the others are likely
		// stale too. Retry once over a fresh connection.
		t.dropIdle()
	}
}

// get returns an idle connection (reused=true) or dials a new one.
func (t *dotTransport) get(ctx context.Context) (c net.Conn, reused bool, err error) {
	t.mu.Lock()
	for len(t.idle) > 0 {
		ic := t.idle[len(t.idle)-1]
		t.idle = t.idle[:len(t.idle)-1]
		if time.Since(ic.since) < dotIdleTimeout {
			t.mu.Unlock()
			return ic.c, true, nil
		}
		_ = ic.c.Close()
	}
	t.mu.Unlock()
	c, err = t.dial(ctx)
	return c, false, err
}

func (t *dotTransport) put(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || len(t.idle) >= dotMaxIdle {
		_ = c.Close()
		return
	}
	t.idle = append(t.idle, idleConn{c: c, since: time.Now()})
}

func (t *dotTransport) dropIdle() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ic := range t.idle {
		_ = ic.c.Close()
	}
	t.idle = nil
}

func (t *dotTransport) close() {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.dropIdle()
}

func (t *dotTransport) dial(ctx context.Context) (net.Conn, error) {
	addrs, err := t.addrs(ctx)
	if err != nil {
		return nil, err
	}
	d := tls.Dialer{NetDialer: &net.Dialer{}, Config: t.conf}
	lastErr := errNoAddrs
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", netip.AddrPortFrom(a, t.port).String())
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, ctxErr(ctx, lastErr)
}

// addrs returns the IP literal or the bootstrap-resolved addresses of host.
func (t *dotTransport) addrs(ctx context.Context) ([]netip.Addr, error) {
	if t.ip.IsValid() {
		return []netip.Addr{t.ip}, nil
	}
	return t.boot.lookup(ctx, t.host)
}
