package services

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestCustomServiceLifecycle(t *testing.T) {
	r, e := loadedRegistry(t)
	ctx := context.Background()

	sv, err := r.CreateCustom(ctx, ServiceInput{Name: "  My LAN Mirror! ", Description: "local", Domains: []string{"Mirror.Example.org.", "*.dl.example.org", "mirror.example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if sv.ID != "custom-my-lan-mirror" || !sv.Custom || !sv.Enabled || sv.Name != "My LAN Mirror!" ||
		!slices.Equal(sv.Domains, []string{"mirror.example.org", "*.dl.example.org"}) || !slices.Equal(sv.ExtraDomains, sv.Domains) {
		t.Fatalf("created = %+v", sv)
	}
	if id, ok := r.MatchDNS("a.dl.example.org"); !ok || id != sv.ID {
		t.Fatalf("custom pattern not matched: %q %v", id, ok)
	}
	if _, err := r.CreateCustom(ctx, ServiceInput{Name: "my lan mirror!", Domains: []string{"x.example.org"}}); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("duplicate name: %v", err)
	}
	sv2, err := r.CreateCustom(ctx, ServiceInput{Name: "My LAN mirror", Domains: []string{"y.example.org"}})
	if err != nil || sv2.ID != "custom-my-lan-mirror-2" {
		t.Fatalf("slug collision: %+v %v", sv2, err)
	}

	up, err := r.UpdateCustom(ctx, sv.ID, ServiceInput{Name: "Renamed", Domains: []string{"new.example.org"}})
	if err != nil || up.ID != sv.ID || up.Name != "Renamed" || !slices.Equal(up.Domains, []string{"new.example.org"}) {
		t.Fatalf("updated = %+v %v", up, err)
	}
	if _, ok := r.MatchDNS("a.dl.example.org"); ok {
		t.Fatal("old pattern still matched")
	}
	if _, err := r.UpdateCustom(ctx, "blizzard", ServiceInput{Name: "x", Domains: []string{"x.example.org"}}); apperr.KindOf(err) != apperr.KindForbidden {
		t.Fatalf("editing a source service: %v", err)
	}
	if err := r.DeleteCustom(ctx, "steam"); apperr.KindOf(err) != apperr.KindForbidden {
		t.Fatalf("deleting a source service: %v", err)
	}
	if err := r.DeleteCustom(ctx, "custom-nope"); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("deleting an unknown service: %v", err)
	}

	// Persisted across restarts (offline start).
	if err := r.SetEnabled(ctx, sv.ID, false); err != nil {
		t.Fatal(err)
	}
	e.cdn.setDown(true)
	r2 := e.registry(t)
	got, err := r2.Service(ctx, sv.ID)
	if err != nil || got.Name != "Renamed" || got.Enabled || !slices.Equal(got.Domains, []string{"new.example.org"}) {
		t.Fatalf("after restart = %+v %v", got, err)
	}

	// Deleting forgets the disabled state and the domains.
	if err := r2.DeleteCustom(ctx, sv.ID); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(e.set.Get().DownloadCache.DisabledServices, sv.ID) {
		t.Fatal("disabled state of a deleted service kept")
	}
	if _, err := r2.Service(ctx, sv.ID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("deleted service still listed: %v", err)
	}
	var n int
	if err := e.db.R.QueryRow(`SELECT COUNT(*) FROM services_extra_domains WHERE service_id = ?`, sv.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("domains left: %d %v", n, err)
	}
}

func TestCustomServiceValidation(t *testing.T) {
	r, _ := loadedRegistry(t)
	ctx := context.Background()
	tests := []struct {
		name  string
		in    ServiceInput
		field string
	}{
		{"no name", ServiceInput{Domains: []string{"a.example.org"}}, "name"},
		{"long name", ServiceInput{Name: strings.Repeat("x", maxNameLen+1), Domains: []string{"a.example.org"}}, "name"},
		{"control char", ServiceInput{Name: "a\nb", Domains: []string{"a.example.org"}}, "name"},
		{"no domains", ServiceInput{Name: "x"}, "domains"},
		{"public suffix", ServiceInput{Name: "x", Domains: []string{"a.example.org", "*.co.uk"}}, "domains[1]"},
		{"bare wildcard", ServiceInput{Name: "x", Domains: []string{"*"}}, "domains[0]"},
		{"ip", ServiceInput{Name: "x", Domains: []string{"10.0.0.1"}}, "domains[0]"},
		{"long description", ServiceInput{Name: "x", Description: strings.Repeat("d", maxDescLen+1), Domains: []string{"a.example.org"}}, "description"},
	}
	for _, tt := range tests {
		_, err := r.CreateCustom(ctx, tt.in)
		ae, ok := apperr.As(err)
		if !ok || ae.Kind != apperr.KindInvalid || ae.Field != tt.field {
			t.Errorf("%s: err = %v, want invalid %s", tt.name, err, tt.field)
		}
	}
	many := make([]string, maxExtraDomains+1)
	for i := range many {
		many[i] = "a.example.org"
	}
	if err := r.SetExtraDomains(ctx, "steam", many); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("too many extra domains: %v", err)
	}
}

