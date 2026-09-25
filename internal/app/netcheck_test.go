package app

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

const (
	macRouter = "3c:a6:2f:00:00:01"
	macLaptop = "aa:00:00:00:00:20"
	macPhone  = "aa:00:00:00:00:21"
	macTV     = "aa:00:00:00:00:22"
	macOld    = "aa:00:00:00:00:23"
	macSelf   = "dc:a6:32:00:00:10"
)

var netNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func pfx(s string) netip.Prefix { return netip.MustParsePrefix(s) }
func addr(s string) netip.Addr  { return netip.MustParseAddr(s) }

func neigh(ip, mac string) clients.Neighbour {
	return clients.Neighbour{IP: addr(ip), MAC: mac, Iface: "eth0"}
}

// fritzInputs is a home network behind a FRITZ!Box with IPv6: a laptop that
// uses PiCache, a phone and an old device that did not ask for two days, a
// TV that never did.
func fritzInputs() netInputs {
	refused := make([]dnsserver.RefusedSource, 25)
	for i := range refused {
		refused[i] = dnsserver.RefusedSource{Address: "2001:db8:1::" + string(rune('a'+i)), Count: 1, Last: netNow}
	}
	return netInputs{
		now:   netNow,
		since: netNow.Add(-48 * time.Hour),
		host: dnsserver.HostNetwork{Prefixes: []netip.Prefix{pfx("192.168.178.10/24"), pfx("fd00::10/64"),
			pfx("2001:db8:1::10/64"), pfx("fe80::10/64")}},
		gw4: addr("192.168.178.1"), gw6: addr("fe80::1"),
		routerName: "fritz.box", domain: "fritz.box",
		neighbours: []clients.Neighbour{
			neigh("192.168.178.1", macRouter), neigh("fe80::1", macRouter), neigh("fd00::1", macRouter),
			neigh("192.168.178.20", macLaptop), neigh("fd00::20", macLaptop), neigh("fe80::20", macLaptop),
			neigh("192.168.178.21", macPhone), neigh("192.168.178.22", macTV), neigh("192.168.178.23", macOld),
			neigh("192.168.178.10", macSelf), // this machine seen through a bridge
		},
		known: []clients.Known{
			{IP: "192.168.178.20", LastSeen: netNow.Add(-time.Hour), Queries: 5000},
			{IP: "192.168.178.21", LastSeen: netNow.Add(-50 * time.Hour), Queries: 70},
			{IP: "192.168.178.99", MAC: macOld, LastSeen: netNow.Add(-72 * time.Hour), Queries: 3}, // its address then
		},
		stats: []logs.ClientStat{
			{ClientIP: "192.168.178.20", Queries: 100, LastSeen: netNow.Add(-2 * time.Hour)},
			{ClientIP: "fd00::20", Queries: 50, LastSeen: netNow.Add(-10 * time.Minute)},
			{ClientIP: "192.168.178.1", Queries: 30, LastSeen: netNow.Add(-time.Hour)},
			{ClientIP: "127.0.0.1", Queries: 1000, LastSeen: netNow},
			{ClientIP: "192.168.178.10", Queries: 500, LastSeen: netNow},
		},
		statsOK: true,
		refused: refused,
		dnsIPv6: true,
		ownMACs: []string{macSelf},
		describe: func(ip netip.Addr, mac string) (int64, string, string) {
			switch {
			case mac == macLaptop:
				return 5, "Laptop", ""
			case ip == addr("192.168.178.21"):
				return 0, "", "phone.fritz.box"
			}
			return 0, "", ""
		},
	}
}

func checkByID(t *testing.T, nc api.NetworkCheck, id string) api.NetworkItem {
	t.Helper()
	for _, c := range nc.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", id, nc.Checks)
	return api.NetworkItem{}
}

