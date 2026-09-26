package filter

import (
	"context"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Search bounds (GET /filter/search).
const (
	minSearchQuery = 3
	maxSearchQuery = 253
	maxSearchLimit = 200
)

// searchBudget bounds the scan of the lists (Explain's budget; tests lower
// it).
var searchBudget = explainTimeout

// SearchItem is one hit of Search: a user rule, an IP rule or a list entry
// whose pattern, domain or line contains the query. Entry is the export
// line of a rule, the pattern of an IP rule or the line of a list as the
// parser reads it (shownLine: trimmed, without its inline comment, at most
// 1 100 bytes); Name the rule pattern or the list name.
// Enabled is the rule's or IP rule's flag (list entries: true, only enabled
// lists are searched). Applies (only when a client was given) reports
// whether the entry is enabled and shares an enabled group with the client
// (pauses ignored, as in Explain).
type SearchItem struct {
	Source       string   `json:"source"` // rule | ip-rule | list
	RuleID       int64    `json:"ruleId,omitempty"`
	ListID       int64    `json:"listId,omitempty"`
	Name         string   `json:"name"`
	Entry        string   `json:"entry"`
	Kind         string   `json:"kind"`   // exact | subtree | regex | ip
	Action       string   `json:"action"` // allow | block
	Qtypes       []string `json:"qtypes"`
	QtypesNegate bool     `json:"qtypesNegate"`
	Denyallow    []string `json:"denyallow"`
	GroupIDs     []int64  `json:"groupIds"`
	Enabled      bool     `json:"enabled"`
	Applies      *bool    `json:"applies,omitempty"`
}

// SearchResult is the answer of Search. Truncated: the limit was reached;
// TimedOut: the 10 s budget ended the scan (the hits so far are returned);
// ScannedLists of TotalLists (the enabled lists with a local copy) were
// read completely.
type SearchResult struct {
	Q            string       `json:"q"`
	Items        []SearchItem `json:"items"`
	Truncated    bool         `json:"truncated"`
	TimedOut     bool         `json:"timedOut"`
	ScannedLists int          `json:"scannedLists"`
	TotalLists   int          `json:"totalLists"`
}

// NormalizeSearchQuery trims and lower-cases a search query and checks it:
// 3–253 bytes of printable ASCII (field q).
func NormalizeSearchQuery(q string) (string, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if len(q) < minSearchQuery || len(q) > maxSearchQuery {
		return "", apperr.Invalid("q", "must be %d to %d characters", minSearchQuery, maxSearchQuery)
	}
	for i := 0; i < len(q); i++ {
		if q[i] < 0x20 || q[i] > 0x7e {
			return "", apperr.Invalid("q", "must be printable ASCII (international names in punycode)")
		}
	}
	return q, nil
}

// Search finds q (NormalizeSearchQuery) in the user rules and IP rules
// (their patterns), then in the enabled lists that have a local copy, in
// id order, one file at a time with the parser of Explain (domains of exact
// and subtree entries, the lines of patterns and address entries; entries
// cancelled by a $badfilter of the same list are left out). It never
// downloads. It stops at limit hits (Truncated) or after Explain's 10 s
// budget (TimedOut); it shares Explain's semaphore without waiting for it
// (busy: apperr.Unavailable). With client, Applies is set from groups (the
// client's enabled groups).
func (e *Engine) Search(ctx context.Context, q string, limit int, groups []int64, client bool) (SearchResult, error) {
	q, err := NormalizeSearchQuery(q)
	if err != nil {
		return SearchResult{}, err
	}
	if limit < 1 || limit > maxSearchLimit {
		return SearchResult{}, apperr.Invalid("limit", "must be between 1 and %d", maxSearchLimit)
	}
	select {
	case e.explain <- struct{}{}:
		defer func() { <-e.explain }()
	default:
		return SearchResult{}, apperr.Unavailable("too many search requests, try again")
	}
	budget, cancel := context.WithTimeout(ctx, searchBudget)
	defer cancel()

	res := SearchResult{Q: q, Items: []SearchItem{}}
	applies := func(enabled bool, entryGroups []int64) *bool {
		if !client {
			return nil
		}
		v := enabled && sharesGroup(entryGroups, groups)
		return &v
	}
	// add appends a hit; false once the limit is reached (Truncated).
	add := func(it SearchItem) bool {
		if len(res.Items) >= limit {
			res.Truncated = true
			return false
		}
		res.Items = append(res.Items, it)
		return true
	}
	rules, err := e.Rules(ctx, RuleQuery{})
	if err != nil {
		return SearchResult{}, err
	}
	for _, r := range rules {
		if !strings.Contains(strings.ToLower(r.Pattern), q) {
			continue
		}
		if !add(SearchItem{Source: "rule", RuleID: r.ID, Name: r.Pattern, Entry: RuleLine(r), Kind: r.Type, Action: r.Action,
			Qtypes: nonNilStrings(r.Qtypes), QtypesNegate: r.QtypesNegate, Denyallow: nonNilStrings(r.Denyallow),
			GroupIDs: nonNil(r.GroupIDs), Enabled: r.Enabled, Applies: applies(r.Enabled, r.GroupIDs)}) {
			return res, nil
		}
	}
	ipRules, err := e.IPRules(ctx, IPRuleQuery{})
	if err != nil {
		return SearchResult{}, err
	}
	for _, r := range ipRules {
		if !strings.Contains(r.Pattern, q) {
			continue
		}
		if !add(SearchItem{Source: "ip-rule", RuleID: r.ID, Name: r.Pattern, Entry: r.Pattern, Kind: "ip", Action: r.Action,
			Qtypes: []string{}, Denyallow: []string{}, GroupIDs: nonNil(r.GroupIDs), Enabled: r.Enabled, Applies: applies(r.Enabled, r.GroupIDs)}) {
			return res, nil
		}
	}

	type job struct {
		id     int64
		name   string
		format listFormat
		groups []int64
	}
	var jobs []job
	e.mu.Lock()
	for _, rt := range sortedLists(e.lists) {
		if rt.Enabled && e.hasCache(rt.ID) {
			jobs = append(jobs, job{rt.ID, rt.Name, rt.format(), slices.Clone(nonNil(rt.GroupIDs))})
		}
	}
	e.mu.Unlock()
	res.TotalLists = len(jobs)
	for _, j := range jobs {
		hits, err := e.searchList(budget, e.cachePath(j.id), j.format, q, limit-len(res.Items)+1)
		if budget.Err() != nil {
			res.TimedOut = true
			return res, nil
		}
		if err != nil {
			continue // unreadable copy: not scanned
		}
		res.ScannedLists++
		for _, h := range hits {
			h.ListID, h.Name, h.GroupIDs, h.Enabled, h.Applies = j.id, j.name, j.groups, true, applies(true, j.groups)
			if !add(h) {
				return res, nil
			}
		}
	}
	return res, nil
}

// searchList scans a cached list file for entries containing q (at most
// max hits, $badfilter cancellations of the same list applied).
func (e *Engine) searchList(ctx context.Context, path string, format listFormat, q string, max int) ([]SearchItem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type hit struct {
		item SearchItem
		key  badKey
		ip   bool // an address entry (no $badfilter)
	}
	var hits []hit
	bad := map[badKey]struct{}{}
	lp := newLineParser(format)
	allowList := format.kind == "allow"
	// Every line is parsed, also after maxMatches hits: a later $badfilter
	// line still cancels one of them.
	err = scanLines(ctx, io.LimitReader(f, e.maxBytes), func(line []byte, long bool) {
		if long || format.ips && len(hits) >= maxMatches {
			return
		}
		s := string(line)
		body := lineBody(s)
		if format.ips {
			ie, st := parseIPLine(s)
			if st != lineOK || !strings.Contains(strings.ToLower(body), q) {
				return
			}
			action := "block"
			if ie.allow || allowList {
				action = "allow"
			}
			hits = append(hits, hit{item: SearchItem{Source: "list", Entry: shownLine(body), Kind: "ip", Action: action,
				Qtypes: []string{}, Denyallow: []string{}}, ip: true})
			return
		}
		entries, st := lp.parse(s)
		if st != lineOK {
			return
		}
		for i := range entries {
			en := &entries[i]
			tier := en.tier(allowList)
			if en.badfilter {
				bad[en.badKey(tier)] = struct{}{}
				continue
			}
			if len(hits) >= maxMatches {
				continue
			}
			kind, match := "regex", false
			switch en.kind {
			case kindExact:
				kind, match = "exact", strings.Contains(en.domain, q)
			case kindSubtree:
				kind, match = "subtree", strings.Contains(en.domain, q)
			default:
				match = strings.Contains(strings.ToLower(body), q)
			}
			if !match {
				continue
			}
			action := "block"
			if tier == tierImpAllow || tier == tierAllow {
				action = "allow"
			}
			hits = append(hits, hit{item: SearchItem{Source: "list", Entry: shownLine(body), Kind: kind, Action: action,
				Qtypes: en.types.names(), QtypesNegate: en.types.negate, Denyallow: slices.Clone(nonNilStrings(en.deny))},
				key: en.badKey(tier)})
		}
	})
	if err != nil {
		return nil, err
	}
	var out []SearchItem
	for _, h := range hits {
		if _, cancelled := bad[h.key]; cancelled && !h.ip {
			continue
		}
		out = append(out, h.item)
		if len(out) >= max {
			break
		}
	}
	return out, nil
}
