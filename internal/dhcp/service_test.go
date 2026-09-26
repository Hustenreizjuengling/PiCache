package dhcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// sent is a datagram the service sent.
type sent struct {
	b       []byte
	ifIndex int
	src     netip.Addr
	dst     netip.AddrPort
}

// fakeConn4 records what the service sends; onWrite may answer (a probe).
type fakeConn4 struct {
	mu      sync.Mutex
	out     []sent
	onWrite func(s sent)
	closed  chan struct{}
	once    sync.Once
}

func newFakeConn4() *fakeConn4 { return &fakeConn4{closed: make(chan struct{})} }

func (f *fakeConn4) ReadFrom([]byte) (int, int, netip.AddrPort, error) {
	<-f.closed
	return 0, 0, netip.AddrPort{}, net.ErrClosed
}

func (f *fakeConn4) WriteTo(b []byte, ifIndex int, src netip.Addr, dst netip.AddrPort) error {
	s := sent{b: slices.Clone(b), ifIndex: ifIndex, src: src, dst: dst}
	f.mu.Lock()
	f.out = append(f.out, s)
	hook := f.onWrite
	f.mu.Unlock()
	if hook != nil {
		hook(s)
	}
	return nil
}

func (f *fakeConn4) SetReadDeadline(time.Time) error { return nil }
func (f *fakeConn4) Close() error                    { f.once.Do(func() { close(f.closed) }); return nil }

// take returns and forgets the sent datagrams.
func (f *fakeConn4) take() []sent {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.out
	f.out = nil
	return out
}

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

const testIfIndex = 2

var testIfMAC = net.HardwareAddr{0x02, 0xaa, 0, 0, 0, 0x10}

// testEnvFor is a machine with eth0 (192.168.1.10/24, dynamic as given,
// ULA fd00::10), lo and docker0; the default gateway is 192.168.1.1.
func testEnvFor(dynamic bool) env {
	return env{
		interfaces: func() ([]net.Interface, error) {
			return []net.Interface{
				{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
				{Index: testIfIndex, Name: "eth0", Flags: net.FlagUp | net.FlagBroadcast, HardwareAddr: testIfMAC},
				{Index: 3, Name: "docker0", Flags: net.FlagUp, HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 3}},
			}, nil
		},
		addrs: func() []netutil.HostAddr {
			return []netutil.HostAddr{
				{Iface: "lo", Prefix: netip.MustParsePrefix("127.0.0.1/8"), Up: true, Loopback: true},
				{Iface: "eth0", Prefix: netip.MustParsePrefix("192.168.1.10/24"), Up: true, Dynamic: dynamic},
				{Iface: "eth0", Prefix: netip.MustParsePrefix("fd00::10/64"), Up: true},
				{Iface: "eth0", Prefix: netip.MustParsePrefix("fe80::10/64"), Up: true},
				{Iface: "docker0", Prefix: netip.MustParsePrefix("172.17.0.1/16"), Up: true},
			}
		},
		gateway4: func() (netip.Addr, error) { return testGW, nil },
		bridge:   func() bool { return false },
	}
}

// testSvc is a service on a temporary database with fake sockets, clock,
// neighbour table and environment.
type testSvc struct {
	*Service
	conn  *fakeConn4
	set   *settings.Store
	d     *db.DB
	clk   *clock
	neigh map[netip.Addr]string
	nmu   sync.Mutex
}

// enabledDHCP configures eth0 with the range .100–.199.
func enabledDHCP(a *settings.All) {
	a.DHCP.Enabled = true
	a.DHCP.Interface = "eth0"
	a.DHCP.RangeStart = "192.168.1.100"
	a.DHCP.RangeEnd = "192.168.1.199"
	a.DNS.LocalDomain = "lan"
}

func newTestSvc(t *testing.T, configure func(*settings.All)) *testSvc {
	t.Helper()
	probeWait = 5 * time.Millisecond
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "picache.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return openTestSvc(t, d, configure)
}