func TestNetworkCheck(t *testing.T) {
	nc := computeNetworkCheck(fritzInputs())
	if nc.Mode != "host" || !nc.StatsAvailable || !nc.CheckedAt.Equal(netNow) {
		t.Fatalf("header %+v", nc)
	}
	r := nc.Router
	if r == nil || r.IPv4 != "192.168.178.1" || !slices.Equal(r.IPv6, []string{"fd00::1", "fe80::1"}) || r.MAC != macRouter ||
		r.Name != "fritz.box" || r.Kind != "fritzbox" {
		t.Fatalf("router %+v", r)
	}
	if s := nc.Self; !slices.Equal(s.IPv4, []string{"192.168.178.10"}) || !slices.Equal(s.ULA, []string{"fd00::10"}) ||
		!slices.Equal(s.Global, []string{"2001:db8:1::10"}) || !s.DNSIPv6 {
		t.Fatalf("self %+v", s)
	}
	if q := nc.Queries24h; q != (api.NetworkQueries{Total: 180, IPv4: 130, IPv6: 50, FromRouter: 30}) {
		t.Fatalf("queries %+v", q)
	}
	var ids []string
	for _, c := range nc.Checks {
		ids = append(ids, c.ID+"="+c.Status)
	}
	if want := []string{"router-forwarding=ok", "ipv6-dns=ok", "ipv6-address=ok", "refused=warn", "devices=info"}; !slices.Equal(ids, want) {
		t.Fatalf("checks %v, want %v", ids, want)
	}
	fwd := checkByID(t, nc, "router-forwarding").Data.(api.NetworkForwarding)
	if fwd.RouterQueries != 30 || fwd.TotalQueries != 180 || fwd.Share != 0.167 ||
		!slices.Equal(fwd.RouterAddresses, []string{"192.168.178.1", "fd00::1", "fe80::1"}) {
		t.Fatalf("forwarding %+v", fwd)
	}
	v6 := checkByID(t, nc, "ipv6-dns").Data.(api.NetworkIPv6DNS)
	if !v6.LANHasIPv6 || v6.IPv6Queries != 50 || v6.IPv6Clients != 1 || len(v6.ULA) != 1 || len(v6.Global) != 1 {
		t.Fatalf("ipv6-dns %+v", v6)
	}
	ref := checkByID(t, nc, "refused").Data.(api.NetworkRefused)
	if len(ref.Sources) != 20 || ref.Sources[0].Address != "2001:db8:1::a" || !ref.Since.Equal(netNow.Add(-48*time.Hour)) {
		t.Fatalf("refused %+v", ref)
	}
	if c := checkByID(t, nc, "devices").Data.(api.NetworkDeviceCounts); c != (api.NetworkDeviceCounts{Total: 4, Active: 1, Inactive: 2, Never: 1}) {
		t.Fatalf("device counts %+v", c)
	}

	// Devices: grouped by MAC, router and this machine left out, never →
	// inactive → active, then by address.
	var got []string
	for _, d := range nc.Devices {
		got = append(got, d.MAC+" "+d.Status+" "+strings.Join(d.IPs, ","))
	}
	want := []string{
		macTV + " never 192.168.178.22",
		macPhone + " inactive 192.168.178.21",
		macOld + " inactive 192.168.178.23",
		macLaptop + " active 192.168.178.20,fd00::20,fe80::20",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("devices\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	laptop, phone, old := nc.Devices[3], nc.Devices[1], nc.Devices[2]
	if laptop.Name != "Laptop" || laptop.ClientID != 5 || laptop.Queries24h != 150 || !laptop.LastQuery.Equal(netNow.Add(-10*time.Minute)) {
		t.Errorf("laptop %+v", laptop)
	}
	if phone.Name != "phone.fritz.box" || phone.ClientID != 0 || phone.Queries24h != 0 || !phone.LastQuery.Equal(netNow.Add(-50*time.Hour)) {
		t.Errorf("phone %+v", phone)
	}
	if !old.LastQuery.Equal(netNow.Add(-72*time.Hour)) || nc.Devices[0].Name != "" || !nc.Devices[0].LastQuery.IsZero() {
		t.Errorf("old %+v, tv %+v", old, nc.Devices[0])
	}
}

// router-forwarding: warn from 80 % of at least 200 queries, info from 20 %.
func TestNetworkForwardingThresholds(t *testing.T) {
	for _, tc := range []struct {
		router, other int64
		want          string
	}{
		{160, 40, "warn"},
		{159, 41, "info"},
		{199, 0, "ok"}, // too few queries to judge
		{200, 800, "info"},
		{199, 801, "ok"},
		{0, 0, "ok"},
	} {
		in := fritzInputs()
		in.stats = []logs.ClientStat{{ClientIP: "192.168.178.1", Queries: tc.router}, {ClientIP: "192.168.178.20", Queries: tc.other}}
		nc := computeNetworkCheck(in)
		c := checkByID(t, nc, "router-forwarding")
		if c.Status != tc.want {
			t.Errorf("%d of %d from the router: %s, want %s", tc.router, tc.router+tc.other, c.Status, tc.want)
		}
		st, msg, hint := networkHealth(nc)
		if tc.want == "warn" {
			if st != "warn" || msg != "Most DNS queries come from the router (192.168.178.1): PiCache cannot tell devices apart" ||
				!strings.Contains(hint, "DHCP") {
				t.Errorf("health %q %q %q", st, msg, hint)
			}
		} else if st != "ok" || msg != "" {
			t.Errorf("health %q %q", st, msg)
		}
	}
}

func TestNetworkRouterDetection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*netInputs)
		kind, ipv4 string
		ipv6       []string
	}{
		{"fritz.box by name", func(in *netInputs) { in.domain = "lan" }, "fritzbox", "192.168.178.1", []string{"fd00::1", "fe80::1"}},
		{"fritz.box by domain", func(in *netInputs) { in.routerName = "" }, "fritzbox", "192.168.178.1", []string{"fd00::1", "fe80::1"}},
		{"below fritz.box", func(in *netInputs) { in.routerName, in.domain = "Box.Fritz.Box.", "lan" }, "fritzbox", "192.168.178.1", nil},
		{"lookalike", func(in *netInputs) { in.routerName, in.domain = "notfritz.box", "home.arpa" }, "generic", "192.168.178.1", nil},
		{"no gateway", func(in *netInputs) { in.gw4, in.gw6 = netip.Addr{}, netip.Addr{} }, "unknown", "", []string{}},
		{"IPv6 gateway only", func(in *netInputs) { in.gw4, in.domain, in.routerName = netip.Addr{}, "lan", "" }, "generic", "",
			[]string{"fd00::1", "fe80::1"}},
		{"gateway is this machine", func(in *netInputs) { in.gw4, in.gw6 = addr("192.168.178.10"), netip.Addr{} }, "unknown", "", []string{}},
	} {
		in := fritzInputs()
		tc.mutate(&in)
		r := computeNetworkCheck(in).Router
		if r.Kind != tc.kind || r.IPv4 != tc.ipv4 || tc.ipv6 != nil && !slices.Equal(r.IPv6, tc.ipv6) {
			t.Errorf("%s: %+v", tc.name, r)
		}
	}
	// The router's addresses (by MAC) are not devices, and IPv6 queries it
	// forwards do not count as LAN IPv6 clients.
	in := fritzInputs()
	in.stats = []logs.ClientStat{{ClientIP: "fd00::1", Queries: 500}}
	nc := computeNetworkCheck(in)
	if v6 := checkByID(t, nc, "ipv6-dns"); v6.Status != "warn" || v6.Data.(api.NetworkIPv6DNS).IPv6Clients != 0 {
		t.Errorf("router IPv6 queries: %+v", v6)
	}
	if c := checkByID(t, nc, "router-forwarding"); c.Status != "warn" || c.Data.(api.NetworkForwarding).RouterAddresses[0] != "fd00::1" {
		t.Errorf("busiest router address first: %+v", c)
	}
	for _, d := range nc.Devices {
		if d.MAC == macRouter || d.MAC == macSelf {
			t.Errorf("device %+v", d)
		}
	}
}

