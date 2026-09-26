package settings

import "slices"

// UpstreamPreset is a family-safe resolver a client group can use instead
// of the DNS upstreams (clients.Group.UpstreamPreset): an operator's
// filtering DoH endpoint and, for the clock guard (plain DNS while the
// clock is not set), its plain addresses. Only adult-content filters are
// offered: the malware-only services equal the default upstream and
// fallback; a group that wants another resolver lists its own upstreams.
type UpstreamPreset struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	Upstreams []string `json:"upstreams"` // DoH, used normally
	Plain     []string `json:"plain"`     // plain DNS addresses, used only while the clock guard is active
}

// upstreamPresets is the preset table (API and UI order). Every URL is
// verified for a release like the list catalogue: the DoH endpoint answers
// a probe and a known adult test name is blocked.
var upstreamPresets = []UpstreamPreset{
	// Cloudflare for Families, "malware and adult content":
	// https://developers.cloudflare.com/1.1.1.1/setup/#1111-for-families
	{
		Key: "cloudflare-family", Name: "Cloudflare for Families (malware and adult content)",
		Upstreams: []string{"https://family.cloudflare-dns.com/dns-query"},
		Plain:     []string{"1.1.1.3", "1.0.0.3", "2606:4700:4700::1113", "2606:4700:4700::1003"},
	},
	// OpenDNS FamilyShield: https://www.opendns.com/setupguide/#familyshield
	// (plain addresses) and the DoH endpoint
	// https://support.opendns.com/hc/en-us/articles/360038086532
	{
		Key: "opendns-familyshield", Name: "OpenDNS FamilyShield",
		Upstreams: []string{"https://doh.familyshield.opendns.com/dns-query"},
		Plain:     []string{"208.67.222.123", "208.67.220.123", "2620:119:35::123", "2620:119:53::123"},
	},
	// CleanBrowsing Family Filter: https://cleanbrowsing.org/filters/
	{
		Key: "cleanbrowsing-family", Name: "CleanBrowsing Family Filter",
		Upstreams: []string{"https://doh.cleanbrowsing.org/doh/family-filter/"},
		Plain:     []string{"185.228.168.168", "185.228.169.168", "2a0d:2a00:1::", "2a0d:2a00:2::"},
	},
}

// UpstreamPresets returns the preset table in its order (a copy).
func UpstreamPresets() []UpstreamPreset {
	out := make([]UpstreamPreset, len(upstreamPresets))
	for i, p := range upstreamPresets {
		out[i] = UpstreamPreset{Key: p.Key, Name: p.Name, Upstreams: slices.Clone(p.Upstreams), Plain: slices.Clone(p.Plain)}
	}
	return out
}

// UpstreamPresetByKey returns the preset with key (a copy).
func UpstreamPresetByKey(key string) (UpstreamPreset, bool) {
	for _, p := range UpstreamPresets() {
		if p.Key == key {
			return p, true
		}
	}
	return UpstreamPreset{}, false
}
