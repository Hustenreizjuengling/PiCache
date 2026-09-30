package netutil

import (
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
)

// fakeConn is one end of a pipe that reports a chosen peer address.
type fakeConn struct {
	net.Conn
	peer   net.Addr
	closed atomic.Bool
}

func (c *fakeConn) RemoteAddr() net.Addr { return c.peer }
func (c *fakeConn) Close() error         { c.closed.Store(true); return c.Conn.Close() }

// fakeListener hands out the connections sent to it.
type fakeListener struct{ ch chan net.Conn }

func (l *fakeListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l *fakeListener) Close() error   { return nil }
func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

// SEC-1: at the total, a DNS listener closes the oldest connection of the
// client key holding the most connections for a newcomer that would hold
// fewer; a newcomer that would hold as many is refused; loopback may use a
// reserve beyond the total. One host with many source addresses could
// otherwise keep every other client, this machine included, off DNS over
// TCP. The plain LimitListener still refuses at the total.
func TestLimitDNSListenerEvictsHeaviest(t *testing.T) {
	fl := &fakeListener{ch: make(chan net.Conn, 8)}
	ll := LimitDNSListener(fl, func() *ACL { return nil }, 2, 4)
	dial := func(ip string) *fakeConn {
		t.Helper()
		a, b := net.Pipe()
		t.Cleanup(func() { a.Close(); b.Close() })
		c := &fakeConn{Conn: a, peer: net.TCPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(ip), 40000))}
		fl.ch <- c
		return c
	}
	// Fill the total with two keys of two connections each.
	var attacker []*fakeConn
	for _, ip := range []string{"10.0.0.1", "10.0.0.1", "10.0.0.2", "10.0.0.2"} {
		c := dial(ip)
		if _, err := ll.Accept(); err != nil {
			t.Fatal(err)
		}
		attacker = append(attacker, c)
	}
	// A newcomer: the oldest connection of the heaviest key goes.
	c := dial("10.0.0.9")
	if _, err := ll.Accept(); err != nil {
		t.Fatal(err)
	}
	if !attacker[0].closed.Load() || c.closed.Load() {
		t.Fatal("the newcomer did not replace the oldest connection of the heaviest key")
	}
	c = dial("10.0.0.10")
	if _, err := ll.Accept(); err != nil {
		t.Fatal(err)
	}
	if !attacker[2].closed.Load() || attacker[1].closed.Load() || attacker[3].closed.Load() {
		t.Fatal("the second newcomer did not replace the oldest connection of the then heaviest key")
	}
	// Every key holds one now: a newcomer would hold as many and is refused
	// (closed), then loopback may use its reserve.
	refused := dial("10.0.0.11")
	lo := dial("127.0.0.1")
	if _, err := ll.Accept(); err != nil {
		t.Fatal(err)
	}
	if !refused.closed.Load() || lo.closed.Load() {
		t.Fatalf("refused %v, loopback closed %v", refused.closed.Load(), lo.closed.Load())
	}

	// The plain listener refuses at the total.
	fl2 := &fakeListener{ch: make(chan net.Conn, 1)}
	pl := LimitListener(fl2, func() *ACL { return nil }, 2, 1)
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	fl2.ch <- &fakeConn{Conn: a, peer: net.TCPAddrFromAddrPort(netip.MustParseAddrPort("10.0.0.1:1"))}
	first, err := pl.Accept()
	if err != nil {
		t.Fatal(err)
	}
	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	second := &fakeConn{Conn: a2, peer: net.TCPAddrFromAddrPort(netip.MustParseAddrPort("10.0.0.2:1"))}
	fl2.ch <- second
	close(fl2.ch)
	if _, err := pl.Accept(); err == nil || !second.closed.Load() || first.(*limitConn).Conn.(*fakeConn).closed.Load() {
		t.Fatalf("plain listener at the total: %v", err)
	}
}