func TestNetworkIPv6Checks(t *testing.T) {
	v4only := func(in *netInputs) {
		in.host.Prefixes = []netip.Prefix{pfx("192.168.178.10/24"), pfx("fe80::10/64")}
		in.neighbours = []clients.Neighbour{neigh("192.168.178.1", macRouter), neigh("fe80::1", macRouter), neigh("192.168.178.20", macLaptop)}
		in.stats = in.stats[:1]
	}
	for _, tc := range []struct {
		name          string
		mutate        func(*netInputs)
		dns, address  string
		lanHasIPv6    bool
		ipv6Queries   int64
		selfULA, glob int
	}{
		{"IPv4 only", v4only, "ok", "ok", false, 0, 0, 0},
		{"IPv6 neighbours, no IPv6 queries", func(in *netInputs) { in.stats = in.stats[:1] }, "warn", "ok", true, 0, 1, 1},
		{"no own IPv6 address", func(in *netInputs) {
			in.host.Prefixes = []netip.Prefix{pfx("192.168.178.10/24"), pfx("fe80::10/64")}
		}, "ok", "warn", true, 50, 0, 0},
		{"global address only", func(in *netInputs) {
			in.host.Prefixes = []netip.Prefix{pfx("192.168.178.10/24"), pfx("2001:db8:1::10/64")}
		}, "ok", "info", true, 50, 0, 1},
		{"own ULA only", func(in *netInputs) {
			v4only(in)
			in.host.Prefixes = append(in.host.Prefixes, pfx("fd00::10/64"))
		}, "warn", "ok", true, 0, 1, 0},
	} {
		in := fritzInputs()
		tc.mutate(&in)
		nc := computeNetworkCheck(in)
		d, a := checkByID(t, nc, "ipv6-dns"), checkByID(t, nc, "ipv6-address")
		data := d.Data.(api.NetworkIPv6DNS)
		if d.Status != tc.dns || a.Status != tc.address || data.LANHasIPv6 != tc.lanHasIPv6 || data.IPv6Queries != tc.ipv6Queries ||
			len(data.ULA) != tc.selfULA || len(data.Global) != tc.glob {
			t.Errorf("%s: ipv6-dns %s %+v, ipv6-address %s", tc.name, d.Status, data, a.Status)
		}
	}
}

