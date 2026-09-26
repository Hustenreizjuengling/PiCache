package dnsserver

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// handleMsg runs m through the handler with a fake writer.
func (e *testEnv) handleMsg(w *fakeWriter, m *dns.Msg) {
	(&dnsHandler{s: e.srv, ctx: context.Background()}).ServeDNS(w, m)
}

// ednsQuery builds a query for name with the given EDNS options.
func ednsQuery(name string, qtype uint16, opts ...dns.EDNS0) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.SetEdns0(1232, false)
	m.IsEdns0().Option = append(m.IsEdns0().Option, opts...)
	return m
}

func ecsOpt(addr string, bits uint8) *dns.EDNS0_SUBNET {
	ip := net.ParseIP(addr)
	fam := uint16(1)
	if ip.To4() == nil {
		fam = 2
	}
	return &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: fam, SourceNetmask: bits, Address: ip}
}

func macOpt(b ...byte) *dns.EDNS0_LOCAL { return &dns.EDNS0_LOCAL{Code: ednsMACOption, Data: b} }

// captureLog records the server's log records.
type captureLog struct {
	mu   sync.Mutex
	msgs []string
}

func (c *captureLog) Enabled(context.Context, slog.Level) bool { return true }
func (c *captureLog) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool { b.WriteString(" " + a.String()); return true })
	c.msgs = append(c.msgs, b.String())
	return nil
}
func (c *captureLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureLog) WithGroup(string) slog.Handler      { return c }

func (c *captureLog) count(sub string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, m := range c.msgs {
		if strings.Contains(m, sub) {
			n++
		}
	}
	return n
}

// Blocked sources get no answer at all (UDP dropped before parsing, TCP
// closed); nothing is logged, recorded as seen or counted as refused.
func TestBlockedSources(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.BlockedClients = []string{"192.168.1.0/24", "10.1.2.3"} })
	for _, from := range []string{"192.168.1.77", "10.1.2.3"} {
		w := udpFrom(from)
		e.handle(w, "blocked.example", dns.TypeA)
		if len(w.msgs) != 0 {
			t.Errorf("%s got an answer", from)
		}
		tw := tcpFrom(from)
		e.handle(tw, "blocked.example", dns.TypeA)
		if len(tw.msgs) != 0 || !tw.closed {
			t.Errorf("%s over TCP: %d answers, closed %v", from, len(tw.msgs), tw.closed)
		}
	}
	w := udpFrom("192.168.2.1")
	e.handle(w, "allowed.example", dns.TypeA)
	if len(w.msgs) != 1 {
		t.Fatal("another client must be answered")
	}
	st := e.srv.Stats()
	if st.BlockedClients != 4 || st.Refused != 0 {
		t.Errorf("stats %+v", st)
	}
	if e.logs.count("blocked.example") != 0 || len(e.srv.RefusedSources()) != 0 {
		t.Error("a blocked client was logged or listed as refused")
	}
	e.cl.mu.Lock()
	seen := len(e.cl.seen)
	e.cl.mu.Unlock()
	if seen != 1 {
		t.Errorf("seen %d addresses, want only the allowed one", seen)
	}
	// The UDP reader drops blocked sources before parsing.
	if e.srv.admitUDP(netip.MustParseAddr("192.168.1.9")) || !e.srv.admitUDP(netip.MustParseAddr("192.168.2.9")) {
		t.Error("admitUDP")
	}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "x.example", ClientIP: "192.168.1.9"}, netip.Addr{})
	if err != nil || res.Status != StatusDropped || res.RCode != "" || !slices.Contains(res.Steps, "blocked client: 192.168.1.0/24") {
		t.Fatalf("lookup %+v %v", res, err)
	}
}

