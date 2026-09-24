package services

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestGroupFor(t *testing.T) {
	tests := []struct {
		name, service, host, path string
		key, label, version       string
	}{
		{"steam chunk", "steam", "cache1-fra1.steamcontent.com",
			"/depot/228990/chunk/0123456789abcdef0123456789abcdef01234567",
			"steam:depot:228990", "Steam depot 228990", ""},
		{"steam chunk upper-case hex", "steam", "cache1-fra1.steamcontent.com",
			"/depot/1521/chunk/0123456789ABCDEF0123456789ABCDEF01234567",
			"steam:depot:1521", "Steam depot 1521", ""},
		{"steam manifest", "steam", "cache26-lhr1.steamcontent.com",
			"/depot/1521/manifest/6898858218219951821/5",
			"steam:depot:1521", "Steam depot 1521", "6898858218219951821"},
		{"steam manifest with request code", "steam", "h", "/depot/1521/manifest/6898858218219951821/5/1234567890",
			"steam:depot:1521", "Steam depot 1521", "6898858218219951821"},
		{"steam patch", "steam", "h", "/depot/1521/patch/abc/def", "steam:depot:1521", "Steam depot 1521", ""},
		{"steam other path", "steam", "cache1.steamcontent.com", "/server-status",
			"steam:cache1.steamcontent.com", "Steam · cache1.steamcontent.com", ""},
		{"blizzard wow", "blizzard", "level3.blizzard.com",
			"/tpr/wow/data/3a/b2/3ab2c1d0e9f8a7b6c5d4e3f2a1b0c9d8", "blizzard:wow", "World of Warcraft", ""},
		{"blizzard index", "blizzard", "us.cdn.blizzard.com",
			"/tpr/ovw/data/3a/b2/3ab2c1d0e9f8a7b6c5d4e3f2a1b0c9d8.index", "blizzard:ovw", "Overwatch 2", ""},
		{"blizzard shared configs", "blizzard", "level3.blizzard.com",
			"/tpr/configs/data/aa/bb/aabbccddeeff00112233445566778899", "blizzard:configs", "Battle.net configs (shared)", ""},
		{"blizzard cortez", "blizzard", "level3.blizzard.com",
			"/cortez/Cerberus-B-Live/data/aa/bb/aabbccddeeff00112233445566778899", "blizzard:cerberus-b-live", "Call of Duty: Black Ops 6", ""},
		{"blizzard mixed case", "blizzard", "level3.blizzard.com",
			"/tpr/Hero-Live-a/patch/aa/bb/aabbccddeeff00112233445566778899", "blizzard:hero-live-a", "Heroes of the Storm", ""},
		{"blizzard unknown product", "blizzard", "level3.blizzard.com",
			"/tpr/newgame/config/aa/bb/aabbccddeeff00112233445566778899", "blizzard:newgame", "Newgame", ""},
		{"epic org build", "epicgames", "download.epicgames.com",
			"/Builds/Org/o-dhz7kpvrvqngzdpnx5vn3jrljj53ca/68d2cc08f9a94b8fb51af4f5cfa6d41b/default/ChunksV4/34/4FC5209EAD7B5D6D_F9414B27445C8D727F27F8810CE708A1.chunk",
			"epic:o-dhz7kpvrvqngzdpnx5vn3jrljj53ca/68d2cc08f9a94b8fb51af4f5cfa6d41b", "Epic item 68d2cc08", ""},
		{"epic legacy app", "epicgames", "download.epicgames.com",
			"/Builds/Fortnite/CloudDir/ChunksV4/87/0776F9A6524228DA_2D44B0A04D85F196A59976B00E339A4F.chunk",
			"epic:Fortnite", "Fortnite", ""},
		{"riot bundle", "riot", "lol.dyn.riotcdn.net", "/channels/public/bundles/0123456789ABCDEF.bundle",
			"riot:lol", "League of Legends", ""},
		{"riot manifest", "riot", "ks-foundation.dyn.riotcdn.net", "/channels/public/releases/C1CEE1BFA8098D54.manifest",
			"riot:ks-foundation", "Riot Client", "C1CEE1BFA8098D54"},
		{"riot secure", "riot", "valorant.secure.dyn.riotcdn.net", "/channels/public/bundles/0123456789ABCDEF.bundle",
			"riot:valorant", "VALORANT", ""},
		{"riot other host", "riot", "l3cdn.riotgames.com", "/releases/x",
			"riot:l3cdn.riotgames.com", "Riot Games · l3cdn.riotgames.com", ""},
		{"xbox package", "xboxlive", "assets1.xboxlive.com",
			"/8/436e95ce-0e3c-4a4e-9b8b-1e2f3a4b5c6d/74191251-1c2d-4e5f-8a9b-0c1d2e3f4a5b/101.101.32875.0.72e025b9-2a3b-4c5d-9e8f-a0b1c2d3e4f5/Microsoft.MSPhoenix_101.101.32875.0_x64__8wekyb3d8bbwe.msixvc",
			"xbox:Microsoft.MSPhoenix", "MS Phoenix", "101.101.32875.0"},
		{"xbox package humanised", "xboxlive", "assets2.xboxlive.com",
			"/5/x/BehaviourInteractive.DeadbyDaylightWindows_7.2.0.0_x64__b1gz2xhdanwfm.msixvc",
			"xbox:BehaviourInteractive.DeadbyDaylightWindows", "Deadby Daylight Windows", "7.2.0.0"},
		{"wsus kb", "wsus", "catalog.s.download.windowsupdate.com",
			"/c/msdownload/update/software/secu/2025/09/windows10.0-kb5065428-x64_3c83d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0fb1.msu",
			"win:kb5065428", "KB5065428", "2025-09"},
		{"wsus delivery optimization with query", "wsus", "msedge.b.tlu.dl.delivery.mp.microsoft.com",
			"/filestreamingservice/files/31e601a4-fc62-4f8c-a2e0-504db186136c?P1=1&P4=secret",
			"win:do", "Windows Update (Delivery Optimization)", ""},
		{"wsus office", "wsus", "officecdn.microsoft.com",
			"/pr/492350f6-3a01-4f97-b9c0-c7c6ddf67d60/Office/Data/16.0.17928.20156/stream.x64.x-none.dat",
			"win:office:492350f6-3a01-4f97-b9c0-c7c6ddf67d60", "Microsoft 365 Apps (492350f6…)", "16.0.17928.20156"},
		{"wsus other", "wsus", "dl.delivery.mp.microsoft.com", "/other/file.bin",
			"wsus:dl.delivery.mp.microsoft.com", "Windows Update · dl.delivery.mp.microsoft.com", ""},
		{"sony", "sony", "gs2.ww.prod.dl.playstation.net",
			"/gs2/ppkgo/prod/CUSA07320_00/35/f_3a26abcdef/f/EP9000-CUSA07320_00-HRZ0000000000000-A0152-V0101-DP.pkg",
			"psn:CUSA07320", "CUSA07320", "0101"},
		{"sony ps5", "sony", "gs2.ww.prod.dl.playstation.net", "/gs2/ppkgo/prod/PPSA01234_00/1/f/x.pkg",
			"psn:PPSA01234", "PPSA01234", ""},
		{"nintendo", "nintendo", "atum.hac.lp1.d4c.nintendo.net", "/c/c/0123456789abcdef0123456789abcdef",
			"nintendo:switch", "Nintendo eShop content", ""},
		{"uplay", "uplay", "uplaypc-s-ubisoft.cdn.ubi.com", "/uplaypc/downloads/635/slices_v3/abc",
			"ubi:635", "Ubisoft 635", ""},
		{"origin", "origin", "origin-a.akamaihd.net", "/eamaster/s/shift/battlefield_v/files/x.zip",
			"ea:battlefield_v", "EA Battlefield V", ""},
		{"wargaming", "wargaming", "dl-wows-ak.wargaming.net", "/eu/patches/x.bin",
			"wg:wows", "World of Warships", ""},
		{"fallback", "cod", "cod-assets.cdn.callofduty.com", "/x/y.bin",
			"cod:cod-assets.cdn.callofduty.com", "Call of Duty · cod-assets.cdn.callofduty.com", ""},
		{"rule of another service is not applied", "cod", "h.example.com", "/depot/1/chunk/0123456789abcdef0123456789abcdef01234567",
			"cod:h.example.com", "Call of Duty · h.example.com", ""},
		{"oversized key falls back", "epicgames", "download.epicgames.com", "/Builds/" + strings.Repeat("a", 300) + "/CloudDir/x",
			"epicgames:download.epicgames.com", "Epic Games · download.epicgames.com", ""},
	}
	for _, tt := range tests {
		g := GroupFor(tt.service, tt.host, tt.path)
		if g.Key != tt.key || g.Label != tt.label || g.Version != tt.version {
			t.Errorf("%s: GroupFor = %+v, want {%s %s %s}", tt.name, g, tt.key, tt.label, tt.version)
		}
	}
}