// openTestSvc starts a service on an existing database (restart tests).
func openTestSvc(t *testing.T, d *db.DB, configure func(*settings.All)) *testSvc {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := settings.Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		if _, err := set.Update(ctx, func(a *settings.All) error { configure(a); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	ts := &testSvc{conn: newFakeConn4(), set: set, d: d, clk: &clock{now: t0}, neigh: map[netip.Addr]string{}}
	socks := &Sockets{bindCapable: true, rawCapable: true, v4: ts.conn, v6Err: "not in tests"}
	s, err := New(ctx, Deps{DB: d, Settings: set, Sockets: socks, Log: log,
		Neighbour: func(ip netip.Addr) (string, bool) {
			ts.nmu.Lock()
			defer ts.nmu.Unlock()
			m, ok := ts.neigh[ip]
			return m, ok
		}})
	if err != nil {
		t.Fatal(err)
	}
	s.goos, s.env, s.now, s.sleep, s.prime = "linux", testEnvFor(false), ts.clk.Now, func(time.Duration) {}, nil
	ts.Service = s
	s.evaluate(ctx, true)
	ts.conn.take() // the probe
	return ts
}

// packet sends a client packet into the service from 0.0.0.0 on eth0.
func (ts *testSvc) packet(r *req) []sent {
	ts.handle4(r.bytes(), testIfIndex, netip.AddrPortFrom(ip("0.0.0.0"), clientPort))
	return ts.cur4().take()
}

// cur4 is the UDP 67 socket in use (the service may have opened a new one).
func (ts *testSvc) cur4() *fakeConn4 {
	if v4, _, _, _, _, _ := ts.Service.d.Sockets.get(); v4 != nil {
		if c, ok := v4.(*fakeConn4); ok {
			return c
		}
	}
	return ts.conn
}

// reply parses the single reply of a packet exchange.
func (ts *testSvc) reply(t *testing.T, r *req) (*message, sent) {
	t.Helper()
	out := ts.packet(r)
	if len(out) != 1 {
		t.Fatalf("%d replies, want 1", len(out))
	}
	m, err := parseMessage(out[0].b)
	if err != nil {
		t.Fatal(err)
	}
	return m, out[0]
}

func reqFrom(typ byte, mac [6]byte) *req {
	r := newReq(typ)
	r.mac = mac
	return r
}

func macN(n byte) [6]byte { return [6]byte{0x02, 0, 0, 0, 0, n} }

// The full exchange: DISCOVER → OFFER (broadcast from eth0 with the
// options), REQUEST → ACK, the lease is stored, has a DNS name and
// survives a restart.
func TestExchange(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	if st := ts.Status(); st.State != StateServing || len(st.Blockers) != 0 {
		t.Fatalf("status %+v", st)
	}
	disc := reqFrom(msgDiscover, macN(1)).opt(optHostName, []byte("Laptop")...).opt(optParamRequest, optSubnetMask, optRouter, optDNS, optDomainName, optDomainSearch)
	off, out := ts.reply(t, disc)
	if out.dst != netip.AddrPortFrom(broadcast4, clientPort) || out.src != testSelf || out.ifIndex != testIfIndex {
		t.Fatalf("offer delivery %+v", out)
	}
	if off.msgType() != msgOffer || off.yiaddr != ip("192.168.1.100") || off.addrOpt(optServerID) != testSelf {
		t.Fatalf("offer %+v %v", off, off.opts)
	}
	for code, want := range map[byte]string{optSubnetMask: "\xff\xff\xff\x00", optRouter: "\xc0\xa8\x01\x01", optDNS: "\xc0\xa8\x01\x0a",
		optDomainName: "lan", optBroadcast: "\xc0\xa8\x01\xff", optLeaseTime: "\x00\x01\x51\x80", optRenewal: "\x00\x00\xa8\xc0",
		optRebinding: "\x00\x01\x27\x50", optHostName: "laptop", optDomainSearch: "\x03lan\x00"} {
		if got := string(off.opts[code]); got != want {
			t.Errorf("option %d = %q, want %q", code, got, want)
		}
	}
	ack, _ := ts.reply(t, reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.100").addr(optServerID, "192.168.1.10").
		opt(optHostName, []byte("Laptop")...))
	if ack.msgType() != msgAck || ack.yiaddr != ip("192.168.1.100") {
		t.Fatalf("ack %+v", ack)
	}
	leases := ts.Leases()
	if len(leases) != 1 || leases[0].IP != "192.168.1.100" || leases[0].DNSName != "laptop.lan" || !leases[0].Active ||
		!leases[0].Expires.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("leases %+v", leases)
	}
	if a, ttl, ok := ts.LeaseAddr("laptop.lan"); !ok || a != ip("192.168.1.100") || ttl != 300 {
		t.Fatalf("name %v %d %v", a, ttl, ok)
	}
	if n, _, ok := ts.LeasePTR(ip("192.168.1.100")); !ok || n != "laptop.lan" {
		t.Fatalf("ptr %q", n)
	}
	if st := ts.Status(); st.Pool == nil || st.Pool.Used != 1 || st.Pool.Size != 100 || st.Counters.Offers != 1 || st.Counters.Acks != 1 {
		t.Fatalf("status %+v %+v", st.Pool, st.Counters)
	}

	// Restart: the lease is loaded; a renewal (unicast from the address)
	// is acknowledged to ciaddr.
	ts2 := openTestSvc(t, ts.d, nil)
	renew := reqFrom(msgRequest, macN(1))
	renew.ciaddr = ip("192.168.1.100")
	ts2.clk.Add(12 * time.Hour)
	m, out := ts2.reply(t, renew)
	if m.msgType() != msgAck || m.yiaddr != ip("192.168.1.100") || out.dst != netip.AddrPortFrom(ip("192.168.1.100"), clientPort) {
		t.Fatalf("renewal %+v to %v", m, out.dst)
	}
	if l := ts2.Leases(); len(l) != 1 || !l[0].Expires.Equal(t0.Add(36*time.Hour)) || l[0].Hostname != "laptop" {
		t.Fatalf("renewed lease %+v", l)
	}
}

// NAK rules end to end: another client's address, outside the subnet; NAKs
// are broadcast.
func TestNak(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ts.reply(t, reqFrom(msgDiscover, macN(1)))
	ts.reply(t, reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.100").addr(optServerID, "192.168.1.10"))
	for _, want := range []string{"192.168.1.100", "10.0.0.7"} {
		r := reqFrom(msgRequest, macN(2)).addr(optRequestedIP, want) // init-reboot
		m, out := ts.reply(t, r)
		if m.msgType() != msgNak || m.yiaddr.IsValid() && !m.yiaddr.IsUnspecified() || out.dst.Addr() != broadcast4 {
			t.Fatalf("%s: %+v to %v", want, m, out.dst)
		}
		if len(m.opts) != 2 { // type and server identifier only
			t.Fatalf("nak options %v", m.opts)
		}
	}
	// A renewal of an address outside the subnet is nak'ed by broadcast too.
	r := reqFrom(msgRequest, macN(3))
	r.ciaddr = ip("10.0.0.7")
	if m, out := ts.reply(t, r); m.msgType() != msgNak || out.dst.Addr() != broadcast4 {
		t.Fatalf("renewal outside: %+v", m)
	}
	// A client's REQUEST for another server's offer is not answered.
	if out := ts.packet(reqFrom(msgRequest, macN(4)).addr(optRequestedIP, "192.168.1.150").addr(optServerID, "192.168.1.1")); len(out) != 0 {
		t.Fatalf("answered a request for another server: %d", len(out))
	}
}

// DECLINE quarantines the address; the neighbour check skips addresses in
// use and quarantines them; at most 4 checks per DISCOVER.
func TestDeclineAndConflict(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ts.reply(t, reqFrom(msgDiscover, macN(1)))
	ts.reply(t, reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.100").addr(optServerID, "192.168.1.10"))
	if out := ts.packet(reqFrom(msgDecline, macN(1)).addr(optRequestedIP, "192.168.1.100").addr(optServerID, "192.168.1.10")); len(out) != 0 {
		t.Fatal("decline answered")
	}
	if len(ts.Leases()) != 0 {
		t.Fatal("declined lease kept")
	}
	// A decline of an address the client does not hold is ignored.
	ts.packet(reqFrom(msgDecline, macN(9)).addr(optRequestedIP, "192.168.1.150"))
	ts.nmu.Lock()
	ts.neigh[ip("192.168.1.101")] = "02:99:00:00:00:01" // another device
	ts.nmu.Unlock()
	m, _ := ts.reply(t, reqFrom(msgDiscover, macN(2)))
	if m.yiaddr != ip("192.168.1.102") {
		t.Fatalf("offered %v, want .102 (.100 declined, .101 in use)", m.yiaddr)
	}
	ts.mu.Lock()
	q100, q101, q150 := ts.t.quarantined(ip("192.168.1.100"), t0), ts.t.quarantined(ip("192.168.1.101"), t0), ts.t.quarantined(ip("192.168.1.150"), t0)
	ts.mu.Unlock()
	if !q100 || !q101 || q150 {
		t.Fatalf("quarantine %v %v %v", q100, q101, q150)
	}
	// After 10 minutes .100 is free again.
	ts.clk.Add(quarantine + time.Second)
	if m, _ := ts.reply(t, reqFrom(msgDiscover, macN(3))); m.yiaddr != ip("192.168.1.100") {
		t.Fatalf("after the quarantine: %v", m.yiaddr)
	}
	// Four devices in a row: no offer.
	ts.nmu.Lock()
	for i := 100; i <= 106; i++ {
		ts.neigh[netip.AddrFrom4([4]byte{192, 168, 1, byte(i)})] = "02:99:00:00:00:01"
	}
	ts.nmu.Unlock()
	ts.clk.Add(quarantine + time.Second)
	if out := ts.packet(reqFrom(msgDiscover, macN(4))); len(out) != 0 {
		t.Fatalf("offered after 4 conflicts: %d", len(out))
	}
}

// RELEASE ends the lease; the address stays with the client for 24 h and
// is purged afterwards.
func TestReleaseAndRetention(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ts.reply(t, reqFrom(msgDiscover, macN(1)))
	ts.reply(t, reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.100").addr(optServerID, "192.168.1.10").opt(optHostName, 'p', 'c'))
	rel := reqFrom(msgRelease, macN(1)).addr(optServerID, "192.168.1.10")
	rel.ciaddr = ip("192.168.1.100")
	ts.packet(rel)
	if l := ts.Leases(); len(l) != 1 || l[0].Active {
		t.Fatalf("released %+v", l)
	}
	if _, _, ok := ts.LeaseAddr("pc.lan"); ok {
		t.Fatal("name of a released lease answered")
	}
	// Another client gets another address; the client itself its old one.
	if m, _ := ts.reply(t, reqFrom(msgDiscover, macN(2))); m.yiaddr != ip("192.168.1.101") {
		t.Fatalf("other client %v", m.yiaddr)
	}
	ts.clk.Add(23 * time.Hour)
	if m, _ := ts.reply(t, reqFrom(msgDiscover, macN(1))); m.yiaddr != ip("192.168.1.100") {
		t.Fatalf("same client %v", m.yiaddr)
	}
	ts.clk.Add(2 * time.Hour)
	ts.purge(context.Background())
	if l := ts.Leases(); len(l) != 0 {
		t.Fatalf("not purged: %+v", l)
	}
	var n int
	if err := ts.d.R.QueryRow(`SELECT COUNT(*) FROM dhcp_leases`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows %d %v", n, err)
	}
}

// Expiry: the DNS name's TTL shrinks with the lease, and an expired lease
// has no name.
func TestNameTTL(t *testing.T) {
	ts := newTestSvc(t, func(a *settings.All) { enabledDHCP(a); a.DHCP.LeaseSeconds = 600 })
	ts.reply(t, reqFrom(msgDiscover, macN(1)))
	ts.reply(t, reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.100").addr(optServerID, "192.168.1.10").opt(optHostName, 'p', 'c'))
	ts.clk.Add(500 * time.Second)
	if _, ttl, ok := ts.LeaseAddr("pc.lan"); !ok || ttl != 100 {
		t.Fatalf("ttl %d %v", ttl, ok)
	}
	ts.clk.Add(100 * time.Second)
	if _, _, ok := ts.LeaseAddr("pc.lan"); ok {
		t.Fatal("expired lease answered")
	}
}

// Host names: a name held by another active lease gives no DNS name to
// the second; a static entry's name wins; without registerHostnames there
// are no DNS names.
func TestHostnameConflicts(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	lease := func(mac [6]byte, want, host string) {
		ts.reply(t, reqFrom(msgDiscover, mac))
		ts.reply(t, reqFrom(msgRequest, mac).addr(optRequestedIP, want).addr(optServerID, "192.168.1.10").opt(optHostName, []byte(host)...))
	}
	lease(macN(1), "192.168.1.100", "android")
	lease(macN(2), "192.168.1.101", "Android")
	byIP := map[string]Lease{}
	for _, l := range ts.Leases() {
		byIP[l.IP] = l
	}
	if l := byIP["192.168.1.100"]; l.DNSName != "android.lan" || l.NameConflict {
		t.Fatalf("first %+v", l)
	}
	// The second gets its generated name (DNS only: the display name stays).
	if l := byIP["192.168.1.101"]; l.DNSName != "192-168-1-101.lan" || !l.NameGenerated || !l.NameConflict || l.Hostname != "android" {
		t.Fatalf("second %+v", l)
	}
	if ts.LeaseName(ip("192.168.1.101")) != "android" {
		t.Fatalf("display of the second %q", ts.LeaseName(ip("192.168.1.101")))
	}
	// A static entry for the second client with its own name.
	if _, err := ts.CreateStatic(context.Background(), StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.101", Hostname: "tablet"}); err != nil {
		t.Fatal(err)
	}
	if ip, _, ok := ts.LeaseAddr("tablet.lan"); !ok || ip != netip.MustParseAddr("192.168.1.101") {
		t.Fatalf("static name %v %v", ip, ok)
	}
	if ts.LeaseName(ip("192.168.1.101")) != "tablet.lan" {
		t.Fatalf("display %q", ts.LeaseName(ip("192.168.1.101")))
	}
	if _, err := ts.set.Update(context.Background(), func(a *settings.All) error { a.DHCP.RegisterHostnames = false; return nil }); err != nil {
		t.Fatal(err)
	}
	ts.refreshNames()
	if _, _, ok := ts.LeaseAddr("android.lan"); ok || ts.LeaseName(ip("192.168.1.100")) != "android" {
		t.Fatal("names registered without registerHostnames")
	}
}

// At most 50 packets per second in total and 5 per client MAC.
func TestRateLimit(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	inform := func(mac [6]byte) *req {
		r := reqFrom(msgInform, mac)
		r.ciaddr = ip("192.168.1.50")
		return r
	}
	for range 8 {
		ts.packet(inform(macN(1)))
	}
	if got, answered := ts.c.dropped.Load(), ts.c.informs.Load(); got != 3 || answered != 5 {
		t.Fatalf("per MAC: %d dropped, %d answered", got, answered)
	}
	for i := range 60 {
		ts.packet(inform(macN(byte(10 + i))))
	}
	// 5 of the first MAC + 45 others = 50 handled; 3 + 15 dropped.
	if got, answered := ts.c.dropped.Load(), ts.c.informs.Load(); got != 18 || answered != 50 {
		t.Fatalf("total: %d dropped, %d answered", got, answered)
	}
	ts.clk.Add(time.Second)
	ts.packet(inform(macN(1)))
	if ts.c.informs.Load() != 51 {
		t.Fatal("new second not admitted")
	}
}

// Packets from other interfaces, relayed requests, replies and sources
// outside the subnet are never answered.
func TestIgnored(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	if out := ts.handle4Out(reqFrom(msgDiscover, macN(1)).bytes(), 3, "0.0.0.0"); len(out) != 0 {
		t.Fatal("answered another interface")
	}
	relayed := reqFrom(msgDiscover, macN(1))
	relayed.giaddr = ip("192.168.1.2")
	if out := ts.handle4Out(relayed.bytes(), testIfIndex, "192.168.1.2"); len(out) != 0 {
		t.Fatal("answered a relayed request")
	}
	if out := ts.handle4Out(reqFrom(msgDiscover, macN(1)).bytes(), testIfIndex, "8.8.8.8"); len(out) != 0 {
		t.Fatal("answered a source outside the subnet")
	}
	bad := reqFrom(msgDiscover, macN(1))
	bad.htype = 6
	if out := ts.handle4Out(bad.bytes(), testIfIndex, "0.0.0.0"); len(out) != 0 {
		t.Fatal("answered a non-Ethernet request")
	}
	group := reqFrom(msgDiscover, [6]byte{1, 0, 0x5e, 0, 0, 1})
	if out := ts.handle4Out(group.bytes(), testIfIndex, "0.0.0.0"); len(out) != 0 {
		t.Fatal("answered a multicast MAC")
	}
}

func (ts *testSvc) handle4Out(b []byte, ifIndex int, from string) []sent {
	ts.handle4(b, ifIndex, netip.AddrPortFrom(ip(from), clientPort))
	return ts.cur4().take()
}

// INFORM gets the options without a lease, unicast to ciaddr.
func TestInform(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	r := reqFrom(msgInform, macN(1))
	r.ciaddr = ip("192.168.1.50")
	m, out := ts.reply(t, r)
	if m.msgType() != msgAck || !m.yiaddr.IsUnspecified() || m.ciaddr != ip("192.168.1.50") || m.opts[optLeaseTime] != nil ||
		out.dst != netip.AddrPortFrom(ip("192.168.1.50"), clientPort) || m.opts[optDNS] == nil {
		t.Fatalf("inform %+v %v to %v", m, m.opts, out.dst)
	}
	if len(ts.Leases()) != 0 {
		t.Fatal("inform created a lease")
	}
}

// The gates: a dynamic own address blocks (hard), as do a missing
// interface and a range outside the subnet; unavailable without the
// sockets, on other systems and in a bridge network.
func TestGates(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	ts.env = testEnvFor(true)
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateBlocked || !slices.Equal(st.Blockers, []string{BlockerDynamicAddress}) || !st.Interface.Dynamic {
		t.Fatalf("dynamic: %+v", st)
	}
	if h, msg, _, _ := ts.Health(); h != "warn" || msg == "" {
		t.Fatalf("health %s %s", h, msg)
	}
	ts.env = testEnvFor(false)
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Interface = "eth9"; return nil })
	ts.evaluate(ctx, true)
	if st := ts.Status(); !slices.Equal(st.Blockers, []string{BlockerNoInterface}) || st.Interface != nil {
		t.Fatalf("missing interface: %+v", st)
	}
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Interface = "docker0"; return nil })
	ts.evaluate(ctx, true)
	if st := ts.Status(); !slices.Equal(st.Blockers, []string{BlockerNoInterface}) {
		t.Fatalf("virtual interface: %+v", st)
	}
	ts.set.Update(ctx, func(a *settings.All) error {
		a.DHCP.Interface, a.DHCP.RangeStart, a.DHCP.RangeEnd = "eth0", "10.0.0.1", "10.0.0.9"
		return nil
	})
	ts.evaluate(ctx, true)
	if st := ts.Status(); !slices.Equal(st.Blockers, []string{BlockerRange}) {
		t.Fatalf("range: %+v", st)
	}
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = false; return nil })
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateOff {
		t.Fatalf("off: %+v", st)
	}
	if h, _, _, show := ts.Health(); h != "ok" || !show {
		t.Fatalf("health off %s %v", h, show)
	}
	ts.env.bridge = func() bool { return true }
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateUnavailable || st.Reason != reasonBridge || st.ReasonCode != ReasonBridge || st.Available {
		t.Fatalf("bridge: %+v", st)
	}
	ts.env.bridge = func() bool { return false }
	ts.goos = "windows"
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.Reason != reasonNotLinux || st.ReasonCode != ReasonNotLinux {
		t.Fatalf("windows: %+v", st)
	}
	ts.goos = "linux"
	ts.d2().optOut = true
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.Reason != reasonOptOut || st.ReasonCode != ReasonOptOut || st.Available {
		t.Fatalf("PICACHE_DHCP=off: %+v", st)
	}
	if _, _, _, show := ts.Health(); show {
		t.Fatal("health shown while off and unavailable")
	}
	ts.set.Update(ctx, func(a *settings.All) error { enabledDHCP(a); return nil })
	ts.evaluate(ctx, true)
	if h, _, _, _ := ts.Health(); h != "warn" {
		t.Fatalf("enabled but PICACHE_DHCP=off: %s", h)
	}
	ts.d2().optOut, ts.d2().v4, ts.d2().v4Err, ts.d2().v4Code = false, nil, "UDP port 67 is in use by another program", ReasonSocket
	ts.evaluate(ctx, true)
	if h, msg, _, _ := ts.Health(); h != "fail" || msg == "" {
		t.Fatalf("socket failed: %s %s", h, msg)
	}
}

