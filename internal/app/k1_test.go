package app

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/applog"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/ntp"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// A PICACHE_LOG_FILE below <data dir>/logs gets its directory (0750)
// before the sinks start; other allowed places are left alone.
func TestPrepareLogDir(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		file, dir string
		created   bool
	}{
		{filepath.Join(dir, "logs", "picache.log"), filepath.Join(dir, "logs"), true},
		{filepath.Join(dir, "logs", "sub", "picache.log"), filepath.Join(dir, "logs", "sub"), true},
		{filepath.Join(dir, "picache.log"), filepath.Join(dir, "logs2"), false},
		{"", filepath.Join(dir, "logs3"), false},
	} {
		a := &App{cfg: &config.Config{DataDir: dir + string(filepath.Separator), LogFile: tc.file}}
		a.prepareLogDir()
		fi, err := os.Stat(tc.dir)
		if created := err == nil && fi.IsDir(); created != tc.created {
			t.Errorf("%q: created %v, want %v", tc.file, created, tc.created)
		}
		if tc.created && runtime.GOOS == "linux" && fi.Mode().Perm() != 0o750 {
			t.Errorf("%q: mode %v", tc.file, fi.Mode().Perm())
		}
	}
}

// The check "logging" is present while a sink is configured and warns
// for a sink that cannot write and for dropped records.
func TestLoggingHealth(t *testing.T) {
	var drops dropTracker
	now := time.Now()
	if _, _, _, show := loggingHealth(nil, &drops, now); show {
		t.Error("shown without a sink")
	}
	if st, _, _, show := loggingHealth([]applog.SinkState{{Kind: applog.SinkFile, Target: "/var/log/picache/picache.log", OK: true}},
		&drops, now); !show || st != "ok" {
		t.Errorf("working sink: %s", st)
	}
	st, msg, _, _ := loggingHealth([]applog.SinkState{
		{Kind: applog.SinkFile, Target: "/var/log/picache/picache.log", Error: "permission denied"},
		{Kind: applog.SinkSyslog, Target: "192.168.1.10:514", Error: "connection refused"},
		{Kind: applog.SinkSyslog, Target: "192.168.1.11:514", OK: true, Dropped: 12},
	}, &drops, now)
	want := "the log file /var/log/picache/picache.log cannot be written: permission denied; " +
		"syslog 192.168.1.10:514 is not reachable: connection refused; 12 log records were dropped"
	if st != "warn" || msg != want {
		t.Errorf("%s %q", st, msg)
	}
	// The dropped count is cumulative: the check recovers once no record
	// was dropped for 10 minutes, and a later loss counts from there.
	syslog := func(dropped int64, at time.Duration) (string, string) {
		st, msg, _, _ := loggingHealth([]applog.SinkState{{Kind: applog.SinkSyslog, Target: "192.168.1.11:514", OK: true, Dropped: dropped}},
			&drops, now.Add(at))
		return st, msg
	}
	if st, msg := syslog(12, 5*time.Minute); st != "warn" || msg != "12 log records were dropped" {
		t.Errorf("within the window: %s %q", st, msg)
	}
	if st, _ := syslog(12, 11*time.Minute); st != "ok" {
		t.Errorf("after the window: %s", st)
	}
	if st, msg := syslog(15, 30*time.Minute); st != "warn" || msg != "3 log records were dropped" {
		t.Errorf("a new loss: %s %q", st, msg)
	}
	if st, msg := syslog(17, 35*time.Minute); st != "warn" || msg != "5 log records were dropped" {
		t.Errorf("the same episode: %s %q", st, msg)
	}
	if st, _ := syslog(17, 46*time.Minute); st != "ok" {
		t.Errorf("recovered: %s", st)
	}
}

