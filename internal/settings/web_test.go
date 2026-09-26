package settings

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

func TestWebAccessDefaults(t *testing.T) {
	w := Defaults().Web
	if !w.RestrictToNetworks || w.TLSMinVersion != "1.2" || w.AllowedNetworks == nil || w.TrustedProxies == nil ||
		len(w.AllowedNetworks) != 0 || len(w.TrustedProxies) != 0 {
		t.Fatalf("web defaults %+v", w)
	}
}

// The web access members are normalised like the IP entries of
// dns.blockedClients and validated with their own bounds.
func TestWebAccessValidation(t *testing.T) {
	many := func(n int, f func(i int) string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = f(i)
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		edit     func(*Web)
		field    string // "" = valid
		msg      string
		networks []string // web.allowedNetworks after normalisation (nil: not checked)
		proxies  []string // web.trustedProxies after normalisation (nil: not checked)
	}{
		{name: "normalised", edit: func(w *Web) {
			w.AllowedNetworks = []string{" 203.0.113.9/24 ", "::FFFF:198.51.100.7", "2001:DB8::/32", "10.1.2.3/32", "", "203.0.113.0/24"}
			w.TrustedProxies = []string{"192.168.1.10", " 192.168.1.10/32", "fd00::1/128", "::1", "127.0.0.1"}
			w.TLSMinVersion = " 1.3 "
		},
			networks: []string{"203.0.113.0/24", "198.51.100.7", "2001:db8::/32", "10.1.2.3"},
			proxies:  []string{"192.168.1.10", "fd00::1", "::1", "127.0.0.1"}},
		{name: "networks at most 64", edit: func(w *Web) {
			w.AllowedNetworks = many(65, func(i int) string { return "10.0.0." + strconv.Itoa(i) })
		}, field: "web.allowedNetworks", msg: "at most 64 entries"},
		{name: "64 networks", edit: func(w *Web) {
			w.AllowedNetworks = many(64, func(i int) string { return "10.0.0." + strconv.Itoa(i) })
		}},
		{name: "network garbage", edit: func(w *Web) { w.AllowedNetworks = []string{"10.0.0.0/8", "lan"} },
			field: "web.allowedNetworks[1]", msg: "must be an IP address or CIDR"},
		{name: "network zone", edit: func(w *Web) { w.AllowedNetworks = []string{"fe80::1%eth0"} },
			field: "web.allowedNetworks[0]", msg: "must be an IP address or CIDR"},
		{name: "network too broad v4", edit: func(w *Web) { w.AllowedNetworks = []string{"0.0.0.0/0"} },
			field: "web.allowedNetworks[0]", msg: "network is too broad: use at least /8 (IPv4) or /32 (IPv6)"},
		{name: "network /8", edit: func(w *Web) { w.AllowedNetworks = []string{"100.0.0.0/8", "2001:db8::/32"} }},
		{name: "network too broad v6", edit: func(w *Web) { w.AllowedNetworks = []string{"2000::/3"} },
			field: "web.allowedNetworks[0]", msg: "network is too broad"},
		{name: "proxies at most 16", edit: func(w *Web) {
			w.TrustedProxies = many(17, func(i int) string { return "192.168.1." + strconv.Itoa(i) })
		}, field: "web.trustedProxies", msg: "at most 16 entries"},
		{name: "proxy /23", edit: func(w *Web) { w.TrustedProxies = []string{"192.168.0.0/23"} },
			field: "web.trustedProxies[0]", msg: "network is too broad: use at least /24 (IPv4) or /64 (IPv6)"},
		{name: "proxy everything", edit: func(w *Web) { w.TrustedProxies = []string{"::/0"} },
			field: "web.trustedProxies[0]", msg: "network is too broad"},
		{name: "proxy mapped everything", edit: func(w *Web) { w.TrustedProxies = []string{"::ffff:0.0.0.0/96"} },
			field: "web.trustedProxies[0]", msg: "network is too broad"},
		{name: "proxy /24 and /64", edit: func(w *Web) { w.TrustedProxies = []string{"192.168.5.0/24", "fd00:1::/64"} }},
		{name: "proxy name", edit: func(w *Web) { w.TrustedProxies = []string{"proxy.lan"} },
			field: "web.trustedProxies[0]", msg: "must be an IP address or CIDR"},
		{name: "tls 1.1", edit: func(w *Web) { w.TLSMinVersion = "1.1" }, field: "web.tlsMinVersion", msg: "must be 1.2 or 1.3"},
		{name: "tls empty", edit: func(w *Web) { w.TLSMinVersion = "" }, field: "web.tlsMinVersion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := Defaults()
			tc.edit(&a.Web)
			a.normalize()
			err := a.Validate()
			if tc.field == "" {
				if err != nil {
					t.Fatalf("valid: %v", err)
				}
			} else {
				e, ok := apperr.As(err)
				if !ok || e.Field != tc.field || !strings.Contains(e.Message, tc.msg) {
					t.Fatalf("err %v, want %s: %s", err, tc.field, tc.msg)
				}
				return
			}
			if tc.networks != nil && !slices.Equal(a.Web.AllowedNetworks, tc.networks) {
				t.Errorf("allowedNetworks %q, want %q", a.Web.AllowedNetworks, tc.networks)
			}
			if tc.proxies != nil && !slices.Equal(a.Web.TrustedProxies, tc.proxies) {
				t.Errorf("trustedProxies %q, want %q", a.Web.TrustedProxies, tc.proxies)
			}
		})
	}
}

