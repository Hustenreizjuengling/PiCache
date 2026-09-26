package dhcp

import (
	"context"
	"encoding/binary"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/dhcp/dhcpv6"
	"github.com/hustenreizjuengling/picache/internal/dhcp/ra"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// raWithDNS is a parsed advertisement announcing one DNS server.
func raWithDNS(addr string, lifetime int) ra.Received {
	return ra.Received{RouterLifetime: 1800 * time.Second,
		DNS: []ra.RDNSS{{Addr: netip.MustParseAddr(addr), Lifetime: time.Duration(lifetime) * time.Second}}}
}

// advert encodes a router advertisement: flags, router lifetime and one
// RDNSS option (lifetime, addresses), with a source link-layer option.
func advert(flags byte, routerLife uint16, rdnssLife uint32, mac [6]byte, dns ...string) []byte {
	b := make([]byte, 16)
	b[0], b[5] = 134, flags
	binary.BigEndian.PutUint16(b[6:8], routerLife)
	b = append(b, 1, 1)
	b = append(b, mac[:]...)
	if len(dns) > 0 {
		b = append(b, 25, byte(1+2*len(dns)), 0, 0)
		b = binary.BigEndian.AppendUint32(b, rdnssLife)
		for _, d := range dns {
			a := netip.MustParseAddr(d).As16()
			b = append(b, a[:]...)
		}
	}
	return b
}

var routerMAC = [6]byte{0x3c, 0xa6, 0x2f, 0, 0, 1}

// Other routers' advertisements: only valid ones on the interface count;
// a record is created by DNS or M/O, removed by an RA with neither, its DNS
// entries go with lifetime 0, it expires after max(router lifetime, RDNSS
// lifetime) (at most 1 h), and it warns only once seen twice with a DNS
// server that is not PiCache's own.
func TestOtherRouterAdvertisements(t *testing.T) {
	ts, _, _ := newTestSvc6(t, withIPv6)
	src := netip.MustParseAddr("fe80::1")
	rs := func(b []byte, ifIndex, hops int, from netip.Addr) { ts.advertisement(b, ifIndex, hops, from) }
	status := func() []Announcer { return ts.Status().IPv6.OtherAnnouncers }

	// Invalid: another interface, hop limit, global source, PiCache's own
	// address or MAC.
	good := advert(0, 1800, 1800, routerMAC, "fd00::1")
	rs(good, 3, 255, src)
	rs(good, testIfIndex, 64, src)
	rs(good, testIfIndex, 255, netip.MustParseAddr("2001:db8::1"))
	rs(good, testIfIndex, 255, netip.MustParseAddr("fe80::10")) // this machine
	rs(advert(0, 1800, 1800, [6]byte(testIfMAC), "fd00::1"), testIfIndex, 255, netip.MustParseAddr("fe80::2"))
	if len(status()) != 0 {
		t.Fatalf("invalid RAs recorded: %+v", status())
	}
	rs(good, testIfIndex, 255, src)
	a := status()
	if len(a) != 1 || a[0].Kind != AnnouncerRA || a[0].Address != "fe80::1" || !slices.Equal(a[0].DNS, []string{"fd00::1"}) ||
		a[0].Conflict || *a[0].RouterLifetime != 1800 || *a[0].Managed || a[0].Interface != "eth0" {
		t.Fatalf("first RA %+v", a)
	}
	if h, _, _, _ := ts.Health(); h != "ok" {
		t.Fatalf("health after one RA: %s", h)
	}
	// Seen twice: a conflict and a health warning.
	ts.clk.Add(time.Second)
	rs(good, testIfIndex, 255, src)
	if a := status(); !a[0].Conflict {
		t.Fatalf("second RA %+v", a)
	}
	if h, msg, hint, _ := ts.Health(); h != "warn" || !containsAll(msg, "fd00::1", "another router") || hint == "" {
		t.Fatalf("health %s %q", h, msg)
	}
	// The same router announcing PiCache's own ULA never warns.
	own := netip.MustParseAddr("fe80::5")
	for range 3 {
		rs(advert(0, 1800, 1800, routerMAC, "fd00::10"), testIfIndex, 255, own)
	}
	for _, x := range status() {
		if x.Address == "fe80::5" && (x.Conflict || !slices.Equal(x.OwnDNS, []string{"fd00::10"})) {
			t.Fatalf("PiCache's ULA warns: %+v", x)
		}
		if x.Address == "fe80::1" && (x.OwnDNS == nil || len(x.OwnDNS) != 0) {
			t.Fatalf("own DNS of a foreign record: %+v", x)
		}
	}
	// Next to a foreign address, PiCache's own is marked as such (the UI
	// names only the foreign one as the conflict).
	for range 2 {
		rs(advert(0, 1800, 1800, routerMAC, "fd00::10", "fd00::7"), testIfIndex, 255, netip.MustParseAddr("fe80::8"))
	}
	if i := slices.IndexFunc(status(), func(x Announcer) bool { return x.Address == "fe80::8" }); i < 0 {
		t.Fatal("mixed record missing")
	}
	for _, x := range status() {
		if x.Address == "fe80::8" && (!x.Conflict || !slices.Equal(x.DNS, []string{"fd00::10", "fd00::7"}) ||
			!slices.Equal(x.OwnDNS, []string{"fd00::10"})) {
			t.Fatalf("mixed record %+v", x)
		}
	}
	// M/O flags alone: a record, never a warning.
	for range 3 {
		rs(advert(0x80|0x40, 1800, 0, routerMAC), testIfIndex, 255, netip.MustParseAddr("fe80::6"))
	}
	for _, x := range status() {
		if x.Address == "fe80::6" && (x.Conflict || !*x.Managed || !*x.Other) {
			t.Fatalf("M/O %+v", x)
		}
	}
	// RDNSS lifetime 0 removes the address; an RA with neither DNS nor M/O
	// removes the record.
	rs(advert(0, 1800, 0, routerMAC, "fd00::1"), testIfIndex, 255, src)
	for _, x := range status() {
		if x.Address == "fe80::1" {
			t.Fatalf("record kept after an RA announcing nothing: %+v", x)
		}
	}
	rs(advert(0x40, 1800, 1800, routerMAC, "fd00::1", "fd00::2"), testIfIndex, 255, src)
	rs(advert(0x40, 1800, 0, routerMAC, "fd00::1"), testIfIndex, 255, src)
	for _, x := range status() {
		if x.Address == "fe80::1" && !slices.Equal(x.DNS, []string{"fd00::2"}) {
			t.Fatalf("lifetime 0 kept the address: %+v", x)
		}
	}
	// Expiry: max(router lifetime, RDNSS lifetime), at most 1 h.
	rs(advert(0, 0, 7200, routerMAC, "fd00::7"), testIfIndex, 255, netip.MustParseAddr("fe80::7"))
	ts.clk.Add(time.Hour + time.Second)
	if len(status()) != 0 {
		t.Fatalf("not expired: %+v", status())
	}
	// Not while PiCache does not announce: no conflict.
	rs(good, testIfIndex, 255, src)
	rs(good, testIfIndex, 255, src)
	ts.update(t, func(a *settings.All) { a.DHCP.IPv6.RouterAdvertisements, a.DHCP.IPv6.DHCPv6 = false, false })
	if a := status(); len(a) != 1 || a[0].Conflict {
		t.Fatalf("conflict while PiCache does not announce: %+v", a)
	}
}

// At most 50 advertisements per second are parsed; at most 32 records are
// kept, the least recently seen go first.
func TestOtherRouterLimits(t *testing.T) {
	ts, _, _ := newTestSvc6(t, withIPv6)
	for i := range 60 {
		src := netip.AddrFrom16([16]byte{0xfe, 0x80, 15: byte(i + 1)})
		ts.advertisement(advert(0, 1800, 1800, routerMAC, "fd00::1"), testIfIndex, 255, src)
	}
	if n := len(ts.Status().IPv6.OtherAnnouncers); n != 32 {
		t.Fatalf("records %d", n)
	}
	ts.clk.Add(time.Second)
	for i := range 60 {
		src := netip.AddrFrom16([16]byte{0xfe, 0x80, 14: 1, 15: byte(i)})
		ts.advertisement(advert(0, 1800, 1800, routerMAC, "fd00::1"), testIfIndex, 255, src)
	}
	a := ts.Status().IPv6.OtherAnnouncers
	if len(a) != 32 || !a[0].LastSeen.Equal(ts.now().UTC()) {
		t.Fatalf("eviction %d %+v", len(a), a[0])
	}
}

// The search: a router solicitation to ff02::2 (raw socket) and a
// Relay-Forward with an Information-Request to ff02::1:2 from UDP 547.
// Only Relay-Replies with one Reply of the search's transaction and its
// peer address count; PiCache's own DUID and addresses are ignored; a
// server seen in two searches in a row with a foreign DNS server warns.
func TestSearchOtherDHCPv6Servers(t *testing.T) {
	old := searchListen
	searchListen = 5 * time.Millisecond
	t.Cleanup(func() { searchListen = old })
	ts, icmp, c6 := newTestSvc6(t, withIPv6)
	server := netip.MustParseAddrPort("[fe80::1%eth0]:547")
	var fwd []byte
	c6.onWrite = func(b []byte) {
		if b[0] != dhcpv6.MsgRelayForward {
			return
		}
		fwd = b
		txid := [3]byte(b[34+4+1 : 34+4+4]) // RELAY_MSG header, then the inner message
		peer := netip.AddrFrom16([16]byte(b[18:34]))
		reply := func(sid []byte, dns string, tx [3]byte, p netip.Addr) []byte {
			return relayReplyPkt(tx, p, sid, dns)
		}
		ts.relayReply(reply([]byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 6}, "fd00::1", txid, peer), testIfIndex, server)
		ts.relayReply(reply([]byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 7}, "fd00::2", [3]byte{9, 9, 9}, peer), testIfIndex, server) // other tx
		ts.relayReply(reply([]byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 8}, "fd00::3", txid, netip.MustParseAddr("fe80::99")), testIfIndex, server)
		ts.relayReply(reply(dhcpv6.DUIDLL([6]byte(testIfMAC)), "fd00::10", txid, peer), testIfIndex, server) // own DUID
		ts.relayReply(reply([]byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 9}, "fd00::4", txid, peer), testIfIndex,
			netip.MustParseAddrPort("[fe80::10]:547")) // own address
		ts.relayReply(reply([]byte{0, 3, 0, 1, 1, 2, 3, 4, 5, 10}, "fd00::5", txid, peer), 3, server) // other interface
	}
	ts.search(context.Background())
	if len(fwd) == 0 {
		t.Fatal("no Relay-Forward sent")
	}
	if c6.dst[len(c6.dst)-1] != netip.AddrPortFrom(allDHCPAgents, 547) {
		t.Fatalf("sent to %v", c6.dst)
	}
	if fwd[1] != 0 || netip.AddrFrom16([16]byte(fwd[2:18])) != netip.MustParseAddr("fd00::10") ||
		netip.AddrFrom16([16]byte(fwd[18:34])) != netip.MustParseAddr("fe80::10") {
		t.Fatalf("relay header % x", fwd[:34])
	}
	out := icmp.take()
	if len(out) == 0 || out[len(out)-1][0] != ra.TypeRouterSolicitation {
		t.Fatal("no router solicitation")
	}
	st := ts.Status().IPv6
	if st.LastSearch == nil || !st.LastSearch.RA || !st.LastSearch.DHCPv6 {
		t.Fatalf("last search %+v", st.LastSearch)
	}
	if len(st.OtherAnnouncers) != 1 || st.OtherAnnouncers[0].Kind != AnnouncerDHCPv6 || st.OtherAnnouncers[0].ServerID != "00:03:00:01:01:02:03:04:05:06" ||
		!slices.Equal(st.OtherAnnouncers[0].DNS, []string{"fd00::1"}) || st.OtherAnnouncers[0].Conflict ||
		st.OtherAnnouncers[0].Managed != nil {
		t.Fatalf("announcers %+v", st.OtherAnnouncers)
	}
	if st.DHCPv6.Ignored < 5 {
		t.Fatalf("ignored %d", st.DHCPv6.Ignored)
	}
	// The next search replaces the answers; a server seen twice warns.
	ts.clk.Add(10 * time.Minute)
	ts.search(context.Background())
	if a := ts.Status().IPv6.OtherAnnouncers; len(a) != 1 || !a[0].Conflict {
		t.Fatalf("second search %+v", a)
	}
	// Expires 10 minutes after the search.
	ts.clk.Add(10*time.Minute + time.Second)
	if a := ts.Status().IPv6.OtherAnnouncers; len(a) != 0 {
		t.Fatalf("not expired %+v", a)
	}
	// Without the raw socket only the DHCPv6 part runs.
	ts.Service.d.Sockets = &Sockets{bindCapable: true, rawCapable: true, v4: ts.conn, v6: c6}
	ts.search(context.Background())
	if ls := ts.Status().IPv6.LastSearch; ls.RA || !ls.DHCPv6 {
		t.Fatalf("last search without the raw socket %+v", ls)
	}
}

