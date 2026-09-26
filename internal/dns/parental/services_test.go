package parental

import (
	"context"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/publicsuffix"
)

// publicSuffixes are suffixes a catalogue domain must never be, on top of
// x/net/publicsuffix (ICANN and private section).
var publicSuffixes = []string{"co.uk", "com.au", "co.jp", "com.br", "co.nz", "org.uk", "github.io", "blogspot.com",
	"githubusercontent.com", "herokuapp.com", "netlify.app", "vercel.app", "pages.dev", "workers.dev", "web.app",
	"firebaseapp.com", "appspot.com", "azurewebsites.net"}

// shared are names whose blocking breaks other services or security: a
// catalogue domain may not equal one or be a parent of one (its subtree
// would cover it); a specific name below one is fine
// (youtubei.googleapis.com).
var shared = []string{"googleapis.com", "akamaihd.net", "akamaized.net", "cloudfront.net", "fastly.net", "media-amazon.com",
	"amazonaws.com", "google.com", "gstatic.com", "googleusercontent.com", "azureedge.net", "edgekey.net", "akamai.net",
	"cloudflare.com", "microsoft.com", "apple.com", "ggpht.com", "l.google.com",
	// updates, identity, certificates, time and connectivity checks
	"windowsupdate.com", "update.microsoft.com", "swcdn.apple.com", "mesu.apple.com", "login.microsoftonline.com",
	"microsoftonline.com", "live.com", "accounts.google.com", "appleid.apple.com", "icloud.com", "sharepoint.com",
	"office.com", "digicert.com", "letsencrypt.org", "pki.goog", "time.apple.com", "time.windows.com", "pool.ntp.org",
	"captive.apple.com", "connectivitycheck.gstatic.com", "msftconnecttest.com", "msftncsi.com", "proton.me"}

// frozenServices are the ids of v0.9.0 with their domains then: ids never
// change and domain sets only grow.
var frozenServices = map[string][]string{
	"disneyplus": {"disneyplus.com", "disney-plus.net", "dssott.com", "bamgrid.com", "disneystreaming.com"},
	"kick":       {"kick.com"},
	"netflix":    {"netflix.com", "netflix.net", "nflxext.com", "nflximg.com", "nflximg.net", "nflxso.net", "nflxvideo.net"},
	"primevideo": {"primevideo.com", "aiv-cdn.net", "aiv-delivery.net", "amazonvideo.com", "pv-cdn.net"},
	"tiktok": {"tiktok.com", "tiktokv.com", "tiktokv.us", "tiktokw.us", "tiktokcdn.com", "tiktokcdn-us.com", "tiktokcdn-eu.com",
		"byteoversea.com", "ibytedtos.com", "ibyteimg.com", "muscdn.com", "musical.ly", "tik-tokapi.com", "ttwstatic.com"},
	"twitch": {"twitch.tv", "ttvnw.net", "jtvnw.net", "twitchcdn.net", "twitchsvc.net", "ext-twitch.tv", "live-video.net"},
	"youtube": {"youtube.com", "youtu.be", "yt.be", "ytimg.com", "googlevideo.com", "youtube-nocookie.com", "youtubekids.com",
		"youtubei.googleapis.com", "youtube.googleapis.com", "yt3.ggpht.com", "youtube-ui.l.google.com"},
	"bereal":    {"bereal.com", "bere.al"},
	"facebook":  {"facebook.com", "facebook.net", "fb.com", "fb.me", "fbcdn.net", "fbsbx.com", "messenger.com", "m.me"},
	"instagram": {"instagram.com", "cdninstagram.com", "ig.me", "instagr.am"},
	"pinterest": {"pinterest.com", "pinimg.com", "pin.it", "pinterest.de", "pinterest.at", "pinterest.ch", "pinterest.co.uk", "pinterest.fr"},
	"reddit":    {"reddit.com", "redd.it", "redditmedia.com", "redditstatic.com"},
	"snapchat": {"snapchat.com", "snap.com", "sc-cdn.net", "sc-static.net", "sc-prod.net", "sc-gw.com", "snapads.com", "snapkit.co",
		"bitmoji.com"},
	"threads":     {"threads.net", "threads.com"},
	"x":           {"x.com", "twitter.com", "twimg.com", "t.co", "twttr.com"},
	"discord":     {"discord.com", "discord.gg", "discordapp.com", "discordapp.net", "discord.media", "discordcdn.com"},
	"telegram":    {"telegram.org", "telegram.me", "t.me", "telesco.pe", "tdesktop.com", "telegra.ph"},
	"whatsapp":    {"whatsapp.com", "whatsapp.net", "wa.me"},
	"fortnite":    {"fortnite.com", "epicgames.com", "epicgames.dev"},
	"minecraft":   {"minecraft.net", "mojang.com", "minecraftservices.com", "minecraft-services.net"},
	"nintendo":    {"nintendo.com", "nintendo.net", "nintendo.de", "nintendo-europe.com"},
	"playstation": {"playstation.com", "playstation.net", "sonyentertainmentnetwork.com"},
	"roblox":      {"roblox.com", "rbxcdn.com", "rbx.com", "robloxlabs.com"},
	"steam": {"steampowered.com", "steamcommunity.com", "steamstatic.com", "steamcontent.com", "steamserver.net", "steamgames.com",
		"steam-chat.com"},
	"xbox":    {"xbox.com", "xboxlive.com", "xboxservices.com"},
	"spotify": {"spotify.com", "scdn.co", "spotifycdn.com", "spotifycdn.net", "pscdn.co"},
	"chatgpt": {"chatgpt.com", "openai.com", "oaistatic.com", "oaiusercontent.com"},
}

