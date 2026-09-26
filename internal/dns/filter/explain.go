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
	kind  int // exact < subtree < regex within a step (the order Check probes)
	spec  int // subtree: length of the matched domain (more specific first)
	order int // discovery order: rules and lists by ID, lines in file order
}

// Explain lists every source matching qname and marks which applies to groups
// and which is decisive ("why is this blocked?").
func (e *Engine) Explain(ctx context.Context, qname string, groups []int64) ([]Match, error) {
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

	var found []explained
	add := func(m Match, kind, spec int) {
		if len(found) >= maxMatches {
			return
		}
		found = append(found, explained{m: m, rank: matchRank(m), kind: kind, spec: spec, order: len(found)})
	}
	for _, r := range e.snap.Load().rules.rules {
		spec := 0
		switch r.typ {
		case "exact":
			if r.pattern != q {
				continue
			}
		case "subtree":
			if !subtreeMatch(q, r.pattern) {
				continue
			}
			spec = len(r.pattern)
		default:
			if r.re == nil || !r.re.MatchString(q) {
				continue
			}
		}
		add(Match{
			Action: r.action.String(), Source: "rule", Kind: r.typ, RuleID: r.id, Name: r.pattern,
			Pattern: r.pattern, GroupIDs: slices.Clone(nonNil(r.groups)),
		}, kindIndex(r.typ), spec)
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
		if rt.Enabled && rt.parsed != nil {
			jobs = append(jobs, job{rt.ID, rt.Name, rt.format(), slices.Clone(nonNil(rt.GroupIDs))})
		}
	}
	e.mu.Unlock()
	for _, j := range jobs {
		err := e.scanList(ctx, e.cachePath(j.id), j.format, q, func(en *entry, line string) {
			if len(line) > maxPatternShown {
				line = line[:maxPatternShown]
			}
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
				Important: tier < tierAllow, GroupIDs: slices.Clone(j.groups),
			}
			if tier == tierImpAllow || tier == tierAllow {
				m.Action = "allow"
			}
			add(m, int(en.kind), len(en.domain))
		})
		if ctx.Err() != nil {
			return nil, apperr.Unavailable("explain timed out after %s", explainTimeout)
		}
		if err != nil {
			e.log.Warn("explain: cannot scan list", slog.Int64("id", j.id), slog.Any("err", err))
		}
	}

	slices.SortFunc(found, func(a, b explained) int {
		return cmp.Or(cmp.Compare(a.rank, b.rank), cmp.Compare(a.kind, b.kind),
			cmp.Compare(b.spec, a.spec), cmp.Compare(a.order, b.order))
	})
	out := make([]Match, len(found))
	decided := false
	for i, f := range found {
		f.m.Applies = sharesGroup(f.m.GroupIDs, groups)
		if f.m.Applies && !decided {
			f.m.Decisive, decided = true, true
		}
		out[i] = f.m
	}
	return out, nil
}

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

func kindIndex(typ string) int {
	switch typ {
	case "exact":
		return kindExact
	case "subtree":
		return kindSubtree
	}
	return kindPattern
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
