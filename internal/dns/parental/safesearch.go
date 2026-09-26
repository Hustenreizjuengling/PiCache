package parental

import (
	"fmt"
	"time"
)

// Safe search (docs/ARCHITECTURE.md 7.1 step 7c): the DNS server answers
// the names of a search engine with a CNAME to the engine's restricted
// host, per group. The table below was written from the vendors'
// documentation; every target and every name resolved on 2026-09-26.
// Names match exactly (one map lookup, no suffix walk: mail.google.com is
// never rewritten); every target is a host name reached by CNAME, never a
// fixed address, and no target is a table name.

// searchEngine is a member of SafeSearch.
type searchEngine uint8

const (
	engineGoogle searchEngine = iota
	engineYouTube
	engineBing
	engineDuckDuckGo
	engineEcosia
	engineYandex
	enginePixabay
	numEngines
)

// YouTube restricted mode levels as numbers (strict beats moderate).
const (
	youTubeOff = iota
	youTubeModerate
	youTubeStrict
)

// Targets and labels of the engines (the labels are the log reasons).
const (
	targetGoogle         = "forcesafesearch.google.com"
	targetYouTubeMod     = "restrictmoderate.youtube.com"
	targetYouTubeStrict  = "restrict.youtube.com"
	targetBing           = "strict.bing.com"
	targetDuckDuckGo     = "safe.duckduckgo.com"
	targetEcosia         = "strict-safe-search.ecosia.org"
	targetYandex         = "familysearch.yandex.ru"
	targetPixabay        = "safesearch.pixabay.com"
	labelYouTubeModerate = "YouTube restricted mode (moderate)"
	labelYouTubeStrict   = "YouTube restricted mode (strict)"
)

// engineTargets and engineLabels by engine (YouTube: see youTubeRewrite).
var (
	engineTargets = [numEngines]string{
		engineGoogle: targetGoogle, engineBing: targetBing, engineDuckDuckGo: targetDuckDuckGo,
		engineEcosia: targetEcosia, engineYandex: targetYandex, enginePixabay: targetPixabay,
	}
	engineLabels = [numEngines]string{
		engineGoogle: "Google safe search", engineBing: "Bing safe search", engineDuckDuckGo: "DuckDuckGo safe search",
		engineEcosia: "Ecosia safe search", engineYandex: "Yandex family search", enginePixabay: "Pixabay safe search",
	}
)

// googleDomains are the domains <d> of Google's supported-domains list
// (https://www.google.com/supported_domains, 2026-09-26, 187 entries):
// google.<d> and www.google.<d> are rewritten. Google documents mapping
// its domains to forcesafesearch.google.com at
// https://support.google.com/websearch/answer/186669.
var googleDomains = []string{
	"com", "ad", "ae", "com.af", "com.ag", "al", "am", "co.ao", "com.ar", "as", "at", "com.au", "az", "ba", "com.bd",
	"be", "bf", "bg", "com.bh", "bi", "bj", "com.bn", "com.bo", "com.br", "bs", "bt", "co.bw", "by", "com.bz", "ca",
	"cd", "cf", "cg", "ch", "ci", "co.ck", "cl", "cm", "cn", "com.co", "co.cr", "com.cu", "cv", "com.cy", "cz", "de",
	"dj", "dk", "dm", "com.do", "dz", "com.ec", "ee", "com.eg", "es", "com.et", "fi", "com.fj", "fm", "fr", "ga", "ge",
	"gg", "com.gh", "com.gi", "gl", "gm", "gr", "com.gt", "gy", "com.hk", "hn", "hr", "ht", "hu", "co.id", "ie",
	"co.il", "im", "co.in", "iq", "is", "it", "je", "com.jm", "jo", "co.jp", "co.ke", "com.kh", "ki", "kg", "co.kr",
	"com.kw", "kz", "la", "com.lb", "li", "lk", "co.ls", "lt", "lu", "lv", "com.ly", "co.ma", "md", "me", "mg", "mk",
	"ml", "com.mm", "mn", "com.mt", "mu", "mv", "mw", "com.mx", "com.my", "co.mz", "com.na", "com.ng", "com.ni", "ne",
	"nl", "no", "com.np", "nr", "nu", "co.nz", "com.om", "com.pa", "com.pe", "com.pg", "com.ph", "com.pk", "pl", "pn",
	"com.pr", "ps", "pt", "com.py", "com.qa", "ro", "ru", "rw", "com.sa", "com.sb", "sc", "se", "com.sg", "sh", "si",
	"sk", "com.sl", "sn", "so", "sm", "sr", "st", "com.sv", "td", "tg", "co.th", "com.tj", "tl", "tm", "tn", "to",
	"com.tr", "tt", "com.tw", "co.tz", "com.ua", "co.ug", "co.uk", "com.uy", "co.uz", "com.vc", "co.ve", "co.vi",
	"com.vn", "vu", "ws", "rs", "co.za", "co.zm", "co.zw", "cat",
}

// yandexDomains are the Yandex Search domains <d>: yandex.<d> and
// www.yandex.<d> are rewritten, like ya.ru and www.ya.ru. Yandex documents
// its family search address for yandex.ru
// (https://yandex.com/support/search/en/schoolsearch, host
// familysearch.yandex.ru); the regional search domains serve the same
// search.
var yandexDomains = []string{
	"ru", "com", "com.tr", "by", "kz", "uz", "az", "com.am", "com.ge", "co.il", "md", "tj", "tm", "lt", "lv", "ee", "eu",
}

