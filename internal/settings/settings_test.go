package settings

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

func TestDefaultsValid(t *testing.T) {
	d := Defaults()
	if err := d.Validate(); err != nil {
		t.Fatalf("defaults invalid: %v", err)
	}
}

func TestUpdatePersistsAndNotifies(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "s.db")
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	var got *All
	unsub := s.Subscribe(func(_, n *All) { got = n })
	if _, err := s.Update(ctx, func(a *All) error { a.DNS.RateLimitQPS = 0; return nil }); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.DNS.RateLimitQPS != 0 {
		t.Fatal("listener not called with new value")
	}
	unsub()
	_, err = s.Update(ctx, func(a *All) error { a.DNS.Upstreams = nil; return nil })
	if apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("want invalid, got %v", err)
	}
	if len(s.Get().DNS.Upstreams) == 0 {
		t.Fatal("failed update must not change state")
	}
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if _, err := s.Update(ctx, func(a *All) error { a.Filter.PausedUntil = &until; return nil }); err != nil {
		t.Fatal(err)
	}
	d.Close()

	d2, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	s2, err := Open(ctx, d2, log)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get().DNS.RateLimitQPS != 0 {
		t.Fatal("update not persisted")
	}
	if p := s2.Get().Filter.PausedUntil; p == nil || !p.Equal(until) {
		t.Fatalf("pausedUntil not persisted: %v", p)
	}
	if s2.Get().Filter.BlockingActive(time.Now()) {
		t.Fatal("blocking should be paused")
	}
}