func TestCustomServiceLimit(t *testing.T) {
	r, _ := loadedRegistry(t)
	ctx := context.Background()
	for i := range maxCustomServices {
		if _, err := r.CreateCustom(ctx, ServiceInput{Name: fmt.Sprintf("svc %d", i), Domains: []string{"a.example.org"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.CreateCustom(ctx, ServiceInput{Name: "one too many", Domains: []string{"a.example.org"}}); apperr.KindOf(err) != apperr.KindConflict {
		t.Fatalf("limit: %v", err)
	}
	list, _ := r.Services(ctx)
	for _, s := range list {
		if !ValidServiceID(s.ID) {
			t.Fatalf("invalid generated id %q", s.ID)
		}
	}
}

func TestExtraDomains(t *testing.T) {
	r, e := loadedRegistry(t)
	ctx := context.Background()
	if err := r.SetExtraDomains(ctx, "epicgames", []string{"EGS-Cloudfront-Chunks.epicgamescdn.com", "download.epicgames.com"}); err != nil {
		t.Fatal(err)
	}
	sv, _ := r.Service(ctx, "epicgames")
	if !slices.Equal(sv.ExtraDomains, []string{"egs-cloudfront-chunks.epicgamescdn.com", "download.epicgames.com"}) ||
		sv.DomainCount != 4 || sv.Domains[3] != "egs-cloudfront-chunks.epicgamescdn.com" {
		t.Fatalf("service = %+v", sv)
	}
	if id, ok := r.MatchDNS("egs-cloudfront-chunks.epicgamescdn.com"); !ok || id != "epicgames" {
		t.Fatal("extra domain not matched")
	}
	err := r.SetExtraDomains(ctx, "epicgames", []string{"ok.example.org", "*.com"})
	if ae, ok := apperr.As(err); !ok || ae.Field != "extraDomains[1]" || !strings.Contains(ae.Message, `"*.com"`) {
		t.Fatalf("invalid pattern: %v", err)
	}
	if err := r.SetExtraDomains(ctx, "nope", nil); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown service: %v", err)
	}
	// Survives a source refresh and a restart.
	if err := r.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	r2 := e.registry(t)
	if _, ok := r2.MatchDNS("egs-cloudfront-chunks.epicgamescdn.com"); !ok {
		t.Fatal("extra domain lost after restart")
	}
	if err := r2.SetExtraDomains(ctx, "epicgames", nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := r2.MatchDNS("egs-cloudfront-chunks.epicgamescdn.com"); ok {
		t.Fatal("cleared extra domain still matched")
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"My LAN Mirror!":           "my-lan-mirror",
		"--Ünïcode--":              "n-code",
		"!!!":                      "service",
		strings.Repeat("abc ", 20): "abc-abc-abc-abc-abc-abc-a",
	} {
		if got := slugify(in, 32-len(customPrefix)); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
