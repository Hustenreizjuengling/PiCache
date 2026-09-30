package app

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/db"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// configSchema016 is a copy of ConfigSchemaVersions of 0.16.0: a database
// with a newer component version is refused by it (db.Migrate).
var configSchema016 = map[string]int{"app": 1, "auth": 3, "clients": 4, "dhcp": 2, "dns": 3, "filter": 3, "notify": 1,
	"parental": 2, "proxy": 1, "services": 1, "settings": 6, "storage": 1}

// make016DB builds a picache.db with the steps of 0.16 (settings v6, dns
// v3), a settings document with dns.dnssec and a forwarder.
func make016DB(t *testing.T, path string, dnssec bool) {
	t.Helper()
	migrateConfig(t, path, map[string]int{"settings": 6, "dns": 3})
	d, err := db.Open(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	b := "false"
	if dnssec {
		b = "true"
	}
	if _, err := d.W.Exec(`INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`,
		`{"dns":{"upstreams":["9.9.9.9"],"dnssec":`+b+`},"web":{"language":"de"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO dns_forwarders (id, domain, upstreams, enabled, comment, created_at, updated_at)
		VALUES (1, 'corp.example', '["192.0.2.53"]', 1, '', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO dns_forwarder_domains (forwarder_id, position, domain) VALUES (1, 0, 'corp.example')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.W.Exec(`INSERT INTO auth_users (id, username, password_hash, role, created_at) VALUES (1, 'owner', 'x', 'admin', 1)`); err != nil {
		t.Fatal(err)
	}
}

// A 0.16 picache.db migrates to 0.17: dns.dnssec true → passthrough,
// false → off (an upgrade never switches validation on); every forwarder
// validate:false; settings v7 and dns v4 are newer than 0.16's (0.16
// refuses the database).
func TestUpgradeFrom016(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		dnssec bool
		mode   string
	}{{true, settings.DNSSECPassthrough}, {false, settings.DNSSECOff}} {
		path := filepath.Join(t.TempDir(), "picache.db")
		make016DB(t, path, tc.dnssec)
		d, err := db.Open(path, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := MigrateConfigDB(ctx, d); err != nil {
			t.Fatal(err)
		}
		set, err := settings.Open(ctx, d, slog.New(slog.DiscardHandler))
		if err != nil {
			t.Fatal(err)
		}
		if got := set.Get().DNS; got.DNSSECMode != tc.mode || got.DNSSEC != tc.dnssec {
			t.Errorf("dnssec %v: mode %q dnssec %v", tc.dnssec, got.DNSSECMode, got.DNSSEC)
		}
		var validate int
		if err := d.R.QueryRow(`SELECT validate FROM dns_forwarders WHERE id = 1`).Scan(&validate); err != nil || validate != 0 {
			t.Errorf("forwarder validate %d, %v", validate, err)
		}
		for comp, want := range map[string]int{"settings": 7, "dns": 4} {
			var v int
			_ = d.R.QueryRow(`SELECT MAX(version) FROM schema_migrations WHERE component = ?`, comp).Scan(&v)
			if v != want || v <= configSchema016[comp] {
				t.Errorf("%s at v%d", comp, v)
			}
		}
		d.Close()
	}
	for comp, v := range ConfigSchemaVersions() {
		if old, ok := configSchema016[comp]; !ok || v < old {
			t.Errorf("%s: %d (0.16: %d, %v)", comp, v, old, ok)
		}
	}
}

// A 0.16 backup restored as a whole or as the section settings gets the
// mode of its dns.dnssec.
func TestRestore016Backup(t *testing.T) {
	ctx := context.Background()
	for _, sections := range [][]string{nil, {settings.SectionSettings}} {
		a := newRestoreApp(t)
		makeConfigDB(t, a.paths.ConfigDB, "owner", "owner password", "en")
		migrateConfig(t, a.paths.ConfigDB, nil)
		openLive(t, a)
		up := filepath.Join(t.TempDir(), "upload.db")
		make016DB(t, up, true)
		staged, err := a.StageRestore(ctx, bytes.NewReader(readFile(t, up)), sections)
		if err != nil {
			t.Fatalf("sections %v: %v", sections, err)
		}
		if staged == nil || staged.DNS.DNSSECMode != settings.DNSSECPassthrough {
			t.Errorf("sections %v: staged settings %+v", sections, staged)
		}
		closeLive(a)
		if restored, err := a.applyStagedRestore(); err != nil || !restored {
			t.Fatalf("sections %v: applyStagedRestore = %v, %v", sections, restored, err)
		}
		_, set, _ := openRestored(t, a)
		if got := set.Get().DNS; got.DNSSECMode != settings.DNSSECPassthrough || !got.DNSSEC || set.Get().Web.Language != "de" {
			t.Errorf("sections %v: %q %v %q", sections, got.DNSSECMode, got.DNSSEC, set.Get().Web.Language)
		}
	}
}

// PICACHE_INITIAL_CONFIG with the alias dns.dnssec and with dns.dnssecMode.
func TestInitialConfigDNSSEC(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		body, mode string
	}{
		{`{"dns":{"dnssec":false}}`, settings.DNSSECOff},
		{`{"dns":{"dnssecMode":"passthrough"}}`, settings.DNSSECPassthrough},
		{`{"dns":{"dnssecMode":"off","dnssec":false}}`, settings.DNSSECOff},
	} {
		a := syncApp(t)
		p := filepath.Join(t.TempDir(), "initial.json")
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		a.cfg.InitialConfig = p
		if err := a.applyInitialConfig(ctx); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if got := a.set.Get().DNS; got.DNSSECMode != tc.mode || got.DNSSEC != (tc.mode != settings.DNSSECOff) {
			t.Errorf("%s: %q %v", tc.body, got.DNSSECMode, got.DNSSEC)
		}
	}
	a := syncApp(t)
	p := filepath.Join(t.TempDir(), "initial.json")
	if err := os.WriteFile(p, []byte(`{"dns":{"dnssecMode":"passthrough","dnssec":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	a.cfg.InitialConfig = p
	if err := a.applyInitialConfig(ctx); err == nil || !strings.Contains(err.Error(), "dns.dnssecMode: dnssec contradicts dnssecMode") {
		t.Errorf("contradicting file: %v", err)
	}
}

// A follower of 0.17 applies the export of a 0.16 primary: dns.dnssec is
// mapped by the alias, forwarders without validate get false.
func TestSyncFrom016Primary(t *testing.T) {
	ctx := context.Background()
	p, f := syncApp(t), syncApp(t)
	if _, err := f.set.Update(ctx, func(s *settings.All) error { s.DNS.DNSSECMode = settings.DNSSECOff; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := p.dns.CreateForwarder(ctx, dnsserver.ForwarderInput{Domains: []string{"corp.example"},
		Upstreams: []string{"192.0.2.53"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	secs := []string{settings.SectionDNSSettings, settings.SectionLocalDNS}
	exp := exportOf(t, p, secs...)
	// As 0.16 exports them: no dns.dnssecMode, no validate.
	var dnsSet map[string]map[string]jsontext.Value
	if err := json.Unmarshal(exp.Sections[settings.SectionDNSSettings], &dnsSet); err != nil {
		t.Fatal(err)
	}
	delete(dnsSet["dns"], "dnssecMode")
	dnsSet["dns"]["dnssec"] = jsontext.Value("true")
	exp.Sections[settings.SectionDNSSettings] = mustJSON(t, dnsSet)
	exp.Sections[settings.SectionLocalDNS] = jsontext.Value(strings.ReplaceAll(string(exp.Sections[settings.SectionLocalDNS]), `"validate":false,`, ""))
	exp.Schema, exp.Version = configSchema016, "v0.16.0"
	var err error
	if exp.ContentSHA256, err = api.ExportContentSHA256(exp.Sections); err != nil {
		t.Fatal(err)
	}
	if err := f.applySync(ctx, exp, secs); err != nil {
		t.Fatal(err)
	}
	if got := f.set.Get().DNS; got.DNSSECMode != settings.DNSSECPassthrough || !got.DNSSEC {
		t.Errorf("mode %q dnssec %v", got.DNSSECMode, got.DNSSEC)
	}
	fwds, err := f.dns.Forwarders(ctx)
	if err != nil || len(fwds) != 1 || fwds[0].Validate {
		t.Errorf("forwarders %+v %v", fwds, err)
	}
	// A 0.17 export carries dnssecMode and validate.
	if exp := exportOf(t, p, secs...); !strings.Contains(string(exp.Sections[settings.SectionDNSSettings]), `"dnssecMode"`) ||
		!strings.Contains(string(exp.Sections[settings.SectionLocalDNS]), `"validate"`) {
		t.Errorf("export %s", exp.Sections)
	}
}

func mustJSON(t *testing.T, v any) jsontext.Value {
	t.Helper()
	b, err := json.Marshal(v, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The check "dnssec": each message, the first match.
func TestDNSSECHealth(t *testing.T) {
	ok := dnssecHealthInput{timeActive: true}
	for _, tc := range []struct {
		name         string
		in           dnssecHealthInput
		status, want string
	}{
		{"ok", ok, "ok", ""},
		{"anchor mismatch first", dnssecHealthInput{anchorMismatch: true, timeActive: false, timeReason: upstream.TimeReasonUnsynced},
			"fail", "the DNSSEC trust anchors of this version do not match the root zone: update PiCache"},
		{"unsynced", dnssecHealthInput{timeReason: upstream.TimeReasonUnsynced, noDNSSEC: []string{"x"}}, "warn",
			"DNSSEC time checks are suspended: the host clock is not synchronised"},
		{"clock guard", dnssecHealthInput{timeReason: upstream.TimeReasonClockGuard}, "warn",
			"DNSSEC time checks are suspended: the system clock is before the build date"},
		{"root signatures", dnssecHealthInput{timeReason: upstream.TimeReasonRootSignatures}, "warn",
			"DNSSEC time checks are suspended: the host clock disagrees with the root zone's signatures"},
		{"one upstream", dnssecHealthInput{timeActive: true, noDNSSEC: []string{"192.168.178.1"}}, "warn",
			"192.168.178.1 does not return DNSSEC data: its answers are not validated"},
		{"more", dnssecHealthInput{timeActive: true, noDNSSEC: []string{"forwarder corp.example: 10.0.0.1", "b", "c"}, newRootKey: true}, "warn",
			"forwarder corp.example: 10.0.0.1 does not return DNSSEC data: its answers are not validated (and 2 more)"},
		{"new root key", dnssecHealthInput{timeActive: true, newRootKey: true, bogus: 50, total: 60}, "warn",
			"a new root key is published: update PiCache before it is used"},
		{"bogus share", dnssecHealthInput{timeActive: true, bogus: 20, total: 1999}, "warn",
			"20 of 1999 validated answers in the last hour failed DNSSEC validation"},
		{"bogus below 20", dnssecHealthInput{timeActive: true, bogus: 19, total: 19}, "ok", ""},
		{"bogus at 1 %", dnssecHealthInput{timeActive: true, bogus: 20, total: 2000}, "ok", ""},
	} {
		st, msg, _ := dnssecHealth(tc.in)
		if st != tc.status || msg != tc.want {
			t.Errorf("%s: %s %q", tc.name, st, msg)
		}
	}
	// Absent outside the mode validate.
	a := syncApp(t)
	for _, mode := range []string{settings.DNSSECOff, settings.DNSSECPassthrough} {
		set := settings.Defaults()
		set.DNS.DNSSECMode = mode
		if _, _, _, show := a.dnssecCheck(context.Background(), &set); show {
			t.Errorf("shown in mode %s", mode)
		}
	}
	if settingRules["dns.dnssecMode"] != ruleKeep {
		t.Error("dns.dnssecMode is not kept in support bundles")
	}
}
