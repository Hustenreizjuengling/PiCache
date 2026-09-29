package app

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// The first start of a fresh installation stores onboardingDone false (the
// checklist shows), PICACHE_INITIAL_CONFIG (applied after it) can set it,
// and a document of an earlier version (without the member) keeps true.
func TestOnboardingFirstStart(t *testing.T) {
	ctx := context.Background()
	a := syncApp(t)
	if !a.set.Created() {
		t.Fatal("test setup: the settings document was not created")
	}
	a.applyDetectedDefaults(ctx)
	if a.set.Get().Web.OnboardingDone {
		t.Fatal("the first start did not store onboardingDone false")
	}

	b := syncApp(t)
	b.applyDetectedDefaults(ctx)
	p := filepath.Join(t.TempDir(), "initial.json")
	if err := os.WriteFile(p, []byte(`{"web":{"onboardingDone":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b.cfg.InitialConfig = p
	if err := b.applyInitialConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if !b.set.Get().Web.OnboardingDone {
		t.Fatal("PICACHE_INITIAL_CONFIG could not hide the checklist")
	}

	// A 0.15 document (no member) keeps true: no checklist on upgrades.
	path := filepath.Join(t.TempDir(), "picache.db")
	make014DB(t, path, false)
	r := newRestoreApp(t)
	r.paths.ConfigDB = path
	openLive(t, r)
	if err := MigrateConfigDB(ctx, r.cdb); err != nil {
		t.Fatal(err)
	}
	set, err := settings.Open(ctx, r.cdb, r.log)
	if err != nil {
		t.Fatal(err)
	}
	if set.Created() || !set.Get().Web.OnboardingDone {
		t.Fatalf("an upgraded document: created %v, onboardingDone %v", set.Created(), set.Get().Web.OnboardingDone)
	}
}

// The support bundle keeps web.onboardingDone as it is.
func TestBundleRuleOnboardingDone(t *testing.T) {
	if settingRules["web.onboardingDone"] != ruleKeep {
		t.Fatal("web.onboardingDone must be kept in the support bundle")
	}
}

func TestIPv4Dynamic(t *testing.T) {
	routes := []netutil.Route{
		{Prefix: pfx("::/0"), Iface: "eth1", Metric: 1},
		{Prefix: pfx("0.0.0.0/0"), Iface: "eth0", Gateway: addr("192.168.1.1"), Metric: 100},
		{Prefix: pfx("0.0.0.0/0"), Iface: "wlan0", Gateway: addr("192.168.2.1"), Metric: 600},
	}
	addrs := []netutil.HostAddr{
		{Iface: "lo", Prefix: pfx("127.0.0.1/8"), Loopback: true},
		{Iface: "eth0", Prefix: pfx("192.168.1.10/24")},
		{Iface: "eth0", Prefix: pfx("fd00::10/64"), Dynamic: true},
		{Iface: "wlan0", Prefix: pfx("192.168.2.10/24"), Dynamic: true},
	}
	if d, ok := ipv4Dynamic(routes, addrs); !ok || d {
		t.Fatalf("static eth0: %v %v", d, ok)
	}
	addrs = append(addrs, netutil.HostAddr{Iface: "eth0", Prefix: pfx("192.168.1.11/24"), Dynamic: true})
	if d, ok := ipv4Dynamic(routes, addrs); !ok || !d {
		t.Fatalf("a DHCP address on eth0: %v %v", d, ok)
	}
	if _, ok := ipv4Dynamic(routes[:1], addrs); ok {
		t.Fatal("no IPv4 default route must be unknown")
	}
	if _, ok := ipv4Dynamic(routes, addrs[:1]); ok {
		t.Fatal("no IPv4 address on the route's interface must be unknown")
	}
}

// The addresses of the interfaces of the IPv4 and the IPv6 default route
// (the lowest metric each).
func TestPrimaryAddrs(t *testing.T) {
	routes := []netutil.Route{
		{Prefix: pfx("0.0.0.0/0"), Iface: "eth0", Gateway: addr("192.168.1.1"), Metric: 100},
		{Prefix: pfx("0.0.0.0/0"), Iface: "wlan0", Gateway: addr("192.168.2.1"), Metric: 600},
		{Prefix: pfx("::/0"), Iface: "eth1", Metric: 100},
	}
	addrs := []netutil.HostAddr{
		{Iface: "eth0", Prefix: pfx("192.168.1.10/24")},
		{Iface: "wlan0", Prefix: pfx("192.168.2.10/24")},
		{Iface: "tailscale0", Prefix: pfx("100.101.5.7/32")},
		{Iface: "eth1", Prefix: pfx("fd00::10/64")},
		{Iface: "eth0", Prefix: pfx("fd00::11/64")},
	}
	got := primaryAddrs(routes, addrs)
	if want := []netip.Addr{addr("192.168.1.10"), addr("fd00::10"), addr("fd00::11")}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := primaryAddrs(nil, addrs); len(got) != 0 {
		t.Fatalf("no default route: %v", got)
	}
}

// self lists the addresses of the default routes' interfaces first (the
// router steps, the checklist and the device guides tell LAN devices to use
// the first IPv4 address and ULA) and leaves Tailscale's addresses out
// unless they are on such an interface; they stay this machine's own.
func TestNetworkSelfPrimaryFirst(t *testing.T) {
	in := fritzInputs()
	in.host.Prefixes = append([]netip.Prefix{pfx("100.101.5.7/32"), pfx("fd7a:115c:a1e0::1234/128"),
		pfx("10.0.0.5/24"), pfx("fd00:aaaa::5/64")}, in.host.Prefixes...)
	in.primary = map[netip.Addr]bool{addr("192.168.178.10"): true, addr("fd00::10"): true, addr("2001:db8:1::10"): true}
	s, own := in.self()
	if !slices.Equal(s.IPv4, []string{"192.168.178.10", "10.0.0.5"}) || !slices.Equal(s.ULA, []string{"fd00::10", "fd00:aaaa::5"}) {
		t.Fatalf("default route first: %v %v", s.IPv4, s.ULA)
	}
	if !own[addr("100.101.5.7")] || !own[addr("fd7a:115c:a1e0::1234")] {
		t.Fatal("the tailnet addresses are still this machine's")
	}
	in.primary = nil // unknown: by value, still without the tailnet
	if s, _ = in.self(); !slices.Equal(s.IPv4, []string{"10.0.0.5", "192.168.178.10"}) || !slices.Equal(s.ULA, []string{"fd00::10", "fd00:aaaa::5"}) {
		t.Fatalf("no default route: %v %v", s.IPv4, s.ULA)
	}
	in.primary = map[netip.Addr]bool{addr("100.101.5.7"): true} // a LAN in CGNAT space
	if s, _ = in.self(); !slices.Equal(s.IPv4, []string{"100.101.5.7", "10.0.0.5", "192.168.178.10"}) {
		t.Fatalf("CGNAT LAN: %v", s.IPv4)
	}
	// The live check passes them on: the laptop guide names the address of
	// the default route's interface.
	e := newNetEnv(t, false)
	e.n.src.primary = func() []netip.Addr { return []netip.Addr{addr("fd00::10")} }
	if nc := e.n.Check(context.Background(), netip.Addr{}); nc.Self.ULA[0] != "fd00::10" {
		t.Fatalf("check: %v", nc.Self.ULA)
	}
}

// A dual-stack device that opens the web UI over IPv4 but asks over IPv6
// (the router announces PiCache's ULA) has asked PiCache: the requester
// counts every address of its device, as the device list does; without a
// device only the address itself counts.
func TestRequesterCountsTheWholeDevice(t *testing.T) {
	in := fritzInputs()
	in.known = nil
	in.stats = slices.DeleteFunc(in.stats, func(s logs.ClientStat) bool { return s.ClientIP == "192.168.178.20" })
	nc, st := computeNetworkState(in)
	r := requesterOf(addr("192.168.178.20"), nc.Devices, st, false)
	if r.MAC != macLaptop || r.Queries24h == nil || *r.Queries24h != 50 || !r.LastQuery.Equal(netNow.Add(-10*time.Minute)) {
		t.Fatalf("IPv4 requester of a device that asks over IPv6: %+v", r)
	}
	r = requesterOf(addr("192.168.178.20"), nil, st, false)
	if r.MAC != "" || r.Queries24h == nil || *r.Queries24h != 0 || !r.LastQuery.IsZero() {
		t.Fatalf("no device: %+v", r)
	}
}

// self.dynamic4 follows its source; it is absent in a bridge network and
// when the source cannot tell.
func TestNetworkCheckDynamic4(t *testing.T) {
	e := newNetEnv(t, false)
	val, known := true, true
	e.n.src.dynamic4 = func() (bool, bool) { return val, known }
	check := func() *bool {
		e.advance(time.Minute) // past the cache
		return e.n.Check(context.Background(), netip.Addr{}).Self.Dynamic4
	}
	if d := check(); d == nil || !*d {
		t.Fatalf("dynamic: %v", d)
	}
	val = false
	if d := check(); d == nil || *d {
		t.Fatalf("static: %v", d)
	}
	known = false
	if d := check(); d != nil {
		t.Fatalf("unknown: %v", *d)
	}
	known, val = true, true
	e.bridge.Store(true)
	if d := check(); d != nil {
		t.Fatalf("bridge: %v", *d)
	}
	e.bridge.Store(false)
	e.advance(time.Minute)
	b, err := json.Marshal(e.n.Check(context.Background(), netip.Addr{}).Self)
	if err != nil || !strings.Contains(string(b), `"dynamic4":true`) {
		t.Fatalf("JSON %s %v", b, err)
	}
}

// The requester is described for every request from the check's state
// (two clients differ within the cache lifetime): local for loopback and
// this machine, the device of its address with the activity of all its
// addresses (the laptop: 100 queries over IPv4, 50 over its ULA, the last
// 10 minutes ago), else the activity of the address, no counts while
// client addresses are anonymised.
func TestNetworkCheckRequester(t *testing.T) {
	e := newNetEnv(t, false)
	anon := false
	e.n.src.anonymized = func() bool { return anon }
	ctx := context.Background()
	get := func(ip string) *api.NetworkRequester {
		t.Helper()
		nc := e.n.Check(ctx, addr(ip))
		if nc.Requester == nil {
			t.Fatalf("%s: no requester", ip)
		}
		return nc.Requester
	}
	laptop := get("192.168.178.20")
	if laptop.Address != "192.168.178.20" || laptop.Local || laptop.MAC != macLaptop || laptop.Name != "Laptop" ||
		laptop.ClientID != 5 || laptop.Queries24h == nil || *laptop.Queries24h != 150 ||
		!laptop.LastQuery.Equal(netNow.Add(-10*time.Minute)) || laptop.Privacy {
		t.Fatalf("laptop %+v", laptop)
	}
	other := get("192.168.178.77")
	if other.Address != "192.168.178.77" || other.Local || other.MAC != "" || other.Name != "" || other.ClientID != 0 ||
		other.Queries24h == nil || *other.Queries24h != 0 || !other.LastQuery.IsZero() {
		t.Fatalf("unknown device %+v", other)
	}
	if e.computed.Load() != 1 {
		t.Fatalf("the requester must come from the cached check: %d computations", e.computed.Load())
	}
	for _, ip := range []string{"127.0.0.1", "::1", "192.168.178.10", "fd00::10", "::ffff:192.168.178.10"} {
		if r := get(ip); !r.Local {
			t.Errorf("%s is not local: %+v", ip, r)
		}
	}
	if r := get("::ffff:192.168.178.20"); r.Address != "192.168.178.20" || r.MAC != macLaptop {
		t.Fatalf("a mapped address: %+v", r)
	}
	if nc := e.n.Check(ctx, netip.Addr{}); nc.Requester != nil {
		t.Fatalf("no client, requester %+v", nc.Requester)
	}
	anon = true
	r := get("192.168.178.20")
	if !r.Privacy || r.Queries24h != nil || !r.LastQuery.IsZero() || r.MAC != macLaptop {
		t.Fatalf("anonymised %+v", r)
	}
	b, err := json.Marshal(r)
	if err != nil || string(b) != `{"address":"192.168.178.20","local":false,"mac":"`+macLaptop+`","name":"Laptop","clientId":5,"privacy":true}` {
		t.Fatalf("JSON %s %v", b, err)
	}
	anon = false
	b, err = json.Marshal(get("192.168.178.77"))
	if err != nil || string(b) != `{"address":"192.168.178.77","local":false,"queries24h":0}` {
		t.Fatalf("JSON %s %v", b, err)
	}
	if e.computed.Load() != 1 {
		t.Fatalf("%d computations", e.computed.Load())
	}
}

// Package mode: the overview names the .deb of the available release, and
// queueing an update answers 409 (the host is updated with apt).
func TestUpdaterPackageMode(t *testing.T) {
	f := &fakeReleases{rel: &update.Release{Version: "v0.16.1", URL: "https://github.com/" + update.Repository + "/releases/tag/v0.16.1"}}
	u, _, _ := newTestUpdater(t, "v0.16.0", update.ModePackage, f)
	u.debArch = "arm64"
	o := u.CheckUpdate(t.Context())
	if o.Mode != update.ModePackage || !o.UpdateAvailable || o.Package == nil || o.Package.Format != "deb" || o.Package.Arch != "arm64" ||
		o.Package.File != "picache_0.16.1_arm64.deb" ||
		o.Package.URL != "https://github.com/Hustenreizjuengling/PiCache/releases/download/v0.16.1/picache_0.16.1_arm64.deb" {
		t.Fatalf("overview %+v %+v", o, o.Package)
	}
	err := u.QueueUpdate(t.Context(), "v0.16.1", "admin")
	if apperr.KindOf(err) != apperr.KindConflict ||
		err.Error() != "PiCache was installed as a Debian package: update it with apt (System → Updates shows the steps)" {
		t.Fatalf("queue in package mode: %v", err)
	}
	// Without a release only the architecture.
	f.rel = nil
	u.lastTry = time.Time{}
	if o := u.CheckUpdate(t.Context()); o.Package == nil || o.Package.File != "" || o.Package.URL != "" || o.Package.Arch != "arm64" {
		t.Fatalf("no release: %+v", o.Package)
	}
	// Other modes have no package member.
	h, _, _ := newTestUpdater(t, "v0.16.0", update.ModeHelper, &fakeReleases{})
	if o := h.UpdateOverview(t.Context()); o.Package != nil {
		t.Fatalf("helper mode: %+v", o.Package)
	}
}

// A stored check result is used only by the same kind of installation: after
// install.sh → Debian package (or back) the release found before may lack the
// .deb (or the package check skipped a newer binary-only release), so it is
// dropped until the next check.
func TestUpdaterDropsResultOfOtherKind(t *testing.T) {
	f := &fakeReleases{rel: &update.Release{Version: "v0.16.2", URL: "https://github.com/" + update.Repository + "/releases/tag/v0.16.2"}}
	u, d, set := newTestUpdater(t, "v0.16.1", update.ModeHelper, f)
	if o := u.CheckUpdate(t.Context()); !o.UpdateAvailable {
		t.Fatalf("helper check %+v", o)
	}
	restart := func(mode string) *updater {
		n := newUpdater(u.dataDir, "v0.16.1", d, set, func() string { return mode }, f.latest, slog.New(slog.DiscardHandler))
		n.debArch = "amd64"
		n.load(t.Context())
		return n
	}
	// Now installed as a Debian package: no release, no .deb, never checked.
	p := restart(update.ModePackage)
	if o := p.UpdateOverview(t.Context()); o.Latest != nil || o.UpdateAvailable || !o.CheckedAt.IsZero() || o.Package == nil || o.Package.File != "" {
		t.Fatalf("package mode after a helper check: %+v %+v", o, o.Package)
	}
	// Other non-package modes keep it (helper → manual: same releases).
	if o := restart(update.ModeManual).UpdateOverview(t.Context()); o.Latest == nil || o.Latest.Version != "v0.16.2" {
		t.Fatalf("manual mode after a helper check: %+v", o)
	}
	// A package check is kept by the package mode and dropped by the others.
	f.rel = &update.Release{Version: "v0.16.1", URL: "https://github.com/" + update.Repository + "/releases/tag/v0.16.1"}
	if o := p.CheckUpdate(t.Context()); o.Latest == nil || o.Latest.Version != "v0.16.1" {
		t.Fatalf("package check %+v", o)
	}
	if o := restart(update.ModePackage).UpdateOverview(t.Context()); o.Latest == nil || o.Package.File != "picache_0.16.1_amd64.deb" {
		t.Fatalf("package mode after a package check: %+v %+v", o, o.Package)
	}
	if o := restart(update.ModeHelper).UpdateOverview(t.Context()); o.Latest != nil || !o.CheckedAt.IsZero() {
		t.Fatalf("helper mode after a package check: %+v", o)
	}
}