// Settings v5: a document stored before 0.11.0 (or restored from such a
// backup) gets web.restrictToNetworks false; a stored value is kept; a
// fresh database gets the default true; invalid JSON is left alone.
func TestMigrateRestrictToNetworks(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.DiscardHandler)
	v010 := func(t *testing.T) map[string]any {
		b, err := json.Marshal(Defaults())
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		web := doc["web"].(map[string]any)
		for _, k := range []string{"restrictToNetworks", "allowedNetworks", "trustedProxies", "tlsMinVersion"} {
			delete(web, k)
		}
		return doc
	}
	for _, tc := range []struct {
		name string
		doc  func(t *testing.T) string // "" = no stored document (fresh database)
		want bool
		fail bool // Open fails (the document is left alone)
	}{
		{"fresh database", func(*testing.T) string { return "" }, true, false},
		{"v0.10 document", func(t *testing.T) string { b, _ := json.Marshal(v010(t)); return string(b) }, false, false},
		{"stored true", func(t *testing.T) string {
			doc := v010(t)
			doc["web"].(map[string]any)["restrictToNetworks"] = true
			b, _ := json.Marshal(doc)
			return string(b)
		}, true, false},
		{"stored false", func(t *testing.T) string {
			doc := v010(t)
			doc["web"].(map[string]any)["restrictToNetworks"] = false
			b, _ := json.Marshal(doc)
			return string(b)
		}, false, false},
		{"no web object", func(t *testing.T) string {
			doc := v010(t)
			delete(doc, "web")
			b, _ := json.Marshal(doc)
			return string(b)
		}, false, false},
		{"web null", func(t *testing.T) string {
			doc := v010(t)
			doc["web"] = nil
			b, _ := json.Marshal(doc)
			return string(b)
		}, false, false},
		{"invalid JSON", func(*testing.T) string { return `{"web":` }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.db")
			d, err := db.Open(path, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if doc := tc.doc(t); doc != "" {
				if err := d.Migrate(ctx, "settings", migrations[:4]); err != nil {
					t.Fatal(err)
				}
				if _, err := d.W.ExecContext(ctx, `INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, doc); err != nil {
					t.Fatal(err)
				}
			}
			s, err := Open(ctx, d, log)
			if tc.fail {
				if err == nil {
					t.Fatal("invalid JSON must stay and fail to decode")
				}
				var stored string
				if err := d.R.QueryRowContext(ctx, `SELECT doc FROM settings`).Scan(&stored); err != nil || stored != `{"web":` {
					t.Fatalf("invalid document changed: %q %v", stored, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			w := s.Get().Web
			if w.RestrictToNetworks != tc.want || w.TLSMinVersion != "1.2" || w.AllowedNetworks == nil || w.TrustedProxies == nil {
				t.Fatalf("web %+v, want restrictToNetworks %v", w, tc.want)
			}
			// A later save of any section keeps the value.
			if _, err := s.Update(ctx, func(a *All) error { a.Logs.QueryLogRetentionHours = 24; return nil }); err != nil {
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
			if got := s2.Get().Web.RestrictToNetworks; got != tc.want {
				t.Fatalf("after a save and a new start: %v, want %v", got, tc.want)
			}
			var v int
			if err := d2.R.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations WHERE component = 'settings'`).
				Scan(&v); err != nil || v != len(migrations) || v != 5 {
				t.Fatalf("settings schema version %d (%v)", v, err)
			}
		})
	}
}

// DecodeStored judges a staged document the way the next start will.
func TestDecodeStored(t *testing.T) {
	s, err := DecodeStored([]byte(`{"web":{"allowedNetworks":["203.0.113.9/24"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Web.RestrictToNetworks || !slices.Equal(s.Web.AllowedNetworks, []string{"203.0.113.0/24"}) ||
		s.Web.TLSMinVersion != "1.2" || s.DNS.LocalDomain != "lan" {
		t.Fatalf("decoded %+v", s.Web)
	}
	s, err = DecodeStored([]byte(`{"web":{"restrictToNetworks":true,"tlsMinVersion":"1.3"}}`))
	if err != nil || !s.Web.RestrictToNetworks || s.Web.TLSMinVersion != "1.3" {
		t.Fatalf("decoded %+v %v", s, err)
	}
	if _, err := DecodeStored([]byte(`{`)); err == nil {
		t.Fatal("invalid JSON must fail")
	}
}

// Recover (the web access reset) saves its change although the stored
// document is invalid in another member; on a valid document it validates
// like Update.
func TestRecoverInvalidDocument(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(filepath.Join(t.TempDir(), "picache.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	log := slog.New(slog.DiscardHandler)
	s, err := Open(ctx, d, log)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Recover(ctx, func(a *All) error { a.Web.TLSMinVersion = "1.1"; return nil }); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("a valid document is validated: %v", err)
	}
	if _, err := d.W.Exec(`UPDATE settings SET doc = json_set(doc, '$.dhcp.leaseSeconds', -5, '$.web.restrictToNetworks', json('true'))`); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(ctx, d, log); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, func(a *All) error { a.Web.RestrictToNetworks = false; return nil }); err == nil {
		t.Fatal("Update refuses the invalid document")
	}
	if _, err := s.Recover(ctx, func(a *All) error { a.Web.RestrictToNetworks = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(ctx, d, log); err != nil || s.Get().Web.RestrictToNetworks {
		t.Fatalf("the recovery must be stored: %v", err)
	}
}