// The catalogue is a plain literal; this test is its validation: slugs,
// categories, order, valid A-label domains, no duplicates or nesting
// across services, no public suffixes and no shared infrastructure.
func TestCatalogue(t *testing.T) {
	ids := map[string]bool{}
	owner := map[string]string{}
	prevCat, prevName := 0, ""
	for i, s := range catalogue {
		if s.ID == "" || strings.Trim(s.ID, "abcdefghijklmnopqrstuvwxyz0123456789") != "" || ids[s.ID] {
			t.Errorf("service %d: bad or duplicate id %q", i, s.ID)
		}
		ids[s.ID] = true
		if s.Name == "" || len(s.Domains) == 0 {
			t.Errorf("%s: needs a name and at least one domain", s.ID)
		}
		cat := slices.Index(categories, s.Category)
		if cat < 0 {
			t.Errorf("%s: unknown category %q", s.ID, s.Category)
		}
		if cat < prevCat || cat == prevCat && strings.ToLower(s.Name) < strings.ToLower(prevName) {
			t.Errorf("%s: catalogue not sorted by category, then name", s.ID)
		}
		prevCat, prevName = cat, s.Name
		for _, d := range s.Domains {
			if !validDomain(d) {
				t.Errorf("%s: %q is not a valid lower-case A-label domain", s.ID, d)
			}
			if ps, _ := publicsuffix.PublicSuffix(d); ps == d || !strings.Contains(d, ".") || slices.Contains(publicSuffixes, d) {
				t.Errorf("%s: %q is a public suffix", s.ID, d)
			}
			for _, sh := range shared {
				if d == sh || strings.HasSuffix(sh, "."+d) {
					t.Errorf("%s: %q is or covers the shared name %s", s.ID, d, sh)
				}
			}
			if o, ok := owner[d]; ok {
				t.Errorf("%s: %q is already a domain of %s", s.ID, d, o)
			}
			owner[d] = s.ID
		}
	}
	// No domain lies inside another domain of the catalogue (the match
	// would depend on the walk, not on the catalogue), within a service
	// or across services.
	for d, o := range owner {
		for p := d; strings.Contains(p, "."); {
			p = p[strings.IndexByte(p, '.')+1:]
			if po, ok := owner[p]; ok {
				t.Errorf("%q (%s) is inside %q (%s)", d, o, p, po)
			}
		}
	}
	if got := Services(); len(got) != len(catalogue) || &got[0].Domains[0] == &catalogue[0].Domains[0] {
		t.Error("Services must return a copy of the catalogue")
	}
	if len(catalogue) < 120 || len(catalogue) > maxServices {
		t.Errorf("catalogue has %d services (about 140, at most %d)", len(catalogue), maxServices)
	}
	for _, c := range categories {
		if !slices.ContainsFunc(catalogue, func(s Service) bool { return s.Category == c }) {
			t.Errorf("category %s has no service", c)
		}
	}
}

