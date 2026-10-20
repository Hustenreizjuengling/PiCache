package services

import (
	"maps"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxGroupKeyLen bounds group keys; longer derived keys fall back to
// "<service>:<host>".
const maxGroupKeyLen = 200

// GroupRules is the version of the rules in groupKey. Raise it with every
// change of a key they derive: the cache store then moves the objects cached
// before into their new groups once (cachestore.Options.GroupRules).
const GroupRules = 1

// Content grouping rules (ARCHITECTURE 8.4). The regexes were verified
// against real CDN URLs (research: verify-observability.md §7; 1.3.0: the
// other cache-domains services, from access logs, launcher sources and the
// CDNs themselves).
var (
	steamDepotRE   = regexp.MustCompile(`^/depot/(\d+)/(?:chunk/[0-9a-fA-F]{40}|manifest/(\d+)/\d+(?:/\d+)?|[^/]+/)`)
	blizzardRE     = regexp.MustCompile(`^/(tpr|cortez)/([^/]+)(?:/\d+)?/(config|data|patch)/`)
	epicOrgRE      = regexp.MustCompile(`^/Builds/Org/(o-[a-z0-9]+)/([0-9a-f]{32})/`)
	epicAppRE      = regexp.MustCompile(`^/Builds/([^/]+)/(?:[^/]+/)*CloudDir/`)
	riotHostRE     = regexp.MustCompile(`^([a-z0-9-]+)\.(?:secure\.)?dyn\.riotcdn\.net$`)
	riotManifestRE = regexp.MustCompile(`^/channels/public/releases/([0-9A-F]{16})\.manifest$`)
	msPackageRE    = regexp.MustCompile(`^([A-Za-z0-9.\-]+)_(\d+(?:\.\d+){3})_(x64|x86|arm64|neutral)__([a-z0-9]{13})\.(msixvc|xvc|appx|appxbundle|msix|msixbundle|eappx|eappxbundle)$`)
	msKBRE         = regexp.MustCompile(`(?i)(?:^|[-_/])kb(\d{6,8})(?:[-_.]|$)`)
	msKBDateRE     = regexp.MustCompile(`/(?:secu|updt|crup|ftpk|uprl)/(\d{4})/(\d{2})/`)
	// msKBFileRE: the product, KB, architecture and .NET part of an update
	// file ("windows11.0-kb5066613-x64-ndp481_<sha1>.msu").
	msKBFileRE     = regexp.MustCompile(`(?i)^(windows\d+(?:\.\d+)?(?:-rt)?)-kb(\d{6,8})-(x86|x64|arm64)(?:-(ndp\d+))?[_.]`)
	msNDPFileRE    = regexp.MustCompile(`(?i)^(ndp\d+)-kb(\d{6,8})-`)
	msMSRTRE       = regexp.MustCompile(`(?i)^windows-kb890830-(?:(?:x86|x64|arm64)-)?v(\d+(?:\.\d+)+)`)
	msDORE         = regexp.MustCompile(`^/filestreamingservice/files/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	msOfficeRE     = regexp.MustCompile(`^/pr/([0-9a-f-]{36})/Office/Data/(\d+\.\d+\.\d+\.\d+)/`)
	sonyTitleRE    = regexp.MustCompile(`/((?:CUSA|PPSA)\d{5})_00/`)
	sonyVersionRE  = regexp.MustCompile(`-V(\d{4})-`)
	nintendoRE     = regexp.MustCompile(`^/c/[csa]/[0-9a-f]{32}`)
	nintendoCCSRE  = regexp.MustCompile(`(?i)^/ccs/download/([0-9a-f]{16})/`)
	uplayRE        = regexp.MustCompile(`^/uplaypc/downloads/([^/]+)/`)
	originShiftRE  = regexp.MustCompile(`^/eamaster/s/shift/([^/]+)/([^/]+)/[^/]+/[^/]`) // franchise/title/build/file
	originRE       = regexp.MustCompile(`^/eamaster/s/shift/([^/]+)/`)
	eaAppRE        = regexp.MustCompile(`^/(?:EA-Desktop-Client-Download|Origin-Client-Download)/`)
	wargamingRE    = regexp.MustCompile(`^dl-(wot|wows|wowp)-`)
	wargamingVerRE = regexp.MustCompile(`^/[a-z]+/patches/(?:wot|wows|wowp)_(\d+(?:\.\d+)+)_`)
	rockstarRE     = regexp.MustCompile(`^/prod/([a-z0-9_]+)/`)
	// ffxivRE: Final Fantasy XIV patch files; the 8 hex digits are a fixed
	// repository ID, D/H incremental or full patches, trailing letters part
	// numbers of a full patch.
	ffxivRE       = regexp.MustCompile(`^/(boot|game)/(?:(ex[1-9])/)?[0-9a-f]{8}/[DH](\d{4}\.\d{2}\.\d{2}\.\d{4}\.\d{4})[a-z]{0,2}\.patch$`)
	daybreakRE    = regexp.MustCompile(`^/patch/sha/([a-z0-9]+)/`)
	arenanetRE    = regexp.MustCompile(`^/program/(\d+)/`)
	cohRE         = regexp.MustCompile(`^/(?:hc|coh)/`)
	cohVersionRE  = regexp.MustCompile(`^/hc/assets/([a-z]+-\d+)/`)
	renegadexRE   = regexp.MustCompile(`^/(Patch|BlackDawn)(\d[0-9A-Za-z.]*)/`)
	nexusRE       = regexp.MustCompile(`^/(\d{1,6})/`)
	poeRE         = regexp.MustCompile(`^/(\d+(?:\.\d+){2,5})/`)
	bsgGameRE     = regexp.MustCompile(`^/(?:(eft|arena)/)?client/[a-z]+/(?:distribs|updates)/(?:\d+(?:\.\d+)+-)?(\d+(?:\.\d+)+)_[0-9a-fA-F-]{36}/`)
	bsgLauncherRE = regexp.MustCompile(`^/LauncherDistribs/(\d+(?:\.\d+)+)_`)
	codRE         = regexp.MustCompile(`^/([a-z0-9]+)/([a-z0-9]+_\d+)/[0-9a-f]+/[0-9a-f]{2}/[0-9a-f]{16}_[0-9a-f]{8}_\d+$`)
	frontierRE    = regexp.MustCompile(`^/([a-z0-9]+)/(?:win|mac)/`)
	frontierVerRE = regexp.MustCompile(`(?:\(|%28)(\d{4}\.\d{2}\.\d{2}\.\d+)(?:\)|%29)\.xml(?:\.gz)?$`)
	tesoRE        = regexp.MustCompile(`^/products/([a-z0-9_]+)/`)

	// Special pass-through paths (ARCHITECTURE 8.2 step 6).
	riotReleaseListingRE = regexp.MustCompile(`^.+(releaselisting_.*|.version$)`)
	certTrustCabRE       = regexp.MustCompile(`(?i)(authrootstl|pinrulesstl|disallowedcertstl)\.cab$`)
	// ArenaNet's build pointers (/latest64/<product> is matched as a prefix
	// for compatibility) and Warframe's file index: they change in place.
	arenanetLatestRE = regexp.MustCompile(`^/latest(?:32|mac64)?/\d+$`)
	warframeIndexRE  = regexp.MustCompile(`(?i)^/(?:origin/[0-9a-f]{8}/)?index\.txt\.lzma$`)
)

// blizzardProducts maps TACT CDN paths (lower-case) to product names.
var blizzardProducts = map[string]string{
	"wow":             "World of Warcraft",
	"ovw":             "Overwatch",
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
	"auks":            "Call of Duty HQ",
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
	"aris":            "DOOM: The Dark Ages",
	"ark":             "The Outer Worlds 2",
	"lbra":            "Tony Hawk's Pro Skater 3 + 4",
	"war1":            "Warcraft: Orcs & Humans",
	"bnt001":          "Battle.net Agent",
	"bnt002":          "Battle.net App",
	"bnt004":          "Battle.net Bootstrapper",
	"catalogs":        "Battle.net catalogs",
	"configs":         "Battle.net configs (shared)",
}

// steamRedistApp is Steamworks Common Redistributables: Steam installs its
// depots (DirectX, .NET, Visual C++, …) along with the games that need them.
const steamRedistApp = 228980

// steamRedists names its depots after the install scripts Valve ships with
// the app (_CommonRedist\<component>\<version>\installscript.vdf).
var steamRedists = map[uint32]string{
	228981: "Visual C++ 2005",
	228982: "Visual C++ 2008",
	228983: "Visual C++ 2010",
	228984: "Visual C++ 2012",
	228985: "Visual C++ 2013",
	228986: "Visual C++ 2015",
	228987: "Visual C++ 2017",
	228988: "Visual C++ 2019",
	228989: "Visual C++ 2022",
	228990: "DirectX (June 2010)",
	229000: ".NET Framework 3.5",
	229001: ".NET Framework 3.5 Client Profile",
	229002: ".NET Framework 4.0",
	229003: ".NET Framework 4.0 Client Profile",
	229004: ".NET Framework 4.5.2",
	229005: ".NET Framework 4.6",
	229006: ".NET Framework 4.7",
	229007: ".NET Framework 4.8",
	229010: "XNA Framework 3.0",
	229011: "XNA Framework 3.1",
	229012: "XNA Framework 4.0",
	229020: "OpenAL 2.0.7.0",
	229030: "PhysX 8.09.04",
	229031: "PhysX 9.12.1031",
	229032: "PhysX 9.13.1220",
	229033: "PhysX 9.14.0702",
}

// officeChannels names the update channels of Microsoft 365 Apps by the
// GUID in their Office CDN path (officecdn.microsoft.com/pr/<guid>/, the
// CDNBaseUrl of an installation).
var officeChannels = map[string]string{
	"492350f6-3a01-4f97-b9c0-c7c6ddf67d60": "Current Channel",
	"64256afe-f5d9-4f86-8936-8840a6a4f5be": "Current Channel (Preview)",
	"55336b82-a18d-4dd6-b5f6-9e5095c314a6": "Monthly Enterprise Channel",
	"7ffbc6bf-bc32-4f92-8982-f9dd17fd3114": "Semi-Annual Enterprise Channel",
	"b8f9b850-328d-4355-9145-c59439a0c4cf": "Semi-Annual Enterprise Channel (Preview)",
}

var riotProducts = map[string]string{
	"lol":              "League of Legends",
	"valorant":         "VALORANT",
	"ks-foundation":    "Riot Client",
	"riot-client":      "Riot Client (content)",
	"bacon":            "Legends of Runeterra",
	"lion":             "2XKO",
	"teamfighttactics": "Teamfight Tactics",
	"wildrift":         "League of Legends: Wild Rift",
	"ritoplus":         "Riot Mobile",
}

var wargamingProducts = map[string]string{
	"wot":  "World of Tanks",
	"wows": "World of Warships",
	"wowp": "World of Warplanes",
}

// Epic app names of CloudDir builds that do not read well humanised.
var epicApps = map[string]string{
	"Fortnite":             "Fortnite",
	"UnrealEngineLauncher": "Epic Games Launcher",
	"Rocket":               "Unreal Engine",
}

// ubisoftProducts names Ubisoft Connect product IDs (/uplaypc/downloads/<id>/)
// of well-known games, after the product configurations Ubisoft served up
// to November 2022 (later games keep "Ubisoft product <id>").
var ubisoftProducts = func() map[string]string {
	m := map[string]string{}
	for name, ids := range map[string][]int{
		"Assassin's Creed":                        {82},
		"Assassin's Creed II":                     {4, 56, 963, 964, 4919, 5481},
		"Assassin's Creed Brotherhood":            {26, 428, 965, 966},
		"Assassin's Creed Revelations":            {40, 59, 967, 5063},
		"Assassin's Creed III":                    {54, 102, 103, 105, 139, 600},
		"Assassin's Creed III Remastered":         {5183, 5184},
		"Assassin's Creed IV Black Flag":          {437, 617, 618},
		"Assassin's Creed Freedom Cry":            {664, 665},
		"Assassin's Creed Liberation HD":          {625, 632},
		"Assassin's Creed Unity":                  {720, 857, 944, 945},
		"Assassin's Creed Rogue":                  {895, 934, 1186, 1187, 1661, 1662},
		"Assassin's Creed Syndicate":              {1875, 1955, 1956, 1957, 1958, 5642},
		"Assassin's Creed Origins":                {3539, 4923},
		"Assassin's Creed Odyssey":                {7, 5059, 5092},
		"Assassin's Creed Valhalla":               {13504},
		"Far Cry":                                 {84},
		"Far Cry 2":                               {85},
		"Far Cry 3":                               {46, 101, 300, 599},
		"Far Cry 3 Blood Dragon":                  {205, 316, 5488},
		"Far Cry 4":                               {420, 856, 969, 970, 971, 972},
		"Far Cry Primal":                          {2010, 2029},
		"Far Cry 5":                               {1803, 4311},
		"Far Cry New Dawn":                        {5210, 5211},
		"Far Cry 6":                               {5266},
		"Tom Clancy's Rainbow Six Vegas":          {88},
		"Tom Clancy's Rainbow Six Vegas 2":        {108},
		"Tom Clancy's Rainbow Six Siege":          {635, 1842, 11799, 60695},
		"Tom Clancy's Rainbow Six Extraction":     {5271},
		"Tom Clancy's The Division":               {568, 1835, 2076, 2077, 2700},
		"Tom Clancy's The Division 2":             {4932},
		"Tom Clancy's Ghost Recon Future Soldier": {53, 65, 674},
		"Tom Clancy's Ghost Recon Wildlands":      {1771, 3559},
		"Tom Clancy's Ghost Recon Breakpoint":     {5452, 11903, 14924},
		"Watch Dogs":                              {274, 541, 543, 544, 545, 737, 738, 739, 740, 741, 828, 896, 1428, 1880},
		"Watch Dogs 2":                            {2688, 3619},
		"Watch Dogs: Legion":                      {3353},
		"The Crew":                                {413, 414, 507, 508},
		"The Crew 2":                              {2855, 5037},
		"For Honor":                               {569},
		"Steep":                                   {3279},
		"Riders Republic":                         {5487, 5733},
		"Immortals Fenyx Rising":                  {5405},
		"Anno 2205":                               {1253, 2052, 2080, 2081, 3350},
		"Anno 1800":                               {4553},
		"Rayman Legends":                          {410, 418, 509},
		"South Park: The Stick of Truth":          {670, 3584},
		"South Park: The Fractured but Whole":     {3088, 4502},
		"Prince of Persia: The Forgotten Sands":   {11, 63, 673},
		"Prince of Persia: The Sands of Time":     {111},
		"Prince of Persia: Warrior Within":        {121},
		"Prince of Persia: The Two Thrones":       {113},
		"Trackmania":                              {5595},
		"Trackmania Turbo":                        {2024, 2070},
		"TrackMania² Canyon":                      {127},
		"TrackMania² Stadium":                     {275},
		"TrackMania² Valley":                      {429},
		"The Settlers 7: Paths to a Kingdom":      {8, 61, 5062},
	} {
		for _, id := range ids {
			m[strconv.Itoa(id)] = name
		}
	}
	return m
}()

var rockstarTitles = map[string]string{
	"gtav":       "Grand Theft Auto V",
	"gta5":       "Grand Theft Auto V",
	"gta4":       "Grand Theft Auto IV",
	"gta3":       "Grand Theft Auto III",
	"gtavc":      "Grand Theft Auto: Vice City",
	"gtasa":      "Grand Theft Auto: San Andreas",
	"rdr2":       "Red Dead Redemption 2",
	"lanoire":    "L.A. Noire",
	"mp3":        "Max Payne 3",
	"bully":      "Bully: Scholarship Edition",
	"socialclub": "Rockstar Games Social Club",
	"launcher":   "Rockstar Games Launcher",
}

// ffxivRepos names Final Fantasy XIV's patch repositories (boot, the base
// game and an expansion each).
var ffxivRepos = map[string]string{
	"boot": "Launcher",
	"game": "Base game",
	"ex1":  "Heavensward",
	"ex2":  "Stormblood",
	"ex3":  "Shadowbringers",
	"ex4":  "Endwalker",
	"ex5":  "Dawntrail",
}

var daybreakGames = map[string]string{
	"planetside2": "PlanetSide 2",
	"eq":          "EverQuest",
	"eq2":         "EverQuest II",
}

// nexusGames names the Nexus Mods game IDs with the most downloads.
var nexusGames = map[string]string{
	"100":  "Morrowind",
	"101":  "Oblivion",
	"110":  "Skyrim",
	"120":  "Fallout 3",
	"130":  "Fallout New Vegas",
	"140":  "Dragon Age: Origins",
	"952":  "The Witcher 3: Wild Hunt",
	"1151": "Fallout 4",
	"1303": "Stardew Valley",
	"1704": "Skyrim Special Edition",
	"2531": "Monster Hunter: World",
	"3024": "Red Dead Redemption 2",
	"3174": "Mount & Blade II: Bannerlord",
	"3333": "Cyberpunk 2077",
	"3474": "Baldur's Gate 3",
	"4187": "Starfield",
	"4333": "Elden Ring",
	"6063": "Palworld",
}

// otherProducts are the fixed labels of single games and parts of the
// smaller services.
var otherProducts = map[string]string{
	"arenanet:101":            "Guild Wars 2",
	"bsg:eft":                 "Escape from Tarkov",
	"bsg:arena":               "Escape from Tarkov: Arena",
	"bsg:launcher":            "Battlestate Games Launcher",
	"cityofheroes:homecoming": "City of Heroes: Homecoming",
	"ea:app":                  "EA app",
	"ea:swtor":                "Star Wars: The Old Republic",
	"frontier:elitedangerous": "Elite Dangerous",
	"pathofexile:poe":         "Path of Exile",
	"renegadex:renegadex":     "Renegade X",
	"renegadex:blackdawn":     "Renegade X: Black Dawn",
	"teso:eso":                "The Elder Scrolls Online",
	"warframe:game":           "Warframe",
	"warframe:publicexport":   "Warframe · Public Export",
	"win:kb890830":            "Windows Malicious Software Removal Tool",
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
	for depot, v := range steamRedists {
		m["steam:depot:"+strconv.FormatUint(uint64(depot), 10)] = v + " · Steamworks Common Redistributables"
	}
	for guid, v := range officeChannels {
		m["win:office:"+guid] = "Microsoft 365 Apps · " + v
	}
	for k, v := range epicApps {
		m["epic:"+k] = v
	}
	for k, v := range ubisoftProducts {
		m["ubi:"+k] = v
	}
	for k, v := range rockstarTitles {
		m["rockstar:"+k] = v
	}
	for k, v := range ffxivRepos {
		m["ffxiv:"+k] = "Final Fantasy XIV · " + v
	}
	for k, v := range daybreakGames {
		m["daybreak:"+k] = v
	}
	for k, v := range nexusGames {
		m["nexus:"+k] = v + " · Nexus Mods"
	}
	maps.Copy(m, otherProducts)
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
		if m := nintendoCCSRE.FindStringSubmatch(path); m != nil {
			return "nintendo:" + strings.ToLower(m[1]), ""
		}
	case "uplay":
		if m := uplayRE.FindStringSubmatch(path); m != nil {
			return "ubi:" + m[1], ""
		}
	case "origin":
		switch {
		case host == "cdn-patch.swtor.com":
			return "ea:swtor", ""
		case eaAppRE.MatchString(path):
			return "ea:app", ""
		}
		if m := originShiftRE.FindStringSubmatch(path); m != nil {
			return "ea:" + m[1] + "/" + m[2], ""
		}
		if m := originRE.FindStringSubmatch(path); m != nil {
			return "ea:" + m[1], ""
		}
	case "wargaming":
		if m := wargamingRE.FindStringSubmatch(host); m != nil {
			if v := wargamingVerRE.FindStringSubmatch(path); v != nil {
				return "wg:" + m[1], v[1]
			}
			return "wg:" + m[1], ""
		}
	case "rockstar":
		if m := rockstarRE.FindStringSubmatch(path); m != nil {
			return "rockstar:" + m[1], ""
		}
	case "square":
		if m := ffxivRE.FindStringSubmatch(path); m != nil {
			if m[2] != "" {
				return "ffxiv:" + m[2], m[3]
			}
			return "ffxiv:" + m[1], m[3]
		}
	case "daybreak":
		if m := daybreakRE.FindStringSubmatch(path); m != nil {
			return "daybreak:" + m[1], ""
		}
	case "arenanet":
		if m := arenanetRE.FindStringSubmatch(path); m != nil {
			return "arenanet:" + m[1], ""
		}
	case "cityofheroes":
		if cohRE.MatchString(path) {
			if v := cohVersionRE.FindStringSubmatch(path); v != nil {
				return "cityofheroes:homecoming", v[1]
			}
			return "cityofheroes:homecoming", ""
		}
	case "renegadex":
		if m := renegadexRE.FindStringSubmatch(path); m != nil {
			if m[1] == "BlackDawn" {
				return "renegadex:blackdawn", m[2]
			}
			return "renegadex:renegadex", m[2]
		}
	case "nexusmods":
		if m := nexusRE.FindStringSubmatch(path); m != nil {
			return "nexus:" + m[1], ""
		}
	case "pathofexile":
		if m := poeRE.FindStringSubmatch(path); m != nil {
			return "pathofexile:poe", m[1]
		}
	case "bsg":
		if m := bsgGameRE.FindStringSubmatch(path); m != nil {
			if m[1] == "arena" {
				return "bsg:arena", m[2]
			}
			return "bsg:eft", m[2]
		}
		if m := bsgLauncherRE.FindStringSubmatch(path); m != nil {
			return "bsg:launcher", m[1]
		}
	case "cod":
		if m := codRE.FindStringSubmatch(path); m != nil {
			return "cod:" + m[1], m[2]
		}
	case "frontier":
		if m := frontierRE.FindStringSubmatch(path); m != nil {
			if v := frontierVerRE.FindStringSubmatch(path); v != nil {
				return "frontier:" + m[1], v[1]
			}
			return "frontier:" + m[1], ""
		}
	case "teso":
		if m := tesoRE.FindStringSubmatch(path); m != nil {
			return "teso:" + m[1], ""
		}
	case "warframe":
		if strings.HasPrefix(path, "/PublicExport/") {
			return "warframe:publicexport", ""
		}
		return "warframe:game", ""
	}
	return "", ""
}

// microsoftGroup groups Xbox/Store packages, KB updates, Delivery
// Optimization and Office CDN content. An update file names its product,
// architecture and .NET part ("win:kb5066613:windows11.0-x64-ndp481").
func microsoftGroup(path string) (key, version string) {
	file := lastSegment(path)
	if m := msPackageRE.FindStringSubmatch(file); m != nil {
		return "xbox:" + m[1], m[2]
	}
	if m := msMSRTRE.FindStringSubmatch(file); m != nil {
		return "win:kb890830", m[1]
	}
	if m := msKBRE.FindStringSubmatch(path); m != nil {
		key = "win:kb" + m[1]
		if f := msKBFileRE.FindStringSubmatch(file); f != nil && f[2] == m[1] {
			key += ":" + strings.ToLower(f[1]+"-"+f[3])
			if f[4] != "" {
				key += "-" + strings.ToLower(f[4])
			}
		} else if f := msNDPFileRE.FindStringSubmatch(file); f != nil && f[2] == m[1] {
			key += ":" + strings.ToLower(f[1])
		}
		if d := msKBDateRE.FindStringSubmatch(path); d != nil {
			return key, d[1] + "-" + d[2]
		}
		return key, ""
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
			return kbLabel(rest)
		case strings.HasPrefix(rest, "office:"):
			return "Microsoft 365 Apps (" + clip(rest[len("office:"):], 8) + ")"
		}
	case "psn":
		return rest
	case "nintendo":
		if l := nintendoTitleLabel(rest); l != "" {
			return l
		}
	case "ubi":
		if isDigits(rest) {
			return "Ubisoft product " + rest
		}
		return "Ubisoft " + humanise(rest)
	case "ea":
		if _, title, ok := strings.Cut(rest, "/"); ok {
			return humanise(title)
		}
		return "EA " + humanise(rest)
	case "ffxiv":
		if n, ok := strings.CutPrefix(rest, "ex"); ok && isDigits(n) {
			return "Final Fantasy XIV · Expansion " + n
		}
	case "nexus":
		if isDigits(rest) {
			return "Nexus Mods · game " + rest
		}
	case "cod":
		if !hostLike {
			p := strings.ToUpper(rest)
			if rest == "android" {
				p = "Android"
			}
			return "Call of Duty · texture streaming (" + p + ")"
		}
	case "arenanet":
		if isDigits(rest) {
			return "ArenaNet product " + rest
		}
	}
	return name(ns) + " · " + rest
}

// kbLabel labels a Windows update ("kb5066613:windows11.0-x64-ndp481" →
// "KB5066613 · .NET Framework 4.8.1 · Windows 11 (x64)").
func kbLabel(rest string) string {
	kb, file, _ := strings.Cut(rest, ":")
	l := "KB" + kb[2:]
	if file == "" {
		return l
	}
	// <product>[-rt]-<arch>[-ndp<v>], or ndp<v> for a standalone installer
	parts := strings.Split(file, "-")
	var ndp, arch string
	if n := len(parts); n > 1 && strings.HasPrefix(parts[n-1], "ndp") {
		ndp, parts = parts[n-1], parts[:n-1]
	}
	if n := len(parts); n > 1 {
		arch, parts = parts[n-1], parts[:n-1]
	}
	product := strings.Join(parts, "-")
	if v, ok := strings.CutPrefix(product, "ndp"); ok {
		return l + " · " + dotNetName(v)
	}
	if ndp != "" {
		l += " · " + dotNetName(ndp[len("ndp"):])
	}
	w, ok := windowsVersions[product]
	if !ok {
		w = titleCase(product)
	}
	l += " · " + w
	if arch != "" {
		l += " (" + arch + ")"
	}
	return l
}

// windowsVersions names the product part of update file names ("windows10.0"
// also covers Windows 11 21H2 and Windows Server 2016–2022, "windows11.0"
// Windows Server 2025).
var windowsVersions = map[string]string{
	"windows6.0":  "Windows Vista",
	"windows6.1":  "Windows 7",
	"windows8-rt": "Windows 8",
	"windows8.1":  "Windows 8.1",
	"windows10.0": "Windows 10",
	"windows11.0": "Windows 11",
}

// dotNetName names a .NET Framework version from an ndp suffix ("481" →
// ".NET Framework 4.8.1").
func dotNetName(v string) string {
	if !isDigits(v) || len(v) < 2 {
		return ".NET Framework"
	}
	return ".NET Framework " + strings.Join(strings.Split(v, ""), ".")
}

// nintendoTitleLabel labels a Wii U or 3DS title ID by the kind its high
// word stands for ("0005000e1010ec00" → "Wii U update 1010EC00").
func nintendoTitleLabel(tid string) string {
	if len(tid) != 16 || !isHex(tid) {
		return ""
	}
	var platform string
	switch tid[:4] {
	case "0005":
		platform = "Wii U"
	case "0004":
		platform = "3DS"
	default:
		return ""
	}
	kind := "title"
	switch tid[4:8] {
	case "0000":
		kind = "game"
	case "000e":
		kind = "update"
	case "000c", "008c":
		kind = "DLC"
	case "0002":
		kind = "demo"
	case "0010", "001b", "0030", "0130", "0138":
		kind = "system title"
	}
	return platform + " " + kind + " " + strings.ToUpper(tid[8:])
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return s != ""
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
		arenanetLatestRE.MatchString(path) ||
		warframeIndexRE.MatchString(path) ||
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
