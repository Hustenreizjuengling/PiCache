package settings

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// The privacy level is derived from the four switches; every combination
// that is not a preset is "custom".
func TestPrivacyLevelDerived(t *testing.T) {
	for _, tc := range []struct {
		queryLog, anon, hide, stats bool
		want                        string
	}{
		{true, false, false, true, PrivacyFull},
		{true, false, true, true, PrivacyHideDomains},
		{true, true, true, true, PrivacyAnonymous},
		{false, true, true, false, PrivacyOff},
		{true, true, false, true, PrivacyCustom},
		{false, false, false, true, PrivacyCustom},
		{false, true, false, true, PrivacyCustom},
		{true, false, false, false, PrivacyCustom},
		{false, false, false, false, PrivacyCustom},
	} {
		a := Defaults()
		a.Logs.QueryLogEnabled, a.Logs.AnonymizeClientIPs, a.Logs.HideDomains, a.Logs.StatsEnabled = tc.queryLog, tc.anon, tc.hide, tc.stats
		a.Logs.PrivacyLevel = "full" // never trusted
		a.normalize()
		if a.Logs.PrivacyLevel != tc.want {
			t.Errorf("%+v: level %q, want %q", tc, a.Logs.PrivacyLevel, tc.want)
		}
	}
}

// A 0.11 document has only queryLogEnabled and anonymizeClientIps: every
// combination keeps its behaviour and gets the new members' defaults and
// the derived level, through Open and through DecodeStored.
func TestPrivacyUpgradeFrom011(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, tc := range []struct {
		queryLog, anon bool
		want           string
	}{
		{true, false, PrivacyFull},
		{true, true, PrivacyCustom},
		{false, false, PrivacyCustom},
		{false, true, PrivacyCustom},
	} {
		doc := `{"dns":{"upstreams":["9.9.9.9"]},"logs":{"queryLogEnabled":` + boolStr(tc.queryLog) +
			`,"anonymizeClientIps":` + boolStr(tc.anon) + `,"queryLogRetentionHours":24,"cacheLogRetentionHours":48,` +
			`"sessionRetentionDays":90,"statsRetentionDays":30,"maxDbSizeMiB":512}}`
		check := func(what string, g Logs, h Health) {
			t.Helper()
			if g.QueryLogEnabled != tc.queryLog || g.AnonymizeClientIPs != tc.anon || g.HideDomains || !g.StatsEnabled ||
				g.StatsOnlyAddressQueries || g.FlushSeconds != 5 || g.IgnoredDomains == nil || len(g.IgnoredDomains) != 0 ||
				g.PrivacyLevel != tc.want || g.StatsRetentionDays != 30 {
				t.Errorf("%s %+v: logs %+v", what, tc, g)
			}
			if h != Defaults().Health {
				t.Errorf("%s: health %+v", what, h)
			}
		}
		a, err := DecodeStored([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		check("DecodeStored", a.Logs, a.Health)

		d, err := db.Open(filepath.Join(t.TempDir(), "s.db"), 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Migrate(ctx, "settings", migrations); err != nil {
			t.Fatal(err)
		}
		if _, err := d.W.Exec(`INSERT INTO settings (id, doc, updated_at) VALUES (1, ?, 0)`, doc); err != nil {
			t.Fatal(err)
		}
		s, err := Open(ctx, d, log)
		if err != nil {
			t.Fatal(err)
		}
		check("Open", s.Get().Logs, s.Get().Health)
		d.Close()
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// A change of one switch keeps the others; privacyLevel sent by a client
// is ignored and derived again.
func TestPrivacyUpdateKeepsOtherSwitches(t *testing.T) {
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
	next, err := s.Update(ctx, func(a *All) error {
		// A PATCH body {"hideDomains":true,"privacyLevel":"off"} decoded on
		// top of the current section.
		return json.Unmarshal([]byte(`{"hideDomains":true,"privacyLevel":"off"}`), &a.Logs)
	})
	if err != nil {
		t.Fatal(err)
	}
	g := next.Logs
	if !g.HideDomains || !g.QueryLogEnabled || g.AnonymizeClientIPs || !g.StatsEnabled || g.PrivacyLevel != PrivacyHideDomains {
		t.Fatalf("logs %+v", g)
	}
	next, err = s.Update(ctx, func(a *All) error { a.Logs.PrivacyLevel = PrivacyAnonymous; return nil })
	if err != nil || next.Logs.PrivacyLevel != PrivacyHideDomains {
		t.Fatalf("level %q (%v)", next.Logs.PrivacyLevel, err)
	}
}

func TestIgnoredDomainsValidation(t *testing.T) {
	a := Defaults()
	a.Logs.IgnoredDomains = []string{" Example.COM. ", "lan", "example.com", "_dns.resolver.arpa", "", "xn--bcher-kva.example"}
	a.normalize()
	if want := []string{"example.com", "lan", "_dns.resolver.arpa", "xn--bcher-kva.example"}; !slices.Equal(a.Logs.IgnoredDomains, want) {
		t.Fatalf("normalised %q, want %q", a.Logs.IgnoredDomains, want)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"*.example.com", "example.com:AAAA", "/ads/", "exa mple.com", "-bad.example", "a..b",
		strings.Repeat("a", 64) + ".example", strings.Repeat("abcdefghi.", 25) + "example", "bücher.example"} {
		b := Defaults()
		b.Logs.IgnoredDomains = []string{"ok.example", bad}
		b.normalize()
		err := b.Validate()
		if e, ok := apperr.As(err); !ok || e.Field != "logs.ignoredDomains[1]" || e.Message != "must be a domain name" {
			t.Errorf("%q: %v", bad, err)
		}
	}
	c := Defaults()
	for i := range MaxIgnoredDomains + 1 {
		c.Logs.IgnoredDomains = append(c.Logs.IgnoredDomains, "d"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+".example"+string(rune('a'+i/26)))
	}
	c.normalize()
	if e, ok := apperr.As(c.Validate()); !ok || e.Field != "logs.ignoredDomains" || e.Message != "at most 256 entries" {
		t.Fatalf("257 entries: %v", c.Validate())
	}
	var nilList All = Defaults()
	nilList.Logs.IgnoredDomains = nil
	nilList.normalize()
	if nilList.Logs.IgnoredDomains == nil {
		t.Fatal("ignoredDomains must never be null")
	}
}

func TestFlushSecondsAndHealthValidation(t *testing.T) {
	for _, tc := range []struct {
		field string
		mod   func(*All)
	}{
		{"logs.flushSeconds", func(a *All) { a.Logs.FlushSeconds = 4 }},
		{"logs.flushSeconds", func(a *All) { a.Logs.FlushSeconds = 301 }},
		{"health.memoryAvailableMinPercent", func(a *All) { a.Health.MemoryAvailableMinPercent = 0 }},
		{"health.memoryAvailableMinPercent", func(a *All) { a.Health.MemoryAvailableMinPercent = 51 }},
		{"health.loadPerCpuMax", func(a *All) { a.Health.LoadPerCPUMax = 0 }},
		{"health.loadPerCpuMax", func(a *All) { a.Health.LoadPerCPUMax = 17 }},
		{"health.temperatureMaxCelsius", func(a *All) { a.Health.TemperatureMaxCelsius = 49 }},
		{"health.temperatureMaxCelsius", func(a *All) { a.Health.TemperatureMaxCelsius = 111 }},
	} {
		a := Defaults()
		tc.mod(&a)
		if e, ok := apperr.As(a.Validate()); !ok || e.Field != tc.field {
			t.Errorf("%s: %v", tc.field, a.Validate())
		}
	}
	for _, ok := range []func(*All){
		func(a *All) { a.Logs.FlushSeconds = 300 },
		func(a *All) {
			a.Health = Health{MemoryAvailableMinPercent: 50, LoadPerCPUMax: 16, TemperatureMaxCelsius: 110}
		},
		func(a *All) {
			a.Health = Health{MemoryAvailableMinPercent: 1, LoadPerCPUMax: 1, TemperatureMaxCelsius: 50}
		},
	} {
		a := Defaults()
		ok(&a)
		if err := a.Validate(); err != nil {
			t.Error(err)
		}
	}
}
