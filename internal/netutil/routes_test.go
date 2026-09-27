package netutil

import (
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
)

// nlRoute encodes one RTM_NEWROUTE message (host byte order).
func nlRoute(family byte, dst netip.Prefix, rtype byte, table uint32, oif int, gw netip.Addr, metric uint32) []byte {
	attr := func(t uint16, v []byte) []byte {
		b := make([]byte, (4+len(v)+3)&^3)
		binary.NativeEndian.PutUint16(b[0:2], uint16(4+len(v)))
		binary.NativeEndian.PutUint16(b[2:4], t)
		copy(b[4:], v)
		return b
	}
	u32 := func(v uint32) []byte { b := make([]byte, 4); binary.NativeEndian.PutUint32(b, v); return b }
	body := make([]byte, rtMsgLen)
	body[0], body[1], body[4], body[7] = family, byte(dst.Bits()), byte(min(table, 255)), rtype
	if dst.Bits() > 0 {
		body = append(body, attr(rtaDst, dst.Addr().AsSlice())...)
	}
	if oif > 0 {
		body = append(body, attr(rtaOIF, u32(uint32(oif)))...)
	}
	if gw.IsValid() {
		body = append(body, attr(rtaGateway, gw.AsSlice())...)
	}
	body = append(body, attr(rtaPriority, u32(metric))...)
	body = append(body, attr(rtaTable, u32(table))...)
	msg := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(body))
	binary.NativeEndian.PutUint32(msg[0:4], uint32(nlmsgHdrLen+len(body)))
	binary.NativeEndian.PutUint16(msg[4:6], rtmNewRoute)
	return append(msg, body...)
}

func TestParseRouteDump(t *testing.T) {
	p, a := netip.MustParsePrefix, netip.MustParseAddr
	names := map[int]string{1: "lo", 2: "eth0", 3: "wg0", 4: "br-guest"}
	var dump []byte
	for _, m := range [][]byte{
		nlRoute(2, p("0.0.0.0/0"), rtnUnicast, 254, 2, a("192.168.1.1"), 100),
		nlRoute(2, p("192.168.1.0/24"), rtnUnicast, 254, 2, netip.Addr{}, 100),
		nlRoute(2, p("10.9.0.0/24"), rtnUnicast, 254, 3, netip.Addr{}, 0),
		nlRoute(2, p("192.168.50.0/24"), rtnUnicast, 254, 4, netip.Addr{}, 0),
		nlRoute(2, p("127.0.0.0/8"), 2, 255, 1, netip.Addr{}, 0),            // local table, type local
		nlRoute(2, p("192.168.1.255/32"), 3, 255, 2, netip.Addr{}, 0),       // broadcast
		nlRoute(2, p("10.99.0.0/16"), 6, 254, 0, netip.Addr{}, 0),           // blackhole: no interface
		nlRoute(2, p("10.98.0.0/16"), 7, 254, 2, netip.Addr{}, 0),           // unreachable
		nlRoute(2, p("172.16.0.0/12"), rtnUnicast, 100, 2, netip.Addr{}, 0), // another table
		nlRoute(10, p("::/0"), rtnUnicast, 254, 2, a("fe80::1"), 1024),
		nlRoute(10, p("2001:db8:1::/64"), rtnUnicast, 254, 2, netip.Addr{}, 256),
		nlRoute(10, p("fe80::/64"), rtnUnicast, 254, 2, netip.Addr{}, 256),
		nlRoute(10, p("ff00::/8"), 5, 255, 2, netip.Addr{}, 256),          // multicast
		nlRoute(2, p("10.7.0.0/16"), rtnUnicast, 254, 9, netip.Addr{}, 0), // unknown interface
	} {
		dump = append(dump, m...)
	}
	routes := ParseRouteDump(dump, names)
	got := map[string]string{}
	for _, r := range routes {
		got[r.Prefix.String()] = r.Iface
	}
	want := map[string]string{"0.0.0.0/0": "eth0", "192.168.1.0/24": "eth0", "10.9.0.0/24": "wg0",
		"192.168.50.0/24": "br-guest", "::/0": "eth0", "2001:db8:1::/64": "eth0", "fe80::/64": "eth0"}
	if len(got) != len(want) {
		t.Fatalf("routes %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("route %s: %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	// Malformed data ends the parse without a panic.
	for i := range dump {
		_ = ParseRouteDump(dump[:i], names)
	}

	table, truncated := NewRouteTable(append(routes,
		Route{Prefix: p("192.168.1.0/24"), Iface: "eth9", Metric: ^uint32(0)}, // a connected prefix loses to the kernel route
		Route{Prefix: p("10.9.0.7/32"), Iface: "tun1", Metric: 5}))
	if truncated {
		t.Fatal("truncated")
	}
	for _, tc := range []struct{ addr, iface string }{
		{"192.168.1.20", "eth0"},
		{"::ffff:192.168.1.20", "eth0"},
		{"10.9.0.5", "wg0"},
		{"10.9.0.7", "tun1"}, // the most specific route
		{"192.168.50.3", "br-guest"},
		{"8.8.8.8", ""},           // only the default route: no interface
		{"2001:db8:1::5", "eth0"}, // a global IPv6 prefix
		{"fe80::1234", ""},        // link-local: every interface has it
		{"2001:db8:9::1", ""},     // no route
		{"127.0.0.1", ""},         // local routes are skipped
		{"172.16.0.1", ""},        // other tables are skipped
		{"10.98.0.1", ""},         // unreachable
	} {
		if got := table.InterfaceOf(netip.MustParseAddr(tc.addr)); got != tc.iface {
			t.Errorf("InterfaceOf(%s) = %q, want %q", tc.addr, got, tc.iface)
		}
	}
	if d := table.DefaultRoutes(); len(d) != 2 || d[0].Gateway != a("192.168.1.1") || d[1].Gateway != a("fe80::1") {
		t.Fatalf("default routes %+v", d)
	}
}

func TestRouteTableLimitAndRefresh(t *testing.T) {
	var many []Route
	for i := range maxRoutes + 10 {
		many = append(many, Route{Prefix: netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i >> 8), byte(i), 0}), 24), Iface: "eth0"})
	}
	table, truncated := NewRouteTable(many)
	if !truncated || len(table.Routes()) != maxRoutes {
		t.Fatalf("truncated %v, %d routes", truncated, len(table.Routes()))
	}
	old := readRoutes
	t.Cleanup(func() { readRoutes = old; routes.Store(nil) })
	cur := []Route{{Prefix: netip.MustParsePrefix("192.168.7.0/24"), Iface: "eth1"}}
	readRoutes = func() []Route { return cur }
	if changed, _ := RefreshRoutes(); !changed || InterfaceOf(netip.MustParseAddr("192.168.7.9")) != "eth1" {
		t.Fatal("first refresh")
	}
	if changed, _ := RefreshRoutes(); changed {
		t.Fatal("an unchanged table reported a change")
	}
	cur = []Route{{Prefix: netip.MustParsePrefix("192.168.7.0/24"), Iface: "eth2"}}
	if changed, _ := RefreshRoutes(); !changed || InterfaceOf(netip.MustParseAddr("192.168.7.9")) != "eth2" {
		t.Fatal("a changed table")
	}
}

