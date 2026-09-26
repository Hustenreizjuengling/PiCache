package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/applog"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// fullSettings returns settings with every optional member set.
func fullSettings() settings.All {
	a := settings.Defaults()
	until := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	a.Filter.PausedUntil = &until
	return a
}

// settingLeaves returns the dotted paths of every leaf of the settings.
func settingLeaves(t *testing.T, a *settings.All) []string {
	t.Helper()
	raw, err := jsonMarshal(a)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	if _, err := rewriteJSON(raw, func(path string, tok jsontext.Token) (jsontext.Token, bool) {
		seen[path] = true
		return tok, true
	}); err != nil {
		t.Fatal(err)
	}
	// Arrays that are empty have no element: add their paths.
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		if m, ok := v.(map[string]any); ok {
			for k, x := range m {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, x)
			}
			return
		}
		seen[prefix] = true
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	walk("", v)
	return slices.Sorted(maps.Keys(seen))
}

// Every leaf of the settings has a rule, and every rule a leaf.
func TestSettingRulesCoverEveryLeaf(t *testing.T) {
	a := fullSettings()
	leaves := settingLeaves(t, &a)
	for _, l := range leaves {
		if _, ok := settingRules[l]; !ok {
			t.Errorf("settings member %s has no rule in settingRules (support bundle)", l)
		}
	}
	for p := range settingRules {
		if !slices.Contains(leaves, p) {
			t.Errorf("rule for %s, which is no settings member", p)
		}
	}
}

// scrubbedSettings returns the members of settings.json as a flat map.
func scrubbedSettings(t *testing.T, sc *scrubber, a *settings.All) map[string]any {
	t.Helper()
	raw, err := sc.scrubSettings(a)
	if err != nil {
		t.Fatal(err)
	}
	var sections map[string]map[string]any
	if err := json.Unmarshal(raw, &sections); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	out := map[string]any{}
	var flat func(prefix string, m map[string]any)
	flat = func(prefix string, m map[string]any) {
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok {
				flat(prefix+k+".", sub)
				continue
			}
			out[prefix+k] = v
		}
	}
	for s, m := range sections {
		flat(s+".", m)
	}
	return out
}