func (ts *testSvc) d2() *Sockets { return ts.Service.d.Sockets }

// Settings are checked against the live interface when enabled.
func TestCheckSettings(t *testing.T) {
	ts := newTestSvc(t, nil)
	base := ts.set.Get().Clone()
	enabledDHCP(base)
	for _, tc := range []struct {
		change func(*settings.DHCP)
		field  string
	}{
		{func(h *settings.DHCP) {}, ""},
		{func(h *settings.DHCP) { h.Enabled = false; h.Interface = "eth9" }, ""},
		{func(h *settings.DHCP) { h.Interface = "eth9" }, "dhcp.interface"},
		{func(h *settings.DHCP) { h.Interface = "docker0" }, "dhcp.interface"},
		{func(h *settings.DHCP) { h.RangeStart = "10.0.0.1" }, "dhcp.rangeStart"},
		{func(h *settings.DHCP) { h.RangeEnd = " 192.168.2.10 " }, "dhcp.rangeEnd"},
		{func(h *settings.DHCP) { h.Router = "10.1.1.1" }, "dhcp.router"},
		{func(h *settings.DHCP) { h.RangeStart, h.RangeEnd = "192.168.1.10", "192.168.1.10" }, "dhcp.rangeEnd"},
	} {
		a := base.Clone()
		tc.change(&a.DHCP)
		err := ts.CheckSettings(a)
		if tc.field == "" {
			if err != nil {
				t.Errorf("%+v: %v", a.DHCP, err)
			}
			continue
		}
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Field != tc.field {
			t.Errorf("%+v: %v, want field %s", a.DHCP, err, tc.field)
		}
	}
}