// Without statistics (logs.db unavailable, anonymised addresses) the
// in-memory activity of the last 24 h stands in.
func TestNetworkWithoutStats(t *testing.T) {
	in := fritzInputs()
	in.stats, in.statsOK = nil, false
	nc := computeNetworkCheck(in)
	if nc.StatsAvailable || nc.Queries24h.Total != 5000 || nc.Queries24h.FromRouter != 0 {
		t.Fatalf("queries %+v", nc.Queries24h)
	}
	if d := nc.Devices[len(nc.Devices)-1]; d.MAC != macLaptop || d.Status != "active" || d.Queries24h != 5000 {
		t.Fatalf("laptop %+v", d)
	}
}

// In a container bridge network the bridge's gateway forwards every
// query: container-nat instead of router-forwarding, no device list.
func TestNetworkBridgeMode(t *testing.T) {
	in := fritzInputs()
	in.host = dnsserver.HostNetwork{Bridge: true, Prefixes: []netip.Prefix{pfx("172.17.0.2/16")}}
	in.gw4, in.gw6, in.routerName, in.domain = addr("172.17.0.1"), netip.Addr{}, "", "lan"
	in.neighbours = []clients.Neighbour{neigh("172.17.0.1", "02:42:00:00:00:01")}
	in.stats = []logs.ClientStat{{ClientIP: "172.17.0.1", Queries: 400}}
	nc := computeNetworkCheck(in)
	var ids []string
	for _, c := range nc.Checks {
		ids = append(ids, c.ID+"="+c.Status)
	}
	if nc.Mode != "bridge" || !slices.Equal(ids, []string{"container-nat=warn", "ipv6-dns=ok", "ipv6-address=ok", "refused=warn"}) {
		t.Fatalf("checks %v", ids)
	}
	if len(nc.Devices) != 0 || nc.Router.Kind != "unknown" || nc.Router.IPv4 != "" {
		t.Fatalf("devices %v, router %+v", nc.Devices, nc.Router)
	}
	st, msg, hint := networkHealth(nc)
	if st != "warn" || !strings.Contains(msg, "container network's gateway (172.17.0.1)") || !strings.Contains(hint, "host networking") {
		t.Fatalf("health %q %q %q", st, msg, hint)
	}
}