func TestParseUpstream(t *testing.T) {
	cases := []struct {
		in    string
		proto string
		addr  string
		ok    bool
	}{
		{"9.9.9.9", "udp", "9.9.9.9:53", true},
		{"tcp://1.1.1.1:5353", "tcp", "1.1.1.1:5353", true},
		{"[2620:fe::fe]:53", "udp", "[2620:fe::fe]:53", true},
		{"[fe80::1%eth0]:53", "udp", "[fe80::1%eth0]:53", true}, // a router on its link-local address
		{"[fe80::1%25eth0]:53", "udp", "[fe80::1%eth0]:53", true},
		{"tls://dns.quad9.net", "tls", "dns.quad9.net:853", true},
		{"https://dns.quad9.net/dns-query", "https", "dns.quad9.net:443", true},
		{"https://dns.quad9.net", "https", "dns.quad9.net:443", true},
		{"dns.quad9.net", "", "", false},
		{"ftp://x", "", "", false},
		{"https://user:pw@x.example/dns-query", "", "", false},
	}
	for _, c := range cases {
		spec, err := ParseUpstream(c.in)
		if (err == nil) != c.ok {
			t.Errorf("%q: err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && (spec.Proto != c.proto || spec.Addr() != c.addr) {
			t.Errorf("%q: got %s %s", c.in, spec.Proto, spec.Addr())
		}
	}
}

// A document stored by a version without the updates section gets its
// defaults: daily checks on, stable releases only.
func TestUpdatesDefaultsForOlderDocuments(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if u := s.Get().Updates; !u.CheckEnabled || u.IncludePrereleases {
		t.Fatalf("defaults: %+v", u)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_remove(doc, '$.updates')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_set(doc, '$.web.language', 'de')`); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if u := s.Get().Updates; !u.CheckEnabled || u.IncludePrereleases || s.Get().Web.Language != "de" {
		t.Fatalf("older document: %+v", s.Get())
	}
	if _, err := s.Update(ctx, func(a *All) error { a.Updates = Updates{CheckEnabled: false, IncludePrereleases: true}; return nil }); err != nil {
		t.Fatal(err)
	}
	if u := s.Get().Updates; u.CheckEnabled || !u.IncludePrereleases {
		t.Fatalf("after update: %+v", u)
	}
}

// A document of 0.1.x (settings schema v1) has the download cache section
// under its old name. Settings v2 moves it to "downloadCache" once, at the
// first Open; a restored older backup has schema v1 as well and is moved at
// the start that applies it. An existing "downloadCache" section wins, and
// a document that is not JSON is left to the decoder's error.
func TestMigrateDownloadCacheSection(t *testing.T) {
	const oldKey = "lancache" // section name of 0.1.x
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	section := func(fn func(*DownloadCache)) jsontext.Value {
		c := Defaults().DownloadCache
		fn(&c)
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	doc := func(members map[string]jsontext.Value) string {
		b, err := json.Marshal(Defaults())
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]jsontext.Value
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		delete(m, "downloadCache")
		maps.Copy(m, members)
		if b, err = json.Marshal(m); err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	old := section(func(c *DownloadCache) {
		c.Enabled, c.CacheIPv4, c.DisabledServices = true, []string{"192.168.1.2"}, []string{"blizzard"}
	})
	cur := section(func(c *DownloadCache) { c.CacheIPv4 = []string{"10.0.0.9"} })
	tests := []struct {
		name     string
		doc      string
		want     DownloadCache // compared: Enabled, CacheIPv4, DisabledServices
		stored   bool          // "downloadCache" is in the stored document afterwards
		openFail string        // Open error substring
	}{
		{name: "old section", doc: doc(map[string]jsontext.Value{oldKey: old}), stored: true,
			want: DownloadCache{Enabled: true, CacheIPv4: []string{"192.168.1.2"}, DisabledServices: []string{"blizzard"}}},
		{name: "new section wins", doc: doc(map[string]jsontext.Value{oldKey: old, "downloadCache": cur}), stored: true,
			want: DownloadCache{CacheIPv4: []string{"10.0.0.9"}, DisabledServices: []string{"test"}}},
		{name: "old section null", doc: doc(map[string]jsontext.Value{oldKey: jsontext.Value("null")}),
			want: DownloadCache{CacheIPv4: []string{}, DisabledServices: []string{"test"}}},
		{name: "no section", doc: doc(nil),
			want: DownloadCache{CacheIPv4: []string{}, DisabledServices: []string{"test"}}},
		{name: "not JSON", doc: `{"` + oldKey + `": {`, openFail: "decode stored document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Migrate(ctx, "settings", migrations[:1]); err != nil {
				t.Fatal(err)
			}
			if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, tt.doc); err != nil {
				t.Fatal(err)
			}
			for range 2 { // the second Open finds the migrated document
				s, err := Open(ctx, d, log)
				if tt.openFail != "" {
					if err == nil || !strings.Contains(err.Error(), tt.openFail) {
						t.Fatalf("Open = %v, want an error with %q", err, tt.openFail)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				got := s.Get().DownloadCache
				if got.Enabled != tt.want.Enabled || !slices.Equal(got.CacheIPv4, tt.want.CacheIPv4) ||
					!slices.Equal(got.DisabledServices, tt.want.DisabledServices) {
					t.Fatalf("download cache settings %+v, want %+v", got, tt.want)
				}
				if got.DNSTTL != 60 || got.DomainsSource == "" {
					t.Fatalf("members not in the stored section must keep their defaults: %+v", got)
				}
				var oldType, newType sql.NullString
				if err := d.R.QueryRowContext(ctx, `SELECT json_type(doc, '$.`+oldKey+`'), json_type(doc, '$.downloadCache')
					FROM settings`).Scan(&oldType, &newType); err != nil {
					t.Fatal(err)
				}
				if oldType.Valid || newType.Valid != tt.stored {
					t.Fatalf("stored document: old section %v, downloadCache %v (want %v)", oldType, newType, tt.stored)
				}
			}
			var v int
			if err := d.R.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations WHERE component = 'settings'`).
				Scan(&v); err != nil || v != len(migrations) {
				t.Fatalf("settings schema version %d (%v), want %d", v, err, len(migrations))
			}
		})
	}
}

