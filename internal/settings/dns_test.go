package settings

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// The default fallback is another operator than the default upstreams, so
// an outage of one does not take the other along.
func TestDefaultFallbackIsAnotherOperator(t *testing.T) {
	d := Defaults().DNS
	if len(d.FallbackUpstreams) == 0 {
		t.Fatal("no default fallback")
	}
	for _, f := range d.FallbackUpstreams {
		fs, err := ParseUpstream(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, u := range d.Upstreams {
			us, err := ParseUpstream(u)
			if err != nil {
				t.Fatal(err)
			}
			if fs.Host == us.Host {
				t.Fatalf("fallback %s has the host of the upstream %s", f, u)
			}
		}
	}
	if !slices.Equal(d.Upstreams, []string{"https://dns.quad9.net/dns-query"}) || d.UpstreamBlockedTTL != 300 ||
		!d.RebindProtection || !slices.Equal(d.RebindAllow, []string{"plex.direct"}) || !d.DomainNeeded ||
		d.RateLimitIPv4Prefix != 32 || d.RateLimitIPv6Prefix != 64 || d.ECS.Mode != ECSOff || d.BootstrapPreferIPv6 {
		t.Fatalf("defaults: %+v", d)
	}
	for name, l := range map[string][]string{"privateReverseNetworks": d.PrivateReverseNetworks, "blockedClients": d.BlockedClients,
		"droppedDomains": d.DroppedDomains, "bogusNxdomain": d.BogusNXDomain, "ednsClientTrusted": d.EDNSClientTrusted} {
		if l == nil || len(l) != 0 {
			t.Errorf("%s: %v, want []", name, l)
		}
	}
}

// storedDoc builds a settings document of an older version: the defaults
// with the given dns members replaced (nil values are removed).
func storedDoc(t *testing.T, dns map[string]any) string {
	t.Helper()
	b, err := json.Marshal(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for k, v := range dns {
		if v == nil {
			delete(doc["dns"], k)
		} else {
			doc["dns"][k] = v
		}
	}
	if b, err = json.Marshal(doc); err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Settings v4: installations on the old default upstreams get Quad9 and
// the default fallback; custom lists get no fallback; an existing
// fallback list is left alone.
func TestMigrateUpstreamsV4(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	oldDefault := []string{"https://dns.quad9.net/dns-query", "https://cloudflare-dns.com/dns-query"}
	quad9 := []string{"https://dns.quad9.net/dns-query"}
	defFallback := Defaults().DNS.FallbackUpstreams
	for _, tc := range []struct {
		name          string
		dns           map[string]any
		wantUps       []string
		wantFallbacks []string
	}{
		{"old default", map[string]any{"upstreams": oldDefault, "fallbackUpstreams": nil}, quad9, defFallback},
		{"custom list", map[string]any{"upstreams": []string{"https://dns.example/dns-query"}, "fallbackUpstreams": nil},
			[]string{"https://dns.example/dns-query"}, []string{}},
		{"reordered old default is custom", map[string]any{"upstreams": []string{oldDefault[1], oldDefault[0]}, "fallbackUpstreams": nil},
			[]string{oldDefault[1], oldDefault[0]}, []string{}},
		{"quad9 only", map[string]any{"upstreams": quad9, "fallbackUpstreams": nil}, quad9, defFallback},
		{"missing upstreams", map[string]any{"upstreams": nil, "fallbackUpstreams": nil}, quad9, defFallback},
		{"existing fallbacks untouched", map[string]any{"upstreams": []string{"9.9.9.9"}, "fallbackUpstreams": []string{"1.1.1.1"}},
			[]string{"9.9.9.9"}, []string{"1.1.1.1"}},
		{"existing empty fallbacks untouched", map[string]any{"upstreams": oldDefault, "fallbackUpstreams": []string{}}, quad9, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Migrate(ctx, "settings", migrations[:3]); err != nil {
				t.Fatal(err)
			}
			if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, storedDoc(t, tc.dns)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, d, log)
			if err != nil {
				t.Fatal(err)
			}
			got := s.Get().DNS
			if !slices.Equal(got.Upstreams, tc.wantUps) || !slices.Equal(got.FallbackUpstreams, tc.wantFallbacks) {
				t.Fatalf("upstreams %v fallbacks %v, want %v %v", got.Upstreams, got.FallbackUpstreams, tc.wantUps, tc.wantFallbacks)
			}
			if err := s.Get().Validate(); err != nil {
				t.Fatalf("migrated document invalid: %v", err)
			}
		})
	}
}

// An installation with IP-only upstreams and no bootstrap servers stays
// valid after the upgrade (no fallback by name is added), and any later
// change of another section saves.
func TestMigrateIPOnlyWithoutBootstrap(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, "settings", migrations[:3]); err != nil {
		t.Fatal(err)
	}
	doc := storedDoc(t, map[string]any{"upstreams": []string{"192.168.1.1"}, "bootstrap": []string{}, "fallbackUpstreams": nil,
		"rebindProtection": nil, "domainNeeded": nil, "rebindAllow": nil, "upstreamBlockedTtl": nil, "ecs": nil,
		"rateLimitIpv4Prefix": nil, "rateLimitIpv6Prefix": nil})
	if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, doc); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	got := s.Get().DNS
	if err := s.Get().Validate(); err != nil || len(got.FallbackUpstreams) != 0 {
		t.Fatalf("after the upgrade: %v, fallbacks %v", err, got.FallbackUpstreams)
	}
	// The new members of 0.9.0 decode to their defaults (ARCHITECTURE 13).
	if !got.RebindProtection || !got.DomainNeeded || !slices.Equal(got.RebindAllow, []string{"plex.direct"}) ||
		got.UpstreamBlockedTTL != 300 || got.ECS.Mode != ECSOff || got.RateLimitIPv4Prefix != 32 || got.RateLimitIPv6Prefix != 64 {
		t.Fatalf("new members: %+v", got)
	}
	for _, fn := range []func(*All){
		func(a *All) { a.Filter.BlockedTTL = 20 },
		func(a *All) { a.Web.SessionIdleMinutes = 30 },
		func(a *All) { a.DNS.RateLimitQPS = 100; a.DNS.RateLimitBurst = 400 },
	} {
		if _, err := s.Update(ctx, func(a *All) error { fn(a); return nil }); err != nil {
			t.Fatalf("later change: %v", err)
		}
	}
	// A fallback by name needs bootstrap servers.
	_, err = s.Update(ctx, func(a *All) error { a.DNS.FallbackUpstreams = []string{"tls://dns.example"}; return nil })
	if e, ok := apperr.As(err); !ok || e.Field != "dns.bootstrap" || e.Message != "required when an upstream or fallback is given by host name" {
		t.Fatalf("fallback by name without bootstrap: %v", err)
	}
}