// Static leases: validation, uniqueness, active flag, delete.
func TestStatics(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	st, err := ts.CreateStatic(ctx, StaticInput{MAC: "02-00-00-00-00-01", IP: "192.168.1.50", Hostname: "Printer", Comment: "office"})
	if err != nil {
		t.Fatal(err)
	}
	if st.MAC != "02:00:00:00:00:01" || st.Hostname != "printer" || st.Active || st.CreatedAt.IsZero() {
		t.Fatalf("%+v", st)
	}
	for _, tc := range []struct {
		in    StaticInput
		field string
	}{
		{StaticInput{MAC: "nope", IP: "192.168.1.51"}, "mac"},
		{StaticInput{MAC: "02:00:00:00:00:01", IP: "192.168.1.51"}, "mac"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.50"}, "ip"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.2.50"}, "ip"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.10"}, "ip"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.1"}, "ip"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.255"}, "ip"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "8.8.8.8"}, "ip"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.51", Hostname: "printer"}, "hostname"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.51", Hostname: "bad_name"}, "hostname"},
		{StaticInput{MAC: "02:00:00:00:00:02", IP: "192.168.1.51", Comment: "a\nb"}, "comment"},
	} {
		_, err := ts.CreateStatic(ctx, tc.in)
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Field != tc.field {
			t.Errorf("%+v: %v, want field %s", tc.in, err, tc.field)
		}
	}
	// The client gets its static address, even outside the range.
	m, _ := ts.reply(t, reqFrom(msgDiscover, macN(1)))
	if m.yiaddr != ip("192.168.1.50") || string(m.opts[optHostName]) != "printer" {
		t.Fatalf("offer %v %q", m.yiaddr, m.opts[optHostName])
	}
	ts.reply(t, reqFrom(msgRequest, macN(1)).addr(optRequestedIP, "192.168.1.50").addr(optServerID, "192.168.1.10"))
	if l := ts.Statics(); len(l) != 1 || !l[0].Active {
		t.Fatalf("statics %+v", l)
	}
	if l := ts.Leases(); len(l) != 1 || !l[0].Static {
		t.Fatalf("leases %+v", l)
	}
	if _, err := ts.UpdateStatic(ctx, "02:00:00:00:00:01", StaticUpdate{IP: "192.168.1.60"}); err != nil {
		t.Fatal(err)
	}
	// The renewal of the old address is nak'ed: the client moves.
	r := reqFrom(msgRequest, macN(1))
	r.ciaddr = ip("192.168.1.50")
	if m, _ := ts.reply(t, r); m.msgType() != msgNak {
		t.Fatalf("renewal after a static change: %v", m.msgType())
	}
	if _, err := ts.UpdateStatic(ctx, "02:00:00:00:00:09", StaticUpdate{IP: "192.168.1.61"}); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("update unknown: %v", err)
	}
	if err := ts.DeleteStatic(ctx, "02:00:00:00:00:01"); err != nil || len(ts.Statics()) != 0 {
		t.Fatalf("delete %v", err)
	}
	if err := ts.DeleteStatic(ctx, "02:00:00:00:00:01"); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("delete again: %v", err)
	}
	if err := ts.DeleteLease(ctx, "02:00:00:00:00:01"); err != nil || len(ts.Leases()) != 0 {
		t.Fatalf("delete lease %v", err)
	}
}

