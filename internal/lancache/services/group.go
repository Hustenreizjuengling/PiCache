package services

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxGroupKeyLen bounds group keys; longer derived keys fall back to
// "<service>:<host>".
const maxGroupKeyLen = 200

// Content grouping rules (ARCHITECTURE 8.4). The regexes were verified
// against real CDN URLs (research: verify-observability.md §7).
var (
	steamDepotRE   = regexp.MustCompile(`^/depot/(\d+)/(?:chunk/[0-9a-fA-F]{40}|manifest/(\d+)/\d+(?:/\d+)?|[^/]+/)`)
	blizzardRE     = regexp.MustCompile(`^/(tpr|cortez)/([^/]+)/(config|data|patch)/`)
	epicOrgRE      = regexp.MustCompile(`^/Builds/Org/(o-[a-z0-9]+)/([0-9a-f]{32})/`)
	epicAppRE      = regexp.MustCompile(`^/Builds/(.+?)/CloudDir/`)
	riotHostRE     = regexp.MustCompile(`^([a-z0-9-]+)\.(?:secure\.)?dyn\.riotcdn\.net$`)
	riotManifestRE = regexp.MustCompile(`^/channels/public/releases/([0-9A-F]{16})\.manifest$`)
	msPackageRE    = regexp.MustCompile(`^([A-Za-z0-9.\-]+)_(\d+(?:\.\d+){3})_(x64|x86|arm64|neutral)__([a-z0-9]{13})\.(msixvc|xvc|appx|appxbundle|msix|msixbundle|eappx|eappxbundle)$`)
	msKBRE         = regexp.MustCompile(`(?i)(?:^|[-_/])kb(\d{6,8})(?:[-_.]|$)`)
	msKBDateRE     = regexp.MustCompile(`/(?:secu|updt|crup|ftpk|uprl)/(\d{4})/(\d{2})/`)
	msDORE         = regexp.MustCompile(`^/filestreamingservice/files/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	msOfficeRE     = regexp.MustCompile(`^/pr/([0-9a-f-]{36})/Office/Data/(\d+\.\d+\.\d+\.\d+)/`)
	sonyTitleRE    = regexp.MustCompile(`/((?:CUSA|PPSA)\d{5})_00/`)
	sonyVersionRE  = regexp.MustCompile(`-V(\d{4})-`)
	nintendoRE     = regexp.MustCompile(`^/c/[csa]/[0-9a-f]{32}`)
	uplayRE        = regexp.MustCompile(`^/uplaypc/downloads/([^/]+)/`)
	originRE       = regexp.MustCompile(`^/eamaster/s/shift/([^/]+)/`)
	wargamingRE    = regexp.MustCompile(`^dl-(wot|wows|wowp)-`)

	// Special pass-through paths (ARCHITECTURE 8.2 step 6).
	riotReleaseListingRE = regexp.MustCompile(`^.+(releaselisting_.*|.version$)`)
	certTrustCabRE       = regexp.MustCompile(`(?i)(authrootstl|pinrulesstl|disallowedcertstl)\.cab$`)
)

// blizzardProducts maps TACT CDN paths (lower-case) to product names.
var blizzardProducts = map[string]string{
	"wow":             "World of Warcraft",
	"ovw":             "Overwatch 2",
	"fenris":          "Diablo IV",
	"diablo3":         "Diablo III",
	"hs":              "Hearthstone",
	"sc2":             "StarCraft II",
	"sc1live":         "StarCraft: Remastered",
	"war3":            "Warcraft III: Reforged",
	"w1r-live":        "Warcraft I: Remastered",
	"w2r-live":        "Warcraft II: Remastered",
	"w2bn":            "WarCraft II: Battle.net Edition",
	"hero-live-a":     "Heroes of the Storm",
	"osi":             "Diablo II: Resurrected",
	"anbs":            "Diablo Immortal",
	"gryphon":         "Warcraft Rumble",
	"b04":             "Call of Duty: Black Ops 4",
	"odin":            "Call of Duty: Modern Warfare (2019)",
	"auks":            "Call of Duty",
	"zeus":            "Call of Duty: Black Ops Cold War",
	"fore":            "Call of Duty: Vanguard",
	"lazr":            "Call of Duty: Modern Warfare 2 Campaign Remastered",
	"nina":            "Call of Duty: Modern Warfare II",
	"pinta":           "Call of Duty: Modern Warfare III",
	"cerberus-b-live": "Call of Duty: Black Ops 6",
	"wallaby":         "Crash Bandicoot 4",
	"aqua":            "Avowed",
	"scor":            "Sea of Thieves",
	"rtro":            "Blizzard Arcade Collection",
	"drtl":            "Diablo",
	"bnt001":          "Battle.net App",
	"bnt002":          "Battle.net App",
	"catalogs":        "Battle.net catalogs",
	"configs":         "Battle.net configs (shared)",
}

var riotProducts = map[string]string{
	"lol":           "League of Legends",
	"valorant":      "VALORANT",
	"ks-foundation": "Riot Client",
}

var wargamingProducts = map[string]string{
	"wot":  "World of Tanks",
	"wows": "World of Warships",
	"wowp": "World of Warplanes",
}

// serviceNames are display names of the cache-domains services.
var serviceNames = map[string]string{
	"arenanet":     "ArenaNet",
	"blizzard":     "Blizzard (Battle.net)",
	"bsg":          "Battlestate Games",
	"cityofheroes": "City of Heroes",
	"cod":          "Call of Duty",
	"daybreak":     "Daybreak Games",
	"epicgames":    "Epic Games",
	"frontier":     "Frontier",
	"neverwinter":  "Neverwinter",
	"nexusmods":    "Nexus Mods",
	"nintendo":     "Nintendo",
	"origin":       "EA (Origin)",
	"pathofexile":  "Path of Exile",
	"renegadex":    "Renegade X",
	"riot":         "Riot Games",
	"rockstar":     "Rockstar Games",
	"sony":         "PlayStation",
	"square":       "Square Enix",
	"steam":        "Steam",
	"teso":         "The Elder Scrolls Online",
	"test":         "Test",
	"uplay":        "Ubisoft Connect",
	"warframe":     "Warframe",
	"wargaming":    "Wargaming",
	"wsus":         "Windows Update",
	"xboxlive":     "Xbox Live",
}

// productLabels are the fixed labels of well-known group keys (searchable).
var productLabels = func() map[string]string {
	m := map[string]string{
		"nintendo:switch": "Nintendo eShop content",
		"win:do":          "Windows Update (Delivery Optimization)",
	}
	for k, v := range blizzardProducts {
		m["blizzard:"+k] = v
	}
	for k, v := range riotProducts {
		m["riot:"+k] = v
	}
	for k, v := range wargamingProducts {
		m["wg:"+k] = v
	}
	return m
}()

// displayName returns the built-in display name of a service ID.
func displayName(id string) string {
	if n, ok := serviceNames[id]; ok {
		return n
	}
	return titleCase(id)
}

// GroupFor derives the content group of a request (pure function, rules in
// ARCHITECTURE 8.4). path is the canonical path without query; anything
// after a '?' is ignored.
func GroupFor(service, host, path string) Group {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	key, version := groupKey(service, host, path)
	if key == "" || len(key) > maxGroupKeyLen {
		key, version = service+":"+host, ""
	}
	return Group{Key: key, Label: defaultLabel(key, displayName), Version: version}
}

// groupKey applies the service's rules; "" means no rule matched.
func groupKey(service, host, path string) (key, version string) {
	switch service {
	case "steam":
		if m := steamDepotRE.FindStringSubmatch(path); m != nil {
			return "steam:depot:" + m[1], m[2]
		}
	case "blizzard":
		if m := blizzardRE.FindStringSubmatch(path); m != nil {
			return "blizzard:" + strings.ToLower(m[2]), ""
		}
	case "epicgames":
		if m := epicOrgRE.FindStringSubmatch(path); m != nil {
			return "epic:" + m[1] + "/" + m[2], ""
		}
		if m := epicAppRE.FindStringSubmatch(path); m != nil {
			return "epic:" + m[1], ""
		}
	case "riot":
		if m := riotHostRE.FindStringSubmatch(host); m != nil {
			if v := riotManifestRE.FindStringSubmatch(path); v != nil {
				return "riot:" + m[1], v[1]
			}
			return "riot:" + m[1], ""
		}
	case "xboxlive", "wsus":
		return microsoftGroup(path)
	case "sony":
		if m := sonyTitleRE.FindStringSubmatch(path); m != nil {
			if v := sonyVersionRE.FindStringSubmatch(lastSegment(path)); v != nil {
				return "psn:" + m[1], v[1]
			}
			return "psn:" + m[1], ""
		}
	case "nintendo":
		if nintendoRE.MatchString(path) {
			return "nintendo:switch", ""
		}
	case "uplay":
		if m := uplayRE.FindStringSubmatch(path); m != nil {
			return "ubi:" + m[1], ""
		}
	case "origin":
		if m := originRE.FindStringSubmatch(path); m != nil {
			return "ea:" + m[1], ""
		}
	case "wargaming":
		if m := wargamingRE.FindStringSubmatch(host); m != nil {
			return "wg:" + m[1], ""
		}
	}
	return "", ""
}

// microsoftGroup groups Xbox/Store packages, KB updates, Delivery
// Optimization and Office CDN content.
func microsoftGroup(path string) (key, version string) {
	if m := msPackageRE.FindStringSubmatch(lastSegment(path)); m != nil {
		return "xbox:" + m[1], m[2]
	}
	if m := msKBRE.FindStringSubmatch(path); m != nil {
		if d := msKBDateRE.FindStringSubmatch(path); d != nil {
			return "win:kb" + m[1], d[1] + "-" + d[2]
		}
		return "win:kb" + m[1], ""
	}
	if msDORE.MatchString(path) {
		return "win:do", ""
	}
	if m := msOfficeRE.FindStringSubmatch(path); m != nil {
		return "win:office:" + m[1], m[2]
	}
	return "", ""
}

func lastSegment(path string) string { return path[strings.LastIndexByte(path, '/')+1:] }

// defaultLabel derives the display label of a group key without user
// overrides. name resolves service IDs for fallback keys.
func defaultLabel(key string, name func(serviceID string) string) string {
	ns, rest, ok := strings.Cut(key, ":")
	if !ok {
		return key
	}
	if l, ok := productLabels[key]; ok {
		return l
	}
	hostLike := strings.Contains(rest, ".")
	switch ns {
	case "steam":
		if depot, ok := strings.CutPrefix(rest, "depot:"); ok {
			return "Steam depot " + depot
		}
	case "blizzard", "riot":
		if !hostLike && rest != "" {
			return titleCase(rest)
		}
	case "epic":
		if org, build, ok := strings.Cut(rest, "/"); ok && strings.HasPrefix(org, "o-") && len(build) == 32 {
			return "Epic item " + build[:8]
		}
		return humanise(rest)
	case "xbox":
		return humanise(packageName(rest))
	case "win":
		switch {
		case strings.HasPrefix(rest, "kb"):
			return "KB" + rest[2:]
		case strings.HasPrefix(rest, "office:"):
			return "Microsoft 365 Apps (" + clip(rest[len("office:"):], 8) + ")"
		}
	case "psn":
		return rest
	case "ubi":
		return "Ubisoft " + humanise(rest)
	case "ea":
		return "EA " + humanise(rest)
	}
	return name(ns) + " · " + rest
}

// packageName returns the product part of a package identity
// ("Publisher.Product" → "Product").
func packageName(pkg string) string {
	if _, p, ok := strings.Cut(pkg, "."); ok && p != "" {
		return p
	}
	return pkg
}

// titleCase upper-cases the first letter.
func titleCase(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// humanise turns identifiers like "DeadbyDaylightWindows", "MSPhoenix" or
// "some_game-name" into words: separators become spaces, camel case is
// split and every word starts with a capital letter.
func humanise(s string) string {
	var words []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			words = append(words, titleCase(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '-' || r == '_' || r == '+' || r == '.' || r == ' ' || r == '/':
			flush()
			continue
		case unicode.IsUpper(r) && len(cur) > 0:
			prev := cur[len(cur)-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	if len(words) == 0 {
		return s
	}
	return strings.Join(words, " ")
}

// IsBypassPath reports whether a request path must be passed through
// uncached (ARCHITECTURE 8.2 step 6).
func IsBypassPath(path string) bool {
	return path == "/server-status" ||
		strings.HasPrefix(path, "/latest64") ||
		riotReleaseListingRE.MatchString(path) ||
		certTrustCabRE.MatchString(path)
}

// isSteamPath reports whether path is Steam-shaped for the User-Agent rule:
// ^/depot/[0-9]+/ or exactly /server-status.
func isSteamPath(path string) bool {
	if path == "/server-status" {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/depot/")
	if !ok {
		return false
	}
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	return i > 0 && i < len(rest) && rest[i] == '/'
}
