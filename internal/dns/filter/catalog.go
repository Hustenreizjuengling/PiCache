package filter

import "slices"

// List categories (API enum and UI order). User lists may also have
// CategoryOther; catalogue entries never do.
const (
	CategoryGeneral       = "general"
	CategorySecurity      = "security" // phishing, malware, threat intelligence, crypto-mining
	CategoryPrivacy       = "privacy"  // trackers, telemetry of TVs and vendors
	CategoryAdult         = "adult"
	CategoryGambling      = "gambling"
	CategoryDating        = "dating"
	CategoryPiracy        = "piracy"
	CategorySocial        = "social"
	CategoryBypass        = "doh-vpn-bypass"
	CategoryAbusedTLDs    = "abused-tlds"
	CategoryURLShorteners = "url-shorteners"
	CategoryStalkerware   = "stalkerware"
	CategoryRegional      = "regional" // ad lists of a language or country
	CategoryAllow         = "allow"    // small, curated allowlists that fix common false positives
	CategoryOther         = "other"    // user lists only
)

// Categories are the catalogue categories in API and UI order.
var Categories = []string{
	CategoryGeneral, CategorySecurity, CategoryPrivacy, CategoryAdult, CategoryGambling, CategoryDating,
	CategoryPiracy, CategorySocial, CategoryBypass, CategoryAbusedTLDs, CategoryURLShorteners,
	CategoryStalkerware, CategoryRegional, CategoryAllow,
}

// protectionCategories are the categories whose enabled lists are
// protection lists: enforced like parental controls (7.1 step 7a), also
// while blocking is paused or disabled.
var protectionCategories = []string{CategoryAdult, CategoryGambling, CategoryDating, CategoryPiracy, CategoryBypass}

// IsProtection reports whether category is a protection category.
func IsProtection(category string) bool { return slices.Contains(protectionCategories, category) }

// validCategory reports whether c may be stored for a list.
func validCategory(c string) bool { return c == CategoryOther || slices.Contains(Categories, c) }

// Memory of a list entry and the entry budget of a small host.
const (
	// EntryBytes approximates the memory of one entry: at most 16 bytes
	// in the matcher and about 8 in the kept parse result (plus the peak
	// while a list is parsed).
	EntryBytes = 24
	// LargeEntries marks a "large" list: never recommended, never bound
	// to a category switch.
	LargeEntries = 1_000_000
	// EntryBudget is the number of compiled entries above which the health
	// check warns that a small host may run short of memory (measured,
	// ARCHITECTURE 7.2).
	EntryBudget = 4_000_000
)

// categoryPreset is a parental category switch (PUT
// /parental/groups/{id} categories): it binds the catalogue lists keys
// (all of the switch's category).
type categoryPreset struct {
	Switch   string
	Category string
	Keys     []string
}

// categoryPresets are the category switches of the parental controls.
var categoryPresets = []categoryPreset{
	{Switch: "adult", Category: CategoryAdult, Keys: []string{"oisd-nsfw"}},
	{Switch: "gambling", Category: CategoryGambling, Keys: []string{"hagezi-gambling-medium"}},
	{Switch: "dating", Category: CategoryDating, Keys: []string{"shadowwhisperer-dating"}},
	{Switch: "piracy", Category: CategoryPiracy, Keys: []string{"hagezi-anti-piracy"}},
	{Switch: "bypass", Category: CategoryBypass, Keys: []string{"hagezi-doh-vpn-bypass"}},
}

// catalogAliases keeps the key of a bound catalogue list that a release
// replaced with another entry recognised (old key → new key), so a switch
// still finds the list the user already has.
var catalogAliases = map[string]string{}