func TestScanTargets(t *testing.T) {
	count := func(ps ...string) (int, []netip.Addr) {
		var in []netip.Prefix
		for _, p := range ps {
			in = append(in, pfx(p))
		}
		got := scanTargets(in)
		return len(got), got
	}
	n, got := count("192.168.178.10/24")
	if n != 253 || got[0] != addr("192.168.178.1") || got[len(got)-1] != addr("192.168.178.254") || slices.Contains(got, addr("192.168.178.10")) {
		t.Fatalf("/24: %d %v … %v", n, got[0], got[len(got)-1])
	}
	// A /16 is scanned only in the /24 around this machine; .0 and .255 are
	// hosts there.
	if n, got := count("10.1.2.3/16"); n != 255 || got[0] != addr("10.1.2.0") || got[len(got)-1] != addr("10.1.2.255") {
		t.Fatalf("/16: %d", n)
	}
	if n, _ := count("192.168.1.10/25"); n != 125 {
		t.Fatalf("/25: %d", n)
	}
	// Public, IPv6, point-to-point and CGNAT subnets are not scanned.
	if n, _ := count("203.0.113.5/24", "fd00::10/64", "fe80::1/64", "192.168.9.1/31", "192.168.9.5/32", "100.64.0.1/24"); n != 0 {
		t.Fatalf("not private: %d", n)
	}
	// At most 512, own addresses of other subnets skipped, no duplicates.
	n, got = count("192.168.178.10/24", "10.1.2.3/16", "172.16.5.1/28", "192.168.178.11/24")
	if n != scanMaxAddrs || slices.Contains(got, addr("192.168.178.11")) || slices.Contains(got, addr("172.16.5.0")) {
		t.Fatalf("cap: %d", n)
	}
	seen := map[netip.Addr]bool{}
	for _, a := range got {
		if seen[a] {
			t.Fatalf("duplicate %v", a)
		}
		seen[a] = true
	}
}

// netEnv is a netChecker with fake sources and a settable clock.
type netEnv struct {
	n         *netChecker
	mu        sync.Mutex
	now       time.Time
	computed  atomic.Int32
	sent      chan []netip.Addr
	bridge    atomic.Bool
	cancelRun context.CancelFunc
	done      chan struct{}
}

func newNetEnv(t *testing.T, start bool) *netEnv {
	t.Helper()
	old := scanSettle
	scanSettle = 10 * time.Millisecond
	t.Cleanup(func() { scanSettle = old })
	e := &netEnv{now: netNow, sent: make(chan []netip.Addr, 4)}
	in := fritzInputs()
	e.n = newNetChecker(netSources{
		now:   func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now },
		since: in.since,
		host: func() dnsserver.HostNetwork {
			if e.bridge.Load() {
				return dnsserver.HostNetwork{Bridge: true, Prefixes: []netip.Prefix{pfx("172.17.0.2/16")}}
			}
			return in.host
		},
		gateway4: func() (netip.Addr, error) { return in.gw4, nil },
		gateway6: func() (netip.Addr, error) { return netip.Addr{}, errors.New("no IPv6 default route") },
		ptr:      func(context.Context, netip.Addr) (string, error) { return "fritz.box.", nil },
		domain:   func() string { return "lan" },
		neighbours: func(context.Context) ([]clients.Neighbour, error) {
			e.computed.Add(1)
			return in.neighbours, nil
		},
		known:         func(context.Context, time.Duration) ([]clients.Known, error) { return in.known, nil },
		describe:      in.describe,
		stats:         func(context.Context, time.Time, time.Time) ([]logs.ClientStat, bool) { return in.stats, true },
		refused:       func() []dnsserver.RefusedSource { return nil },
		dnsIPv6:       func() bool { return true },
		ownMACs:       func() []string { return in.ownMACs },
		scanSupported: true,
		send:          func(_ context.Context, addrs []netip.Addr) { e.sent <- addrs },
	}, slog.New(slog.DiscardHandler))
	if start {
		ctx, cancel := context.WithCancel(context.Background())
		e.cancelRun, e.done = cancel, make(chan struct{})
		go func() { e.n.Start(ctx); close(e.done) }()
		deadline := time.Now().Add(2 * time.Second)
		for {
			e.n.mu.Lock()
			ready := e.n.runCtx != nil
			e.n.mu.Unlock()
			if ready || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		t.Cleanup(func() { cancel(); <-e.done })
	}
	return e
}