// A MAC entry blocks every address of a device (the identity's MAC from
// the neighbour table); an address derived from EDNS is checked too.
func TestBlockedIdentities(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.BlockedClients = []string{"aa:bb:cc:dd:ee:01", "192.168.50.0/24"}
		a.DNS.EDNSClientTrusted = []string{"192.168.1.1"}
	})
	phone6 := netip.MustParseAddr("fd00::77")
	e.cl.set(&clients.Identity{IP: phone6, MAC: "aa:bb:cc:dd:ee:01", GroupIDs: []int64{1}})
	w := udpFrom("fd00::77")
	e.handle(w, "x.example", dns.TypeA)
	if len(w.msgs) != 0 {
		t.Fatal("a blocked MAC was answered")
	}
	// Behind the trusted forwarder: the derived address and the EDNS MAC.
	for _, opts := range [][]dns.EDNS0{{ecsOpt("192.168.50.9", 32)}, {macOpt(0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0x01)}} {
		w := udpFrom("192.168.1.1")
		e.handleMsg(w, ednsQuery("y.example", dns.TypeA, opts...))
		if len(w.msgs) != 0 {
			t.Errorf("blocked identity behind the forwarder answered (%v)", opts)
		}
	}
	w = udpFrom("192.168.1.1")
	e.handleMsg(w, ednsQuery("z.example", dns.TypeA, ecsOpt("192.168.60.9", 32)))
	if len(w.msgs) != 1 {
		t.Error("another client behind the forwarder must be answered")
	}
	if n := e.srv.Stats().BlockedClients; n != 3 {
		t.Errorf("blocked %d, want 3", n)
	}
}

// Whatever the list says, loopback, this machine, the router (addresses
// and MAC), this machine's MACs and trusted forwarders are answered; each
// matching entry is logged at most once an hour.
func TestBlockedClientsSafetyNet(t *testing.T) {
	e := routerEnv(t, true, false)
	logs := &captureLog{}
	e.srv.log = slog.New(logs)
	e.update(func(a *settings.All) {
		a.DNS.RateLimitQPS = 0
		a.DNS.BlockedClients = []string{"127.0.0.0/8", "192.168.1.0/24", "3c:00:00:00:00:01", "aa:00:00:00:00:99", "fd00::/64"}
		a.DNS.EDNSClientTrusted = []string{"fd00::53"}
	})
	e.srv.host.Store(&hostInfo{own: []netip.Addr{testCacheIP}, macs: []string{"aa:00:00:00:00:99"}})
	e.cl.set(&clients.Identity{IP: netip.MustParseAddr("fd01::99"), MAC: "aa:00:00:00:00:99", GroupIDs: []int64{1}})
	e.cl.set(&clients.Identity{IP: netip.MustParseAddr("192.168.1.1"), MAC: "3c:00:00:00:00:01", GroupIDs: []int64{1}})
	for _, from := range []string{"127.0.0.1", "192.168.1.1", "192.168.1.10", "fd00::53", "fd01::99"} {
		for range 2 {
			w := udpFrom(from)
			e.handle(w, "safe.example", dns.TypeA)
			if len(w.msgs) != 1 {
				t.Errorf("%s was dropped", from)
			}
		}
	}
	w := udpFrom("192.168.1.77")
	e.handle(w, "x.example", dns.TypeA)
	if len(w.msgs) != 0 {
		t.Error("another address of the blocked network was answered")
	}
	if n := logs.count("a dns.blockedClients entry matches"); n != 5 {
		t.Errorf("%d warnings, want one per entry (5): %v", n, logs.msgs)
	}
	if got := e.srv.router.Load(); !slices.Equal(got.macs, []string{"3c:00:00:00:00:01"}) || !slices.Contains(got.protect, netip.MustParseAddr("192.168.1.1")) {
		t.Errorf("router state %+v", got)
	}
}

