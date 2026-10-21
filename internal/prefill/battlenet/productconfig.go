package battlenet

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The product config (JSON, from the CDN's config path) says which tags
// an install uses: the platform's, the 64-bit ones, "noigr", the tags of
// the install packages (Call of Duty's modes, HD textures), the country
// tags, and whether the data is encrypted.

// ProductConfig is what the prefill reads of a product config.
type ProductConfig struct {
	Tags      []string // platform, 64-bit, noigr, extra and package tags
	Encrypted bool
	Locales   []string
	Replace   map[string]string
	GeoIP     bool // tags "geoip-<ISO 3166 alpha-2>"
	Account   bool // tags "acct-<ISO 3166 alpha-3>"
}

type obj = map[string]any

func child(o obj, path ...string) obj {
	for _, p := range path {
		n, ok := o[p].(obj)
		if !ok {
			return nil
		}
		o = n
	}
	return o
}

func strs(v any) []string {
	a, _ := v.([]any)
	var out []string
	for _, x := range a {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func truthy(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return x == "true" || x == "1"
	}
	return false
}

// ParseProductConfig reads a product config for an install in region.
func ParseProductConfig(b []byte, region string) (*ProductConfig, error) {
	var top obj
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("battlenet: product config: %w", err)
	}
	// Mirrors wrap the file in its regions ("eu+kr+us": {...}).
	if child(top, "all") == nil && child(top, "platform") == nil {
		var first obj
		for _, k := range slices.Sorted(maps.Keys(top)) {
			c, ok := top[k].(obj)
			if !ok || (child(c, "all") == nil && child(c, "platform") == nil) {
				continue
			}
			if first == nil {
				first = c
			}
			if slices.Contains(strings.Split(k, "+"), region) {
				first = c
				break
			}
		}
		if first != nil {
			top = first
		}
	}
	all := child(top, "all", "config")
	win := child(top, "platform", "win", "config")
	pc := &ProductConfig{Replace: map[string]string{}}
	add := func(t ...string) {
		for _, s := range t {
			if !slices.Contains(pc.Tags, s) {
				pc.Tags = append(pc.Tags, s)
			}
		}
	}
	add(strs(win["tags"])...)
	add(strs(win["tags_64bit"])...)
	add(strs(all["noigr_tags"])...)
	add(strs(all["extra_tags"])...)
	add(strs(child(top, region, "config")["extra_tags"])...)
	var packages func(v any)
	packages = func(v any) {
		a, _ := v.([]any)
		for _, x := range a {
			p, _ := x.(obj)
			add(strs(p["tags"])...)
			packages(p["install_packages"])
		}
	}
	packages(child(all, "opaque_complex_data", "tags_config_uri")["install_packages"])
	if s, _ := all["decryption_key_name"].(string); s != "" {
		pc.Encrypted = true
	}
	pc.GeoIP = truthy(all["tact_has_geoip_country_tags"])
	pc.Account = truthy(all["tact_has_account_country_tags"])
	pc.Locales = strs(all["supported_locales"])
	if r, ok := all["replacement_locales"].(obj); ok {
		for k, v := range r {
			if s, ok := v.(string); ok {
				pc.Replace[k] = s
			}
		}
	}
	return pc, nil
}

// countries maps the locales' countries to ISO 3166 alpha-3 (the account
// country tags).
var countries = map[string]string{
	"DE": "DEU", "GB": "GBR", "US": "USA", "ES": "ESP", "MX": "MEX", "FR": "FRA", "IT": "ITA", "JP": "JPN", "KR": "KOR",
	"PL": "POL", "BR": "BRA", "PT": "PRT", "RU": "RUS", "TH": "THA", "TR": "TUR", "CN": "CHN", "TW": "TWN",
}

// Query is the agent's tag query of an install: Windows (64-bit), the
// region, the country of the locale and the locale, for speech and text.
func (pc *ProductConfig) Query(region, locale string) Query {
	if len(pc.Locales) > 0 && !slices.Contains(pc.Locales, locale) {
		if r, ok := pc.Replace[locale]; ok {
			locale = r
		} else if slices.Contains(pc.Locales, "enUS") {
			locale = "enUS"
		}
	}
	base := slices.Clone(pc.Tags)
	base = append(base, strings.ToUpper(region))
	if len(locale) == 4 {
		c := locale[2:]
		if pc.GeoIP {
			base = append(base, "geoip-"+c)
		}
		if a, ok := countries[c]; ok && pc.Account {
			base = append(base, "acct-"+a)
		}
	}
	base = append(base, locale)
	return Query{append(slices.Clone(base), "speech"), append(base, "text")}
}