// safeSearchTable maps every rewritten name to its engine.
var safeSearchTable = map[string]searchEngine{}

func init() {
	add := func(e searchEngine, names ...string) {
		for _, n := range names {
			if _, dup := safeSearchTable[n]; dup {
				panic(fmt.Sprintf("parental: safe search name %s listed twice", n))
			}
			safeSearchTable[n] = e
		}
	}
	for _, d := range googleDomains {
		add(engineGoogle, "google."+d, "www.google."+d)
	}
	// https://support.google.com/a/answer/6214622 (restrict.youtube.com,
	// restrictmoderate.youtube.com)
	add(engineYouTube, "www.youtube.com", "m.youtube.com", "youtubei.googleapis.com", "youtube.googleapis.com",
		"www.youtube-nocookie.com")
	// https://help.bing.microsoft.com/#apex/bing/en-us/10003/0 (strict.bing.com)
	add(engineBing, "www.bing.com")
	// https://duckduckgo.com/duckduckgo-help-pages/features/safe-search/ (safe.duckduckgo.com)
	add(engineDuckDuckGo, "duckduckgo.com", "www.duckduckgo.com", "start.duckduckgo.com")
	// https://support.ecosia.org/article/562-how-to-enforce-safe-search-at-your-organization
	add(engineEcosia, "www.ecosia.org")
	for _, d := range yandexDomains {
		add(engineYandex, "yandex."+d, "www.yandex."+d)
	}
	add(engineYandex, "ya.ru", "www.ya.ru")
	// https://pixabay.com (safesearch.pixabay.com)
	add(enginePixabay, "pixabay.com", "www.pixabay.com")
}

// safeGroup is one group's compiled safe search setting.
type safeGroup struct {
	id      int64
	name    string
	engines uint8 // bit e = engine e is restricted (YouTube: see youtube)
	youtube uint8 // youTubeOff | youTubeModerate | youTubeStrict
}

// compileSafeSearch returns the group's setting (nil when everything is off).
func compileSafeSearch(id int64, name string, s SafeSearch) *safeGroup {
	g := &safeGroup{id: id, name: name}
	for e, on := range map[searchEngine]bool{engineGoogle: s.Google, engineBing: s.Bing, engineDuckDuckGo: s.DuckDuckGo,
		engineEcosia: s.Ecosia, engineYandex: s.Yandex, enginePixabay: s.Pixabay} {
		if on {
			g.engines |= 1 << e
		}
	}
	switch s.YouTube {
	case YouTubeModerate:
		g.youtube = youTubeModerate
	case YouTubeStrict:
		g.youtube = youTubeStrict
	}
	if g.engines == 0 && g.youtube == youTubeOff {
		return nil
	}
	return g
}

// SafeSearchRewrite is how SafeSearch answers a name: a CNAME to Target.
type SafeSearchRewrite struct {
	Target  string // the engine's restricted host (lower-case, no trailing dot)
	Label   string // "Google safe search", "YouTube restricted mode (strict)", …
	GroupID int64
	Group   string // the name of the group whose setting applies
}

// Reason is the text of the query log, e.g. "Kids: YouTube restricted mode
// (strict)".
func (r SafeSearchRewrite) Reason() string { return r.Group + ": " + r.Label }

// SafeSearch returns the rewrite of qname (lower-case, no trailing dot) for
// a client in groupIDs, taking the union of the groups' settings; YouTube
// strict beats moderate. The group reported is the first group in the
// order of groupIDs that enables the chosen level. It does not depend on
// the blocking switch, a pause or an allow override (content protection,
// ARCHITECTURE 16); now is part of the signature for symmetry with Check
// (safe search is not scheduled). Hot path: a name that is not in the
// table costs one map lookup; it never allocates.
func (e *Engine) SafeSearch(qname string, groupIDs []int64, _ time.Time) (SafeSearchRewrite, bool) {
	s := e.snap.Load()
	if len(s.safe) == 0 {
		return SafeSearchRewrite{}, false
	}
	eng, ok := safeSearchTable[qname]
	if !ok {
		return SafeSearchRewrite{}, false
	}
	if eng == engineYouTube {
		var best *safeGroup
		for _, gid := range groupIDs {
			if g := s.safe[gid]; g != nil && (best == nil || g.youtube > best.youtube) {
				best = g
			}
		}
		switch {
		case best == nil || best.youtube == youTubeOff:
			return SafeSearchRewrite{}, false
		case best.youtube == youTubeStrict:
			return SafeSearchRewrite{Target: targetYouTubeStrict, Label: labelYouTubeStrict, GroupID: best.id, Group: best.name}, true
		}
		return SafeSearchRewrite{Target: targetYouTubeMod, Label: labelYouTubeModerate, GroupID: best.id, Group: best.name}, true
	}
	for _, gid := range groupIDs {
		if g := s.safe[gid]; g != nil && g.engines&(1<<eng) != 0 {
			return SafeSearchRewrite{Target: engineTargets[eng], Label: engineLabels[eng], GroupID: g.id, Group: g.name}, true
		}
	}
	return SafeSearchRewrite{}, false
}