// A MAC entry never drops a protected client through the neighbour table:
// a trusted forwarder's own queries (and its other addresses, by its MAC)
// and a second gateway with its own MAC are answered; a MAC the trusted
// forwarder names in EDNS is its client's and stays blocked.
func TestBlockedMACsSafetyNet(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.RateLimitQPS = 0
		a.DNS.EDNSClientTrusted = []string{"192.168.1.5"}
		a.DNS.BlockedClients = []string{"aa:bb:cc:dd:ee:05", "3c:00:00:00:00:02", "aa:00:00:00:00:77"}
	})
	logs := &captureLog{}
	e.srv.log = slog.New(logs)
	e.srv.env.gateway = func() (netip.Addr, error) { return netip.MustParseAddr("192.168.1.1"), nil }
	e.srv.env.gateway6 = func() (netip.Addr, error) { return netip.MustParseAddr("fe80::2%eth0"), nil }
	nbs := []clients.Neighbour{
		{IP: netip.MustParseAddr("192.168.1.1"), MAC: "3c:00:00:00:00:01"},
		{IP: netip.MustParseAddr("fe80::2"), MAC: "3c:00:00:00:00:02"}, // a separate IPv6 router
		{IP: netip.MustParseAddr("fd00::2"), MAC: "3c:00:00:00:00:02"},
		{IP: netip.MustParseAddr("192.168.1.5"), MAC: "aa:bb:cc:dd:ee:05"}, // the trusted forwarder
		{IP: netip.MustParseAddr("fd00::5"), MAC: "aa:bb:cc:dd:ee:05"},
	}
	e.srv.env.neighbours = func(context.Context) ([]clients.Neighbour, error) { return nbs, nil }
	e.srv.detectRouter(context.Background())
	for _, nb := range nbs {
		e.cl.set(&clients.Identity{IP: nb.IP, MAC: nb.MAC, GroupIDs: []int64{1}})
	}
	answered := func(from string) bool {
		w := udpFrom(from)
		e.handle(w, "safe.example", dns.TypeA)
		return len(w.msgs) == 1
	}
	for _, from := range []string{"192.168.1.5", "fd00::5", "fd00::2"} {
		if !answered(from) {
			t.Errorf("%s was dropped", from)
		}
	}
	w := udpFrom("192.168.1.5")
	e.handleMsg(w, ednsQuery("kid.example", dns.TypeA, macOpt(0xaa, 0, 0, 0, 0, 0x77)))
	if len(w.msgs) != 0 {
		t.Error("a blocked MAC behind the trusted forwarder was answered")
	}
	if n := logs.count("a dns.blockedClients entry matches"); n != 2 {
		t.Errorf("%d warnings, want one per protected entry (2): %v", n, logs.msgs)
	}
	// Before its MAC is known the trusted address itself is still
	// protected (its identity's MAC is its own); its other address is not.
	st := *e.srv.router.Load()
	st.trustedMACs = nil
	e.srv.router.Store(&st)
	if !answered("192.168.1.5") || answered("fd00::5") {
		t.Error("without the forwarder's MAC only its trusted address is protected")
	}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "x.example", ClientIP: "192.168.1.5"}, netip.Addr{})
	if err != nil || res.Status == StatusDropped {
		t.Errorf("lookup as the trusted forwarder: %+v %v", res, err)
	}
}