// catalog is the curated list selection. Every URL was verified on
// 2026-09-26 (HTTP 200, parsed by this package, entries > 0, at most 1 %
// invalid and unsupported lines); Entries is the count of that run. The
// first entry is the default list created at first start. PiCache ships
// no list content: lists are downloaded from their maintainers.
var catalog = []CatalogEntry{
	// general
	{
		Key: "hagezi-multi", Name: "HaGeZi Multi NORMAL", Category: CategoryGeneral, Recommended: true,
		Description:   "Ads, tracking, metrics, telemetry, phishing, malware and scam. Balanced, rarely breaks anything (default).",
		DescriptionDe: "Werbung, Tracking, Telemetrie, Phishing, Malware und Betrug. Ausgewogen, macht selten etwas kaputt (Standard).",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/multi.txt",
		Entries:       164218,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-light", Name: "HaGeZi Multi LIGHT", Category: CategoryGeneral,
		Description:   "Basic protection against ads, tracking and malware for devices with little memory; blocks almost nothing by mistake.",
		DescriptionDe: "Grundschutz vor Werbung, Tracking und Malware für Geräte mit wenig Speicher; blockiert fast nie versehentlich.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/light.txt",
		Entries:       44936,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-pro", Name: "HaGeZi Multi PRO", Category: CategoryGeneral,
		Description:   "Stricter than NORMAL: more ads and trackers; may occasionally need an allow rule.",
		DescriptionDe: "Strenger als NORMAL: mehr Werbung und Tracker; braucht gelegentlich eine Erlauben-Regel.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/pro.txt",
		Entries:       228217,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-pro-plus", Name: "HaGeZi Multi PRO++", Category: CategoryGeneral,
		Description:   "Aggressive blocking for experienced users; expect to add allow rules.",
		DescriptionDe: "Aggressives Blockieren für erfahrene Nutzer; rechne mit Erlauben-Regeln.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/pro.plus.txt",
		Entries:       249391,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-ultimate", Name: "HaGeZi Multi ULTIMATE", Category: CategoryGeneral,
		Description:   "Most aggressive HaGeZi list; will break some services.",
		DescriptionDe: "Die aggressivste HaGeZi-Liste; einige Dienste funktionieren damit nicht mehr.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/ultimate.txt",
		Entries:       284364,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "oisd-small", Name: "OISD small", Category: CategoryGeneral,
		Description:   "Compact ads and tracking list with very few false positives.",
		DescriptionDe: "Kompakte Liste gegen Werbung und Tracking mit sehr wenigen Fehlalarmen.",
		URL:           "https://small.oisd.nl/",
		Entries:       56379,
		Maintainer:    "Stephan van Ruth", License: "GPL-3.0", Homepage: "https://oisd.nl",
	},
	{
		Key: "oisd-big", Name: "OISD big", Category: CategoryGeneral,
		Description:   "Comprehensive ads, tracking and malware list with few false positives.",
		DescriptionDe: "Umfassende Liste gegen Werbung, Tracking und Malware mit wenigen Fehlalarmen.",
		URL:           "https://big.oisd.nl/",
		Entries:       243948,
		Maintainer:    "Stephan van Ruth", License: "GPL-3.0", Homepage: "https://oisd.nl",
	},
	{
		Key: "stevenblack", Name: "StevenBlack Unified hosts", Category: CategoryGeneral,
		Description:   "Classic hosts file: adware and malware.",
		DescriptionDe: "Klassische Hosts-Datei: Adware und Malware.",
		URL:           "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
		Entries:       75944,
		Maintainer:    "Steven Black", License: "MIT", Homepage: "https://github.com/StevenBlack/hosts",
	},
	{
		Key: "adguard-dns", Name: "AdGuard DNS filter", Category: CategoryGeneral,
		Description:   "DNS filter list against ads and tracking.",
		DescriptionDe: "DNS-Filterliste gegen Werbung und Tracking.",
		URL:           "https://adguardteam.github.io/HostlistsRegistry/assets/filter_1.txt",
		Entries:       183035,
		Maintainer:    "AdGuard", License: "GPL-3.0", Homepage: "https://github.com/AdguardTeam/AdGuardSDNSFilter",
	},
	{
		Key: "1hosts-lite", Name: "1Hosts Lite", Category: CategoryGeneral,
		Description:   "Ads and tracking, tuned to avoid breakage.",
		DescriptionDe: "Werbung und Tracking, darauf abgestimmt, nichts kaputtzumachen.",
		URL:           "https://badmojr.github.io/1Hosts/Lite/adblock.txt",
		Entries:       102259,
		Maintainer:    "badmojr", License: "MPL-2.0", Homepage: "https://badmojr.github.io/1Hosts/",
	},
	{
		Key: "peter-lowe", Name: "Peter Lowe's Ad and tracking server list", Category: CategoryGeneral,
		Description:   "Long-running, small list of ad and tracking servers.",
		DescriptionDe: "Seit Langem gepflegte, kleine Liste von Werbe- und Tracking-Servern.",
		URL:           "https://pgl.yoyo.org/adservers/serverlist.php?hostformat=adblock&showintro=0&mimetype=plaintext",
		Entries:       7100,
		Maintainer:    "Peter Lowe", License: "", Homepage: "https://pgl.yoyo.org/adservers/",
	},
	{
		Key: "adaway", Name: "AdAway default blocklist", Category: CategoryGeneral,
		Description:   "Mobile ad providers and some analytics providers.",
		DescriptionDe: "Werbeanbieter für Mobilgeräte und einige Analyse-Anbieter.",
		URL:           "https://adaway.org/hosts.txt",
		Entries:       6540,
		Maintainer:    "AdAway", License: "CC-BY-3.0", Homepage: "https://adaway.org",
	},
	// security
	{
		Key: "hagezi-tif", Name: "HaGeZi Threat Intelligence Feeds", Category: CategorySecurity,
		Description:   "Malware, phishing, scam, cryptojacking and command-and-control domains. Large.",
		DescriptionDe: "Domains von Malware, Phishing, Betrug, Cryptojacking und Command-and-Control-Servern. Groß.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/tif.txt",
		Entries:       2291471,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-tif-medium", Name: "HaGeZi Threat Intelligence Feeds (medium)", Category: CategorySecurity, Recommended: true,
		Description:   "The most active threats of the full feed: malware, phishing, scam and cryptojacking, at a fraction of its size.",
		DescriptionDe: "Die aktivsten Bedrohungen des vollen Feeds: Malware, Phishing, Betrug und Cryptojacking, bei einem Bruchteil der Größe.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/tif.medium.txt",
		Entries:       873327,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-tif-mini", Name: "HaGeZi Threat Intelligence Feeds (mini)", Category: CategorySecurity,
		Description:   "Compact threat feed for hosts with little memory.",
		DescriptionDe: "Kompakter Bedrohungs-Feed für Geräte mit wenig Speicher.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/tif.mini.txt",
		Entries:       200366,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-fake", Name: "HaGeZi Fake", Category: CategorySecurity,
		Description:   "Fake shops, fake streaming sites, rip-off offers and other scams.",
		DescriptionDe: "Fake-Shops, falsche Streaming-Seiten, Abzocke und anderer Betrug.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/fake.txt",
		Entries:       17357,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "phishing-army", Name: "Phishing Army", Category: CategorySecurity,
		Description:   "Phishing domains collected from several public feeds.",
		DescriptionDe: "Phishing-Domains aus mehreren öffentlichen Quellen.",
		URL:           "https://phishing.army/download/phishing_army_blocklist.txt",
		Entries:       147841,
		Maintainer:    "Andrea Draghetti", License: "CC-BY-NC-4.0", Homepage: "https://phishing.army",
	},
	{
		Key: "urlhaus", Name: "URLhaus malware hosts", Category: CategorySecurity,
		Description:   "Hosts that currently distribute malware, from the abuse.ch URLhaus project.",
		DescriptionDe: "Hosts, die gerade Malware verbreiten, aus dem URLhaus-Projekt von abuse.ch.",
		URL:           "https://urlhaus.abuse.ch/downloads/hostfile/",
		Entries:       383,
		Maintainer:    "abuse.ch", License: "CC0-1.0", Homepage: "https://urlhaus.abuse.ch",
	},
	{
		Key: "threatfox", Name: "ThreatFox indicators", Category: CategorySecurity,
		Description:   "Domains of malware command-and-control servers and botnets, from abuse.ch ThreatFox.",
		DescriptionDe: "Domains von Malware-Steuerservern und Botnetzen, aus ThreatFox von abuse.ch.",
		URL:           "https://threatfox.abuse.ch/downloads/hostfile/",
		Entries:       44606,
		Maintainer:    "abuse.ch", License: "CC0-1.0", Homepage: "https://threatfox.abuse.ch",
	},
	{
		Key: "nocoin", Name: "NoCoin", Category: CategorySecurity,
		Description:   "Browser-based crypto-mining (cryptojacking) scripts.",
		DescriptionDe: "Krypto-Mining-Skripte im Browser (Cryptojacking).",
		URL:           "https://raw.githubusercontent.com/hoshsadiq/adblock-nocoin-list/master/hosts.txt",
		Entries:       312,
		Maintainer:    "hoshsadiq", License: "MIT", Homepage: "https://github.com/hoshsadiq/adblock-nocoin-list",
	},
	{
		Key: "blp-malware", Name: "Block List Project: Malware", Category: CategorySecurity,
		Description:   "Malware domains collected from many sources. Large.",
		DescriptionDe: "Malware-Domains aus vielen Quellen. Groß.",
		URL:           "https://blocklistproject.github.io/Lists/alt-version/malware-nl.txt",
		Entries:       2655820,
		Maintainer:    "The Block List Project", License: "Unlicense", Homepage: "https://blocklistproject.github.io/Lists/",
	},
	{
		Key: "blp-ransomware", Name: "Block List Project: Ransomware", Category: CategorySecurity,
		Description:   "Domains used by ransomware.",
		DescriptionDe: "Von Erpressungstrojanern genutzte Domains.",
		URL:           "https://blocklistproject.github.io/Lists/alt-version/ransomware-nl.txt",
		Entries:       1904,
		Maintainer:    "The Block List Project", License: "Unlicense", Homepage: "https://blocklistproject.github.io/Lists/",
	},
	{
		Key: "scam-blocklist", Name: "Scam Blocklist", Category: CategorySecurity,
		Description:   "Fraudulent online shops and scam sites.",
		DescriptionDe: "Betrügerische Onlineshops und Abzockseiten.",
		URL:           "https://raw.githubusercontent.com/durablenapkin/scamblocklist/master/hosts.txt",
		Entries:       2202,
		Maintainer:    "DurableNapkin", License: "MIT", Homepage: "https://github.com/durablenapkin/scamblocklist",
	},
	{
		Key: "shadowwhisperer-typo", Name: "ShadowWhisperer Typo", Category: CategorySecurity,
		Description:   "Typo domains of popular sites (typosquatting) that lead to scams or malware.",
		DescriptionDe: "Tippfehler-Domains bekannter Seiten (Typosquatting), die zu Betrug oder Malware führen.",
		URL:           "https://raw.githubusercontent.com/ShadowWhisperer/BlockLists/master/Lists/Typo",
		Entries:       73944,
		Maintainer:    "ShadowWhisperer", License: "Unlicense", Homepage: "https://github.com/ShadowWhisperer/BlockLists",
	},
	// privacy
	{
		Key: "hagezi-native-winoffice", Name: "HaGeZi Native Tracker: Windows and Office", Category: CategoryPrivacy,
		Description:   "Telemetry of Windows and Office.",
		DescriptionDe: "Telemetrie von Windows und Office.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.winoffice.txt",
		Entries:       388,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-apple", Name: "HaGeZi Native Tracker: Apple", Category: CategoryPrivacy,
		Description:   "Telemetry of Apple devices.",
		DescriptionDe: "Telemetrie von Apple-Geräten.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.apple.txt",
		Entries:       109,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-amazon", Name: "HaGeZi Native Tracker: Amazon", Category: CategoryPrivacy,
		Description:   "Telemetry of Amazon devices (Fire TV, Echo, Kindle).",
		DescriptionDe: "Telemetrie von Amazon-Geräten (Fire TV, Echo, Kindle).",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.amazon.txt",
		Entries:       371,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-samsung", Name: "HaGeZi Native Tracker: Samsung", Category: CategoryPrivacy,
		Description:   "Telemetry of Samsung phones and smart TVs.",
		DescriptionDe: "Telemetrie von Samsung-Smartphones und -Smart-TVs.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.samsung.txt",
		Entries:       200,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-lgwebos", Name: "HaGeZi Native Tracker: LG webOS", Category: CategoryPrivacy,
		Description:   "Telemetry of LG smart TVs (webOS).",
		DescriptionDe: "Telemetrie von LG-Smart-TVs (webOS).",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.lgwebos.txt",
		Entries:       229,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-roku", Name: "HaGeZi Native Tracker: Roku", Category: CategoryPrivacy,
		Description:   "Telemetry of Roku devices.",
		DescriptionDe: "Telemetrie von Roku-Geräten.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.roku.txt",
		Entries:       73,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-huawei", Name: "HaGeZi Native Tracker: Huawei", Category: CategoryPrivacy,
		Description:   "Telemetry of Huawei devices.",
		DescriptionDe: "Telemetrie von Huawei-Geräten.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.huawei.txt",
		Entries:       135,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-xiaomi", Name: "HaGeZi Native Tracker: Xiaomi", Category: CategoryPrivacy,
		Description:   "Telemetry of Xiaomi devices.",
		DescriptionDe: "Telemetrie von Xiaomi-Geräten.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.xiaomi.txt",
		Entries:       345,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-native-tiktok", Name: "HaGeZi Native Tracker: TikTok", Category: CategoryPrivacy,
		Description:   "Tracking and telemetry of TikTok (the app keeps working).",
		DescriptionDe: "Tracking und Telemetrie von TikTok (die App funktioniert weiter).",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/native.tiktok.txt",
		Entries:       436,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "windows-spy-blocker", Name: "WindowsSpyBlocker (spy)", Category: CategoryPrivacy,
		Description:   "Windows telemetry and data collection hosts.",
		DescriptionDe: "Hosts für Telemetrie und Datensammlung von Windows.",
		URL:           "https://raw.githubusercontent.com/crazy-max/WindowsSpyBlocker/master/data/hosts/spy.txt",
		Entries:       347,
		Maintainer:    "CrazyMax", License: "MIT", Homepage: "https://crazymax.dev/WindowsSpyBlocker/",
	},
	// adult
	{
		Key: "oisd-nsfw", Name: "OISD NSFW", Category: CategoryAdult, Recommended: true,
		Description:   "Adult content: pornography and other sexual content. Used by a category switch of the parental controls.",
		DescriptionDe: "Inhalte für Erwachsene: Pornografie und andere sexuelle Inhalte. Wird von einem Kategorie-Schalter des Jugendschutzes verwendet.",
		URL:           "https://nsfw.oisd.nl/",
		Entries:       469749,
		Maintainer:    "Stephan van Ruth", License: "GPL-3.0", Homepage: "https://oisd.nl",
	},
	{
		Key: "oisd-nsfw-small", Name: "OISD NSFW small", Category: CategoryAdult,
		Description:   "The most visited adult sites only; for hosts with little memory.",
		DescriptionDe: "Nur die meistbesuchten Erwachsenenseiten; für Geräte mit wenig Speicher.",
		URL:           "https://nsfw-small.oisd.nl/",
		Entries:       21428,
		Maintainer:    "Stephan van Ruth", License: "GPL-3.0", Homepage: "https://oisd.nl",
	},
	{
		Key: "hagezi-nsfw", Name: "HaGeZi NSFW", Category: CategoryAdult,
		Description:   "Adult content, also below domains that host other content.",
		DescriptionDe: "Inhalte für Erwachsene, auch unterhalb von Domains mit anderen Inhalten.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/nsfw.txt",
		Entries:       84133,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "blp-porn", Name: "Block List Project: Porn", Category: CategoryAdult,
		Description:   "Pornography sites.",
		DescriptionDe: "Pornografie-Seiten.",
		URL:           "https://blocklistproject.github.io/Lists/alt-version/porn-nl.txt",
		Entries:       953393,
		Maintainer:    "The Block List Project", License: "Unlicense", Homepage: "https://blocklistproject.github.io/Lists/",
	},
	{
		Key: "sinfonietta-porn", Name: "Sinfonietta Pornography", Category: CategoryAdult,
		Description:   "Pornography sites (hosts file).",
		DescriptionDe: "Pornografie-Seiten (Hosts-Datei).",
		URL:           "https://raw.githubusercontent.com/Sinfonietta/hostfiles/master/pornography-hosts",
		Entries:       61155,
		Maintainer:    "Sinfonietta", License: "MIT", Homepage: "https://github.com/Sinfonietta/hostfiles",
	},
	// gambling
	{
		Key: "hagezi-gambling-medium", Name: "HaGeZi Gambling (medium)", Category: CategoryGambling, Recommended: true,
		Description:   "Online casinos, betting and lotteries. Used by a category switch of the parental controls.",
		DescriptionDe: "Online-Casinos, Wetten und Lotterien. Wird von einem Kategorie-Schalter des Jugendschutzes verwendet.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/gambling.medium.txt",
		Entries:       210489,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-gambling", Name: "HaGeZi Gambling", Category: CategoryGambling,
		Description:   "The full gambling list: casinos, betting, lotteries and their affiliates.",
		DescriptionDe: "Die vollständige Glücksspiel-Liste: Casinos, Wetten, Lotterien und ihre Partner.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/gambling.txt",
		Entries:       544424,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-gambling-mini", Name: "HaGeZi Gambling (mini)", Category: CategoryGambling,
		Description:   "The most visited gambling sites only.",
		DescriptionDe: "Nur die meistbesuchten Glücksspielseiten.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/gambling.mini.txt",
		Entries:       143810,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "sinfonietta-gambling", Name: "Sinfonietta Gambling", Category: CategoryGambling,
		Description:   "Gambling sites (hosts file).",
		DescriptionDe: "Glücksspielseiten (Hosts-Datei).",
		URL:           "https://raw.githubusercontent.com/Sinfonietta/hostfiles/master/gambling-hosts",
		Entries:       2690,
		Maintainer:    "Sinfonietta", License: "MIT", Homepage: "https://github.com/Sinfonietta/hostfiles",
	},
	// dating
	{
		Key: "shadowwhisperer-dating", Name: "ShadowWhisperer Dating", Category: CategoryDating, PlainDomains: "subtree", Recommended: true,
		Description:   "Dating sites and apps. Used by a category switch of the parental controls.",
		DescriptionDe: "Dating-Seiten und -Apps. Wird von einem Kategorie-Schalter des Jugendschutzes verwendet.",
		URL:           "https://raw.githubusercontent.com/ShadowWhisperer/BlockLists/master/Lists/Dating",
		Entries:       1377,
		Maintainer:    "ShadowWhisperer", License: "Unlicense", Homepage: "https://github.com/ShadowWhisperer/BlockLists",
	},
	// piracy
	{
		Key: "hagezi-anti-piracy", Name: "HaGeZi Anti-Piracy", Category: CategoryPiracy, Recommended: true,
		Description:   "Illegal streaming, download and torrent sites. Used by a category switch of the parental controls.",
		DescriptionDe: "Illegale Streaming-, Download- und Torrent-Seiten. Wird von einem Kategorie-Schalter des Jugendschutzes verwendet.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/anti.piracy.txt",
		Entries:       52587,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "blp-piracy", Name: "Block List Project: Piracy", Category: CategoryPiracy,
		Description:   "Piracy, illegal downloads and streaming.",
		DescriptionDe: "Raubkopien, illegale Downloads und Streams.",
		URL:           "https://blocklistproject.github.io/Lists/alt-version/piracy-nl.txt",
		Entries:       2154,
		Maintainer:    "The Block List Project", License: "Unlicense", Homepage: "https://blocklistproject.github.io/Lists/",
	},
	// social
	{
		Key: "hagezi-social", Name: "HaGeZi Social Networks", Category: CategorySocial, Recommended: true,
		Description:   "Social networks such as Facebook, Instagram, TikTok, X and Snapchat (not messengers or streaming).",
		DescriptionDe: "Soziale Netzwerke wie Facebook, Instagram, TikTok, X und Snapchat (keine Messenger und kein Streaming).",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/social.txt",
		Entries:       900,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "sinfonietta-social", Name: "Sinfonietta Social", Category: CategorySocial,
		Description:   "Social networks (hosts file).",
		DescriptionDe: "Soziale Netzwerke (Hosts-Datei).",
		URL:           "https://raw.githubusercontent.com/Sinfonietta/hostfiles/master/social-hosts",
		Entries:       3808,
		Maintainer:    "Sinfonietta", License: "MIT", Homepage: "https://github.com/Sinfonietta/hostfiles",
	},
	// doh-vpn-bypass
	{
		Key: "hagezi-doh-vpn-bypass", Name: "HaGeZi DoH/VPN/TOR/Proxy Bypass", Category: CategoryBypass, Recommended: true,
		Description:   "Blocks encrypted DNS, VPN, TOR and proxy services that devices use to bypass DNS filtering. Used by a category switch of the parental controls.",
		DescriptionDe: "Blockiert verschlüsseltes DNS, VPN-, TOR- und Proxy-Dienste, mit denen Geräte die DNS-Filterung umgehen. Wird von einem Kategorie-Schalter des Jugendschutzes verwendet.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/doh-vpn-proxy-bypass.txt",
		Entries:       16313,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-doh", Name: "HaGeZi Encrypted DNS", Category: CategoryBypass,
		Description:   "Encrypted DNS servers only (DoH, DoT, DoQ); VPNs and proxies keep working.",
		DescriptionDe: "Nur verschlüsselte DNS-Server (DoH, DoT, DoQ); VPNs und Proxys funktionieren weiter.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/doh.txt",
		Entries:       3334,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "dibdot-doh", Name: "DoH domains (dibdot)", Category: CategoryBypass,
		Description:   "Public DNS-over-HTTPS servers.",
		DescriptionDe: "Öffentliche DNS-over-HTTPS-Server.",
		URL:           "https://raw.githubusercontent.com/dibdot/DoH-IP-blocklists/master/doh-domains.txt",
		Entries:       1363,
		Maintainer:    "dibdot", License: "GPL-3.0", Homepage: "https://github.com/dibdot/DoH-IP-blocklists",
	},
	// abused-tlds
	{
		Key: "hagezi-spam-tlds", Name: "HaGeZi Most Abused TLDs", Category: CategoryAbusedTLDs, Recommended: true,
		Description:   "Blocks whole top-level domains that are mostly used for spam and scams; pair it with its allowlist.",
		DescriptionDe: "Blockiert ganze Top-Level-Domains, die überwiegend für Spam und Betrug genutzt werden; nutze dazu die passende Erlaubnisliste.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/spam-tlds-adblock.txt",
		Entries:       130,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-spam-tlds-aggressive", Name: "HaGeZi Most Abused TLDs (aggressive)", Category: CategoryAbusedTLDs,
		Description:   "More abused top-level domains, including international ones; expect to allow some sites.",
		DescriptionDe: "Mehr missbrauchte Top-Level-Domains, auch internationale; rechne damit, einzelne Seiten zu erlauben.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/spam-tlds-adblock-aggressive.txt",
		Entries:       445,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	// url-shorteners
	{
		Key: "hagezi-urlshortener", Name: "HaGeZi URL Shortener", Category: CategoryURLShorteners, Recommended: true,
		Description:   "Link shorteners and redirect services that hide where a link leads.",
		DescriptionDe: "Link-Kürzer und Weiterleitungsdienste, die das Ziel eines Links verbergen.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/urlshortener.txt",
		Entries:       9979,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "shadowwhisperer-urlshortener", Name: "ShadowWhisperer URL Shortener", Category: CategoryURLShorteners,
		Description:   "Link shortener services.",
		DescriptionDe: "Link-Kürzer-Dienste.",
		URL:           "https://raw.githubusercontent.com/ShadowWhisperer/BlockLists/master/Lists/UrlShortener",
		Entries:       5964,
		Maintainer:    "ShadowWhisperer", License: "Unlicense", Homepage: "https://github.com/ShadowWhisperer/BlockLists",
	},
	// stalkerware
	{
		Key: "echap-stalkerware", Name: "Stalkerware indicators (Echap)", Category: CategoryStalkerware, Recommended: true,
		Description:   "Servers of stalkerware and spy apps that secretly monitor a phone.",
		DescriptionDe: "Server von Stalkerware und Spionage-Apps, die ein Handy heimlich überwachen.",
		URL:           "https://raw.githubusercontent.com/AssoEchap/stalkerware-indicators/master/generated/hosts",
		Entries:       925,
		Maintainer:    "Echap", License: "CC-BY-4.0", Homepage: "https://github.com/AssoEchap/stalkerware-indicators",
	},
	// regional
	{
		Key: "rpilist-easylist", Name: "RPiList EasyList", Category: CategoryRegional,
		Description:   "Ads and trackers with a focus on German-language sites.",
		DescriptionDe: "Werbung und Tracker mit Schwerpunkt auf deutschsprachigen Seiten.",
		URL:           "https://raw.githubusercontent.com/RPiList/specials/master/Blocklisten/easylist",
		Entries:       170957,
		Maintainer:    "RPiList", License: "CC-BY-NC-4.0", Homepage: "https://github.com/RPiList/specials",
	},
	{
		Key: "rpilist-notserious", Name: "RPiList Notserious", Category: CategoryRegional,
		Description:   "Fake shops and rip-off sites that target German-speaking users.",
		DescriptionDe: "Fake-Shops und Abzockseiten, die sich an deutschsprachige Nutzer richten.",
		URL:           "https://raw.githubusercontent.com/RPiList/specials/master/Blocklisten/notserious",
		Entries:       106320,
		Maintainer:    "RPiList", License: "CC-BY-NC-4.0", Homepage: "https://github.com/RPiList/specials",
	},
	{
		Key: "liste-fr", Name: "Liste FR (hosts)", Category: CategoryRegional,
		Description:   "French ads and trackers.",
		DescriptionDe: "Französische Werbung und Tracker.",
		URL:           "https://raw.githubusercontent.com/easylist/listefr/master/hosts.txt",
		Entries:       6079,
		Maintainer:    "Liste FR", License: "CC-BY-NC-SA-3.0", Homepage: "https://github.com/easylist/listefr",
	},
	{
		Key: "anti-ad", Name: "anti-AD", Category: CategoryRegional,
		Description:   "Chinese ads and trackers.",
		DescriptionDe: "Chinesische Werbung und Tracker.",
		URL:           "https://anti-ad.net/easylist.txt",
		Entries:       95958,
		Maintainer:    "privacy-protection-tools", License: "MIT", Homepage: "https://github.com/privacy-protection-tools/anti-AD",
	},
	{
		Key: "hostsvn", Name: "hostsVN", Category: CategoryRegional,
		Description:   "Vietnamese ads and trackers.",
		DescriptionDe: "Vietnamesische Werbung und Tracker.",
		URL:           "https://raw.githubusercontent.com/bigdargon/hostsVN/master/hosts",
		Entries:       18438,
		Maintainer:    "bigdargon", License: "MIT", Homepage: "https://bigdargon.github.io/hostsVN/",
	},
	{
		Key: "yous-list", Name: "YousList", Category: CategoryRegional,
		Description:   "Korean ads and trackers.",
		DescriptionDe: "Koreanische Werbung und Tracker.",
		URL:           "https://raw.githubusercontent.com/yous/YousList/master/hosts.txt",
		Entries:       625,
		Maintainer:    "yous", License: "CC-BY-4.0", Homepage: "https://github.com/yous/YousList",
	},
	{
		Key: "kadhosts", Name: "KADhosts", Category: CategoryRegional,
		Description:   "Polish scam, fake shop and SMS subscription sites.",
		DescriptionDe: "Polnische Betrugsseiten, Fake-Shops und SMS-Abofallen.",
		URL:           "https://raw.githubusercontent.com/FiltersHeroes/KADhosts/master/KADhosts.txt",
		Entries:       42678,
		Maintainer:    "FiltersHeroes", License: "CC-BY-SA-4.0", Homepage: "https://kadantiscam.netlify.app/",
	},
	{
		Key: "hufilter-dns", Name: "Hufilter DNS", Category: CategoryRegional,
		Description:   "Hungarian ads and trackers.",
		DescriptionDe: "Ungarische Werbung und Tracker.",
		URL:           "https://cdn.jsdelivr.net/gh/hufilter/hufilter@gh-pages/hufilter-dns.txt",
		Entries:       94,
		Maintainer:    "hufilter", License: "CC-BY-4.0", Homepage: "https://hufilter.hu",
	},
	// allow
	{
		Key: "hagezi-allow-referral", Name: "HaGeZi Allowlist Referral", Category: CategoryAllow, Kind: "allow",
		Description:   "Unblocks affiliate and referral links in mails and search results that stricter lists block.",
		DescriptionDe: "Gibt Affiliate- und Empfehlungslinks in Mails und Suchergebnissen frei, die strengere Listen blockieren.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/whitelist-referral.txt",
		Entries:       935,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-allow-spam-tlds", Name: "HaGeZi Most Abused TLDs: allowlist", Category: CategoryAllow, Kind: "allow",
		Description:   "Legitimate sites below the top-level domains that the Most Abused TLDs lists block.",
		DescriptionDe: "Seriöse Seiten unterhalb der Top-Level-Domains, die die Listen der missbrauchten TLDs blockieren.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/spam-tlds-adblock-allow.txt",
		Entries:       1029,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "hagezi-allow-urlshortener", Name: "HaGeZi Allowlist URL Shortener", Category: CategoryAllow, Kind: "allow",
		Description:   "Unblocks common link shorteners that stricter lists block.",
		DescriptionDe: "Gibt verbreitete Link-Kürzer frei, die strengere Listen blockieren.",
		URL:           "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/whitelist-urlshortener.txt",
		Entries:       9974,
		Maintainer:    "HaGeZi", License: "GPL-3.0", Homepage: "https://github.com/hagezi/dns-blocklists",
	},
	{
		Key: "anudeep-allow", Name: "Commonly allowlisted domains (anudeepND)", Category: CategoryAllow, Kind: "allow",
		Description:   "Domains that blocklists often block by mistake and that popular services need.",
		DescriptionDe: "Domains, die Blocklisten oft versehentlich blockieren und die verbreitete Dienste brauchen.",
		URL:           "https://raw.githubusercontent.com/anudeepND/whitelist/master/domains/whitelist.txt",
		Entries:       191,
		Maintainer:    "anudeepND", License: "MIT", Homepage: "https://github.com/anudeepND/whitelist",
	},
}

func init() {
	for i := range catalog {
		c := &catalog[i]
		if c.Kind == "" {
			c.Kind = "block"
		}
		if c.PlainDomains == "" {
			c.PlainDomains = "exact"
		}
	}
}

// catalogEntry returns the catalogue entry with key (aliases resolved).
func catalogEntry(key string) (CatalogEntry, bool) {
	if k, ok := catalogAliases[key]; ok {
		key = k
	}
	for _, c := range catalog {
		if c.Key == key {
			return c, true
		}
	}
	return CatalogEntry{}, false
}

// catalogByURL returns the catalogue entry whose URL equals url exactly.
func catalogByURL(url string) (CatalogEntry, bool) {
	for _, c := range catalog {
		if c.URL == url {
			return c, true
		}
	}
	return CatalogEntry{}, false
}

// Catalog returns the embedded list catalogue.
func (e *Engine) Catalog() []CatalogEntry { return slices.Clone(catalog) }
