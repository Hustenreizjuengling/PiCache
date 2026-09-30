package settings

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// dns.dnssecMode and its alias dns.dnssec (docs/API.md "Section dns —
// members added in v0.17.0").
func TestDNSSECAlias(t *testing.T) {
	ctx := context.Background()
	s, _ := openTestStore(t)
	set := func(fn func(d *DNS)) (DNS, error) {
		a, err := s.Update(ctx, func(a *All) error { fn(&a.DNS); return nil })
		if err != nil {
			return DNS{}, err
		}
		return a.DNS, nil
	}
	want := func(d DNS, err error, mode string) {
		t.Helper()
		if err != nil || d.DNSSECMode != mode || d.DNSSEC != (mode != DNSSECOff) {
			t.Fatalf("mode %q dnssec %v (%v), want %q", d.DNSSECMode, d.DNSSEC, err, mode)
		}
	}
	// The mode alone (trimmed, lower-cased); every save writes dnssec.
	d, err := set(func(d *DNS) { d.DNSSECMode = " Off " })
	want(d, err, DNSSECOff)
	d, err = set(func(d *DNS) { d.DNSSECMode = DNSSECValidate })
	want(d, err, DNSSECValidate)
	// An empty mode keeps the stored one.
	d, err = set(func(d *DNS) { d.DNSSECMode = "" })
	want(d, err, DNSSECValidate)
	// dnssec alone: true keeps passthrough and validate; false → off; true
	// from off → passthrough.
	d, err = set(func(d *DNS) { d.DNSSEC = false })
	want(d, err, DNSSECOff)
	d, err = set(func(d *DNS) { d.DNSSEC = true })
	want(d, err, DNSSECPassthrough)
	d, err = set(func(d *DNS) { d.DNSSEC = false })
	want(d, err, DNSSECOff)
	d, err = set(func(d *DNS) { d.DNSSECMode = DNSSECPassthrough })
	want(d, err, DNSSECPassthrough)
	d, err = set(func(d *DNS) { d.DNSSEC = true }) // unchanged: stays passthrough
	want(d, err, DNSSECPassthrough)
	d, err = set(func(d *DNS) { d.DNSSECMode = DNSSECValidate; d.DNSSEC = true }) // consistent
	want(d, err, DNSSECValidate)
	d, err = set(func(d *DNS) { d.DNSSEC = true; d.UpstreamTimeoutMs = 4000 })
	want(d, err, DNSSECValidate)
	// Both changed and contradicting.
	_, err = set(func(d *DNS) { d.DNSSECMode = DNSSECPassthrough; d.DNSSEC = false })
	wantInvalid(t, err, "dns.dnssecMode", "dnssec contradicts dnssecMode: send dnssecMode only")
	d, err = set(func(d *DNS) { d.DNSSECMode = DNSSECOff; d.DNSSEC = false }) // consistent
	want(d, err, DNSSECOff)
	_, err = set(func(d *DNS) { d.DNSSECMode = DNSSECValidate; d.DNSSEC = false }) // dnssec unchanged: the mode wins
	if err != nil || s.Get().DNS.DNSSECMode != DNSSECValidate || !s.Get().DNS.DNSSEC {
		t.Fatalf("mode with an unchanged dnssec: %+v %v", s.Get().DNS, err)
	}
	// An unknown mode.
	_, err = set(func(d *DNS) { d.DNSSECMode = "strict" })
	wantInvalid(t, err, "dns.dnssecMode", "must be off, passthrough or validate")
}

// Settings v7 derives dns.dnssecMode from dns.dnssec in documents without
// it; DecodeStored mirrors it.
func TestMigrateDNSSECV7(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, doc string
		want      string
	}{
		{"dnssec true", `{"dns":{"upstreams":["9.9.9.9"],"dnssec":true}}`, DNSSECPassthrough},
		{"dnssec false", `{"dns":{"upstreams":["9.9.9.9"],"dnssec":false}}`, DNSSECOff},
		{"dnssec absent", `{"dns":{"upstreams":["9.9.9.9"]}}`, DNSSECOff},
		{"kept", `{"dns":{"upstreams":["9.9.9.9"],"dnssec":true,"dnssecMode":"validate"}}`, DNSSECValidate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if err := d.Migrate(ctx, "settings", migrations[:6]); err != nil {
				t.Fatal(err)
			}
			if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, tc.doc); err != nil {
				t.Fatal(err)
			}
			s, err := Open(ctx, d, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			if got := s.Get().DNS; got.DNSSECMode != tc.want || got.DNSSEC != (tc.want != DNSSECOff) {
				t.Fatalf("mode %q dnssec %v, want %q", got.DNSSECMode, got.DNSSEC, tc.want)
			}
			var mode string
			if err := d.R.QueryRowContext(ctx, `SELECT json_extract(doc, '$.dns.dnssecMode') FROM settings`).Scan(&mode); err != nil ||
				mode != tc.want {
				t.Fatalf("stored mode %q, %v", mode, err)
			}
			a, err := DecodeStored([]byte(tc.doc))
			if err != nil || a.DNS.DNSSECMode != tc.want {
				t.Fatalf("DecodeStored: %+v, %v", a.DNS, err)
			}
		})
	}
	// A document whose dns is not an object, and one that is not valid
	// JSON, are left alone.
	for _, doc := range []string{`{"dns":null,"web":{}}`, `{"dns":`} {
		d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Migrate(ctx, "settings", migrations[:6]); err != nil {
			t.Fatal(err)
		}
		if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, doc); err != nil {
			t.Fatal(err)
		}
		if err := d.Migrate(ctx, "settings", migrations); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := d.R.QueryRowContext(ctx, `SELECT doc FROM settings`).Scan(&got); err != nil || got != doc {
			t.Fatalf("%s → %s, %v", doc, got, err)
		}
		d.Close()
	}
}

func TestDNSSECDefaultsAndSync(t *testing.T) {
	def := Defaults()
	if def.DNS.DNSSECMode != DNSSECValidate || !def.DNS.DNSSEC {
		t.Fatalf("defaults: %q %v", def.DNS.DNSSECMode, def.DNS.DNSSEC)
	}
	if !Syncable("dns.dnssecMode") || !Syncable("dns.dnssec") {
		t.Fatal("dns.dnssecMode and dns.dnssec are synced (dns-settings)")
	}
	// A 0.16 primary sends only dns.dnssec: the alias maps it.
	ctx := context.Background()
	s, _ := openTestStore(t)
	if _, err := s.Update(ctx, func(a *All) error { a.DNS.DNSSECMode = DNSSECOff; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		payload string
		want    string
	}{
		{`{"dns":{"dnssec":true},"filter":{}}`, DNSSECPassthrough},
		{`{"dns":{"dnssec":true},"filter":{}}`, DNSSECPassthrough},
		{`{"dns":{"dnssec":false},"filter":{}}`, DNSSECOff},
		{`{"dns":{"dnssec":true,"dnssecMode":"validate"},"filter":{}}`, DNSSECValidate},
	} {
		a, err := s.Update(ctx, func(a *All) error { return ApplySyncable(a, jsontext.Value(tc.payload)) })
		if err != nil || a.DNS.DNSSECMode != tc.want {
			t.Fatalf("%s: %q %v", tc.payload, a.DNS.DNSSECMode, err)
		}
	}
	// The export carries both members.
	raw, err := SyncableSettings(s.Get())
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		DNS map[string]any `json:"dns"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.DNS["dnssecMode"] != DNSSECValidate || out.DNS["dnssec"] != true {
		t.Fatalf("export %s: %v", raw, err)
	}
}
