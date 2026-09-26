package filter

import (
	"cmp"
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

const (
	explainTimeout        = 10 * time.Second
	maxConcurrentExplains = 2
	maxMatches            = 1000
	maxPatternShown       = 1100
)

// explained is a match with its sort key.
type explained struct {
	m     Match
	rank  int // precedence step of ARCHITECTURE 7.2 (1–11)
	pos   int // position within a step (posExact …): the order Check probes
	spec  int // subtree: length of the matched domain (more specific first)
	order int // discovery order: rules and lists by ID, lines in file order
}

// Positions within a precedence step, the order Check probes a tier: the
// plain exact and subtree entries, then the modified-entries table of list
// entries with $dnstype or $denyallow (exact and subtree rows together,
// most specific name first), then the patterns; inverted rules after the
// other user regex block rules of step 10.
const (
	posExact = iota
	posSubtree
	posModified
	posPattern
	posInverted
)

// Explain lists every source matching qname (queried with type qtype) and
// marks which applies to groups and which is decisive ("why is this
// blocked?"). An entry that matches the name but does not apply to the type
// or is excepted by its denyallow set is listed with Skipped set and never
// applies; an inverted rule is listed when its expression does not match
// the name. Lists of answer addresses hold no names and are not scanned.
func (e *Engine) Explain(ctx context.Context, qname string, qtype uint16, groups []int64) ([]Match, error) {
	q := normalizeName(qname)
	if !validDomain(q) {
		return nil, apperr.Invalid("domain", "must be a valid domain name")
	}
	ctx, cancel := context.WithTimeout(ctx, explainTimeout)
	defer cancel()
	select {
	case e.explain <- struct{}{}:
		defer func() { <-e.explain }()
	case <-ctx.Done():
		return nil, apperr.Unavailable("too many explain requests, try again")
	}

	var sf suffixHashes
	sf.compute(q)
	var found []explained
	add := func(m Match, pos, spec int) {
		if len(found) >= maxMatches {
			return
		}
		found = append(found, explained{m: m, rank: matchRank(m), pos: pos, spec: spec, order: len(found)})
	}
	for _, r := range e.snap.Load().rules.rules {
		spec, pos := 0, posOf(r.typ)
		switch {
		case r.typ == "exact":
			if r.pattern != q {
				continue
			}
		case r.typ == "subtree":
			if !subtreeMatch(q, r.pattern) {
				continue
			}
			spec = len(r.pattern)
		case r.invert:
			if r.re == nil || r.re.MatchString(q) {
				continue
			}
			pos = posInverted
		default:
			if r.re == nil || !r.re.MatchString(q) {
				continue
			}
		}
		m := Match{
			Action: r.action.String(), Source: "rule", Kind: r.typ, RuleID: r.id, Name: r.pattern,
			Pattern: r.pattern, GroupIDs: slices.Clone(nonNil(r.groups)), Qtypes: r.types.names(),
			QtypesNegate: r.types.negate, Denyallow: slices.Clone(nonNilStrings(r.denyall)), Invert: r.invert, Reply: r.reply,
		}
		switch {
		case !r.types.admits(qtype):
			m.Skipped = SkippedQtype
		case excepted(r.deny, &sf):
			m.Skipped = SkippedDenyallow
		}
		add(m, pos, spec)
	}

	type job struct {
		id       int64
		name     string
		category string
		format   listFormat
		groups   []int64
	}
	var jobs []job
	e.mu.Lock()
	for _, rt := range sortedLists(e.lists) {
		if rt.Enabled && rt.parsed != nil && !rt.format().ips {
			jobs = append(jobs, job{rt.ID, rt.Name, rt.Category, rt.format(), slices.Clone(nonNil(rt.GroupIDs))})
		}
	}
	e.mu.Unlock()
	for _, j := range jobs {
		err := e.scanList(ctx, e.cachePath(j.id), j.format, q, func(en *entry, line string) {
			line = shownLine(line)
			tier := en.tier(j.format.kind == "allow")
			kind := "regex"
			switch en.kind {
			case kindExact:
				kind = "exact"
			case kindSubtree:
				kind = "subtree"
			}
			m := Match{
				Action: "block", Source: "list", Kind: kind, ListID: j.id, Name: j.name, Pattern: line,
				Important: tier < tierAllow, GroupIDs: slices.Clone(j.groups), Category: j.category,
				Qtypes: en.types.names(), QtypesNegate: en.types.negate, Denyallow: slices.Clone(nonNilStrings(en.deny)),
			}
			if tier == tierImpAllow || tier == tierAllow {
				m.Action = "allow"
			}
			switch {
			case !en.types.admits(qtype):
				m.Skipped = SkippedQtype
			case excepted(denyHashes(en.deny), &sf):
				m.Skipped = SkippedDenyallow
			}
			pos := posOf(kind)
			if en.kind != kindPattern && en.modified() {
				pos = posModified
			}
			add(m, pos, len(en.domain))
		})
		if ctx.Err() != nil {
			return nil, apperr.Unavailable("explain timed out after %s", explainTimeout)
		}
		if err != nil {
			e.log.Warn("explain: cannot scan list", slog.Int64("id", j.id), slog.Any("err", err))
		}
	}

	slices.SortFunc(found, func(a, b explained) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.pos, b.pos),
			cmp.Compare(b.spec, a.spec), cmp.Compare(a.order, b.order))
	})
	out := make([]Match, len(found))
	decided := false
	for i, f := range found {
		f.m.Applies = f.m.Skipped == "" && sharesGroup(f.m.GroupIDs, groups)
		if f.m.Applies && !decided {
			f.m.Decisive, decided = true, true
		}
		out[i] = f.m
	}
	return out, nil
}

