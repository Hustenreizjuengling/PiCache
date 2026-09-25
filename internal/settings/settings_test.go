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
