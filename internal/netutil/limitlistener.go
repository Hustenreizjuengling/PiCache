package netutil

import (
	"io"
	"net"
	"net/netip"
	"slices"
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

// DNSLoopbackReserve is how many connections loopback (this machine's
// resolver and tools, the health check) may open beyond the total of a
// DNS listener (LimitDNSListener).
const DNSLoopbackReserve = 64

// LimitDNSListener is LimitListener for DNS over TCP and DoT/DoH: at the
// total, a newcomer is not refused while another client key holds more
// connections than the newcomer would: the oldest connection of the client
// key holding the most is closed instead. So one host with many source
// addresses (each key up to perClient) cannot keep every other client,
// which then could not fetch answers truncated over UDP, off the listener
// for as long as it trickles queries. Loopback may use DNSLoopbackReserve
// connections beyond the total and is never closed for a newcomer.
func LimitDNSListener(ln net.Listener, acl func() *ACL, perClient, total int) net.Listener {
	return &limitListener{Listener: ln, acl: acl, perClient: perClient, total: int64(total), per: map[netip.Prefix]int{},
		evict: true, conns: map[netip.Prefix][]*limitConn{}}
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
	// evict (LimitDNSListener): the connections of every client key,
	// oldest first, to close the oldest of the heaviest key at the total.
	evict bool
	conns map[netip.Prefix][]*limitConn
	seq   uint64 // admission order (limitConn.seq)
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
		ok, victim := l.admit(ip, key)
		if !ok {
			l.mu.Unlock()
			l.Refused.Add(1)
			_ = c.Close()
			continue
		}
		l.per[key]++
		l.active.Add(1)
		l.seq++
		lc := &limitConn{Conn: c, l: l, key: key, seq: l.seq}
		if l.evict {
			l.conns[key] = append(l.conns[key], lc)
		}
		l.mu.Unlock()
		if victim != nil {
			l.Refused.Add(1)
			_ = victim.Close() // releases its slot
		}
		return lc, nil
	}
}

// admit decides about a connection from ip (its client key key) with l.mu
// held: whether it is admitted, and the connection to close for it
// (LimitDNSListener at the total).
func (l *limitListener) admit(ip netip.Addr, key netip.Prefix) (bool, *limitConn) {
	switch {
	case l.per[key] >= l.perClient:
		return false, nil
	case l.active.Load() < l.total:
		return true, nil
	case !l.evict:
		return false, nil
	case ip.IsLoopback():
		return l.active.Load() < l.total+DNSLoopbackReserve, nil
	}
	v := l.heaviest(l.per[key] + 1)
	return v != nil, v
}

// heaviest returns the oldest connection of the client key (not loopback)
// holding the most connections (of several, the one with the oldest
// connection), when that is more than n (what the newcomer's key would
// hold); nil otherwise. Called with l.mu held.
func (l *limitListener) heaviest(n int) *limitConn {
	var best *limitConn
	most := n
	for k, c := range l.per {
		list := l.conns[k]
		if len(list) == 0 || k.Addr().IsLoopback() || c < most || c == most && (best == nil || list[0].seq > best.seq) {
			continue
		}
		best, most = list[0], c
	}
	return best
}

func (l *limitListener) release(c *limitConn) {
	key := c.key
	l.mu.Lock()
	if n := l.per[key] - 1; n > 0 {
		l.per[key] = n
	} else {
		delete(l.per, key)
	}
	if l.evict {
		list := l.conns[key]
		if i := slices.Index(list, c); i >= 0 {
			list = slices.Delete(list, i, i+1)
		}
		if len(list) > 0 {
			l.conns[key] = list
		} else {
			delete(l.conns, key)
		}
	}
	l.active.Add(-1)
	l.mu.Unlock()
}

type limitConn struct {
	net.Conn
	l    *limitListener
	key  netip.Prefix
	seq  uint64
	once sync.Once
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { c.l.release(c) })
	return err
}

// ReadFrom delegates to the underlying connection so net/http can use
// sendfile(2)/splice(2) for responses written through this listener.
func (c *limitConn) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.Conn.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(c.Conn, r)
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
