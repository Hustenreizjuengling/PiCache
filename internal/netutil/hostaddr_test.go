package netutil

import (
	"context"
	"encoding/binary"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// fakeHostAddrs replaces the host address source for the duration of the
// test and clears the on-link cache; the returned func changes the
// addresses.
func fakeHostAddrs(t *testing.T, addrs []HostAddr) func([]HostAddr) {
	t.Helper()
	cur := addrs
	old := hostAddrs
	hostAddrs = func() []HostAddr { return cur }
	onLinkAll.reset()
	t.Cleanup(func() {
		hostAddrs = old
		onLinkAll.reset()
	})
	return func(next []HostAddr) { cur = next; onLinkAll.reset() }
}

func up(iface, prefix string) HostAddr {
	return HostAddr{Iface: iface, Prefix: netip.MustParsePrefix(prefix), Up: true}
}

// nlAddr encodes one RTM_NEWADDR message (host byte order) as the kernel
// sends it in an address dump.
func nlAddr(index int, prefix netip.Prefix, flags uint32, local netip.Addr) []byte {
	attr := func(t uint16, v []byte) []byte {
		b := make([]byte, (4+len(v)+3)&^3)
		binary.NativeEndian.PutUint16(b[0:2], uint16(4+len(v)))
		binary.NativeEndian.PutUint16(b[2:4], t)
		copy(b[4:], v)
		return b
	}
	body := make([]byte, ifaMsgLen)
	body[0] = 2 // AF_INET
	if prefix.Addr().Is6() {
		body[0] = 10 // AF_INET6
	}
	body[1] = byte(prefix.Bits())
	body[2] = byte(flags)
	binary.NativeEndian.PutUint32(body[4:8], uint32(index))
	body = append(body, attr(ifaAddress, prefix.Addr().AsSlice())...)
	if local.IsValid() {
		body = append(body, attr(ifaLocal, local.AsSlice())...)
	}
	var f [4]byte
	binary.NativeEndian.PutUint32(f[:], flags)
	body = append(body, attr(ifaFlags, f[:])...)
	return nlMsg(body)
}

// nlAddrLifetime encodes an RTM_NEWADDR message with an IFA_CACHEINFO
// attribute (valid lifetime in seconds; infiniteLifetime for a static
// address).
func nlAddrLifetime(index int, prefix netip.Prefix, valid uint32) []byte {
	b := nlAddr(index, prefix, 0x80, netip.Addr{})
	ci := make([]byte, 4+16)
	binary.NativeEndian.PutUint16(ci[0:2], uint16(len(ci)))
	binary.NativeEndian.PutUint16(ci[2:4], ifaCacheInfo)
	binary.NativeEndian.PutUint32(ci[4:8], valid) // preferred
	binary.NativeEndian.PutUint32(ci[8:12], valid)
	return nlMsg(append(b[nlmsgHdrLen:], ci...))
}

// nlMsg wraps an address message body into a netlink RTM_NEWADDR message.
func nlMsg(body []byte) []byte {
	msg := make([]byte, nlmsgHdrLen, nlmsgHdrLen+len(body))
	binary.NativeEndian.PutUint32(msg[0:4], uint32(nlmsgHdrLen+len(body)))
	binary.NativeEndian.PutUint16(msg[4:6], rtmNewAddr)
	return append(msg, body...)
}

// The netlink address dump yields every address with its interface and
// state; IPv4 point-to-point links use the local address.
func TestParseAddrDump(t *testing.T) {
	p := netip.MustParsePrefix
	var dump []byte
	dump = append(dump, nlAddr(1, p("127.0.0.1/8"), 0x80, netip.MustParseAddr("127.0.0.1"))...)
	dump = append(dump, nlAddr(2, p("192.168.1.10/24"), 0x80, netip.MustParseAddr("192.168.1.10"))...)
	dump = append(dump, nlAddr(2, p("192.168.1.11/24"), 0x01, netip.MustParseAddr("192.168.1.11"))...) // secondary, not temporary
	dump = append(dump, nlAddr(3, p("10.8.0.2/32"), 0x80, netip.MustParseAddr("10.8.0.1"))...)         // point-to-point
	dump = append(dump, nlAddr(2, p("fd00::10/64"), 0x80, netip.Addr{})...)                            // stable
	dump = append(dump, nlAddr(2, p("2001:db8::abcd/64"), ifaFTemporary, netip.Addr{})...)             // privacy address
	dump = append(dump, nlAddr(2, p("2001:db8:0:1::10/64"), ifaFDeprecated, netip.Addr{})...)          // old prefix
	dump = append(dump, nlAddr(2, p("2001:db8::99/64"), ifaFTentative, netip.Addr{})...)               // DAD running
	dump = append(dump, nlAddr(2, p("2001:db8::98/64"), ifaFDadFailed|ifaFTentative, netip.Addr{})...) // duplicate
	dump = append(dump, nlAddr(9, p("fd00::99/64"), 0, netip.Addr{})...)                               // unknown interface
	dump = append(dump, nlAddrLifetime(2, p("192.168.1.12/24"), 86400)...)                             // from a DHCP client
	dump = append(dump, nlAddrLifetime(2, p("192.168.1.13/24"), infiniteLifetime)...)                  // static
	msgs := parseAddrDump(dump)
	if len(msgs) != 12 {
		t.Fatalf("parsed %d messages: %+v", len(msgs), msgs)
	}
	ifs := map[int]net.Interface{
		1: {Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
		2: {Index: 2, Name: "eth0", Flags: net.FlagUp},
		3: {Index: 3, Name: "tun0"},
	}
	got := netlinkHostAddrs(msgs, ifs)
	want := []HostAddr{
		{Iface: "lo", Prefix: p("127.0.0.1/8"), Up: true, Loopback: true},
		{Iface: "eth0", Prefix: p("192.168.1.10/24"), Up: true},
		{Iface: "eth0", Prefix: p("192.168.1.11/24"), Up: true},
		{Iface: "tun0", Prefix: p("10.8.0.1/32")},
		{Iface: "eth0", Prefix: p("fd00::10/64"), Up: true},
		{Iface: "eth0", Prefix: p("2001:db8::abcd/64"), Up: true, Temporary: true},
		{Iface: "eth0", Prefix: p("2001:db8:0:1::10/64"), Up: true, Deprecated: true},
		{Iface: "eth0", Prefix: p("2001:db8::99/64"), Up: true, Tentative: true},
		{Iface: "eth0", Prefix: p("2001:db8::98/64"), Up: true, Tentative: true},
		{Iface: "eth0", Prefix: p("192.168.1.12/24"), Up: true, Dynamic: true},
		{Iface: "eth0", Prefix: p("192.168.1.13/24"), Up: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("host addresses\n%+v\nwant\n%+v", got, want)
	}
	for i := range dump {
		parseAddrDump(dump[:i]) // truncated input never panics
	}
}

// ConnectedSubnets: every on-link prefix of an up interface, public or
// private, without loopback and virtual bridges or tunnels, IPv4 ≥ /8 and
// IPv6 ≥ /48.
func TestConnectedSubnets(t *testing.T) {
	fakeHostAddrs(t, []HostAddr{
		{Iface: "lo", Prefix: netip.MustParsePrefix("127.0.0.1/8"), Up: true, Loopback: true},
		up("eth0", "192.168.1.10/24"), up("eth0", "203.0.113.5/24"), up("eth0", "2001:db8:5::10/64"),
		up("eth0", "fd00::10/64"), up("eth0", "2001:db8:6::1/40"), up("eth0", "10.1.2.3/7"),
		up("docker0", "172.17.0.1/16"), up("wg0", "2001:db8:7::1/64"), up("br-1234", "198.51.100.1/24"),
		{Iface: "eth1", Prefix: netip.MustParsePrefix("198.51.100.77/24")}, // down
	})
	var got []string
	for _, p := range ConnectedSubnets() {
		got = append(got, p.String())
	}
	want := []string{"192.168.1.0/24", "203.0.113.0/24", "2001:db8:5::/64", "fd00::/64"}
	if !slices.Equal(got, want) {
		t.Fatalf("ConnectedSubnets() = %v, want %v", got, want)
	}
}

// dns.trustConnectedNetworks allows the public networks this machine is
// connected to, follows prefix changes when the ACL is rebuilt, and never
// trusts virtual bridges.
func TestTrustConnectedNetworksACL(t *testing.T) {
	fakeInterfaces(t, "192.168.1.10/24", "2001:db8:5::10/64")
	set := fakeHostAddrs(t, []HostAddr{up("eth0", "192.168.1.10/24"), up("eth0", "2001:db8:5::10/64"),
		up("docker0", "2001:db8:99::1/64"), up("virbr0", "203.0.113.1/24")})
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st, err := settings.Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	w := NewACLWatcher(st)
	gua := netip.MustParseAddr("2001:db8:5::20")
	if w.Get().Allowed(gua) {
		t.Fatal("off by default: a public on-link prefix must be refused")
	}
	if _, err := st.Update(ctx, func(a *settings.All) error { a.DNS.TrustConnectedNetworks = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !w.Get().Allowed(gua) {
		t.Fatal("the connected GUA prefix must be allowed once trusted")
	}
	for _, s := range []string{"2001:db8:99::5", "203.0.113.9", "198.51.100.1"} {
		if w.Get().Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s (virtual bridge or elsewhere) must stay refused", s)
		}
	}
	// The provider changes the prefix: the next rebuild follows it.
	set([]HostAddr{up("eth0", "192.168.1.10/24"), up("eth0", "2001:db8:6::10/64")})
	done := make(chan struct{})
	go func() { w.Run(done, 10*time.Millisecond) }()
	defer close(done)
	deadline := time.Now().Add(3 * time.Second)
	for !w.Get().Allowed(netip.MustParseAddr("2001:db8:6::20")) || w.Get().Allowed(gua) {
		if time.Now().After(deadline) {
			t.Fatal("the rebuilt ACL must follow the new prefix")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// OnLink: link-local and connected addresses except this machine's own;
// never loopback or other networks.
func TestOnLink(t *testing.T) {
	fakeHostAddrs(t, []HostAddr{up("eth0", "192.168.1.10/24"), up("eth0", "fd00::10/64"), up("wg0", "10.8.0.1/24")})
	for s, want := range map[string]bool{
		"192.168.1.20": true, "fd00::20": true, "fe80::1": true, "169.254.3.4": true,
		"192.168.1.10": false, "fd00::10": false, // this machine
		"10.8.0.5": false, "8.8.8.8": false, "127.0.0.1": false, "::1": false, "2001:db8::1": false,
	} {
		if got := OnLink(netip.MustParseAddr(s)); got != want {
			t.Errorf("OnLink(%s) = %v, want %v", s, got, want)
		}
	}
}

// accept_ra 0 ignores router advertisements, 1 only while forwarding is
// off, 2 never; unreadable values are unknown.
func TestIgnoresRouterAdvertisements(t *testing.T) {
	dir := t.TempDir()
	old := ipv6ConfDir
	ipv6ConfDir = dir + "/"
	t.Cleanup(func() { ipv6ConfDir = old })
	write := func(iface, acceptRA, forwarding string) {
		if err := os.MkdirAll(filepath.Join(dir, iface), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, v := range map[string]string{"accept_ra": acceptRA, "forwarding": forwarding} {
			if err := os.WriteFile(filepath.Join(dir, iface, name), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("eth0", "0", "0")
	write("eth1", "1", "0")
	write("eth2", "1", "1")
	write("eth3", "2", "1")
	for iface, want := range map[string][2]bool{
		"eth0": {true, true}, "eth1": {false, true}, "eth2": {true, true}, "eth3": {false, true},
		"eth9": {false, false}, "../eth0": {false, false}, "": {false, false}, "..": {false, false},
	} {
		ignores, ok := IgnoresRouterAdvertisements(iface)
		if ignores != want[0] || ok != want[1] {
			t.Errorf("IgnoresRouterAdvertisements(%q) = %v, %v, want %v, %v", iface, ignores, ok, want[0], want[1])
		}
	}
}
