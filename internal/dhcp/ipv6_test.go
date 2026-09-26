package dhcp

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/dhcp/dhcpv6"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// groupOp is a multicast join or leave.
type groupOp struct {
	join    bool
	ifIndex int
	group   netip.Addr
}

// fakeICMP records router advertisements and group changes.
type fakeICMP struct {
	mu     sync.Mutex
	out    [][]byte
	dst    []netip.Addr
	groups []groupOp
	closed bool
}

func (f *fakeICMP) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeICMP) ReadFrom([]byte) (int, int, int, netip.Addr, error) {
	return 0, 0, 0, netip.Addr{}, net.ErrClosed
}

func (f *fakeICMP) WriteTo(b []byte, ifIndex int, dst netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ifIndex != testIfIndex {
		panic("advertisement out of another interface")
	}
	f.out = append(f.out, slices.Clone(b))
	f.dst = append(f.dst, dst)
	return nil
}

func (f *fakeICMP) JoinGroup(i int, g netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, groupOp{true, i, g})
	return nil
}

func (f *fakeICMP) LeaveGroup(i int, g netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, groupOp{false, i, g})
	return nil
}

func (f *fakeICMP) SetReadDeadline(time.Time) error { return nil }
func (f *fakeICMP) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeICMP) take() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.out
	f.out, f.dst = nil, nil
	return out
}

// fakeConn6 records DHCPv6 group changes and what is sent; onWrite may
// answer.
type fakeConn6 struct {
	mu      sync.Mutex
	groups  []groupOp
	out     [][]byte
	dst     []netip.AddrPort
	closed  bool
	onWrite func(b []byte)
}

func (f *fakeConn6) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeConn6) ReadFrom([]byte) (int, int, netip.AddrPort, netip.Addr, error) {
	return 0, 0, netip.AddrPort{}, netip.Addr{}, net.ErrClosed
}
func (f *fakeConn6) WriteTo(b []byte, _ int, dst netip.AddrPort) error {
	f.mu.Lock()
	f.out = append(f.out, slices.Clone(b))
	f.dst = append(f.dst, dst)
	hook := f.onWrite
	f.mu.Unlock()
	if hook != nil {
		hook(slices.Clone(b))
	}
	return nil
}
func (f *fakeConn6) JoinGroup(i int, g netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, groupOp{true, i, g})
	return nil
}
func (f *fakeConn6) LeaveGroup(i int, g netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = append(f.groups, groupOp{false, i, g})
	return nil
}
func (f *fakeConn6) SetReadDeadline(time.Time) error { return nil }
func (f *fakeConn6) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func withIPv6(a *settings.All) {
	enabledDHCP(a)
	a.DHCP.IPv6.RouterAdvertisements = true
	a.DHCP.IPv6.DHCPv6 = true
}

func newTestSvc6(t *testing.T, configure func(*settings.All)) (*testSvc, *fakeICMP, *fakeConn6) {
	t.Helper()
	ts := newTestSvc(t, configure)
	icmp, c6 := &fakeICMP{}, &fakeConn6{}
	ts.Service.d.Sockets = &Sockets{bindCapable: true, rawCapable: true, v4: ts.conn, v6: c6, icmp: icmp}
	ts.evaluate(context.Background(), true)
	return ts, icmp, c6
}

// lifetimes returns the RDNSS and DNSSL lifetimes of an advertisement.
func lifetimes(b []byte) (rdnss, dnssl uint32) {
	for o := b[16:]; len(o) >= 8; o = o[int(o[1])*8:] {
		switch o[0] {
		case 25:
			rdnss = binary.BigEndian.Uint32(o[4:8])
		case 31:
			dnssl = binary.BigEndian.Uint32(o[4:8])
		}
		if o[1] == 0 {
			break
		}
	}
	return rdnss, dnssl
}

