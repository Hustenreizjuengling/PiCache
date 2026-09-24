package netutil

import (
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
)

// LimitListener wraps a TCP listener: connections from clients the ACL does
// not allow are closed right at accept, and concurrent connections are
// capped per client key (/32, /64) and in total. A slot is released when the
// connection is closed.
func LimitListener(ln net.Listener, acl func() *ACL, perClient, total int) net.Listener {
	return &limitListener{Listener: ln, acl: acl, perClient: perClient, total: int64(total), per: map[netip.Prefix]int{}}
}

type limitListener struct {
	net.Listener
	acl       func() *ACL
	perClient int
	total     int64
	active    atomic.Int64
	mu        sync.Mutex
	per       map[netip.Prefix]int
	Refused   atomic.Uint64
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip := AddrFromNet(c.RemoteAddr())
		if acl := l.acl(); acl != nil && !acl.Allowed(ip) {
			l.Refused.Add(1)
			_ = c.Close()
			continue
		}
		key := ClientKey(ip)
		l.mu.Lock()
		if l.active.Load() >= l.total || l.per[key] >= l.perClient {
			l.mu.Unlock()
			l.Refused.Add(1)
			_ = c.Close()
			continue
		}
		l.per[key]++
		l.active.Add(1)
		l.mu.Unlock()
		return &limitConn{Conn: c, l: l, key: key}, nil
	}
}

func (l *limitListener) release(key netip.Prefix) {
	l.mu.Lock()
	if n := l.per[key] - 1; n > 0 {
		l.per[key] = n
	} else {
		delete(l.per, key)
	}
	l.active.Add(-1)
	l.mu.Unlock()
}

type limitConn struct {
	net.Conn
	l    *limitListener
	key  netip.Prefix
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.release(c.key) })
	return err
}

// TCPConn returns the underlying *net.TCPConn (for splice-friendly copies),
// or nil if it is not a TCP connection.
func (c *limitConn) TCPConn() *net.TCPConn {
	tc, _ := c.Conn.(*net.TCPConn)
	return tc
}

// UnwrapTCP returns the *net.TCPConn behind c (possibly wrapped by
// LimitListener), or nil.
func UnwrapTCP(c net.Conn) *net.TCPConn {
	switch v := c.(type) {
	case *net.TCPConn:
		return v
	case interface{ TCPConn() *net.TCPConn }:
		return v.TCPConn()
	}
	return nil
}
