package parental

import (
	"slices"
	"strings"
	"testing"
)

// The catalogue is a plain literal; this test is its validation: slugs,
// categories, order, valid A-label domains, no duplicates or overlaps
// across services, no public suffixes and no shared infrastructure.
func TestCatalogue(t *testing.T) {
	publicSuffixes := []string{"co.uk", "com.au", "co.jp", "com.br", "co.nz", "org.uk", "github.io", "blogspot.com"}
	shared := []string{"googleapis.com", "akamaihd.net", "akamaized.net", "cloudfront.net", "fastly.net", "media-amazon.com",
		"amazonaws.com", "google.com", "gstatic.com", "googleusercontent.com", "azureedge.net", "edgekey.net", "akamai.net",
		"cloudflare.com", "microsoft.com", "apple.com", "ggpht.com", "l.google.com"}
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
			if !strings.Contains(d, ".") || slices.Contains(publicSuffixes, d) {
				t.Errorf("%s: %q is a public suffix", s.ID, d)
			}
			if slices.Contains(shared, d) {
				t.Errorf("%s: %q is shared infrastructure", s.ID, d)
			}
			if o, ok := owner[d]; ok {
				t.Errorf("%s: %q is already a domain of %s", s.ID, d, o)
			}
			owner[d] = s.ID
		}
	}
	// No domain lies inside another service's subtree (the match would
	// depend on the walk, not on the catalogue).
	for d, o := range owner {
		for p := d; strings.Contains(p, "."); {
			p = p[strings.IndexByte(p, '.')+1:]
			if po, ok := owner[p]; ok && po != o {
				t.Errorf("%q (%s) is inside %q (%s)", d, o, p, po)
			}
		}
	}
	if got := Services(); len(got) != len(catalogue) || &got[0].Domains[0] == &catalogue[0].Domains[0] {
		t.Error("Services must return a copy of the catalogue")
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