// Router advertisements: sent when due (the ULA, O flag with DHCPv6, router
// lifetime 0), ff02::2 joined; a solicitation is answered; switching them
// off withdraws the announcement and leaves the group.
func TestRouterAdvertisements(t *testing.T) {
	ts, icmp, c6 := newTestSvc6(t, withIPv6)
	st := ts.Status().IPv6
	if st.RouterAdvertisements.State != StateSending || st.RouterAdvertisements.Address != "fd00::10" || st.DHCPv6.State != StateServing {
		t.Fatalf("ipv6 status %+v", st)
	}
	if !slices.Contains(icmp.groups, groupOp{true, testIfIndex, allRouters}) || !slices.Contains(c6.groups, groupOp{true, testIfIndex, allDHCPAgents}) {
		t.Fatalf("groups %v %v", icmp.groups, c6.groups)
	}
	wait := ts.raTick()
	out := icmp.take()
	if len(out) != 1 || wait != 16*time.Second {
		t.Fatalf("%d advertisements, next in %v", len(out), wait)
	}
	ra := out[0]
	if ra[0] != 134 || ra[5] != 0x40 || ra[6] != 0 || ra[7] != 0 || !bytes.Contains(ra, netip.MustParseAddr("fd00::10").AsSlice()) {
		t.Fatalf("advertisement % x", ra)
	}
	if r, d := lifetimes(ra); r != 1800 || d != 1800 {
		t.Fatalf("lifetimes %d %d", r, d)
	}
	// A valid solicitation on the interface: answered within 500 ms.
	ts.ra.sched.Rand = func(n int64) int64 { return n - 1 }
	ts.clk.Add(10 * time.Second)
	rs := []byte{133, 0, 0, 0, 0, 0, 0, 0}
	ts.solicitation(rs, 3, 255, netip.MustParseAddr("fe80::1"))          // another interface
	ts.solicitation(rs, testIfIndex, 64, netip.MustParseAddr("fe80::1")) // wrong hop limit
	// PiCache's own (the search for other routers), reflected by the network.
	ts.solicitation(rs, testIfIndex, 255, netip.MustParseAddr("fe80::10"))
	if ts.raTick(); len(icmp.take()) != 0 || ts.Status().IPv6.RouterAdvertisements.Solicitations != 0 || len(ts.Log(MaxLog)) != 0 {
		t.Fatal("answered an invalid solicitation")
	}
	ts.solicitation(rs, testIfIndex, 255, netip.MustParseAddr("fe80::1"))
	ts.clk.Add(500 * time.Millisecond)
	if ts.raTick(); len(icmp.take()) != 1 || ts.Status().IPv6.RouterAdvertisements.Solicitations != 1 {
		t.Fatal("solicitation not answered")
	}
	// Switched off: one withdrawal, the group is left.
	ts.set.Update(context.Background(), func(a *settings.All) error { a.DHCP.IPv6.RouterAdvertisements = false; return nil })
	ts.evaluate(context.Background(), true)
	out = icmp.take()
	if len(out) != 1 {
		t.Fatalf("%d withdrawals", len(out))
	}
	if r, d := lifetimes(out[0]); r != 0 || d != 0 {
		t.Fatalf("withdrawal lifetimes %d %d", r, d)
	}
	if !slices.Contains(icmp.groups, groupOp{false, testIfIndex, allRouters}) {
		t.Fatalf("group not left: %v", icmp.groups)
	}
	if ts.raTick(); len(icmp.take()) != 0 || ts.Status().IPv6.RouterAdvertisements.State != StateOff {
		t.Fatal("advertisements after switching them off")
	}
}