// Released ids are frozen and their domain sets only grow.
func TestFrozenServices(t *testing.T) {
	if len(frozenServices) != 27 {
		t.Fatalf("%d frozen ids", len(frozenServices))
	}
	for id, domains := range frozenServices {
		i, ok := serviceIndex[id]
		if !ok {
			t.Errorf("frozen id %s left the catalogue", id)
			continue
		}
		for _, d := range domains {
			if !slices.Contains(catalogue[i].Domains, d) {
				t.Errorf("%s lost the domain %s", id, d)
			}
		}
	}
}

// Blocking every service validates and round-trips, also as the services
// of a schedule.
func TestAllServicesRoundTrip(t *testing.T) {
	env := newTestEnv(t)
	kids := env.group("Kids")
	all := make([]string, 0, len(catalogue))
	for _, s := range catalogue {
		all = append(all, s.ID)
	}
	gc := env.update(kids, Config{BlockedServices: all, Schedules: []Schedule{
		{Name: "All", Enabled: true, Days: []int{1}, Start: "08:00", End: "09:00", Block: BlockServices, Services: all}}})
	want := slices.Sorted(slices.Values(all))
	if !slices.Equal(gc.BlockedServices, want) || !slices.Equal(gc.Schedules[0].Services, want) {
		t.Fatalf("round trip: %d services, %d in the schedule", len(gc.BlockedServices), len(gc.Schedules[0].Services))
	}
	got, err := env.e.Get(context.Background(), kids)
	if err != nil || !slices.Equal(got.BlockedServices, want) {
		t.Fatalf("stored: %d %v", len(got.BlockedServices), err)
	}
}

// validDomain checks a lower-case A-label domain: labels of 1–63 letters,
// digits and hyphens, not starting or ending with a hyphen.
func validDomain(d string) bool {
	if d == "" || len(d) > 253 {
		return false
	}
	for label := range strings.SplitSeq(d, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' ||
			strings.Trim(label, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" {
			return false
		}
	}
	return true
}

func TestServiceSuffixMatching(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
	}{
		{"youtube.com", "youtube"},
		{"www.youtube.com", "youtube"},
		{"r3---sn-4g5e6nzl.googlevideo.com", "youtube"},
		{"youtubei.googleapis.com", "youtube"},
		{"maps.googleapis.com", ""}, // shared infrastructure is never matched
		{"notyoutube.com", ""},
		{"youtube.com.example.net", ""},
		{"t.co", "x"},
		{"est.co", ""},
		{"a.b.c.tiktokcdn-eu.com", "tiktok"},
		{"com", ""},
		{"", ""},
		{strings.Repeat("a.", 120) + "roblox.com", "roblox"},
		{"music.apple.com", "applemusic"},
		{"amp-api.music.apple.com", "applemusic"},
		{"apps.apple.com", "appstore"},
		{"www.apple.com", ""},
		{"drive.google.com", "googledrive"},
		{"mail.google.com", ""},
		{"www.amazon.de", "amazonshop"},
		{"alexa.amazon.de", ""},
		{"onedrive.live.com", "onedrive"},
		{"login.live.com", ""},
	} {
		got := ""
		if i := serviceOf(tc.name); i >= 0 {
			got = catalogue[i].ID
		}
		if got != tc.want {
			t.Errorf("serviceOf(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
	// More labels than a DNS name can have: the walk stops.
	if i := serviceOf(strings.Repeat("a.", 130) + "youtube.com"); i >= 0 {
		t.Error("the suffix walk must stop after 127 labels")
	}
	if n := testing.AllocsPerRun(100, func() { serviceOf("www.example.youtube.com") }); n != 0 {
		t.Errorf("serviceOf allocates %v times", n)
	}
}