// Values of Match.Skipped.
const (
	SkippedQtype     = "qtype"
	SkippedDenyallow = "denyallow"
)

// scanList rescans a cached list file and calls fn for every entry matching
// q that is not cancelled by a $badfilter rule of the same list.
func (e *Engine) scanList(ctx context.Context, path string, format listFormat, q string, fn func(en *entry, line string)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	type hit struct {
		en   entry
		line string
		key  badKey
	}
	var hits []hit
	bad := map[badKey]struct{}{}
	lp := newLineParser(format)
	allowList := format.kind == "allow"
	err = scanLines(ctx, io.LimitReader(f, e.maxBytes), func(line []byte, long bool) {
		if long {
			return
		}
		s := string(line)
		entries, st := lp.parse(s)
		if st != lineOK {
			return
		}
		for i := range entries {
			en := entries[i]
			switch {
			case en.badfilter:
				bad[en.badKey(en.tier(allowList))] = struct{}{}
			case len(hits) < maxMatches && entryMatches(&en, q):
				hits = append(hits, hit{en, strings.TrimSpace(s), en.badKey(en.tier(allowList))})
			}
		}
	})
	if err != nil {
		return err
	}
	for i := range hits {
		if _, cancelled := bad[hits[i].key]; !cancelled {
			fn(&hits[i].en, hits[i].line)
		}
	}
	return nil
}

// lineBody returns the part of a list line the parser reads: trimmed,
// without its inline " #" comment.
func lineBody(s string) string {
	s = strings.TrimSpace(s)
	if i := inlineComment(s); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// shownLine returns a list line as Explain and Search show it: its body
// (lineBody), valid UTF-8 (a list is third-party text; an invalid byte would
// make the JSON encoder fail after the status was sent), at most
// maxPatternShown bytes cut at a rune boundary.
func shownLine(s string) string {
	s = strings.ToValidUTF8(lineBody(s), "")
	if len(s) > maxPatternShown {
		cut := maxPatternShown
		for cut > 0 && s[cut]&0xc0 == 0x80 {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// entryMatches reports whether a parsed list entry matches q.
func entryMatches(en *entry, q string) bool {
	switch en.kind {
	case kindExact:
		return en.domain == q
	case kindSubtree:
		return subtreeMatch(q, en.domain)
	}
	if !strings.Contains(q, en.lit) {
		return false
	}
	re, err := en.compile()
	return err == nil && re.MatchString(q)
}

// matchRank returns the precedence step (ARCHITECTURE 7.2) of a match.
func matchRank(m Match) int {
	allow := m.Action == "allow"
	if m.Source == "rule" {
		switch {
		case allow && m.Kind == "exact":
			return 1
		case allow && m.Kind == "subtree":
			return 2
		case allow:
			return 3
		case m.Kind == "exact":
			return 4
		case m.Kind == "subtree":
			return 5
		}
		return 10
	}
	switch {
	case m.Important && allow:
		return 6
	case m.Important:
		return 7
	case allow:
		return 8
	case m.Kind != "regex":
		return 9
	}
	return 11
}

// posOf returns the position of a plain entry or rule of a type within its
// precedence step.
func posOf(typ string) int {
	switch typ {
	case "exact":
		return posExact
	case "subtree":
		return posSubtree
	}
	return posPattern
}

// sharesGroup reports whether two group ID sets intersect.
func sharesGroup(a, b []int64) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