// The probe: a relay-shaped DISCOVER to 255.255.255.255:67 out of the
// interface; offers with the xid are collected (PiCache's own never);
// 10 s between probes of the API; an answer blocks serving and a clean
// probe lifts it.
func TestProbe(t *testing.T) {
	ts := newTestSvc(t, nil)
	ctx := context.Background()
	ts.set.Update(ctx, func(a *settings.All) error { enabledDHCP(a); a.DHCP.Enabled = false; return nil })
	ts.evaluate(ctx, true)
	ts.conn.onWrite = func(s sent) {
		m, err := parseMessage(s.b)
		if err != nil || m.op != opRequest {
			return
		}
		offer := func(from, sid string, xid uint32) {
			r := &req{op: opReply, htype: hwEther, hlen: hwEtherLen, xid: xid, mac: m.mac(), opts: [][]byte{{optMessageType, 1, msgOffer}}}
			r.addr(optServerID, sid)
			b := r.bytes()
			put4(b[16:20], ip("192.168.1.77"))
			ts.handle4(b, testIfIndex, netip.AddrPortFrom(ip(from), serverPort))
		}
		offer("192.168.1.1", "192.168.1.1", m.xid)
		offer("192.168.1.10", "192.168.1.10", m.xid) // ourselves: never
		offer("192.168.1.2", "192.168.1.2", m.xid+1) // another transaction
	}
	ts.nmu.Lock()
	ts.neigh[ip("192.168.1.1")] = "3c:a6:2f:00:00:01"
	ts.nmu.Unlock()
	res, err := ts.Probe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := ts.conn.take()
	if len(out) != 1 || out[0].dst != netip.AddrPortFrom(broadcast4, serverPort) || out[0].ifIndex != testIfIndex || out[0].src != testSelf {
		t.Fatalf("probe sent %+v", out)
	}
	pm, _ := parseMessage(out[0].b)
	if pm.hops != 1 || pm.giaddr != testSelf || pm.mac() != [6]byte(testIfMAC) || pm.msgType() != msgDiscover {
		t.Fatalf("probe packet %+v", pm)
	}
	want := []ProbeServer{{Address: "192.168.1.1", ServerID: "192.168.1.1", Offer: "192.168.1.77", MAC: "3c:a6:2f:00:00:01"}}
	if !slices.Equal(res.Servers, want) {
		t.Fatalf("servers %+v", res.Servers)
	}
	if _, err := ts.Probe(ctx); apperr.KindOf(err) != apperr.KindTooMany {
		t.Fatalf("second probe: %v", err)
	}
	// Enabled now: blocked by the other server.
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = true; return nil })
	ts.evaluate(ctx, true)
	st := ts.Status()
	if st.State != StateBlocked || !slices.Equal(st.Blockers, []string{BlockerOtherServer}) || len(st.OtherServers) != 1 ||
		st.OtherServers[0].Source != SourceProbe || st.LastProbe == nil || st.LastProbe.Servers != 1 {
		t.Fatalf("status %+v", st)
	}
	// ignoreOtherServers serves anyway, with a health warning.
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.IgnoreOtherServers = true; return nil })
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateServing {
		t.Fatalf("ignore: %+v", st)
	}
	if h, _, _, _ := ts.Health(); h != "warn" {
		t.Fatalf("health while another server answers: %s", h)
	}
	// The router's DHCP is switched off: a clean probe lifts the blocker.
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.IgnoreOtherServers = false; a.DHCP.Enabled = false; return nil })
	ts.evaluate(ctx, true)
	ts.conn.onWrite = nil
	ts.clk.Add(11 * time.Second)
	if res, err := ts.Probe(ctx); err != nil || len(res.Servers) != 0 {
		t.Fatalf("clean probe %+v %v", res, err)
	}
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = true; return nil })
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateServing || len(st.OtherServers) != 0 {
		t.Fatalf("after a clean probe: %+v", st)
	}
}