func FuzzParseRouteDump(f *testing.F) {
	f.Add(nlRoute(2, netip.MustParsePrefix("192.168.1.0/24"), rtnUnicast, 254, 2, netip.Addr{}, 100))
	f.Add(nlRoute(10, netip.MustParsePrefix("2001:db8::/32"), rtnUnicast, 254, 2, netip.MustParseAddr("fe80::1"), 1))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, r := range ParseRouteDump(b, map[int]string{2: "eth0"}) {
			if !r.Prefix.IsValid() || r.Iface != "eth0" {
				t.Fatalf("invalid route %+v", r)
			}
		}
	})
}

// The zone of a link-local peer names the interface it arrived on; every
// other address has none, and the addresses themselves stay canonical.
func TestLinkLocalZone(t *testing.T) {
	for _, tc := range []struct{ in, zone string }{
		{"fe80::5%vlan20", "vlan20"},
		{"fe80::5", ""},
		{"fd00::5%vlan20", ""},
		{"192.168.1.5", ""},
		{"::ffff:192.168.1.5", ""},
	} {
		if z := LinkLocalZone(netip.MustParseAddr(tc.in)); z != tc.zone {
			t.Errorf("%s: %q", tc.in, z)
		}
	}
	if z := PeerZone(&net.TCPAddr{IP: net.ParseIP("fe80::5"), Port: 853, Zone: "wg0"}); z != "wg0" {
		t.Errorf("tcp: %q", z)
	}
	if z := PeerZone(&net.UDPAddr{IP: net.ParseIP("192.168.1.5"), Port: 53}); z != "" {
		t.Errorf("udp v4: %q", z)
	}
	if a := PeerFromRemote("[fe80::5%vlan20]:443"); a.String() != "fe80::5%vlan20" {
		t.Errorf("remote: %s", a)
	}
	if a := PeerFromRemote("[::ffff:192.168.1.5]:443"); a.String() != "192.168.1.5" {
		t.Errorf("remote v4: %s", a)
	}
	if a := AddrFromRemote("[fe80::5%vlan20]:443"); a.String() != "fe80::5" {
		t.Errorf("canonical: %s", a)
	}
}