func TestSettingsScrubbing(t *testing.T) {
	a := fullSettings()
	a.DNS.Upstreams = []string{"https://abc123.dns.nextdns.io/0a1b2c", "https://dns.quad9.net/dns-query", "tls://user@dns.example.net:853",
		"9.9.9.9", "192.168.1.1:53", "https://[2001:db8:1:2::1]/dns-query?x=1#f"}
	a.DNS.Bootstrap = []string{"9.9.9.9", "fd00::53"}
	a.DNS.RouterResolver = "203.0.113.7"
	a.DNS.AllowedNetworks = []string{"203.0.113.0/24", "10.0.0.0/8", "2001:db8:aa:bb::/64"}
	a.DNS.BlockedClients = []string{"203.0.113.9", "aa:bb:cc:dd:ee:ff", "192.168.1.9"}
	a.DNS.ServerNames = []string{"picache", "nas-home"}
	a.DNS.LocalDomain = "smith.home"
	a.DNS.RebindAllow = []string{"plex.direct", "media.smith.home"}
	a.DNS.DroppedDomains = []string{"ads.example.com:AAAA"}
	a.DNS.ECS = settings.ECS{Mode: settings.ECSCustom, CustomSubnet: "198.51.100.0/24"}
	a.Web.AllowedHosts = []string{"picache.smith.home", "192.168.1.2", "198.51.100.7"}
	a.Logs.IgnoredDomains = []string{"noisy.example"}
	a.DHCP.Options.WPADURL = "http://wpad.smith.home/wpad.dat?token=x"
	a.DHCP.Domain = "smith.home"
	a.Backups.Destination = "nas1"
	sc := newScrubber(false)
	got := scrubbedSettings(t, sc, &a)
	ph := func(name string) string { return sc.names[name] }
	want := map[string]any{
		"dns.upstreams": []any{"https://*.nextdns.io/…", "https://dns.quad9.net/dns-query", "tls://*.example.net:853",
			"9.9.0.0", "192.168.1.1:53", "https://[2001:db8:1::]/dns-query"},
		"dns.bootstrap":                  []any{"9.9.0.0", "fd00::53"},
		"dns.routerResolver":             "203.0.0.0",
		"dns.allowedNetworks":            []any{"203.0.0.0/16", "10.0.0.0/8", "2001:db8:aa::/48"},
		"dns.blockedClients":             []any{"203.0.0.0", "192.168.1.9"},
		"dns.serverNames":                []any{"picache", ph("nas-home")},
		"dns.localDomain":                ph("smith.home"),
		"dns.rebindAllow":                []any{"plex.direct", ph("media.smith.home")},
		"dns.droppedDomains":             []any{ph("ads.example.com") + ":AAAA"},
		"dns.fallbackUpstreams":          []any{"https://security.cloudflare-dns.com/dns-query"},
		"web.allowedHosts":               []any{ph("picache.smith.home"), "192.168.1.2", "198.51.0.0"},
		"logs.ignoredDomains":            []any{ph("noisy.example")},
		"dhcp.options.wpadUrl":           "http://*.smith.home/…",
		"dhcp.domain":                    ph("smith.home"),
		"downloadCache.domainsSource":    "https://raw.githubusercontent.com/…",
		"backups.destination":            "nas1",
		"dns.upstreamMode":               "load_balance",
		"logs.privacyLevel":              "full",
		"filter.pausedUntil":             "2026-09-26T12:00:00Z",
		"cache.minFreeBytes":             float64(10 << 30),
		"dns.dns64.prefix":               "64:ff9b::/48",
		"dns.ecs.customSubnet":           "198.51.0.0/16",
		"dns.ecs.mode":                   "custom",
		"downloadCache.disabledServices": []any{"test"},
	}
	for _, n := range []string{"nas-home", "smith.home", "media.smith.home", "ads.example.com", "picache.smith.home", "noisy.example"} {
		if !strings.HasPrefix(ph(n), "name-") {
			t.Fatalf("no placeholder for %s: %v", n, sc.names)
		}
	}
	for k, w := range want {
		if g := got[k]; !jsonEqual(t, g, w) {
			t.Errorf("%s = %v, want %v", k, g, w)
		}
	}
	if sc.counts[countUnknown] != 0 {
		t.Fatalf("%d members without a rule", sc.counts[countUnknown])
	}
	// The same names in other files get the same placeholders (longest
	// first, whole labels).
	if got, want := sc.scrubText("nas-home.smith.home answered for media.smith.home, not smith.homer"),
		ph("nas-home")+"."+ph("smith.home")+" answered for "+ph("media.smith.home")+", not smith.homer"; got != want {
		t.Fatalf("text %q, want %q", got, want)
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, err1 := json.Marshal(a, json.Deterministic(true))
	y, err2 := json.Marshal(b, json.Deterministic(true))
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// The text scrubber with and without the tick "include client names".
func TestTextScrubber(t *testing.T) {
	in := "client 192.168.1.20 (aa:bb:cc:dd:ee:01) via 203.0.113.9:53 and [2001:db8:1:2::5]:853, fd12::1/64, " +
		"lo 127.0.0.1 ::1 0.0.0.0:53, time 12:30:45, version 0.12.0, mac AA-BB-CC-DD-EE-01, host nas.lan, 10.1.2.3."
	sc := newScrubber(false)
	sc.addName("nas.lan")
	got := sc.scrubText(in)
	want := "client 192.168.0.0 (mac-1) via 203.0.0.0:53 and [2001:db8:1::]:853, fd12::/48, " +
		"lo 127.0.0.1 ::1 0.0.0.0:53, time 12:30:45, version 0.12.0, mac mac-1, host name-1, 10.1.0.0."
	if got != want {
		t.Fatalf("without the tick:\n got %q\nwant %q", got, want)
	}
	sc = newScrubber(true)
	sc.addName("nas.lan")
	got = sc.scrubText(in)
	want = "client 192.168.1.20 (aa:bb:cc:dd:ee:01) via 203.0.0.0:53 and [2001:db8:1::]:853, fd12::1/64, " +
		"lo 127.0.0.1 ::1 0.0.0.0:53, time 12:30:45, version 0.12.0, mac AA-BB-CC-DD-EE-01, host name-1, 10.1.2.3."
	if got != want {
		t.Fatalf("with the tick:\n got %q\nwant %q", got, want)
	}
	// localhost and picache are kept.
	sc.addName("localhost")
	sc.addName("picache")
	if got := sc.scrubText("localhost picache"); got != "localhost picache" {
		t.Fatalf("kept names: %q", got)
	}
}

// The log records: username, user and by always redacted, the name keys
// without the tick.
func TestScrubRecord(t *testing.T) {
	r := applog.Record{Msg: "login from 203.0.113.5", Attrs: []applog.Attr{{Key: "username", Value: "alice"},
		{Key: "g.by", Value: "bob"}, {Key: "qname", Value: "secret.example"}, {Key: "client", Value: "192.168.1.5"},
		{Key: "host", Value: "nas"}}}
	got := newScrubber(false).scrubRecord(r)
	if got.Msg != "login from 203.0.0.0" || got.Attrs[0].Value != "[redacted]" || got.Attrs[1].Value != "[redacted]" ||
		got.Attrs[2].Value != "[redacted]" || got.Attrs[3].Value != "192.168.0.0" || got.Attrs[4].Value != "[redacted]" {
		t.Fatalf("without the tick %+v", got)
	}
	got = newScrubber(true).scrubRecord(r)
	if got.Attrs[0].Value != "[redacted]" || got.Attrs[1].Value != "[redacted]" || got.Attrs[2].Value != "secret.example" ||
		got.Attrs[3].Value != "192.168.1.5" || got.Attrs[4].Value != "nas" {
		t.Fatalf("with the tick %+v", got)
	}
}

func TestReducedNetworkCheckAndDHCP(t *testing.T) {
	nc := api.NetworkCheck{Mode: "host", StatsAvailable: true, Router: &api.NetworkRouter{Kind: "fritzbox", IPv4: "192.168.178.1", MAC: "aa:bb:cc:dd:ee:ff"},
		Self:    api.NetworkSelf{IPv4: []string{"192.168.178.2"}, Global: []string{"2001:db8:1:2::5", "2001:db8:1:3::5"}},
		Checks:  []api.NetworkItem{{ID: "ipv6-dns", Status: "warn", Data: map[string]string{"x": "secret"}}},
		Devices: []api.NetworkDevice{{MAC: "aa:bb", Status: "active", Name: "phone"}, {Status: "never"}}}
	raw, _ := jsonMarshal(reducedNetworkCheck(&nc))
	s := string(raw)
	for _, bad := range []string{"secret", "phone", "aa:bb", "192.168.178"} {
		if strings.Contains(s, bad) {
			t.Errorf("reduced check contains %q: %s", bad, s)
		}
	}
	for _, good := range []string{`"routerKind":"fritzbox"`, `"globalPrefixes":["2001:db8:1::/48"]`, `"ipv4":1`,
		`"checks":[{"id":"ipv6-dns","status":"warn"}]`, `"total":2`, `"active":1`, `"never":1`} {
		if !strings.Contains(s, good) {
			t.Errorf("reduced check lacks %s: %s", good, s)
		}
	}
	st := dhcp.Status{Available: true, State: "serving", Deployment: "systemd", Blockers: []string{},
		Interface:    &dhcp.StatusInterface{Name: "eth0", MAC: "aa:bb:cc:dd:ee:ff", IPv4: "192.168.1.2"},
		Pool:         &dhcp.StatusPool{Start: "192.168.1.100", End: "192.168.1.200", Size: 101, Used: 5, Static: 2},
		OtherServers: []dhcp.OtherServer{{Address: "192.168.1.1"}}, Counters: dhcp.Counters{Acks: 3}}
	raw, _ = jsonMarshal(reducedDHCP(&st))
	s = string(raw)
	if strings.Contains(s, "192.168") || strings.Contains(s, "aa:bb") || !strings.Contains(s, `"otherServers":1`) ||
		!strings.Contains(s, `"pool":{"size":101,"static":2,"used":5}`) || !strings.Contains(s, `"acks":3`) {
		t.Fatalf("reduced DHCP %s", s)
	}
}

// bundleApp is an App with the parts the support bundle reads.
func bundleApp(t *testing.T) *App {
	t.Helper()
	a := newTestApp(t)
	h := applog.New(slog.NewTextHandler(io.Discard, nil), slog.LevelDebug)
	a.appLog = h.Log()
	log := slog.New(h)
	for i := range 50 {
		log.Info("record", slog.Int("i", i), slog.String("username", "alice"), slog.String("client", "192.168.1.5"),
			slog.String("filler", strings.Repeat("x", 400)))
	}
	a.health.Store(&api.Health{OK: true, Checks: []api.HealthCheck{{Name: "listeners", Status: "warn",
		Message: "web: listen tcp 203.0.113.4:8080: bind: address in use"}}})
	return a
}

// The bundle holds exactly the allowlisted files; the manifest names the
// rules and counts; the log is cut oldest first to fit.
func TestSupportBundle(t *testing.T) {
	a := bundleApp(t)
	b, err := a.SupportBundle(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	files := readZip(t, b)
	names := slices.Sorted(maps.Keys(files))
	if want := []string{"MANIFEST.txt", "databases.json", "dhcp.json", "health.json", "host.json", "listeners.json",
		"log.ndjson", "network-check.json", "settings.json", "version.json"}; !slices.Equal(names, want) {
		t.Fatalf("files %v", names)
	}
	m := files["MANIFEST.txt"]
	for _, s := range []string{"Review these files before sharing them.", "includeClientNames: false", "health.json: 1 addresses masked",
		"log.ndjson: ", "values redacted"} {
		if !strings.Contains(m, s) {
			t.Errorf("manifest lacks %q:\n%s", s, m)
		}
	}
	if strings.Contains(files["health.json"], "203.0.113.4") || !strings.Contains(files["health.json"], "203.0.0.0:8080") {
		t.Fatalf("health %s", files["health.json"])
	}
	if strings.Contains(files["log.ndjson"], "alice") || strings.Contains(files["log.ndjson"], "192.168.1.5") ||
		strings.Count(files["log.ndjson"], "\n") != 50 {
		t.Fatalf("log %s", files["log.ndjson"][:200])
	}
	var v struct {
		Deployment string `json:"deployment"`
	}
	if err := json.Unmarshal([]byte(files["version.json"]), &v); err != nil || !slices.Contains([]string{"systemd", "docker", "other"}, v.Deployment) {
		t.Fatalf("version %s", files["version.json"])
	}
	// The size limit cuts the oldest log records.
	old := supportBundleMax
	defer func() { supportBundleMax = old }()
	used := 8 << 10
	for n, f := range files {
		if n != "MANIFEST.txt" && n != "log.ndjson" {
			used += len(f)
		}
	}
	supportBundleMax = used + 10*600
	b, err = a.SupportBundle(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	files = readZip(t, b)
	lines := strings.Split(strings.TrimSpace(files["log.ndjson"]), "\n")
	if len(lines) >= 50 || len(lines) == 0 || !strings.Contains(lines[len(lines)-1], `"value":"49"`) ||
		!strings.Contains(files["MANIFEST.txt"], "oldest records left out (size limit)") {
		t.Fatalf("%d lines, last %q", len(lines), lines[len(lines)-1])
	}
	if !strings.Contains(files["log.ndjson"], "192.168.1.5") || strings.Contains(files["log.ndjson"], "alice") {
		t.Fatal("with client names: private addresses kept, user names never")
	}
	if len(b) > 16<<20 {
		t.Fatalf("bundle %d bytes", len(b))
	}
}

// Upstream IDs, DoH paths and query tokens reach no file of the bundle: the
// text scrubber applies the upstream rule of settings.json to the log
// (the configured values, also inside a list, and URLs quoted by errors)
// and to the other files.
func TestSupportBundleUpstreams(t *testing.T) {
	a := newTestApp(t)
	ups := []string{"https://dns.nextdns.io/abc123", "tls://abc123.dns.nextdns.io", "https://doh.example.net/q?token=s3cr3t"}
	if _, err := a.set.Update(context.Background(), func(s *settings.All) error {
		s.DNS.Upstreams = ups
		s.DNS.FallbackUpstreams = []string{"https://dns.quad9.net/dns-query", "udp://abc123.resolver.example.net:53"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := applog.New(slog.NewTextHandler(io.Discard, nil), slog.LevelDebug)
	a.appLog = h.Log()
	log := slog.New(h)
	log.Info("upstreams configured", slog.Any("upstreams", ups))
	log.Warn("upstream is failing", slog.String("upstream", ups[1]),
		slog.Any("err", errors.New(`Get "https://dns.nextdns.io/abc123?dns=AAAB": context deadline exceeded`)))
	log.Info("list downloaded", slog.String("url", "https://lists.example.org/u/abc123/hosts.txt"))
	a.health.Store(&api.Health{OK: false, Checks: []api.HealthCheck{{Name: "upstreams", Status: "warn",
		Message: "upstream " + ups[0] + " is failing, " + ups[2] + " too (abc123.resolver.example.net)."}}})
	for _, names := range []bool{false, true} {
		b, err := a.SupportBundle(context.Background(), names)
		if err != nil {
			t.Fatal(err)
		}
		files := readZip(t, b)
		for name, data := range files {
			for _, secret := range []string{"abc123", "s3cr3t", "token=", "dns=AAAB"} {
				if strings.Contains(data, secret) {
					t.Errorf("includeClientNames %v: %s contains %q:\n%s", names, name, secret, data)
				}
			}
		}
		for _, want := range []string{"https://*.nextdns.io/…", "tls://*.nextdns.io", "https://*.example.net/…",
			"https://lists.example.org/…"} {
			if !strings.Contains(files["log.ndjson"], want) {
				t.Errorf("log lacks %q:\n%s", want, files["log.ndjson"])
			}
		}
		if !strings.Contains(files["health.json"], "https://*.nextdns.io/… is failing, https://*.example.net/… too") ||
			!strings.Contains(files["MANIFEST.txt"], "upstreams and URLs reduced") {
			t.Errorf("health %s\nmanifest %s", files["health.json"], files["MANIFEST.txt"])
		}
	}
}

// URLs in text keep scheme, host and port; a path other than "/" or
// "/dns-query" becomes "/…"; user information, query and fragment go.
func TestReduceTextURL(t *testing.T) {
	sc := newScrubber(true)
	for in, want := range map[string]string{
		"see https://user:pw@h.example:8443/p/q?x=1#f.": "see https://h.example:8443/….",
		"(https://dns.example/dns-query?dns=AAAB)":      "(https://dns.example/dns-query)",
		"[https://a.example/x tls://b.example:853]":     "[https://a.example/… tls://b.example:853]",
		`Get "http://[2001:db8::1]/path": timeout`:      `Get "http://[2001:db8::]/…": timeout`,
		"no url here, 10:30 and a:b":                    "no url here, 10:30 and a:b",
		"https://h.example/ and https://h.example/?q=1": "https://h.example/ and https://h.example/",
	} {
		if got := sc.scrubText(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func readZip(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r)
		r.Close()
		out[f.Name] = string(data)
	}
	return out
}

// Database sizes from the files; missing files are 0, the index files are
// listed.
func TestDatabases(t *testing.T) {
	a := newTestApp(t)
	info := a.Databases()
	if info.PiCache.Bytes == 0 || info.Logs.Bytes != 0 || info.Logs.CapBytes != 2048<<20 || info.Logs.FillPercent != 0 ||
		info.CacheIndexes == nil || len(info.CacheIndexes) != 0 {
		t.Fatalf("databases %+v", info)
	}
	for _, n := range []string{"store1.db", "store1.db-wal", "notes.txt"} {
		if err := os.WriteFile(a.paths.CacheIndexDir+"/"+n, make([]byte, 100), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(a.paths.LogsDB, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	info = a.Databases()
	if len(info.CacheIndexes) != 1 || info.CacheIndexes[0] != (api.CacheIndexDB{StoreID: "store1", Bytes: 100, WALBytes: 100}) ||
		info.Logs.Bytes != 1<<20 || info.Logs.FillPercent <= 0 {
		t.Fatalf("databases %+v", info)
	}
}
