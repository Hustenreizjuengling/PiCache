package services

import (
	"context"
	"slices"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestMatchDNS(t *testing.T) {
	r, _ := loadedRegistry(t)
	tests := []struct {
		q    string
		want string // "" = no match
	}{
		{"level3.blizzard.com", "blizzard"},
		{"LEVEL3.Blizzard.COM.", "blizzard"}, // case-insensitive, trailing dot
		{"us.cdn.blizzard.com", "blizzard"},  // *.x: one label below
		{"a.b.c.cdn.blizzard.com", "blizzard"},
		{"cdn.blizzard.com", ""},         // *.x never matches the apex
		{"xlevel3.blizzard.com", ""},     // anchored: no substring match
		{"level3.blizzard.com.evil", ""}, // anchored at the end
		{"blizzard.com", ""},
		{"fooevil.cdn.blizzard.co", ""},
		{"x.windowsupdate.com", "wsus"},
		{"windowsupdate.com", ""},
		{"download.epicgames.com", "epicgames"},
		{"egdownload.fastly-edge.com", "epicgames"},
		{SteamTrigger, "steam"},
		{"trigger.cache-domains.test", ""}, // service "test" is disabled by default
		{"example.com", ""},
		{"", ""},
		{".", ""},
	}
	for _, tt := range tests {
		id, ok := r.MatchDNS(tt.q)
		if id != tt.want || ok != (tt.want != "") {
			t.Errorf("MatchDNS(%q) = %q %v, want %q", tt.q, id, ok, tt.want)
		}
	}
}

func TestSteamTriggerFollowsSteamState(t *testing.T) {
	cdn := newFakeCDN(standardSource())
	cdn.set("/cache-domains/steam.txt", fakeResp{status: 200, body: "# the trigger line was removed upstream\ncontent.steampowered.example.com\n"})
	e := newEnv(t, cdn)
	r := e.registry(t)
	if err := r.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if id, ok := r.MatchDNS(SteamTrigger); !ok || id != "steam" {
		t.Fatalf("trigger must be answered while steam is enabled: %q %v", id, ok)
	}
	if err := r.SetEnabled(context.Background(), "steam", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.MatchDNS(SteamTrigger); ok {
		t.Fatal("trigger answered with steam disabled")
	}
	if id, en, known := r.Classify("cache7-fra1.steamcontent.com", SteamUserAgentSuffix, "/depot/7/chunk/abc"); id != "steam" || en || !known {
		t.Fatalf("Classify with steam disabled = %q %v %v", id, en, known)
	}
}

func TestClassify(t *testing.T) {
	r, _ := loadedRegistry(t)
	const ua = "Valve/Steam HTTP Client 1.0"
	tests := []struct {
		name, host, ua, path string
		id                   string
		enabled, known       bool
	}{
		{"steam depot on arbitrary host", "cache3-fra1.steamcontent.com", ua, "/depot/228990/chunk/0123456789abcdef0123456789abcdef01234567", "steam", true, true},
		{"steam depot on a foreign service host", "level3.blizzard.com", ua, "/depot/1/manifest/2/5", "steam", true, true},
		{"steam UA with prefix", "x.example.org", "Mozilla " + ua, "/depot/1/x", "steam", true, true},
		{"steam server-status", "cache1.steamcontent.com", ua, "/server-status", "steam", true, true},
		{"steam UA without depot path → by host", "level3.blizzard.com", ua, "/tpr/wow/data/aa/bb/cc", "blizzard", true, true},
		{"steam UA without depot path, unknown host", "evil.example.org", ua, "/secret", "", false, false},
		{"steam UA, non-numeric depot", "evil.example.org", ua, "/depot/abc/x", "", false, false},
		{"steam UA, depot without trailing slash", "evil.example.org", ua, "/depot/12", "", false, false},
		{"steam UA, suffix mismatch", "evil.example.org", ua + " ", "/depot/1/x", "", false, false},
		{"other UA with depot path", "evil.example.org", "curl/8", "/depot/1/x", "", false, false},
		{"known host", "US.cdn.blizzard.com", "Battle.net", "/x", "blizzard", true, true},
		{"disabled service is known", "trigger.cache-domains.test", "", "/", "test", false, true},
		{"unknown", "example.org", "", "/", "", false, false},
	}
	for _, tt := range tests {
		id, en, known := r.Classify(tt.host, tt.ua, tt.path)
		if id != tt.id || en != tt.enabled || known != tt.known {
			t.Errorf("%s: Classify = %q %v %v, want %q %v %v", tt.name, id, en, known, tt.id, tt.enabled, tt.known)
		}
	}
}

func TestSNIAllowed(t *testing.T) {
	r, _ := loadedRegistry(t)
	for sni, want := range map[string]string{
		"us.cdn.blizzard.com":        "blizzard",
		"origin-a.akamaihd.net":      "origin",
		"trigger.cache-domains.test": "", // disabled
		"www.example.com":            "",
		"cdn.blizzard.com":           "",
		"lancache.steamcontent.com":  "steam",
	} {
		id, ok := r.SNIAllowed(sni)
		if id != want || ok != (want != "") {
			t.Errorf("SNIAllowed(%q) = %q %v, want %q", sni, id, ok, want)
		}
	}
}

func TestDisabledServicesRebuild(t *testing.T) {
	r, e := loadedRegistry(t)
	ctx := context.Background()
	if err := r.SetEnabled(ctx, "blizzard", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.set.Get().DownloadCache.DisabledServices, "blizzard") {
		t.Fatal("disabled state not persisted in settings")
	}
	if _, ok := r.MatchDNS("level3.blizzard.com"); ok {
		t.Fatal("disabled service still matched")
	}
	if id, en, known := r.Classify("level3.blizzard.com", "", "/"); id != "blizzard" || en || !known {
		t.Fatalf("Classify disabled = %q %v %v", id, en, known)
	}
	// Changing settings directly (PATCH /settings) rebuilds too.
	if _, err := e.set.Update(ctx, func(a *settings.All) error { a.DownloadCache.DisabledServices = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.MatchDNS("level3.blizzard.com"); !ok {
		t.Fatal("re-enabled service not matched")
	}
	if id, ok := r.MatchDNS("trigger.cache-domains.test"); !ok || id != "test" {
		t.Fatal("test service not enabled")
	}
	if err := r.SetEnabled(ctx, "nope", true); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("unknown service: %v", err)
	}
}

func TestOverlapPrecedence(t *testing.T) {
	r, _ := loadedRegistry(t)
	ctx := context.Background()
	// A custom service claims an exact host below a source wildcard and the
	// same wildcard as a source service.
	if _, err := r.CreateCustom(ctx, ServiceInput{Name: "Mirror", Domains: []string{"eu.cdn.blizzard.com", "*.windowsupdate.com"}}); err != nil {
		t.Fatal(err)
	}
	if id, _ := r.MatchDNS("eu.cdn.blizzard.com"); id != "custom-mirror" {
		t.Fatalf("exact beats wildcard: got %q", id)
	}
	if id, _ := r.MatchDNS("us.cdn.blizzard.com"); id != "blizzard" {
		t.Fatalf("wildcard: got %q", id)
	}
	if id, _ := r.MatchDNS("x.windowsupdate.com"); id != "wsus" {
		t.Fatalf("first service wins identical patterns: got %q", id)
	}
	if err := r.SetEnabled(ctx, "wsus", false); err != nil {
		t.Fatal(err)
	}
	if id, _ := r.MatchDNS("x.windowsupdate.com"); id != "custom-mirror" {
		t.Fatalf("an enabled service beats a disabled one: got %q", id)
	}
}

func TestIsSteamPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/depot/1/":             true,
		"/depot/228990/chunk/x": true,
		"/server-status":        true,
		"/server-status/x":      false,
		"/depot/":               false,
		"/depot//x":             false,
		"/depot/12":             false,
		"/depot/1a/x":           false,
		"/x/depot/1/":           false,
	} {
		if got := isSteamPath(p); got != want {
			t.Errorf("isSteamPath(%q) = %v", p, got)
		}
	}
}