func (e *netEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *netEnv) waitScanDone(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		e.n.mu.Lock()
		running := e.n.scan.Running
		e.n.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("scan did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestNetworkCheckCache(t *testing.T) {
	e := newNetEnv(t, true)
	ctx := context.Background()
	nc := e.n.Check(ctx)
	if e.computed.Load() != 1 || nc.Router.Name != "fritz.box" || nc.Router.Kind != "fritzbox" || nc.Scan.Running {
		t.Fatalf("first check: %d computations, %+v", e.computed.Load(), nc.Router)
	}
	e.advance(29 * time.Second)
	e.n.Check(ctx)
	if e.computed.Load() != 1 {
		t.Fatal("a check younger than 30 s must be served from the cache")
	}
	e.advance(2 * time.Second)
	e.n.Check(ctx)
	if e.computed.Load() != 2 {
		t.Fatal("a check older than 30 s must be computed again")
	}
	// A scan start invalidates the cache and shows in the check.
	n, err := e.n.Scan()
	if err != nil || n != 253 {
		t.Fatalf("scan: %d %v", n, err)
	}
	if addrs := <-e.sent; len(addrs) != 253 {
		t.Fatalf("sent to %d addresses", len(addrs))
	}
	e.waitScanDone(t)
	nc = e.n.Check(ctx)
	if e.computed.Load() != 3 || nc.Scan.Running || nc.Scan.Addresses != 253 || !nc.Scan.StartedAt.Equal(netNow.Add(31*time.Second)) ||
		nc.Scan.FinishedAt.IsZero() {
		t.Fatalf("after the scan: %d computations, %+v", e.computed.Load(), nc.Scan)
	}
	// A cancelled request does not leave a partial result in the cache.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	e.advance(time.Minute)
	e.n.Check(cctx)
	e.n.Check(ctx)
	if e.computed.Load() != 5 {
		t.Fatalf("%d computations", e.computed.Load())
	}
}

func TestNetworkScanLimits(t *testing.T) {
	// Before Start (and after the end) no scan starts.
	e := newNetEnv(t, false)
	if _, err := e.n.Scan(); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("not started: %v", err)
	}
	e = newNetEnv(t, true)
	e.n.src.scanSupported = false
	if _, err := e.n.Scan(); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("not Linux: %v", err)
	}
	e.n.src.scanSupported = true
	e.bridge.Store(true)
	if _, err := e.n.Scan(); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("bridge: %v", err)
	}
	e.bridge.Store(false)

	// 409 while running, 429 within 60 s of the start.
	block := make(chan struct{})
	e.n.src.send = func(context.Context, []netip.Addr) { <-block }
	if _, err := e.n.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.n.Scan(); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("running: %v", err)
	}
	if nc := e.n.Check(context.Background()); !nc.Scan.Running || nc.Scan.Addresses != 253 {
		t.Fatalf("running scan state %+v", nc.Scan)
	}
	close(block)
	e.waitScanDone(t)
	e.advance(59 * time.Second)
	if _, err := e.n.Scan(); apperr.KindOf(err) != apperr.KindTooMany {
		t.Fatalf("cooldown: %v", err)
	}
	e.advance(time.Second)
	e.n.src.send = func(context.Context, []netip.Addr) {}
	if _, err := e.n.Scan(); err != nil {
		t.Fatalf("after the cooldown: %v", err)
	}
	e.waitScanDone(t)
}
