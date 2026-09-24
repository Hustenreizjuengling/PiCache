package filter

import "slices"

// catalog is the curated list selection (URLs verified 2026-09-24). The
// first entry is the default list created at first start.
var catalog = []CatalogEntry{
	{
		Key: "hagezi-multi", Name: "HaGeZi Multi NORMAL", Category: "general", PlainDomains: "exact", Recommended: true,
		Description: "Ads, tracking, metrics, telemetry, phishing, malware and scam. Balanced, rarely breaks anything (default).",
		URL:         "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/multi.txt",
	},
	{
		Key: "hagezi-pro", Name: "HaGeZi Multi PRO", Category: "general", PlainDomains: "exact",
		Description: "Stricter than NORMAL: more ads and trackers; may occasionally need an allow rule.",
		URL:         "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/pro.txt",
	},
	{
		Key: "hagezi-pro-plus", Name: "HaGeZi Multi PRO++", Category: "general", PlainDomains: "exact",
		Description: "Aggressive blocking for experienced users; expect to add allow rules.",
		URL:         "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/pro.plus.txt",
	},
	{
		Key: "hagezi-ultimate", Name: "HaGeZi Multi ULTIMATE", Category: "general", PlainDomains: "exact",
		Description: "Most aggressive HaGeZi list; will break some services.",
		URL:         "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/ultimate.txt",
	},
	{
		Key: "hagezi-tif", Name: "HaGeZi Threat Intelligence Feeds", Category: "security", PlainDomains: "exact",
		Description: "Malware, phishing, scam, cryptojacking and command-and-control domains. Large.",
		URL:         "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/tif.txt",
	},
	{
		Key: "hagezi-doh-vpn-bypass", Name: "HaGeZi DoH/VPN/TOR/Proxy Bypass", Category: "security", PlainDomains: "exact",
		Description: "Blocks encrypted DNS, VPN, TOR and proxy services that devices use to bypass DNS filtering.",
		URL:         "https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/doh-vpn-proxy-bypass.txt",
	},
	{
		Key: "oisd-small", Name: "OISD small", Category: "general", PlainDomains: "exact",
		Description: "Compact ads and tracking list with very few false positives.",
		URL:         "https://small.oisd.nl/",
	},
	{
		Key: "oisd-big", Name: "OISD big", Category: "general", PlainDomains: "exact",
		Description: "Comprehensive ads, tracking and malware list with few false positives.",
		URL:         "https://big.oisd.nl/",
	},
	{
		Key: "stevenblack", Name: "StevenBlack Unified hosts", Category: "general", PlainDomains: "exact",
		Description: "Classic hosts file (Pi-hole default): adware and malware.",
		URL:         "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
	},
	{
		Key: "adguard-dns", Name: "AdGuard DNS filter", Category: "general", PlainDomains: "exact",
		Description: "AdGuard's DNS filter (AdGuard Home default): ads and tracking.",
		URL:         "https://adguardteam.github.io/HostlistsRegistry/assets/filter_1.txt",
	},
	{
		Key: "1hosts-lite", Name: "1Hosts Lite", Category: "general", PlainDomains: "exact",
		Description: "Ads and tracking, tuned to avoid breakage.",
		URL:         "https://badmojr.github.io/1Hosts/Lite/adblock.txt",
	},
}

// Catalog returns the embedded list catalogue.
func (e *Engine) Catalog() []CatalogEntry { return slices.Clone(catalog) }