// The backups section: defaults for documents of older versions, then
// validation and normalisation of every member.
func TestBackupsSection(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.DiscardHandler)
	if _, err := Open(ctx, d, log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_remove(doc, '$.backups')`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	want := Backups{Schedule: "daily", Time: "03:30", Keep: 7, Destination: BackupsLocal}
	if got := s.Get().Backups; got != want {
		t.Fatalf("defaults of an older document: %+v", got)
	}

	for _, tc := range []struct {
		name  string
		fn    func(*Backups)
		field string
	}{
		{"weekly", func(b *Backups) { b.Schedule = " Weekly "; b.Weekday = 6 }, ""},
		{"target", func(b *Backups) { b.Destination = "0123456789ABCDEF0123456789abcdef" }, ""},
		{"midnight", func(b *Backups) { b.Time = "00:00" }, ""},
		{"last minute", func(b *Backups) { b.Time = " 23:59 " }, ""},
		{"schedule", func(b *Backups) { b.Schedule = "hourly" }, "backups.schedule"},
		{"no time", func(b *Backups) { b.Time = "" }, "backups.time"},
		{"hour 24", func(b *Backups) { b.Time = "24:00" }, "backups.time"},
		{"minute 60", func(b *Backups) { b.Time = "12:60" }, "backups.time"},
		{"one digit", func(b *Backups) { b.Time = "3:30" }, "backups.time"},
		{"seconds", func(b *Backups) { b.Time = "03:30:00" }, "backups.time"},
		{"sign", func(b *Backups) { b.Time = "+3:30" }, "backups.time"},
		{"weekday low", func(b *Backups) { b.Weekday = -1 }, "backups.weekday"},
		{"weekday high", func(b *Backups) { b.Weekday = 7 }, "backups.weekday"},
		{"keep 0", func(b *Backups) { b.Keep = 0 }, "backups.keep"},
		{"keep 91", func(b *Backups) { b.Keep = 91 }, "backups.keep"},
		{"destination", func(b *Backups) { b.Destination = "../../etc" }, "backups.destination"},
		{"short id", func(b *Backups) { b.Destination = "0123456789abcdef" }, "backups.destination"},
		{"empty destination", func(b *Backups) { b.Destination = "" }, "backups.destination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := s.Update(ctx, func(a *All) error { a.Backups = want; tc.fn(&a.Backups); return nil })
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				b := next.Backups
				if b.Schedule != "daily" && b.Schedule != "weekly" || strings.TrimSpace(b.Time) != b.Time ||
					strings.ToLower(b.Destination) != b.Destination {
					t.Fatalf("not normalised: %+v", b)
				}
				return
			}
			if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindInvalid || e.Field != tc.field {
				t.Fatalf("err = %v, want invalid %s", err, tc.field)
			}
		})
	}
}

// Settings v3 appends the IPv6 bootstrap servers to a stored list that
// equals the old default exactly; edited lists and newer documents are left
// alone.
func TestMigrateBootstrapIPv6(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	oldDefault := []string{"9.9.9.9", "149.112.112.112", "1.1.1.1", "1.0.0.1"}
	for _, tc := range []struct {
		name   string
		stored any // the stored dns.bootstrap member (nil: absent)
		want   []string
	}{
		{"old default", oldDefault, Defaults().DNS.Bootstrap},
		{"edited", []string{"9.9.9.9", "1.1.1.1"}, []string{"9.9.9.9", "1.1.1.1"}},
		{"reordered", []string{"1.1.1.1", "1.0.0.1", "9.9.9.9", "149.112.112.112"}, []string{"1.1.1.1", "1.0.0.1", "9.9.9.9", "149.112.112.112"}},
		{"extended", append(slices.Clone(oldDefault), "8.8.8.8"), append(slices.Clone(oldDefault), "8.8.8.8")},
		{"empty", []string{}, []string{}},
		{"absent", nil, Defaults().DNS.Bootstrap},
		{"not a list", "9.9.9.9", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Migrate(ctx, "settings", migrations[:2]); err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(Defaults())
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]map[string]any
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			delete(doc["dns"], "bootstrap")
			if tc.stored != nil {
				doc["dns"]["bootstrap"] = tc.stored
			}
			if b, err = json.Marshal(doc); err != nil {
				t.Fatal(err)
			}
			if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, string(b)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, d, log)
			if tc.want == nil {
				if err == nil {
					t.Fatal("a bootstrap member that is not a list must stay (and fail to decode)")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Get().DNS.Bootstrap; !slices.Equal(got, tc.want) {
				t.Fatalf("bootstrap %v, want %v", got, tc.want)
			}
		})
	}
}

// DNS64: only IPv6 /96 prefixes (normalised), and never together with
// dns.disableAAAA.
func TestDNS64AndDisableAAAAValidation(t *testing.T) {
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
	if got := s.Get().DNS; got.DisableAAAA || got.DNS64.Enabled || got.DNS64.Prefix != "64:ff9b::/96" || got.TrustConnectedNetworks {
		t.Fatalf("defaults: %+v", got)
	}
	for _, tc := range []struct {
		name   string
		fn     func(*DNS)
		field  string
		prefix string // the stored prefix when valid
	}{
		{"enabled", func(d *DNS) { d.DNS64.Enabled = true }, "", "64:ff9b::/96"},
		{"network-specific", func(d *DNS) { d.DNS64 = DNS64{Enabled: true, Prefix: " 2001:DB8:64::/96 "} }, "", "2001:db8:64::/96"},
		{"host bits masked", func(d *DNS) { d.DNS64.Prefix = "2001:db8:64::1/96" }, "", "2001:db8:64::/96"},
		{"empty means default", func(d *DNS) { d.DNS64.Prefix = "" }, "", "64:ff9b::/96"},
		{"disable AAAA alone", func(d *DNS) { d.DisableAAAA = true }, "", "64:ff9b::/96"},
		{"not /96", func(d *DNS) { d.DNS64.Prefix = "64:ff9b::/64" }, "dns.dns64.prefix", ""},
		{"IPv4", func(d *DNS) { d.DNS64.Prefix = "10.0.0.0/8" }, "dns.dns64.prefix", ""},
		{"IPv4-mapped", func(d *DNS) { d.DNS64.Prefix = "::ffff:0:0/96" }, "dns.dns64.prefix", ""},
		{"IPv4-compatible", func(d *DNS) { d.DNS64.Prefix = "::/96" }, "dns.dns64.prefix", ""},
		{"multicast", func(d *DNS) { d.DNS64.Prefix = "ff0e::/96" }, "dns.dns64.prefix", ""},
		{"garbage", func(d *DNS) { d.DNS64.Prefix = "nat64" }, "dns.dns64.prefix", ""},
		{"both", func(d *DNS) { d.DisableAAAA = true; d.DNS64.Enabled = true }, "dns.disableAAAA", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := s.Update(ctx, func(a *All) error {
				a.DNS.DisableAAAA, a.DNS.DNS64 = false, DNS64{Prefix: DefaultDNS64Prefix}
				tc.fn(&a.DNS)
				return nil
			})
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				if next.DNS.DNS64.Prefix != tc.prefix {
					t.Fatalf("prefix %q, want %q", next.DNS.DNS64.Prefix, tc.prefix)
				}
				return
			}
			if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindInvalid || e.Field != tc.field {
				t.Fatalf("err = %v, want invalid %s", err, tc.field)
			}
		})
	}
}

// The dhcp section: defaults for older documents, normalisation and the
// form checks (the live interface is checked by the DHCP service).
func TestDHCPSection(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.DiscardHandler)
	if _, err := Open(ctx, d, log); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_remove(doc, '$.dhcp')`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	want := DHCP{LeaseSeconds: 86400, RegisterHostnames: true, GenerateNames: true,
		Options: DHCPOptions{NTPServers: []string{}, ExtraSearchDomains: []string{}}}
	if got := s.Get().DHCP; !got.Equal(want) || got.Options.NTPServers == nil || got.Options.ExtraSearchDomains == nil {
		t.Fatalf("defaults of an older document: %+v", got)
	}
	// A 0.7 document (dhcp without the new members) loads with
	// generateNames on and rapidCommit and onlyReserved off.
	if _, err := d.W.ExecContext(ctx, `UPDATE settings SET doc = json_set(doc, '$.dhcp', json('{"enabled":false,"leaseSeconds":3600,"registerHostnames":true,"ipv6":{"routerAdvertisements":true,"dhcpv6":false}}'))`); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(ctx, d, log); err != nil {
		t.Fatal(err)
	}
	if h := s.Get().DHCP; !h.GenerateNames || h.RapidCommit || h.OnlyReserved || h.LeaseSeconds != 3600 || !h.IPv6.RouterAdvertisements ||
		h.Options.NTPServers == nil {
		t.Fatalf("0.7 document: %+v", h)
	}
	on := DHCP{Enabled: true, Interface: "eth0", RangeStart: "192.168.1.100", RangeEnd: "192.168.1.199", LeaseSeconds: 3600}
	for _, tc := range []struct {
		name  string
		fn    func(*DHCP)
		field string
	}{
		{"enabled", func(h *DHCP) {}, ""},
		{"off without anything", func(h *DHCP) { *h = DHCP{LeaseSeconds: 300} }, ""},
		{"off, half a range", func(h *DHCP) { h.Enabled = false; h.RangeEnd = "" }, ""},
		{"spaces", func(h *DHCP) { h.RangeStart, h.Domain, h.Router = " 192.168.1.100 ", " Home.LAN. ", " 192.168.1.1 " }, ""},
		{"4096 addresses", func(h *DHCP) { h.RangeStart, h.RangeEnd = "10.0.0.0", "10.0.15.255" }, ""},
		{"all options", func(h *DHCP) { h.Router, h.DNSServer, h.Domain = "192.168.1.1", "192.168.1.2", "home.arpa" }, ""},
		{"no interface", func(h *DHCP) { h.Interface = "" }, "dhcp.interface"},
		{"bad interface", func(h *DHCP) { h.Interface = "eth0/../x" }, "dhcp.interface"},
		{"long interface", func(h *DHCP) { h.Interface = "a-very-long-interface" }, "dhcp.interface"},
		{"no start", func(h *DHCP) { h.RangeStart = "" }, "dhcp.rangeStart"},
		{"no end", func(h *DHCP) { h.RangeEnd = "" }, "dhcp.rangeEnd"},
		{"public start", func(h *DHCP) { h.RangeStart = "8.8.8.8" }, "dhcp.rangeStart"},
		{"IPv6 end", func(h *DHCP) { h.RangeEnd = "fd00::1" }, "dhcp.rangeEnd"},
		{"reversed", func(h *DHCP) { h.RangeStart, h.RangeEnd = "192.168.1.199", "192.168.1.100" }, "dhcp.rangeEnd"},
		{"4097 addresses", func(h *DHCP) { h.RangeStart, h.RangeEnd = "10.0.0.0", "10.0.16.0" }, "dhcp.rangeEnd"},
		{"lease short", func(h *DHCP) { h.LeaseSeconds = 299 }, "dhcp.leaseSeconds"},
		{"lease long", func(h *DHCP) { h.LeaseSeconds = 604801 }, "dhcp.leaseSeconds"},
		{"router", func(h *DHCP) { h.Router = "router" }, "dhcp.router"},
		{"router broadcast", func(h *DHCP) { h.Router = "255.255.255.255" }, "dhcp.router"},
		{"dns loopback", func(h *DHCP) { h.DNSServer = "127.0.0.1" }, "dhcp.dnsServer"},
		{"dns IPv6", func(h *DHCP) { h.DNSServer = "fd00::1" }, "dhcp.dnsServer"},
		{"domain", func(h *DHCP) { h.Domain = "bad_domain" }, "dhcp.domain"},
		{"options", func(h *DHCP) {
			h.Options = DHCPOptions{NTPServers: []string{" 192.168.1.2 ", "", "10.0.0.1"}, MTU: 1500, WPADURL: "https://proxy.lan:8443/wpad.dat",
				ExtraSearchDomains: []string{"Corp.Example.", "lab"}}
		}, ""},
		{"ntp count", func(h *DHCP) {
			h.Options.NTPServers = []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.4", "10.0.0.5"}
		}, "dhcp.options.ntpServers"},
		{"ntp name", func(h *DHCP) { h.Options.NTPServers = []string{"10.0.0.1", "pool.ntp.org"} }, "dhcp.options.ntpServers[1]"},
		{"ntp ipv6", func(h *DHCP) { h.Options.NTPServers = []string{"fd00::1"} }, "dhcp.options.ntpServers[0]"},
		{"ntp zero", func(h *DHCP) { h.Options.NTPServers = []string{"0.0.0.0"} }, "dhcp.options.ntpServers[0]"},
		{"ntp broadcast", func(h *DHCP) { h.Options.NTPServers = []string{"255.255.255.255"} }, "dhcp.options.ntpServers[0]"},
		{"ntp loopback", func(h *DHCP) { h.Options.NTPServers = []string{"127.0.0.1"} }, "dhcp.options.ntpServers[0]"},
		{"ntp multicast", func(h *DHCP) { h.Options.NTPServers = []string{"224.0.1.1"} }, "dhcp.options.ntpServers[0]"},
		{"ntp twice", func(h *DHCP) { h.Options.NTPServers = []string{"10.0.0.1", "10.0.0.1"} }, "dhcp.options.ntpServers[1]"},
		{"mtu low", func(h *DHCP) { h.Options.MTU = 575 }, "dhcp.options.mtu"},
		{"mtu high", func(h *DHCP) { h.Options.MTU = 9001 }, "dhcp.options.mtu"},
		{"mtu min", func(h *DHCP) { h.Options.MTU = 576 }, ""},
		{"wpad ftp", func(h *DHCP) { h.Options.WPADURL = "ftp://proxy.lan/wpad.dat" }, "dhcp.options.wpadUrl"},
		{"wpad relative", func(h *DHCP) { h.Options.WPADURL = "/wpad.dat" }, "dhcp.options.wpadUrl"},
		{"wpad user", func(h *DHCP) { h.Options.WPADURL = "http://user:pw@proxy.lan/wpad.dat" }, "dhcp.options.wpadUrl"},
		{"wpad unicode", func(h *DHCP) { h.Options.WPADURL = "http://prüfung.lan/wpad.dat" }, "dhcp.options.wpadUrl"},
		{"wpad space", func(h *DHCP) { h.Options.WPADURL = "http://proxy.lan/w pad.dat" }, "dhcp.options.wpadUrl"},
		{"wpad no host", func(h *DHCP) { h.Options.WPADURL = "http://:80/wpad.dat" }, "dhcp.options.wpadUrl"},
		{"wpad long", func(h *DHCP) { h.Options.WPADURL = "http://proxy.lan/" + strings.Repeat("a", 240) }, "dhcp.options.wpadUrl"},
		{"wpad punycode", func(h *DHCP) { h.Options.WPADURL = "http://xn--prfung-cxa.lan/wpad.dat" }, ""},
		{"search count", func(h *DHCP) { h.Options.ExtraSearchDomains = []string{"a", "b", "c", "d", "e"} }, "dhcp.options.extraSearchDomains"},
		{"search bad", func(h *DHCP) { h.Options.ExtraSearchDomains = []string{"ok", "bad_one"} }, "dhcp.options.extraSearchDomains[1]"},
		{"search twice", func(h *DHCP) { h.Options.ExtraSearchDomains = []string{"a.example", "A.example."} }, "dhcp.options.extraSearchDomains[1]"},
		{"search is the domain", func(h *DHCP) { h.Domain = "home.arpa"; h.Options.ExtraSearchDomains = []string{"home.arpa"} },
			"dhcp.options.extraSearchDomains[0]"},
		{"search is the local domain", func(h *DHCP) { h.Options.ExtraSearchDomains = []string{"x", "lan"} }, "dhcp.options.extraSearchDomains[1]"},
		{"search too long", func(h *DHCP) {
			h.Options.ExtraSearchDomains = []string{strings.Repeat("a", 63) + "." + strings.Repeat("b", 60), strings.Repeat("c", 63) + "." + strings.Repeat("d", 60)}
		}, "dhcp.options.extraSearchDomains"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next, err := s.Update(ctx, func(a *All) error { a.DHCP = on; tc.fn(&a.DHCP); return nil })
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				h := next.DHCP
				if strings.TrimSpace(h.RangeStart) != h.RangeStart || strings.Trim(strings.ToLower(h.Domain), ". ") != h.Domain ||
					strings.TrimSpace(h.Router) != h.Router {
					t.Fatalf("not normalised: %+v", h)
				}
				return
			}
			if e, ok := apperr.As(err); !ok || e.Kind != apperr.KindInvalid || e.Field != tc.field {
				t.Fatalf("err = %v, want invalid %s", err, tc.field)
			}
		})
	}
	// Normalised: trimmed, canonical, empty entries dropped, lists never nil.
	next, err := s.Update(ctx, func(a *All) error {
		a.DHCP = on
		a.DHCP.Options = DHCPOptions{NTPServers: []string{" 192.168.1.2 ", ""}, WPADURL: " http://p.lan/w.dat ", ExtraSearchDomains: []string{"Corp.Example."}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if o := next.DHCP.Options; !slices.Equal(o.NTPServers, []string{"192.168.1.2"}) || o.WPADURL != "http://p.lan/w.dat" ||
		!slices.Equal(o.ExtraSearchDomains, []string{"corp.example"}) {
		t.Fatalf("normalised %+v", o)
	}
	next, err = s.Update(ctx, func(a *All) error {
		a.DHCP.Options.NTPServers, a.DHCP.Options.ExtraSearchDomains = nil, nil
		return nil
	})
	if err != nil || next.DHCP.Options.NTPServers == nil || next.DHCP.Options.ExtraSearchDomains == nil {
		t.Fatalf("nil lists %+v %v", next.DHCP.Options, err)
	}
	// The search list is checked when the DHCP section changes; a later
	// change of the local domain is not refused because of it.
	if _, err := s.Update(ctx, func(a *All) error { a.DHCP.Options.ExtraSearchDomains = []string{"home.example"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, func(a *All) error { a.DNS.LocalDomain = "home.example"; return nil }); err != nil {
		t.Fatalf("local domain change refused: %v", err)
	}
	// Nor is any other DHCP change (switching it off, the lease time)
	// refused because of extras the local domain broke; changing the
	// extras (or dhcp.domain) checks them again.
	if _, err := s.Update(ctx, func(a *All) error { a.DHCP.Enabled, a.DHCP.LeaseSeconds = false, 3600; return nil }); err != nil {
		t.Fatalf("switching DHCP off refused: %v", err)
	}
	_, err = s.Update(ctx, func(a *All) error {
		a.DHCP.Options.ExtraSearchDomains = []string{"x.example", "home.example"}
		return nil
	})
	if e, ok := apperr.As(err); !ok || e.Field != "dhcp.options.extraSearchDomains[1]" {
		t.Fatalf("changed extras: %v", err)
	}
	_, err = s.Update(ctx, func(a *All) error { a.DHCP.Domain = "x"; a.DNS.LocalDomain = "lan"; return nil })
	if err != nil {
		t.Fatalf("domain change: %v", err)
	}
	_, err = s.Update(ctx, func(a *All) error { a.DHCP.Domain = "home.example"; return nil })
	if e, ok := apperr.As(err); !ok || e.Field != "dhcp.options.extraSearchDomains[0]" {
		t.Fatalf("domain equal to an extra: %v", err)
	}
}

// Equal compares every member, the option lists included.
func TestDHCPEqual(t *testing.T) {
	a := Defaults().DHCP
	for name, change := range map[string]func(*DHCP){
		"enabled": func(h *DHCP) { h.Enabled = true }, "generate": func(h *DHCP) { h.GenerateNames = false },
		"only reserved": func(h *DHCP) { h.OnlyReserved = true }, "rapid": func(h *DHCP) { h.RapidCommit = true },
		"ntp": func(h *DHCP) { h.Options.NTPServers = []string{"10.0.0.1"} }, "mtu": func(h *DHCP) { h.Options.MTU = 1500 },
		"wpad": func(h *DHCP) { h.Options.WPADURL = "http://p/w" }, "search": func(h *DHCP) { h.Options.ExtraSearchDomains = []string{"x"} },
		"ipv6": func(h *DHCP) { h.IPv6.DHCPv6 = true }, "domain": func(h *DHCP) { h.Domain = "x" },
	} {
		b := Defaults().DHCP
		change(&b)
		if a.Equal(b) || b.Equal(a) {
			t.Errorf("%s: equal", name)
		}
	}
	b := Defaults().DHCP
	b.Options.NTPServers = nil // nil and empty are the same list
	if !a.Equal(b) || !a.Equal(Defaults().DHCP) {
		t.Fatal("defaults differ")
	}
}