func TestIsBypassPath(t *testing.T) {
	for p, want := range map[string]bool{
		"/server-status":                    true,
		"/latest64/manifest":                true,
		"/releases/live/releaselisting_EUW": true,
		"/patcher/lol.version":              true,
		"/msdownload/update/v3/static/trustedr/en/authrootstl.cab": true,
		"/x/PINRULESSTL.CAB":       true,
		"/x/disallowedcertstl.cab": true,
		"/depot/1/chunk/x":         false,
		"/version":                 false, // ".version$" needs a character before
		"/x/authrootstl.cab.bak":   false,
		"/server-status2":          false,
	} {
		if got := IsBypassPath(p); got != want {
			t.Errorf("IsBypassPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestHumanise(t *testing.T) {
	for in, want := range map[string]string{
		"DeadbyDaylightWindows": "Deadby Daylight Windows",
		"MSPhoenix":             "MS Phoenix",
		"battlefield_v":         "Battlefield V",
		"ue5-releases":          "Ue5 Releases",
		"Fortnite":              "Fortnite",
		"":                      "",
		"---":                   "---",
	} {
		if got := humanise(in); got != want {
			t.Errorf("humanise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLabels(t *testing.T) {
	r, e := loadedRegistry(t)
	ctx := context.Background()
	if got := r.Label("steam:depot:228990"); got != "Steam depot 228990" {
		t.Fatalf("default label = %q", got)
	}
	if err := r.SetLabel(ctx, "steam:depot:228990", "  Steamworks Redistributables "); err != nil {
		t.Fatal(err)
	}
	if got := r.Label("steam:depot:228990"); got != "Steamworks Redistributables" {
		t.Fatalf("user label = %q", got)
	}
	sv, err := r.CreateCustom(ctx, ServiceInput{Name: "LAN Party Mirror", Domains: []string{"m.example.org"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Label(sv.ID + ":m.example.org"); got != "LAN Party Mirror · m.example.org" {
		t.Fatalf("custom fallback label = %q", got)
	}
	got := r.Labels([]string{"blizzard:fenris", "riot:lol"})
	if got["blizzard:fenris"] != "Diablo IV" || got["riot:lol"] != "League of Legends" {
		t.Fatalf("Labels = %v", got)
	}

	// Search: user labels and built-in product labels, case-insensitive.
	if keys := r.SearchLabels("REDIST"); !slices.Equal(keys, []string{"steam:depot:228990"}) {
		t.Fatalf("search user label = %q", keys)
	}
	keys := r.SearchLabels("call of duty")
	if !slices.Contains(keys, "blizzard:cerberus-b-live") || !slices.Contains(keys, "blizzard:auks") || slices.Contains(keys, "blizzard:wow") {
		t.Fatalf("search product label = %q", keys)
	}
	if keys := r.SearchLabels("  "); keys != nil {
		t.Fatalf("empty search = %q", keys)
	}
	// A user label replaces the product label in search too.
	if err := r.SetLabel(ctx, "blizzard:fenris", "D4"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.SearchLabels("diablo iv"), "blizzard:fenris") {
		t.Fatal("overridden product label still searchable")
	}

	// Persisted; "" removes.
	r2 := e.registry(t)
	if got := r2.Label("steam:depot:228990"); got != "Steamworks Redistributables" {
		t.Fatalf("label after restart = %q", got)
	}
	if err := r2.SetLabel(ctx, "steam:depot:228990", ""); err != nil {
		t.Fatal(err)
	}
	if got := r2.Label("steam:depot:228990"); got != "Steam depot 228990" {
		t.Fatalf("label after removal = %q", got)
	}

	for _, bad := range []struct{ key, label string }{
		{"", "x"},
		{"nocolon", "x"},
		{"a:b\n", "x"},
		{"a:" + strings.Repeat("b", maxLabelKeyLen), "x"},
		{"a:b", strings.Repeat("l", maxLabelLen+1)},
		{"a:b", "bad\x00label"},
	} {
		if err := r2.SetLabel(ctx, bad.key, bad.label); apperr.KindOf(err) != apperr.KindInvalid {
			t.Errorf("SetLabel(%q, %q) = %v", bad.key, bad.label, err)
		}
	}
}