// The router stays protected while it is detected again: its gateways from
// the start, its known addresses across a dns.routerResolver change and
// newly read ones already during the probe.
func TestRouterProtectedWhileDetecting(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.RateLimitQPS = 0
		a.DNS.BlockedClients = []string{"192.168.1.0/24"} // e.g. from a restored backup
	})
	gw := netip.MustParseAddr("192.168.1.1")
	e.srv.env.gateway = func() (netip.Addr, error) { return gw, nil }
	e.srv.router.Store(e.srv.startRouter(e.set.Get())) // as New does
	answered := func(from string) bool {
		w := udpFrom(from)
		e.handle(w, "x.example", dns.TypeA)
		return len(w.msgs) == 1
	}
	if !answered("192.168.1.1") {
		t.Error("the gateway was dropped before the first detection")
	}
	nbs := []clients.Neighbour{{IP: gw, MAC: "3c:00:00:00:00:01"}, {IP: netip.MustParseAddr("192.168.1.2"), MAC: "3c:00:00:00:00:01"}}
	e.srv.env.neighbours = func(context.Context) ([]clients.Neighbour, error) { return nbs, nil }
	e.srv.detectRouter(context.Background())
	e.update(func(a *settings.All) { a.DNS.RouterResolver = "auto" })
	if st := e.srv.router.Load(); st.mode != "auto" || !slices.Contains(st.protect, netip.MustParseAddr("192.168.1.2")) || !slices.Contains(st.macs, "3c:00:00:00:00:01") {
		t.Errorf("after a routerResolver change: %+v", st)
	}
	if !answered("192.168.1.2") {
		t.Error("the router's second address was dropped after a routerResolver change")
	}
	nbs = append(nbs, clients.Neighbour{IP: netip.MustParseAddr("192.168.1.3"), MAC: "3c:00:00:00:00:01"})
	probed := false
	e.up.onProbe = func() {
		probed = true
		if !slices.Contains(e.srv.router.Load().protect, netip.MustParseAddr("192.168.1.3")) {
			t.Error("a new router address is not protected during the probe")
		}
	}
	e.srv.detectRouter(context.Background())
	if !probed || !answered("192.168.1.3") || answered("192.168.1.77") {
		t.Errorf("after detection: probed %v", probed)
	}
}

func TestBlockedClientLockout(t *testing.T) {
	prot := []ProtectedClient{
		{Addr: netip.MustParseAddr("192.168.178.1"), What: "the router"},
		{Addr: netip.MustParseAddr("192.168.178.10"), What: "this machine's address"},
		{Addr: netip.MustParseAddr("172.17.0.1"), What: "the container network's gateway"},
		{Addr: netip.MustParseAddr("fd00::53"), What: "the trusted EDNS forwarder"},
		{MAC: "3c:00:00:00:00:01", What: "the router's MAC address"},
	}
	for entry, want := range map[string]string{
		"192.168.178.0/24":  "192.168.178.0/24 contains the router 192.168.178.1",
		"192.168.178.1":     "192.168.178.1 is the router",
		"192.168.178.10":    "192.168.178.10 is this machine's address",
		"172.16.0.0/12":     "172.16.0.0/12 contains the container network's gateway 172.17.0.1",
		"fd00::/64":         "fd00::/64 contains the trusted EDNS forwarder fd00::53",
		"127.0.0.0/8":       "127.0.0.0/8 contains loopback addresses (127.0.0.0/8)",
		"::/0":              "::/0 contains loopback addresses (::1/128)",
		"127.5.0.1":         "127.5.0.1 is a loopback address",
		"::1":               "::1 is a loopback address",
		"3c:00:00:00:00:01": "3c:00:00:00:00:01 is the router's MAC address",
		"192.168.178.77":    "",
		"aa:00:00:00:00:01": "",
		"10.0.0.0/8":        "",
	} {
		if got := BlockedClientLockout(entry, prot); got != want {
			t.Errorf("BlockedClientLockout(%s) = %q, want %q", entry, got, want)
		}
	}
	e := newEnv(t, nil)
	e.srv.router.Store(&routerState{mode: "off", protect: []netip.Addr{netip.MustParseAddr("192.168.1.1")},
		macs: []string{"3c:00:00:00:00:01", "3c:00:00:00:00:02"}})
	macOf := func(ip netip.Addr) (string, bool) {
		return "AA:00:00:00:00:53", ip == netip.MustParseAddr("192.168.1.53")
	}
	prot = e.srv.ProtectedClients([]string{"192.168.1.53/32"}, macOf)
	var sawRouter, sawTrusted, sawLoop bool
	for _, p := range prot {
		switch {
		case p.Addr == netip.MustParseAddr("192.168.1.1") && p.What == "the router":
			sawRouter = true
		case p.Addr == netip.MustParseAddr("192.168.1.53"):
			sawTrusted = true
		case p.Addr == netip.MustParseAddr("127.0.0.1"):
			sawLoop = true
		}
	}
	if !sawRouter || !sawTrusted || !sawLoop {
		t.Errorf("protected clients: router %v trusted %v loopback %v", sawRouter, sawTrusted, sawLoop)
	}
	// MACs: every gateway's and the trusted forwarder's (from the neighbour table).
	for entry, want := range map[string]string{
		"3c:00:00:00:00:02": "3c:00:00:00:00:02 is the router's MAC address",
		"aa:00:00:00:00:53": "aa:00:00:00:00:53 is the MAC address of the trusted EDNS forwarder 192.168.1.53",
	} {
		if got := BlockedClientLockout(entry, prot); got != want {
			t.Errorf("BlockedClientLockout(%s) = %q, want %q", entry, got, want)
		}
	}
}

func TestEDNSClientIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []dns.EDNS0
		addr string
		mac  string
	}{
		{"ECS /32", []dns.EDNS0{ecsOpt("192.168.5.9", 32)}, "192.168.5.9", ""},
		{"ECS /128", []dns.EDNS0{ecsOpt("2001:db8::9", 128)}, "2001:db8::9", ""},
		{"mapped /128", []dns.EDNS0{&dns.EDNS0_SUBNET{Code: 8, Family: 2, SourceNetmask: 128, Address: net.ParseIP("::ffff:192.168.5.9").To16()}}, "192.168.5.9", ""},
		{"ECS /24 is no address", []dns.EDNS0{ecsOpt("192.168.5.0", 24)}, "", ""},
		{"two ECS options", []dns.EDNS0{ecsOpt("192.168.5.9", 32), ecsOpt("192.168.5.10", 32)}, "", ""},
		{"loopback", []dns.EDNS0{ecsOpt("127.0.0.1", 32)}, "", ""},
		{"unspecified", []dns.EDNS0{ecsOpt("0.0.0.0", 32)}, "", ""},
		{"multicast", []dns.EDNS0{ecsOpt("224.0.0.1", 32)}, "", ""},
		{"broadcast", []dns.EDNS0{ecsOpt("255.255.255.255", 32)}, "", ""},
		{"link-local is unicast", []dns.EDNS0{ecsOpt("fe80::1", 128)}, "fe80::1", ""},
		{"MAC", []dns.EDNS0{macOpt(0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF)}, "", "aa:bb:cc:dd:ee:ff"},
		{"MAC and ECS", []dns.EDNS0{ecsOpt("192.168.5.9", 32), macOpt(2, 0, 0, 0, 0, 1)}, "192.168.5.9", "02:00:00:00:00:01"},
		{"two MACs", []dns.EDNS0{macOpt(2, 0, 0, 0, 0, 1), macOpt(2, 0, 0, 0, 0, 2)}, "", ""},
		{"short MAC", []dns.EDNS0{macOpt(2, 0, 0, 0, 1)}, "", ""},
		{"long MAC", []dns.EDNS0{macOpt(2, 0, 0, 0, 0, 1, 7)}, "", ""},
		{"zero MAC", []dns.EDNS0{macOpt(0, 0, 0, 0, 0, 0)}, "", ""},
		{"group MAC", []dns.EDNS0{macOpt(1, 0, 0x5e, 0, 0, 1)}, "", ""},
		{"text MAC option ignored", []dns.EDNS0{&dns.EDNS0_LOCAL{Code: 65073, Data: []byte("aa:bb:cc:dd:ee:ff")}}, "", ""},
	} {
		addr, mac := ednsClientIdentity(tc.opts)
		got := ""
		if addr.IsValid() {
			got = addr.String()
		}
		if got != tc.addr || mac != tc.mac {
			t.Errorf("%s: %q %q, want %q %q", tc.name, got, mac, tc.addr, tc.mac)
		}
	}
	for _, tc := range []struct {
		opts []dns.EDNS0
		want string
	}{
		{[]dns.EDNS0{ecsOpt("203.0.113.77", 24)}, "203.0.113.0/24"},
		{[]dns.EDNS0{ecsOpt("2001:db8:1:2::", 56)}, "2001:db8:1::/56"},
		{[]dns.EDNS0{ecsOpt("203.0.113.77", 24), ecsOpt("198.51.100.1", 24)}, "203.0.113.0/24"},
		{[]dns.EDNS0{&dns.EDNS0_SUBNET{Code: 8, Family: 0}}, ""},
		{nil, ""},
	} {
		if got := clientSubnet(ednsQuery("x.example", dns.TypeA, tc.opts...)); got != tc.want {
			t.Errorf("clientSubnet(%v) = %q, want %q", tc.opts, got, tc.want)
		}
	}
}