// IPv4 blockers do not stop the announcements; a missing ULA or socket
// does, with the reason.
func TestIPv6Gates(t *testing.T) {
	ts, _, _ := newTestSvc6(t, withIPv6)
	ctx := context.Background()
	ts.env = testEnvFor(true) // dynamic IPv4: DHCPv4 blocked
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateBlocked || st.IPv6.RouterAdvertisements.State != StateSending || st.IPv6.DHCPv6.State != StateServing {
		t.Fatalf("dynamic IPv4: %+v %+v", st.State, st.IPv6)
	}
	env := testEnvFor(false)
	all := env.addrs()
	env.addrs = func() []netutil.HostAddr {
		return slices.DeleteFunc(slices.Clone(all), func(a netutil.HostAddr) bool { return netutil.IsULA(a.Prefix.Addr()) })
	}
	ts.env = env
	ts.evaluate(ctx, true)
	st := ts.Status().IPv6
	if st.RouterAdvertisements.State != StateBlocked || !slices.Equal(st.RouterAdvertisements.Blockers, []string{BlockerNoULA}) ||
		st.DHCPv6.State != StateBlocked || !slices.Equal(st.DHCPv6.Blockers, []string{BlockerNoULA}) {
		t.Fatalf("no ULA: %+v", st)
	}
	ts.Service.d.Sockets = &Sockets{bindCapable: true, v4: ts.conn, v6Err: "UDP port 547 is in use"}
	ts.env = testEnvFor(false)
	ts.evaluate(ctx, true)
	st = ts.Status().IPv6
	if st.RouterAdvertisements.Available || st.RouterAdvertisements.Reason != noCapNetRaw ||
		st.RouterAdvertisements.ReasonCode != RAReasonNoCapNetRaw ||
		!slices.Equal(st.RouterAdvertisements.Blockers, []string{BlockerNoRawSocket}) ||
		!slices.Equal(st.DHCPv6.Blockers, []string{BlockerNoSocket}) || st.DHCPv6.Error != "UDP port 547 is in use" {
		t.Fatalf("no sockets: %+v", st)
	}
	if h, msg, _, _ := ts.Health(); h != "warn" || msg == "" {
		t.Fatalf("health %s %q", h, msg)
	}
}

// DHCPv6: information requests from a link-local client on the interface
// get the REPLY; other sources, interfaces and message types do not.
func TestHandle6(t *testing.T) {
	ts, _, _ := newTestSvc6(t, withIPv6)
	req := []byte{dhcpv6.MsgInformationRequest, 1, 2, 3, 0, 1, 0, 10, 0, 3, 0, 1, 2, 0, 0, 0, 0, 1}
	ll := netip.AddrPortFrom(netip.MustParseAddr("fe80::99"), 546)
	// The socket reports link-local sources with their zone; the exchange
	// log shows the address alone (as for router solicitations).
	reply := ts.handle6(req, testIfIndex, netip.AddrPortFrom(ll.Addr().WithZone("eth0"), 546), allDHCPAgents)
	if len(reply) < 4 || reply[0] != dhcpv6.MsgReply || !bytes.Equal(reply[1:4], []byte{1, 2, 3}) ||
		!bytes.Contains(reply, netip.MustParseAddr("fd00::10").AsSlice()) || !bytes.Contains(reply, []byte{3, 'l', 'a', 'n', 0}) ||
		!bytes.Contains(reply, dhcpv6.DUIDLL([6]byte(testIfMAC))) {
		t.Fatalf("reply % x", reply)
	}
	if l := ts.Log(1); len(l) != 1 || l[0].Kind != LogDHCPv6 || l[0].Address != "fe80::99" {
		t.Fatalf("log %+v", l)
	}
	for name, tc := range map[string]struct {
		b   []byte
		ifi int
		src netip.AddrPort
		dst netip.Addr
	}{
		"global source":  {req, testIfIndex, netip.AddrPortFrom(netip.MustParseAddr("2001:db8::9"), 546), allDHCPAgents},
		"other iface":    {req, 3, ll, allDHCPAgents},
		"other dst":      {req, testIfIndex, ll, netip.MustParseAddr("ff02::1")},
		"solicit":        {append([]byte{1}, req[1:]...), testIfIndex, ll, allDHCPAgents},
		"malformed":      {req[:6], testIfIndex, ll, allDHCPAgents},
		"source port 0":  {req, testIfIndex, netip.AddrPortFrom(ll.Addr(), 0), allDHCPAgents},
		"link-local dst": {req, testIfIndex, ll, netip.MustParseAddr("fe80::10")}, // answered
	} {
		got := ts.handle6(tc.b, tc.ifi, tc.src, tc.dst)
		if (got != nil) != (name == "link-local dst") {
			t.Errorf("%s: reply %v", name, got != nil)
		}
	}
	if st := ts.Status().IPv6.DHCPv6; st.Ignored != 2 {
		t.Fatalf("ignored %d", st.Ignored)
	}
}
