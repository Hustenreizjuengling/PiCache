package upstream

import (
	"context"
	"crypto/tls"
	"errors"
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
// idle connections. One query at a time per connection (no pipelining);
// queries are padded (padQuery). A query is sent once more, over a new
// connection, when a pooled connection turns out closed or the server cut
// the TLS handshake of a new one (at most once per query).
type dotTransport struct {
	host string     // TLS server name (hostname or IP literal)
	ip   netip.Addr // valid if the upstream is an IP literal or a stamp with an address
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

func newDoT(spec settings.UpstreamSpec, boot *bootstrap, enc encOpts) *dotTransport {
	t := &dotTransport{host: spec.Host, port: uint16(spec.Port), boot: boot}
	switch {
	case spec.DialAddr.IsValid():
		t.ip, t.port = spec.DialAddr.Addr(), spec.DialAddr.Port()
	case spec.IsIPLit:
		t.ip, _ = netip.ParseAddr(spec.Host)
	}
	t.conf = clientTLS(spec.Host, tls.VersionTLS12, enc, spec.Pins)
	t.conf.NextProtos = []string{"dot"}
	t.conf.ClientSessionCache = tls.NewLRUClientSessionCache(dotMaxIdle)
	return t
}

func (t *dotTransport) exchange(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, error) {
	wire = padQuery(q, wire)
	for try := 0; ; try++ {
		c, reused, err := t.get(ctx)
		if err != nil {
			if try > 0 || ctx.Err() != nil || !errors.As(err, new(cutHandshakeError)) {
				return nil, err
			}
			continue // the server cut the TLS handshake: dial once more
		}
		m, reusable, err := streamRoundTrip(ctx, c, q.Id, wire)
		if err == nil {
			if reusable {
				t.put(c)
			} else {
				_ = c.Close()
			}
			dropPadding(m)
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
	c, err := dialAddrs(ctx, addrs, t.port, func(ctx context.Context, address string) (net.Conn, error) {
		c, err := d.DialContext(ctx, "tcp", address)
		if err != nil && cutHandshake(err) { // connecting never ends with EOF or a reset
			err = cutHandshakeError{err}
		}
		return c, err
	})
	return c, ctxErr(ctx, err)
}

// addrs returns the IP literal or the bootstrap-resolved addresses of host.
func (t *dotTransport) addrs(ctx context.Context) ([]netip.Addr, error) {
	if t.ip.IsValid() {
		return []netip.Addr{t.ip}, nil
	}
	return t.boot.lookup(ctx, t.host)
}