// Only trusted sources may name their client: the derived address is the
// identity (groups, query log, seen), the source stays the transport
// address (ACL, rate limit, loop guard, this server's names).
func TestEDNSTrustedForwarder(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.EDNSClientTrusted = []string{"192.168.1.1", "192.168.1.2"}
		a.DNS.RouterResolver = "192.168.1.1"
		a.DNS.RateLimitQPS, a.DNS.RateLimitBurst = 1, 1
	})
	kid := netip.MustParseAddr("192.168.1.42")
	e.cl.set(&clients.Identity{IP: kid, Name: "kid", GroupIDs: []int64{7}})
	e.cl.byMAC = map[string]*clients.Identity{"aa:00:00:00:00:42": {MAC: "aa:00:00:00:00:42", Name: "tablet", GroupIDs: []int64{8}}}
	w := udpFrom("192.168.1.1")
	e.handleMsg(w, ednsQuery("kid.example", dns.TypeA, ecsOpt("192.168.1.42", 32)))
	ev := e.logs.waitEvent(t, "kid.example", 0)
	if ev.ClientIP != "192.168.1.42" || ev.ClientName != "kid" || ev.ECS != "192.168.1.42/32" {
		t.Errorf("logged %+v", ev)
	}
	e.cl.mu.Lock()
	seen := e.cl.seen[kid]
	e.cl.mu.Unlock()
	if seen != 1 {
		t.Errorf("the derived address was seen %d times", seen)
	}
	// MAC only: the identity of the MAC, the client address stays the source.
	e.handleMsg(udpFrom("192.168.1.1"), ednsQuery("tablet.example", dns.TypeA, macOpt(0xaa, 0, 0, 0, 0, 0x42)))
	if ev := e.logs.waitEvent(t, "tablet.example", 0); ev.ClientIP != "192.168.1.1" || ev.ClientName != "tablet" {
		t.Errorf("MAC only: %+v", ev)
	}
	// An untrusted source cannot claim an identity.
	e.handleMsg(udpFrom("192.168.1.7"), ednsQuery("spoof.example", dns.TypeA, ecsOpt("192.168.1.42", 32)))
	if ev := e.logs.waitEvent(t, "spoof.example", 0); ev.ClientIP != "192.168.1.7" {
		t.Errorf("untrusted source derived an identity: %+v", ev)
	}
	// The loop guard uses the source: the router forwarding a name back.
	w = udpFrom("192.168.1.1")
	e.handleMsg(w, ednsQuery("5.1.168.192.in-addr.arpa", dns.TypePTR, ecsOpt("192.168.1.42", 32)))
	if len(w.msgs) != 1 || w.msgs[0].Rcode != dns.RcodeServerFailure {
		t.Errorf("loop guard: %v", w.msgs)
	}
	// A derived loopback or own address gets no loopback-only answers.
	for _, derived := range []string{"127.0.0.1", testCacheIP.String()} {
		w = udpFrom("192.168.1.1")
		e.handleMsg(w, ednsQuery("picache", dns.TypeA, ecsOpt(derived, 32)))
		if len(w.msgs) != 1 || slices.Contains(answerIPs(w.msgs[0].Answer), "127.0.0.1") {
			t.Errorf("derived %s: %v", derived, w.msgs)
		}
	}
	// Trusted sources are exempt from the rate limit (burst 1).
	for range 5 {
		w := udpFrom("192.168.1.2")
		e.handleMsg(w, ednsQuery("many.example", dns.TypeA, ecsOpt("192.168.1.43", 32)))
		if len(w.msgs) != 1 {
			t.Fatal("the trusted forwarder was rate limited")
		}
	}
}

