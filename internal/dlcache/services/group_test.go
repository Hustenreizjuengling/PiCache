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
			"steam:depot:228990", "DirectX (June 2010) · Steamworks Common Redistributables", ""},
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
			"/tpr/ovw/data/3a/b2/3ab2c1d0e9f8a7b6c5d4e3f2a1b0c9d8.index", "blizzard:ovw", "Overwatch", ""},
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
			"win:kb5065428:windows10.0-x64", "KB5065428 · Windows 10 (x64)", "2025-09"},
		{"wsus delivery optimization with query", "wsus", "msedge.b.tlu.dl.delivery.mp.microsoft.com",
			"/filestreamingservice/files/31e601a4-fc62-4f8c-a2e0-504db186136c?P1=1&P4=secret",
			"win:do", "Windows Update (Delivery Optimization)", ""},
		{"wsus office", "wsus", "officecdn.microsoft.com",
			"/pr/492350f6-3a01-4f97-b9c0-c7c6ddf67d60/Office/Data/16.0.17928.20156/stream.x64.x-none.dat",
			"win:office:492350f6-3a01-4f97-b9c0-c7c6ddf67d60", "Microsoft 365 Apps · Current Channel", "16.0.17928.20156"},
		{"wsus office, unknown channel", "wsus", "officecdn.microsoft.com",
			"/pr/0f0e0d0c-3a01-4f97-b9c0-c7c6ddf67d60/Office/Data/16.0.17928.20156/stream.x64.x-none.dat",
			"win:office:0f0e0d0c-3a01-4f97-b9c0-c7c6ddf67d60", "Microsoft 365 Apps (0f0e0d0c…)", "16.0.17928.20156"},
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
			"ubi:635", "Tom Clancy's Rainbow Six Siege", ""},
		{"origin", "origin", "origin-a.akamaihd.net", "/eamaster/s/shift/battlefield_v/files/x.zip",
			"ea:battlefield_v", "EA Battlefield V", ""},
		{"wargaming", "wargaming", "dl-wows-ak.wargaming.net", "/eu/patches/x.bin",
			"wg:wows", "World of Warships", ""},
		// 1.3.0: the paths below are real requests (access logs, launcher
		// sources and the CDNs themselves).
		{"blizzard event path", "blizzard", "level3.blizzard.com", "/tpr/hse/2/data/aa/bb/aabbccddeeff00112233445566778899",
			"blizzard:hse", "Hse", ""},
		{"blizzard new product", "blizzard", "level3.blizzard.com", "/tpr/aris/data/aa/bb/aabbccddeeff00112233445566778899",
			"blizzard:aris", "DOOM: The Dark Ages", ""},
		{"epic engine build", "epicgames", "egdownload.fastly-edge.com",
			"/Builds/Rocket/Automated/Engine/CloudDir/cYiUOBzQbIx6kgokk0iweeW0If8BDw.manifest",
			"epic:Rocket", "Unreal Engine", ""},
		{"epic launcher", "epicgames", "egdownload.fastly-edge.com",
			"/Builds/UnrealEngineLauncher/CloudDir/ChunksV4/70/03C4BAA989C2ACAE_FA43071F4582E6E1E4454C93023A020B.chunk",
			"epic:UnrealEngineLauncher", "Epic Games Launcher", ""},
		{"riot runeterra", "riot", "bacon.secure.dyn.riotcdn.net", "/channels/public/bundles/0123456789ABCDEF.bundle",
			"riot:bacon", "Legends of Runeterra", ""},
		{"riot 2xko", "riot", "lion.secure.dyn.riotcdn.net", "/channels/public/releases/096F1182650349E2.manifest",
			"riot:lion", "2XKO", "096F1182650349E2"},
		{"wsus kb windows 11 dotnet", "wsus", "catalog.s.download.windowsupdate.com",
			"/d/msdownload/update/software/updt/2025/09/windows11.0-kb5066613-x64-ndp481_0123456789abcdef0123456789abcdef01234567.msu",
			"win:kb5066613:windows11.0-x64-ndp481", "KB5066613 · .NET Framework 4.8.1 · Windows 11 (x64)", "2025-09"},
		{"wsus kb windows 11 on the delivery network", "wsus", "catalog.sf.dl.delivery.mp.microsoft.com",
			"/filestreamingservice/files/7342fa97-0e3c-4a4e-9b8b-1e2f3a4b5c6d/public/windows11.0-kb5065426-arm64_0123456789abcdef0123456789abcdef01234567.msu",
			"win:kb5065426:windows11.0-arm64", "KB5065426 · Windows 11 (arm64)", ""},
		{"wsus kb windows 8 rt", "wsus", "catalog.s.download.windowsupdate.com",
			"/c/msdownload/update/software/secu/2023/10/windows8-rt-kb5031442-x64_0123456789abcdef0123456789abcdef01234567.msu",
			"win:kb5031442:windows8-rt-x64", "KB5031442 · Windows 8 (x64)", "2023-10"},
		{"wsus dotnet installer", "wsus", "download.windowsupdate.com",
			"/c/msdownload/update/software/ftpk/2019/04/ndp48-kb4486153-x86-x64-allos-enu_0123456789abcdef.exe",
			"win:kb4486153:ndp48", "KB4486153 · .NET Framework 4.8", "2019-04"},
		{"wsus msrt", "wsus", "catalog.s.download.windowsupdate.com",
			"/c/msdownload/update/software/uprl/2026/10/windows-kb890830-x64-v5.146_0123456789abcdef0123456789abcdef01234567.exe",
			"win:kb890830", "Windows Malicious Software Removal Tool", "5.146"},
		{"sony ps5 gst", "sony", "gst.prod.dl.playstation.net",
			"/gst/prod/00/PPSA15301_00/app/pkg/51/f_0123abcd/UP1001-PPSA15301_00-NBA2K24000000000_29.pkg",
			"psn:PPSA15301", "PPSA15301", ""},
		{"nintendo wii u update", "nintendo", "ccs.cdn.wup.shop.nintendo.net", "/ccs/download/0005000E1010EC00/tmd",
			"nintendo:0005000e1010ec00", "Wii U update 1010EC00", ""},
		{"nintendo 3ds game", "nintendo", "ccs.cdn.wup.shop.nintendo.net", "/ccs/download/0004000000030700/00000000",
			"nintendo:0004000000030700", "3DS game 00030700", ""},
		{"uplay unknown product", "uplay", "uplaypc-s-ubisoft.cdn.ubi.com", "/uplaypc/downloads/99999/manifests/abc.manifest",
			"ubi:99999", "Ubisoft product 99999", ""},
		{"origin franchise and title", "origin", "lvlt.cdn.ea.com",
			"/eamaster/s/shift/bejeweled/bejeweled_3/fg__ww_rtp_remaster/bejeweled_3pcfg__ww_rtp_remasterrun_1.zip",
			"ea:bejeweled/bejeweled_3", "Bejeweled 3", ""},
		{"origin ea app", "origin", "origin-a.akamaihd.net", "/EA-Desktop-Client-Download/installer-releases/EAappInstaller.exe",
			"ea:app", "EA app", ""},
		{"origin swtor", "origin", "cdn-patch.swtor.com", "/patch/assets_swtor_main/assets_swtor_main_263to264.solidpkg",
			"ea:swtor", "Star Wars: The Old Republic", ""},
		{"wargaming version", "wargaming", "dl-wot-gc.wargaming.net",
			"/eu/patches/wot_2.4.1.5485_eu_0qv8kz/wot_2.4.1.24080_client.wgpkg.001", "wg:wot", "World of Tanks", "2.4.1.5485"},
		{"rockstar", "rockstar", "patches.rockstargames.com", "/prod/gtav/Launcher_EFIGS/GTA_V_Launcher_1_0_323_1.exe",
			"rockstar:gtav", "Grand Theft Auto V", ""},
		{"rockstar unknown title", "rockstar", "patches.rockstargames.com", "/prod/newgame/x.exe",
			"rockstar:newgame", "Rockstar Games · newgame", ""},
		{"ffxiv expansion", "square", "patch-dl.ffxiv.com", "/game/ex3/859d0e24/D2026.10.16.0000.0000.patch",
			"ffxiv:ex3", "Final Fantasy XIV · Shadowbringers", "2026.10.16.0000.0000"},
		{"ffxiv base game full patch part", "square", "patch-dl.ffxiv.com", "/game/4e9a232b/H2024.05.31.0000.0000af.patch",
			"ffxiv:game", "Final Fantasy XIV · Base game", "2024.05.31.0000.0000"},
		{"ffxiv launcher", "square", "patch-dl.ffxiv.com", "/boot/2b5cbc63/D2026.08.18.0000.0001.patch",
			"ffxiv:boot", "Final Fantasy XIV · Launcher", "2026.08.18.0000.0001"},
		{"ffxiv future expansion", "square", "patch-dl.ffxiv.com", "/game/ex6/0123abcd/D2028.01.01.0000.0000.patch",
			"ffxiv:ex6", "Final Fantasy XIV · Expansion 6", "2028.01.01.0000.0000"},
		{"daybreak", "daybreak", "pls.patch.daybreakgames.com", "/patch/sha/planetside2/planetside2.sha.zs/d4/ea1/4634c6be",
			"daybreak:planetside2", "PlanetSide 2", ""},
		{"arenanet", "arenanet", "assetcdn.101.arenanetworks.com", "/program/101/1/0/2115870/compressed",
			"arenanet:101", "Guild Wars 2", ""},
		{"city of heroes", "cityofheroes", "cdn-na1.homecomingservers.com", "/hc/assets/live-7372/patch/bin.pigg",
			"cityofheroes:homecoming", "City of Heroes: Homecoming", "live-7372"},
		{"city of heroes base client", "cityofheroes", "cdn-eu1.homecomingservers.com", "/coh/piggs/texWorld.pigg",
			"cityofheroes:homecoming", "City of Heroes: Homecoming", ""},
		{"renegade x", "renegadex", "patches.totemarts.services", "/Patch18038/full/0123abcd",
			"renegadex:renegadex", "Renegade X", "18038"},
		{"renegade x black dawn", "renegadex", "patches.totemarts.services", "/BlackDawn1.0/instructions.json",
			"renegadex:blackdawn", "Renegade X: Black Dawn", "1.0"},
		{"nexus mods", "nexusmods", "filedelivery.nexusmods.com", "/1151/22579/THBrows-6186-1-1.rar",
			"nexus:1151", "Fallout 4 · Nexus Mods", ""},
		{"nexus mods unknown game", "nexusmods", "filedelivery.nexusmods.com", "/9999/x.7z",
			"nexus:9999", "Nexus Mods · game 9999", ""},
		{"path of exile", "pathofexile", "patchcdn.pathofexile.com", "/3.10.1.5/Metadata/x.hideout",
			"pathofexile:poe", "Path of Exile", "3.10.1.5"},
		{"tarkov", "bsg", "cdn-11.eft-store.com",
			"/eft/client/live/distribs/1.0.2.0.43037_0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0/Client.1.0.2.0.43037.zip",
			"bsg:eft", "Escape from Tarkov", "1.0.2.0.43037"},
		{"tarkov launcher", "bsg", "cdn-11.eft-store.com", "/LauncherDistribs/15.1.0.4635_0123abcd/BsgLauncher.15.1.0.4635.exe",
			"bsg:launcher", "Battlestate Games Launcher", "15.1.0.4635"},
		{"call of duty", "cod", "cod-assets.cdn.callofduty.com", "/pc/iw9_2/23/a2/da8284f8013d91a2_0003f000_0",
			"cod:pc", "Call of Duty · texture streaming (PC)", "iw9_2"},
		{"elite dangerous manifest", "frontier", "cdn.zaonce.net",
			"/elitedangerous/win/manifests/Win64_4_0_0_10_Alpha+%282021.04.09.263090%29.xml.gz",
			"frontier:elitedangerous", "Elite Dangerous", "2021.04.09.263090"},
		{"elite dangerous file", "frontier", "cdn.zaonce.net", "/elitedangerous/win/files/62/2451401c93cb555d2961d81248b2b6cdae261b",
			"frontier:elitedangerous", "Elite Dangerous", ""},
		{"eso", "teso", "live.patcher.elderscrollsonline.com",
			"/products/eso/874DCDCF-C40C-4956-92FE-E39B8DC4764E/game_player/game_player_-1to378/game_player_-1to378.zip",
			"teso:eso", "The Elder Scrolls Online", ""},
		{"warframe", "warframe", "content.warframe.com", "/Cache.Windows/B.Misc.cache.0123456789abcdef0123456789abcdef.bulk",
			"warframe:game", "Warframe", ""},
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
		"/latest/101":                       true,
		"/latestmac64/101":                  true,
		"/latest32/101":                     true,
		"/index.txt.lzma":                   true,
		"/origin/0A1B2C3D/index.txt.lzma":   true,
		"/latest/101/x":                     false,
		"/Tools/index.txt.lzma":             false,
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
	if got := r.Label("steam:depot:1521"); got != "Steam depot 1521" {
		t.Fatalf("default label = %q", got)
	}
	if err := r.SetLabel(ctx, "steam:depot:1521", "  Half-Life 2 Content "); err != nil {
		t.Fatal(err)
	}
	if got := r.Label("steam:depot:1521"); got != "Half-Life 2 Content" {
		t.Fatalf("user label = %q", got)
	}
	if l, ok := r.UserLabel("steam:depot:1521"); !ok || l != "Half-Life 2 Content" {
		t.Fatalf("UserLabel = %q, %v", l, ok)
	}
	if l, ok := r.UserLabel("riot:lol"); ok || l != "" {
		t.Fatalf("built-in label reported as a user label: %q", l)
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
	if keys := r.SearchLabels("HALF-LIFE"); !slices.Equal(keys, []string{"steam:depot:1521"}) {
		t.Fatalf("search user label = %q", keys)
	}
	// The Steamworks redistributables are named after their component.
	if got := r.Label("steam:depot:229003"); got != ".NET Framework 4.0 Client Profile · Steamworks Common Redistributables" {
		t.Fatalf("redistributable = %q", got)
	}
	if keys := r.SearchLabels(".net framework 4.0"); !slices.Equal(keys, []string{"steam:depot:229002", "steam:depot:229003"}) {
		t.Fatalf("search redistributables = %q", keys)
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
	if got := r2.Label("steam:depot:1521"); got != "Half-Life 2 Content" {
		t.Fatalf("label after restart = %q", got)
	}
	if err := r2.SetLabel(ctx, "steam:depot:1521", ""); err != nil {
		t.Fatal(err)
	}
	if got := r2.Label("steam:depot:1521"); got != "Steam depot 1521" {
		t.Fatalf("label after removal = %q", got)
	}
	if _, ok := r2.UserLabel("steam:depot:1521"); ok {
		t.Fatal("removed label still reported as a user label")
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