// The check "ntp": unsynchronised and unreadable clock states warn.
func TestNTPHealth(t *testing.T) {
	if st, _, _ := ntpHealth(ntp.ClockState{Synced: true}, false); st != "ok" {
		t.Errorf("synced: %s", st)
	}
	if st, msg, _ := ntpHealth(ntp.ClockState{}, false); st != "warn" ||
		msg != "the host clock is not synchronised: NTP clients get unsynchronised answers" {
		t.Errorf("unsynced: %s %q", st, msg)
	}
	st, msg, _ := ntpHealth(ntp.ClockState{Err: syscall.EPERM}, false)
	if st != "warn" || !strings.HasPrefix(msg, "the clock state cannot be read (") ||
		!strings.HasSuffix(msg, "): update the unit files (run the one-line installer once)") {
		t.Errorf("EPERM: %s %q", st, msg)
	}
	if _, msg, _ := ntpHealth(ntp.ClockState{Err: syscall.EPERM}, true); strings.Contains(msg, "unit files") {
		t.Errorf("docker: %q", msg)
	}
}

// PICACHE_INITIAL_CONFIG: applied on the first start (strictly decoded on
// top of the current document, cache.activeStoreId ignored, recorded),
// ignored on a later start; an invalid file refuses the start with its
// field; a start with an account is no first start.
func TestInitialConfig(t *testing.T) {
	ctx := context.Background()
	write := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "initial.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := syncApp(t)
	a.cfg.InitialConfig = write(t, `{"web":{"language":"de"},"logs":{"seenRetentionDays":90},"cache":{"activeStoreId":"nas1"}}`)
	if err := a.applyInitialConfig(ctx); err != nil {
		t.Fatal(err)
	}
	got := a.set.Get()
	if got.Web.Language != "de" || got.Logs.SeenRetentionDays != 90 || got.Cache.ActiveStoreID != settings.Defaults().Cache.ActiveStoreID {
		t.Fatalf("applied %q %d %q", got.Web.Language, got.Logs.SeenRetentionDays, got.Cache.ActiveStoreID)
	}
	if got.DNS.Upstreams == nil || len(got.DNS.Upstreams) == 0 {
		t.Error("members the file leaves out lost their values")
	}
	var doc string
	if err := a.cdb.R.QueryRow(`SELECT value FROM app_meta WHERE key = ?`, metaInitialConfig).Scan(&doc); err != nil ||
		!strings.Contains(doc, `"sha256":"`) {
		t.Fatalf("not recorded: %q %v", doc, err)
	}
	// A later start ignores it.
	a.cfg.InitialConfig = write(t, `{"web":{"language":"fr"}}`)
	if err := a.applyInitialConfig(ctx); err != nil {
		t.Fatal(err)
	}
	if a.set.Get().Web.Language != "de" {
		t.Error("applied on a later start")
	}

	for _, c := range []struct{ body, want string }{
		{`{"logs":{"seenRetentionDays":1}}`, "PICACHE_INITIAL_CONFIG: logs.seenRetentionDays: must be between 7 and 365"},
		{`{"nope":1}`, "PICACHE_INITIAL_CONFIG: invalid JSON"},
		{`{"web":{"language":"de","language":"en"}}`, "PICACHE_INITIAL_CONFIG: invalid JSON"},
		{`not json`, "PICACHE_INITIAL_CONFIG: invalid JSON"},
	} {
		b := syncApp(t)
		b.cfg.InitialConfig = write(t, c.body)
		err := b.applyInitialConfig(ctx)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("%s: %v", c.body, err)
		}
		if b.set.Get().Web.Language != settings.Defaults().Web.Language {
			t.Errorf("%s: partly applied", c.body)
		}
	}
	b := syncApp(t)
	b.cfg.InitialConfig = filepath.Join(t.TempDir(), "missing.json")
	if err := b.applyInitialConfig(ctx); err == nil {
		t.Error("a missing file did not refuse the start")
	}
	big := write(t, `{"web":{"language":"de"}}`+strings.Repeat(" ", maxInitialConfig))
	b.cfg.InitialConfig = big
	if err := b.applyInitialConfig(ctx); err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Errorf("oversized: %v", err)
	}
	if err := os.Symlink(write(t, `{}`), filepath.Join(t.TempDir(), "x")); err == nil {
		link := filepath.Join(t.TempDir(), "link.json")
		_ = os.Symlink(write(t, `{"web":{"language":"de"}}`), link)
		b.cfg.InitialConfig = link
		if err := b.applyInitialConfig(ctx); err == nil {
			t.Error("a symbolic link was read")
		}
	}

	// An account exists: no first start.
	r := newRestoreApp(t)
	makeConfigDB(t, r.paths.ConfigDB, "owner", "owner password", "en")
	migrateConfig(t, r.paths.ConfigDB, nil)
	openLive(t, r)
	first, err := firstStart(ctx, r.cdb)
	if err != nil || first {
		t.Errorf("first start with an account: %v %v", first, err)
	}
}