// Every start of serving needs its own probe: a clean probe from before
// switching off does not let serving start again after another server
// appeared.
func TestRestartProbesAgain(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateServing {
		t.Fatalf("first start: %+v", st)
	}
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = false; return nil })
	ts.evaluate(ctx, true)
	// Another server appears; the earlier clean probe is less than 10
	// minutes old.
	ts.conn.onWrite = func(s sent) {
		m, err := parseMessage(s.b)
		if err != nil || m.op != opRequest || m.msgType() != msgDiscover {
			return
		}
		r := &req{op: opReply, htype: hwEther, hlen: hwEtherLen, xid: m.xid, mac: m.mac(), opts: [][]byte{{optMessageType, 1, msgOffer}}}
		r.addr(optServerID, "192.168.1.1")
		ts.handle4(r.bytes(), testIfIndex, netip.AddrPortFrom(ip("192.168.1.1"), serverPort))
	}
	ts.clk.Add(time.Minute)
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = true; return nil })
	ts.evaluate(ctx, true)
	st := ts.Status()
	if st.State != StateBlocked || !slices.Equal(st.Blockers, []string{BlockerOtherServer}) || len(st.OtherServers) != 1 {
		t.Fatalf("restart with another server: %+v", st)
	}
}

// Passive detection: a client's REQUEST naming another server blocks the
// start of serving (24 h), but not a running server (a health warning).
func TestPassiveDetection(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	ts.packet(reqFrom(msgRequest, macN(5)).addr(optRequestedIP, "192.168.1.20").addr(optServerID, "192.168.1.1"))
	ts.evaluate(ctx, true)
	st := ts.Status()
	if st.State != StateServing || len(st.OtherServers) != 1 || st.OtherServers[0].Source != SourceRequest || st.OtherServers[0].ServerID != "192.168.1.1" {
		t.Fatalf("serving: %+v", st)
	}
	if h, _, _, _ := ts.Health(); h != "warn" {
		t.Fatalf("health %s", h)
	}
	// After a restart of the gate it blocks.
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = false; return nil })
	ts.evaluate(ctx, true)
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = true; return nil })
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateBlocked || !slices.Equal(st.Blockers, []string{BlockerOtherServer}) {
		t.Fatalf("blocked: %+v", st)
	}
	ts.clk.Add(requestFresh + time.Minute)
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateServing {
		t.Fatalf("after 24 h: %+v", st)
	}
}