func TestPublicUpstreamName(t *testing.T) {
	for host, want := range map[string]bool{
		"dns.example.com":        true,
		"DNS.Example.COM.":       true,
		"unbound":                false, // one label
		"nas.lan":                false, // the local domain
		"lan":                    false,
		"router.home.arpa":       false,
		"x.localhost":            false,
		"a.test":                 false,
		"a.invalid":              false,
		"a.onion":                false,
		"printer.local":          false,
		"db.internal":            false,
		"1.1.1.1.in-addr.arpa":   false,
		"dns.corp.example":       false, // an extra zone (search domain)
		"dns.other.example":      true,
		"bad_host!.example.com":  false,
		"xn--bcher-kva.example":  true,
		"-leading.example.com":   false,
		"dns.lan.example.com":    true, // lan only as a middle label
		"trailing.example.com..": false,
		"192.168.1781":           false, // a mistyped IPv4 address (all-numeric last label)
		"10.0.0":                 false,
		"dns.example.123":        false,
		"9.dns.example":          true,
	} {
		if got := PublicUpstreamName(host, "lan", "corp.example"); got != want {
			t.Errorf("PublicUpstreamName(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestReverseZone(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.0/8":          "10.in-addr.arpa",
		"192.168.5.0/24":      "5.168.192.in-addr.arpa",
		"172.20.0.0/16":       "20.172.in-addr.arpa",
		"fd12::/16":           "2.1.d.f.ip6.arpa",
		"fd00::/8":            "",
		"2001:db8:1234::/48":  "4.3.2.1.8.b.d.0.1.0.0.2.ip6.arpa",
		"2001:db8::/124":      strings.Repeat("0.", 23) + "8.b.d.0.1.0.0.2.ip6.arpa",
		"192.168.5.0/23":      "",
		"10.0.0.0/7":          "",
		"2001:db8::/15":       "",
		"2001:db8::/126":      "",
		"2001:db8::/50":       "",
		"::ffff:10.0.0.0/104": "",
	} {
		got, ok := ReverseZone(netip.MustParsePrefix(in))
		if got != want || ok != (want != "") {
			t.Errorf("ReverseZone(%s) = %q %v, want %q", in, got, ok, want)
		}
	}
}

func TestParseDroppedDomain(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":            "example.com",
		" Example.COM. ":         "example.com",
		"example.com:aaaa":       "example.com:AAAA",
		"example.com:TYPE28":     "example.com:AAAA",
		"example.com:type65":     "example.com:HTTPS",
		"example.com:TYPE65280":  "example.com:TYPE65280",
		"example.com:TYPE65535":  "example.com:TYPE65535",
		"example.com:TYPE0":      "",
		"example.com:TYPE65536":  "",
		"example.com:TYPE028":    "",
		"example.com:NOPE":       "",
		"example.com:":           "",
		"example.com:NONE":       "",
		"com":                    "",
		"in-addr.arpa":           "",
		"1.168.192.in-addr.arpa": "",
		"a.localhost":            "",
		"x.local":                "",
		"x.internal":             "",
		"x.onion":                "",
		"bad name.com":           "",
	} {
		d, err := ParseDroppedDomain(in)
		if (err == nil) != (want != "") || (err == nil && d.String() != want) {
			t.Errorf("ParseDroppedDomain(%q) = %q %v, want %q", in, d.String(), err, want)
		}
	}
}

func TestParseBlockedClient(t *testing.T) {
	for in, want := range map[string]string{
		"192.168.1.5":          "192.168.1.5",
		"::ffff:192.168.1.5":   "192.168.1.5",
		"192.168.1.77/24":      "192.168.1.0/24",
		"192.168.1.5/32":       "192.168.1.5",
		"10.0.0.0/7":           "",
		"10.0.0.0/8":           "10.0.0.0/8",
		"2001:DB8::1":          "2001:db8::1",
		"2001:db8::/32":        "2001:db8::/32",
		"2001:db8::/31":        "",
		"fe80::1%eth0":         "",
		"fe80::/64":            "fe80::/64",
		"AA-BB-CC-DD-EE-FF":    "aa:bb:cc:dd:ee:ff",
		"aa:bb:cc:dd:ee:ff":    "aa:bb:cc:dd:ee:ff",
		"00:00:00:00:00:00":    "",
		"01:00:5e:00:00:01":    "", // multicast
		"ff:ff:ff:ff:ff:ff":    "",
		"aa:bb:cc:dd:ee":       "",
		"aa:bb:cc:dd:ee:ff:00": "",
		"host.example":         "",
		"":                     "",
	} {
		got, ok := ParseBlockedClient(in)
		if got != want || ok != (want != "") {
			t.Errorf("ParseBlockedClient(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
}

// Every new DNS member: normalisation of valid values and the field of
// the validation error.
func TestDNSListsValidation(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s, err := Open(ctx, d, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	many := func(n int, f func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		fn    func(*DNS)
		field string
		check func(t *testing.T, d DNS)
	}{
		{"fallbacks", func(d *DNS) { d.FallbackUpstreams = []string{" 1.1.1.1 ", "1.1.1.1", "tls://dns.example"} }, "", func(t *testing.T, d DNS) {
			if !slices.Equal(d.FallbackUpstreams, []string{"1.1.1.1", "tls://dns.example"}) {
				t.Errorf("fallbacks %v", d.FallbackUpstreams)
			}
		}},
		{"too many fallbacks", func(d *DNS) {
			d.FallbackUpstreams = many(5, func(i int) string { return "1.1.1." + string(rune('1'+i)) })
		}, "dns.fallbackUpstreams", nil},
		{"invalid fallback", func(d *DNS) { d.FallbackUpstreams = []string{"1.1.1.1", "ftp://x"} }, "dns.fallbackUpstreams[1]", nil},
		{"private fallback name", func(d *DNS) { d.FallbackUpstreams = []string{"udp://dns.lan"} }, "dns.fallbackUpstreams[0]", nil},
		{"plain upstream by public name", func(d *DNS) { d.Upstreams = []string{"dns.example.com", "tcp://dns.example.net:5353"} }, "", nil},
		{"plain upstream by single label", func(d *DNS) { d.Upstreams = []string{"unbound"} }, "dns.upstreams[0]", nil},
		{"plain upstream below the local domain", func(d *DNS) { d.Upstreams = []string{"9.9.9.9", "nas.lan:53"} }, "dns.upstreams[1]", nil},
		{"plain upstream below home.arpa", func(d *DNS) { d.Upstreams = []string{"tcp://dns.home.arpa"} }, "dns.upstreams[0]", nil},
		{"fastest_addr", func(d *DNS) { d.UpstreamMode = "fastest_addr" }, "", nil},
		{"unknown mode", func(d *DNS) { d.UpstreamMode = "fastest" }, "dns.upstreamMode", nil},
		{"blocked ttl low", func(d *DNS) { d.UpstreamBlockedTTL = 9 }, "dns.upstreamBlockedTtl", nil},
		{"blocked ttl high", func(d *DNS) { d.UpstreamBlockedTTL = 86401 }, "dns.upstreamBlockedTtl", nil},
		{"rebind allow", func(d *DNS) { d.RebindAllow = []string{" Plex.Direct. ", "plex.direct", "LAN"} }, "", func(t *testing.T, d DNS) {
			if !slices.Equal(d.RebindAllow, []string{"plex.direct", "lan"}) {
				t.Errorf("rebindAllow %v", d.RebindAllow)
			}
		}},
		{"rebind allow invalid", func(d *DNS) { d.RebindAllow = []string{"ok.example", "not a name"} }, "dns.rebindAllow[1]", nil},
		{"rebind allow count", func(d *DNS) { d.RebindAllow = many(257, func(i int) string { return "d" + itoa(i) + ".example" }) }, "dns.rebindAllow", nil},
		{"reverse networks", func(d *DNS) {
			d.PrivateReverseNetworks = []string{"192.168.5.77/24", "192.168.5.0/24", "2001:DB8::/48"}
		}, "", func(t *testing.T, d DNS) {
			if !slices.Equal(d.PrivateReverseNetworks, []string{"192.168.5.0/24", "2001:db8::/48"}) {
				t.Errorf("privateReverseNetworks %v", d.PrivateReverseNetworks)
			}
			if z := d.PrivateReverseZones(); !slices.Equal(z, []string{"5.168.192.in-addr.arpa", "0.0.0.0.8.b.d.0.1.0.0.2.ip6.arpa"}) {
				t.Errorf("zones %v", z)
			}
		}},
		{"reverse network /23", func(d *DNS) { d.PrivateReverseNetworks = []string{"10.0.0.0/23"} }, "dns.privateReverseNetworks[0]", nil},
		{"reverse network zone", func(d *DNS) { d.PrivateReverseNetworks = []string{"fe80::%eth0/64"} }, "dns.privateReverseNetworks[0]", nil},
		{"reverse network address", func(d *DNS) { d.PrivateReverseNetworks = []string{"10.0.0.1"} }, "dns.privateReverseNetworks[0]", nil},
		{"reverse networks count", func(d *DNS) {
			d.PrivateReverseNetworks = many(33, func(i int) string { return "10." + itoa(i) + ".0.0/16" })
		}, "dns.privateReverseNetworks", nil},
		{"blocked clients", func(d *DNS) {
			d.BlockedClients = []string{"192.168.1.9/24", "192.168.1.0/24", "AA-BB-CC-DD-EE-01", "::ffff:10.1.2.3", "2001:db8::1/128"}
		}, "", func(t *testing.T, d DNS) {
			if !slices.Equal(d.BlockedClients, []string{"192.168.1.0/24", "aa:bb:cc:dd:ee:01", "10.1.2.3", "2001:db8::1"}) {
				t.Errorf("blockedClients %v", d.BlockedClients)
			}
		}},
		{"blocked client too broad", func(d *DNS) { d.BlockedClients = []string{"10.0.0.1", "0.0.0.0/0"} }, "dns.blockedClients[1]", nil},
		{"blocked client zone", func(d *DNS) { d.BlockedClients = []string{"fe80::1%eth0"} }, "dns.blockedClients[0]", nil},
		{"blocked clients count", func(d *DNS) {
			d.BlockedClients = many(257, func(i int) string { return "10.0." + itoa(i/250) + "." + itoa(i%250) })
		}, "dns.blockedClients", nil},
		{"rate limit prefixes", func(d *DNS) { d.RateLimitIPv4Prefix, d.RateLimitIPv6Prefix = 24, 56 }, "", nil},
		{"rate limit ipv4 prefix", func(d *DNS) { d.RateLimitIPv4Prefix = 7 }, "dns.rateLimitIpv4Prefix", nil},
		{"rate limit ipv4 prefix high", func(d *DNS) { d.RateLimitIPv4Prefix = 33 }, "dns.rateLimitIpv4Prefix", nil},
		{"rate limit ipv6 prefix", func(d *DNS) { d.RateLimitIPv6Prefix = 65 }, "dns.rateLimitIpv6Prefix", nil},
		{"rate limit ipv6 prefix low", func(d *DNS) { d.RateLimitIPv6Prefix = 31 }, "dns.rateLimitIpv6Prefix", nil},
		{"dropped domains", func(d *DNS) { d.DroppedDomains = []string{"Example.com:type28", "example.com:AAAA", "ads.example"} }, "", func(t *testing.T, d DNS) {
			if !slices.Equal(d.DroppedDomains, []string{"example.com:AAAA", "ads.example"}) {
				t.Errorf("droppedDomains %v", d.DroppedDomains)
			}
		}},
		{"dropped domain arpa", func(d *DNS) { d.DroppedDomains = []string{"x.example", "10.in-addr.arpa"} }, "dns.droppedDomains[1]", nil},
		{"dropped domains count", func(d *DNS) { d.DroppedDomains = many(257, func(i int) string { return "d" + itoa(i) + ".example" }) }, "dns.droppedDomains", nil},
		{"bogus nxdomain", func(d *DNS) { d.BogusNXDomain = []string{"198.51.100.7", "203.0.113.9/24", "2001:db8::/32"} }, "", func(t *testing.T, d DNS) {
			if !slices.Equal(d.BogusNXDomain, []string{"198.51.100.7", "203.0.113.0/24", "2001:db8::/32"}) {
				t.Errorf("bogusNxdomain %v", d.BogusNXDomain)
			}
		}},
		{"bogus nxdomain too broad", func(d *DNS) { d.BogusNXDomain = []string{"2001:db8::/16"} }, "dns.bogusNxdomain[0]", nil},
		{"bogus nxdomain count", func(d *DNS) { d.BogusNXDomain = many(65, func(i int) string { return "198.51.100." + itoa(i) }) }, "dns.bogusNxdomain", nil},
		{"trusted", func(d *DNS) {
			d.EDNSClientTrusted = []string{"192.168.1.1/32", "::1", "2001:db8::53/128", "192.168.1.1"}
		}, "", func(t *testing.T, d DNS) {
			if !slices.Equal(d.EDNSClientTrusted, []string{"192.168.1.1", "::1", "2001:db8::53"}) {
				t.Errorf("ednsClientTrusted %v", d.EDNSClientTrusted)
			}
		}},
		{"trusted network", func(d *DNS) { d.EDNSClientTrusted = []string{"192.168.1.0/24"} }, "dns.ednsClientTrusted[0]", nil},
		{"trusted unspecified", func(d *DNS) { d.EDNSClientTrusted = []string{"0.0.0.0"} }, "dns.ednsClientTrusted[0]", nil},
		{"trusted multicast", func(d *DNS) { d.EDNSClientTrusted = []string{"10.0.0.1", "ff02::1"} }, "dns.ednsClientTrusted[1]", nil},
		{"trusted zone", func(d *DNS) { d.EDNSClientTrusted = []string{"fe80::1%eth0"} }, "dns.ednsClientTrusted[0]", nil},
		{"trusted count", func(d *DNS) { d.EDNSClientTrusted = many(17, func(i int) string { return "10.0.0." + itoa(i+1) }) }, "dns.ednsClientTrusted", nil},
		{"ecs client", func(d *DNS) { d.ECS = ECS{Mode: " CLIENT "} }, "", func(t *testing.T, d DNS) {
			if d.ECS.Mode != ECSClient {
				t.Errorf("ecs %+v", d.ECS)
			}
		}},
		{"ecs empty mode is off", func(d *DNS) { d.ECS = ECS{} }, "", func(t *testing.T, d DNS) {
			if d.ECS.Mode != ECSOff {
				t.Errorf("ecs %+v", d.ECS)
			}
		}},
		{"ecs custom", func(d *DNS) { d.ECS = ECS{Mode: "custom", CustomSubnet: "198.51.100.77/24"} }, "", func(t *testing.T, d DNS) {
			if d.ECS.CustomSubnet != "198.51.100.0/24" || d.ECSSubnet() != netip.MustParsePrefix("198.51.100.0/24") {
				t.Errorf("ecs %+v", d.ECS)
			}
		}},
		{"ecs custom ipv6", func(d *DNS) { d.ECS = ECS{Mode: "custom", CustomSubnet: "2001:DB8:1234::/48"} }, "", nil},
		{"ecs unknown mode", func(d *DNS) { d.ECS = ECS{Mode: "on"} }, "dns.ecs.mode", nil},
		{"ecs custom without subnet", func(d *DNS) { d.ECS = ECS{Mode: "custom"} }, "dns.ecs.customSubnet", nil},
		{"ecs subnet too long", func(d *DNS) { d.ECS = ECS{Mode: "off", CustomSubnet: "198.51.100.0/25"} }, "dns.ecs.customSubnet", nil},
		{"ecs subnet too broad", func(d *DNS) { d.ECS = ECS{Mode: "custom", CustomSubnet: "2001:db8::/31"} }, "dns.ecs.customSubnet", nil},
		{"ecs subnet ipv6 too long", func(d *DNS) { d.ECS = ECS{Mode: "custom", CustomSubnet: "2001:db8::/64"} }, "dns.ecs.customSubnet", nil},
		{"ecs subnet address", func(d *DNS) { d.ECS = ECS{Mode: "custom", CustomSubnet: "198.51.100.1"} }, "dns.ecs.customSubnet", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := s.Update(ctx, func(a *All) error {
				a.DNS = Defaults().DNS
				tc.fn(&a.DNS)
				return nil
			})
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				if tc.check != nil {
					tc.check(t, next.DNS)
				}
				return
			}
			if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindInvalid || e.Field != tc.field {
				t.Fatalf("err = %v, want invalid %s", err, tc.field)
			}
		})
	}
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