// relayReplyPkt builds a Relay-Reply with a Reply inside.
func relayReplyPkt(txid [3]byte, peer netip.Addr, serverID []byte, dns string) []byte {
	inner := []byte{dhcpv6.MsgReply, txid[0], txid[1], txid[2]}
	inner = appendOpt6(inner, 2, serverID)
	a := netip.MustParseAddr(dns).As16()
	inner = appendOpt6(inner, 23, a[:])
	b := []byte{dhcpv6.MsgRelayReply, 0}
	l := netip.MustParseAddr("fd00::10").As16()
	p := peer.As16()
	b = append(b, l[:]...)
	b = append(b, p[:]...)
	return appendOpt6(b, 9, inner)
}

func appendOpt6(b []byte, code uint16, v []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, code)
	b = binary.BigEndian.AppendUint16(b, uint16(len(v)))
	return append(b, v...)
}

// The default router's RDNSS for the network check: only routers with a
// router lifetime, only while the raw socket is open.
func TestRouterRDNSS(t *testing.T) {
	ts, icmp, _ := newTestSvc6(t, withIPv6)
	ts.advertisement(advert(0, 1800, 1800, routerMAC, "fd00::1"), testIfIndex, 255, netip.MustParseAddr("fe80::1"))
	ts.advertisement(advert(0, 0, 1800, routerMAC, "fd00::2"), testIfIndex, 255, netip.MustParseAddr("fe80::2"))
	got, ok := ts.RouterRDNSS()
	if !ok || !slices.Equal(got[netip.MustParseAddr("fe80::1")], []string{"fd00::1"}) || got[netip.MustParseAddr("fe80::2")] != nil {
		t.Fatalf("rdnss %v %v", got, ok)
	}
	ts.d2().SetDropUnverified("x")
	if _, ok := ts.RouterRDNSS(); ok {
		t.Fatal("reported without the raw socket")
	}
	_ = icmp
}

// The search runs when the announcements start (kicked by the evaluation).
func TestSearchKickedOnStart(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	icmp, c6 := &fakeICMP{}, &fakeConn6{}
	ts.Service.d.Sockets = &Sockets{bindCapable: true, rawCapable: true, v4: ts.conn, v6: c6, icmp: icmp}
	select {
	case <-ts.annKick:
		t.Fatal("kicked before announcing")
	default:
	}
	ts.update(t, func(a *settings.All) { a.DHCP.IPv6.DHCPv6 = true })
	select {
	case <-ts.annKick:
	default:
		t.Fatal("no search when the announcements started")
	}
}