// A search started by the admin that finds no server lifts the blocker of
// earlier REQUESTs at once (the router's DHCP was just switched off);
// automatic probes do not.
func TestManualProbeClearsPassive(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	ctx := context.Background()
	ts.packet(reqFrom(msgRequest, macN(5)).addr(optRequestedIP, "192.168.1.20").addr(optServerID, "192.168.1.1"))
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = false; return nil })
	ts.evaluate(ctx, true)
	ts.set.Update(ctx, func(a *settings.All) error { a.DHCP.Enabled = true; return nil })
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateBlocked {
		t.Fatalf("blocked: %+v", st)
	}
	ts.clk.Add(time.Second)
	if res, err := ts.Probe(ctx); err != nil || len(res.Servers) != 0 {
		t.Fatalf("probe %+v %v", res, err)
	}
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateServing || len(st.OtherServers) != 0 {
		t.Fatalf("after a clean manual search: %+v", st)
	}
	// A later REQUEST naming the router counts again.
	ts.packet(reqFrom(msgRequest, macN(6)).addr(optRequestedIP, "192.168.1.21").addr(optServerID, "192.168.1.1"))
	if st := ts.Status(); len(st.OtherServers) != 1 {
		t.Fatalf("new REQUEST: %+v", st)
	}
}