// ECS sent to the default set: "client" = the source's /24 or /56 when it
// is public, "custom" = the configured subnet; never for private sources.
func TestECSModes(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.ECS = settings.ECS{Mode: settings.ECSClient}
		a.DNS.AllowAllNetworks = true
	})
	for _, tc := range []struct{ from, want string }{
		{"9.9.9.77", "9.9.9.0/24"},
		{"2a00:1450:4001:81c::77", "2a00:1450:4001:800::/56"},
		{"192.168.1.5", ""},
		{"fd00::5", ""},
	} {
		e.handle(udpFrom(tc.from), "ecs-"+strings.ReplaceAll(strings.ReplaceAll(tc.from, ".", "-"), ":", "-")+".example", dns.TypeA)
		c := e.up.last()
		got := ""
		if c.ecs.IsValid() {
			got = c.ecs.String()
		}
		if got != tc.want {
			t.Errorf("from %s: subnet %q, want %q", tc.from, got, tc.want)
		}
	}
	e.update(func(a *settings.All) {
		a.DNS.ECS = settings.ECS{Mode: settings.ECSCustom, CustomSubnet: "198.18.0.0/15"}
	})
	e.handle(udpFrom("192.168.1.5"), "custom-private.example", dns.TypeA)
	if c := e.up.last(); c.ecs.IsValid() {
		t.Errorf("a non-public custom subnet was sent: %v", c.ecs)
	}
	e.update(func(a *settings.All) { a.DNS.ECS = settings.ECS{Mode: settings.ECSCustom, CustomSubnet: "8.8.0.0/16"} })
	e.handle(udpFrom("192.168.1.5"), "custom.example", dns.TypeA)
	if c := e.up.last(); c.ecs != netip.MustParsePrefix("8.8.0.0/16") {
		t.Errorf("custom subnet %v", c.ecs)
	}
}

// Dropped domains (subtree, optionally one type) get no answer: nothing is
// sent, nothing logged; TCP connections are closed; Lookup traces them.
func TestDroppedDomains(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.DroppedDomains = []string{"telemetry.example", "noisy.example:AAAA"} })
	for _, tc := range []struct {
		name    string
		qtype   uint16
		dropped bool
	}{
		{"telemetry.example", dns.TypeA, true},
		{"a.b.telemetry.example", dns.TypeTXT, true},
		{"noisy.example", dns.TypeAAAA, true},
		{"x.noisy.example", dns.TypeAAAA, true},
		{"noisy.example", dns.TypeA, false},
		{"other.example", dns.TypeA, false},
	} {
		w := udpFrom("192.168.1.5")
		e.handle(w, tc.name, tc.qtype)
		if got := len(w.msgs) == 0; got != tc.dropped {
			t.Errorf("%s %s: dropped %v", tc.name, dns.TypeToString[tc.qtype], got)
		}
	}
	tw := tcpFrom("192.168.1.5")
	e.handle(tw, "telemetry.example", dns.TypeA)
	if !tw.closed || len(tw.msgs) != 0 {
		t.Error("TCP connection not closed")
	}
	if n := e.srv.Stats().Dropped; n != 5 {
		t.Errorf("dropped %d, want 5", n)
	}
	if len(e.up.callsFor("telemetry.example")) != 0 || e.logs.count("telemetry.example") != 0 {
		t.Error("a dropped query was forwarded or logged")
	}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "x.noisy.example", Type: "AAAA"}, netip.MustParseAddr("192.168.1.5"))
	if err != nil || res.Status != StatusDropped || !slices.Contains(res.Steps, "dropped by dns.droppedDomains (noisy.example:AAAA)") {
		t.Fatalf("lookup %+v %v", res, err)
	}
	// Local records and special-use names never get here.
	e.addRecord("local.telemetry.example", "A", "192.168.1.9")
	w := udpFrom("192.168.1.5")
	e.handle(w, "local.telemetry.example", dns.TypeA)
	if len(w.msgs) != 1 {
		t.Error("a local record was dropped")
	}
}