// The syncer's health: absent while off; warns after 3 intervals without
// a success.
func TestSyncHealthStale(t *testing.T) {
	f := syncApp(t)
	if _, _, _, show := f.sync.health(); show {
		t.Error("shown while off")
	}
	ca := newSyncCA(t, "CA")
	ps := newPrimaryServer(t, ca.leaf(t, 2), nil)
	follow(t, f, ca.pem, ps, []string{settings.SectionLocalDNS})
	now := time.Now()
	f.sync.now = func() time.Time { return now }
	f.sync.started = now
	if st, _, _, show := f.sync.health(); !show || st != "ok" {
		t.Errorf("fresh follower: %s", st)
	}
	f.sync.now = func() time.Time { return now.Add(47 * time.Minute) }
	if st, msg, _, _ := f.sync.health(); st != "warn" || msg != "no sync from https://primary.test:8443 succeeded for 45 minutes" {
		t.Errorf("stale: %s %q", st, msg)
	}
	// A follower whose token is gone (a backup restored without secrets)
	// warns at once.
	if _, err := f.cdb.W.Exec(`DELETE FROM settings_secrets`); err != nil {
		t.Fatal(err)
	}
	set, err := settings.Open(context.Background(), f.cdb, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f.set = set
	if st, msg, _, _ := f.sync.health(); st != "warn" ||
		msg != "sync from https://primary.test:8443 failed: no sync token is stored: enter a sync token of the primary" {
		t.Errorf("without a token: %s %q", st, msg)
	}
}

// The support bundle's rules for the members of 0.15: the sync source and
// the proxy reduced to scheme://host[:port] with the host scrubbed, the
// trust anchor and the proxy user name only as [set], the flags kept.
func TestSettingsScrubbingK1(t *testing.T) {
	a := fullSettings()
	a.Sync = settings.Sync{Mode: settings.SyncFollower, Source: "https://primary.smith.home:8443", TokenSet: true,
		CAPEM: "-----BEGIN CERTIFICATE-----", IntervalMinutes: 15, Sections: []string{settings.SectionLocalDNS}}
	a.Network.Proxy = settings.Proxy{URL: "http://proxy.smith.home:3128", Username: "alice", PasswordSet: true}
	a.Network.ProxyFor.Lists = true
	a.Clients.NameSources.WHOIS = true
	a.Logs.SeenRetentionDays = 90
	a.NTP = settings.NTP{Enabled: true, Stratum: 4}
	a.Updates.Channel = settings.ChannelBeta
	sc := newScrubber(false)
	got := scrubbedSettings(t, sc, &a)
	want := map[string]any{
		"sync.mode": "follower", "sync.tokenSet": true, "sync.caPem": "[set]", "sync.intervalMinutes": float64(15),
		"sync.sections": []any{"local-dns"}, "network.proxy.username": "[set]", "network.proxy.passwordSet": true,
		"network.proxyFor.lists": true, "clients.nameSources.whois": true, "logs.seenRetentionDays": float64(90),
		"ntp.enabled": true, "ntp.stratum": float64(4), "updates.channel": "beta",
	}
	for k, w := range want {
		if g := got[k]; !jsonEqual(t, g, w) {
			t.Errorf("%s = %v, want %v", k, g, w)
		}
	}
	if got["sync.source"] != "https://*.smith.home:8443" || got["network.proxy.url"] != "http://*.smith.home:3128" {
		t.Errorf("reduced %v %v", got["sync.source"], got["network.proxy.url"])
	}
	if sc.counts[countUnknown] != 0 {
		t.Fatalf("%d members without a rule", sc.counts[countUnknown])
	}
}

// Two default routes via the same gateway on one interface (DHCP plus a
// static route with another metric) list the gateway once: the UI keys
// the list by gateway.
func TestInterfaceGatewaysDeduplicated(t *testing.T) {
	gw := netip.MustParseAddr("192.168.1.1")
	gw6 := netip.MustParseAddr("fe80::1")
	defaults := []netutil.Route{
		{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Iface: "eth0", Gateway: gw, Metric: 100},
		{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Iface: "eth0", Gateway: gw, Metric: 200},
		{Prefix: netip.MustParsePrefix("::/0"), Iface: "eth0", Gateway: gw6, Metric: 100},
		{Prefix: netip.MustParsePrefix("::/0"), Iface: "eth0", Gateway: gw6, Metric: 1024},
		{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Iface: "wlan0", Gateway: gw, Metric: 600},
		{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Iface: "eth0"}, // no gateway (point-to-point)
	}
	got := interfaceGateways(defaults, "eth0")
	want := []api.NetworkInterfaceGate{{Family: "ipv4", Gateway: "192.168.1.1"}, {Family: "ipv6", Gateway: "fe80::1"}}
	if !slices.Equal(got, want) {
		t.Fatalf("gateways %+v", got)
	}
	if got := interfaceGateways(defaults, "wg0"); got == nil || len(got) != 0 {
		t.Fatalf("no default route: %#v", got)
	}
}

// A follower's next run moves to a minute from now when it is switched on,
// points elsewhere, syncs other sections or gets new credentials (another
// CA certificate, another token: e.g. after a run failed with a wrong
// token); an unrelated write or a longer interval changes nothing.
func TestSyncRescheduled(t *testing.T) {
	base := settings.Sync{Mode: settings.SyncFollower, Source: "https://primary.lan:8443", CAPEM: "A", IntervalMinutes: 15,
		Sections: []string{settings.SectionLocalDNS}}
	tok, other := sha256.Sum256([]byte("pc_a")), sha256.Sum256([]byte("pc_b"))
	for _, tc := range []struct {
		name   string
		change func(s *settings.Sync)
		newTok bool
		want   bool
	}{
		{"unchanged", func(*settings.Sync) {}, false, false},
		{"interval", func(s *settings.Sync) { s.IntervalMinutes = 60 }, false, false},
		{"source", func(s *settings.Sync) { s.Source = "https://other.lan:8443" }, false, true},
		{"sections", func(s *settings.Sync) { s.Sections = []string{settings.SectionParental} }, false, true},
		{"ca", func(s *settings.Sync) { s.CAPEM = "B" }, false, true},
		{"token", func(*settings.Sync) {}, true, true},
		{"switched off", func(s *settings.Sync) { s.Mode = settings.SyncOff }, true, false},
	} {
		cur := base
		cur.Sections = slices.Clone(base.Sections)
		tc.change(&cur)
		curTok := tok
		if tc.newTok {
			curTok = other
		}
		if got := rescheduled(base, cur, tok, curTok); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	off := base
	off.Mode = settings.SyncOff
	if !rescheduled(off, base, tok, tok) {
		t.Error("switched on")
	}
}