// Leases survive a restart with their host names; a lease table from an
// edited database is sanitised.
func TestLoadSanitises(t *testing.T) {
	ts := newTestSvc(t, enabledDHCP)
	now := db.Ms(t0.Add(time.Hour))
	for _, row := range [][]any{
		{"02:00:00:00:00:01", "192.168.1.100", "ok", "", now, now},
		{"not-a-mac", "192.168.1.101", "", "", now, now},
		{"02:00:00:00:00:03", "fd00::1", "", "", now, now},
		{"02:00:00:00:00:04", "192.168.1.104", "Bad Name", "", now, now},
	} {
		if _, err := ts.d.W.Exec(`INSERT INTO dhcp_leases (mac, ip, hostname, client_id, expires_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`, row...); err != nil {
			t.Fatal(err)
		}
	}
	ts2 := openTestSvc(t, ts.d, nil)
	if l := ts2.Leases(); len(l) != 1 || l[0].Hostname != "ok" {
		t.Fatalf("%+v", l)
	}
}

// Until the first probe has run the server is blocked by other-server,
// but the health check does not warn about it.
func TestProbePending(t *testing.T) {
	ts := newTestSvc(t, nil)
	ctx := context.Background()
	ts.set.Update(ctx, func(a *settings.All) error { enabledDHCP(a); return nil })
	ts.evaluate(ctx, false)
	if st := ts.Status(); st.State != StateBlocked || !slices.Equal(st.Blockers, []string{BlockerOtherServer}) || len(st.OtherServers) != 0 {
		t.Fatalf("pending: %+v", st)
	}
	if h, msg, _, _ := ts.Health(); h != "ok" || msg == "" {
		t.Fatalf("health while pending: %s %q", h, msg)
	}
	ts.evaluate(ctx, true)
	if st := ts.Status(); st.State != StateServing {
		t.Fatalf("after the probe: %+v", st)
	}
}