// FuzzClientOPT feeds arbitrary OPT data (a query from a trusted forwarder)
// through the DNS library's unpacking and the client option parsers.
func FuzzClientOPT(f *testing.F) {
	f.Add([]byte{0, 8, 0, 8, 0, 1, 32, 0, 192, 168, 1, 5})
	f.Add([]byte{0xfd, 0xe9, 0, 6, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff})
	f.Add([]byte{0, 8, 0, 20, 0, 2, 128, 0, 0x20, 1, 0xd, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1})
	f.Fuzz(func(t *testing.T, rdata []byte) {
		if len(rdata) > 4096 {
			return
		}
		q := new(dns.Msg)
		q.SetQuestion("x.example.", dns.TypeA)
		wire, err := q.Pack()
		if err != nil {
			t.Fatal(err)
		}
		wire[11]++ // ARCOUNT
		wire = append(append(wire, 0, 0, 41, 0x04, 0xd0, 0, 0, 0, 0, byte(len(rdata)>>8), byte(len(rdata))), rdata...)
		var m dns.Msg
		if m.Unpack(wire) != nil || m.IsEdns0() == nil {
			return
		}
		addr, mac := ednsClientIdentity(m.IsEdns0().Option)
		if addr.IsValid() && (addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() || addr.Is4In6() || addr.Zone() != "") {
			t.Fatalf("unusable address %v", addr)
		}
		if mac != "" {
			if hw, err := net.ParseMAC(mac); err != nil || len(hw) != 6 || hw[0]&1 != 0 || hw.String() != mac {
				t.Fatalf("bad MAC %q", mac)
			}
		}
		if s := clientSubnet(&m); s != "" {
			if p, err := netip.ParsePrefix(s); err != nil || p.Masked() != p {
				t.Fatalf("bad subnet %q", s)
			}
		}
	})
}

// dns.rateLimitIpv4Prefix/Ipv6Prefix: public sources of one network share a
// bucket (the key is logged and listed), LAN sources keep their own.
func TestRateLimitPrefixes(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.AllowAllNetworks = true
		a.DNS.RateLimitQPS, a.DNS.RateLimitBurst = 1, 1
		a.DNS.RateLimitIPv4Prefix = 24
	})
	w1, w2 := udpFrom("9.9.9.1"), udpFrom("9.9.9.2")
	e.handle(w1, "a.example", dns.TypeA)
	e.handle(w2, "b.example", dns.TypeA)
	if len(w1.msgs) != 1 || len(w2.msgs) != 0 {
		t.Errorf("one public /24 must share a bucket: %d / %d replies", len(w1.msgs), len(w2.msgs))
	}
	if top := e.srv.Stats().TopRateLimited; len(top) != 1 || top[0].Client != "9.9.9.0/24" {
		t.Errorf("top %+v", top)
	}
	w3, w4 := udpFrom("192.168.1.2"), udpFrom("192.168.1.3")
	e.handle(w3, "c.example", dns.TypeA)
	e.handle(w4, "d.example", dns.TypeA)
	if len(w3.msgs) != 1 || len(w4.msgs) != 1 {
		t.Error("LAN sources keep their own bucket")
	}
}
