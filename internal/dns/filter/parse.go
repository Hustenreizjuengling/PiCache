package filter

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/netip"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxListBytes     = 256 << 20 // largest accepted list file
	maxPatterns      = 20000     // compiled patterns (regex + wildcard), per list and in total
	maxRegexLen      = 1024      // characters of one regular expression
	maxRegexCost     = 4096      // estimated compiled size of one pattern (see regexCost)
	maxPatternCost   = 1 << 20   // estimated compiled size of all patterns, per list and in total
	maxLineLen       = 64 << 10  // longer lines are counted as invalid
	ctxCheckInterval = 1 << 14   // lines between context checks while parsing
)

var (
	errHTML       = errors.New("not a filter list: the server returned an HTML page")
	errBinary     = errors.New("not a filter list: the content is binary (control characters)")
	errTooComplex = errors.New("the pattern is too complex (its repetitions expand to a very large program)")
)

// Precedence tiers of list entries (ARCHITECTURE 7.2 steps 6–9 and 11).
const (
	tierImpAllow = iota // @@…$important
	tierImpBlock        // …$important
	tierAllow           // @@…, allow lists
	tierBlock           // block entries
	numTiers
)

// Entry kinds.
const (
	kindExact = iota
	kindSubtree
	kindPattern
)

// entry is one rule produced by a list line.
type entry struct {
	kind      uint8
	allow     bool
	important bool
	badfilter bool
	domain    string // kindExact, kindSubtree
	re        string // kindPattern: RE2 source (without the case-folding flag)
	lit       string // kindPattern: a literal every match contains ("" if unknown)
	regex     bool   // kindPattern written as /re/ (compiled case-insensitively)
}

// tier returns the precedence tier of e in a list of the given kind.
func (e *entry) tier(allowList bool) int {
	allow := e.allow || allowList
	switch {
	case e.important && allow:
		return tierImpAllow
	case e.important:
		return tierImpBlock
	case allow:
		return tierAllow
	}
	return tierBlock
}

// compile compiles a pattern entry (errTooComplex if it exceeds maxRegexCost).
func (e *entry) compile() (*regexp.Regexp, error) {
	re, _, err := compileRegex(e.source(), maxRegexCost)
	return re, err
}

// compileRegex compiles src (RE2, Perl flags) and returns its estimated
// cost (see regexCost). It refuses with errTooComplex, before compiling,
// a pattern that costs more than min(maxCost, maxRegexCost).
func compileRegex(src string, maxCost int64) (*regexp.Regexp, int64, error) {
	re, err := syntax.Parse(src, syntax.Perl)
	if err != nil {
		return nil, 0, err
	}
	cost := regexCost(re)
	if cost > min(maxCost, maxRegexCost) {
		return nil, cost, errTooComplex
	}
	rx, err := regexp.Compile(src)
	return rx, cost, err
}

// regexCost estimates the size of the compiled program of the parsed (not
// simplified) re in instructions, plus one per four character-class ranges
// (a one-pass program copies the ranges into every instruction). It mirrors
// regexp/syntax's own size estimate and walks the parse tree only, so a
// pattern such as "[^.]{999}[^.]{999}…" is measured, and refused, without
// expanding its repetitions: the length limit alone would admit programs of
// ~100 000 instructions (megabytes each) in 1024 characters.
func regexCost(re *syntax.Regexp) int64 {
	var n int64
	switch re.Op {
	case syntax.OpLiteral:
		n = int64(len(re.Rune))
	case syntax.OpCharClass:
		n = 1 + int64(len(re.Rune)/8)
	case syntax.OpCapture, syntax.OpStar:
		n = 2 + regexCost(re.Sub[0])
	case syntax.OpPlus, syntax.OpQuest:
		n = 1 + regexCost(re.Sub[0])
	case syntax.OpConcat, syntax.OpAlternate:
		for _, sub := range re.Sub {
			n += regexCost(sub)
		}
		if re.Op == syntax.OpAlternate {
			n += int64(len(re.Sub)) - 1
		}
	case syntax.OpRepeat:
		sub := regexCost(re.Sub[0])
		switch {
		case re.Max == -1 && re.Min == 0: // x*
			n = 2 + sub
		case re.Max == -1: // x{n,} = xxx+
			n = 1 + int64(re.Min)*sub
		default: // x{2,5} = xx(x(x(x)?)?)?
			n = int64(re.Max)*sub + int64(re.Max-re.Min)
		}
	}
	return max(1, n)
}

// lineStatus classifies a parsed line.
type lineStatus uint8

const (
	lineSkip        lineStatus = iota // empty, comment, cosmetic rule, junk host
	lineOK                            // produced entries
	lineInvalid                       // malformed
	lineUnsupported                   // valid syntax PiCache does not support
)

// lineParser parses single list lines. It reuses its entry buffer, so the
// entries returned by parse are valid until the next call.
type lineParser struct {
	plainSubtree bool
	buf          []entry
}

// parse parses one line (without line terminator).
func (p *lineParser) parse(line string) ([]entry, lineStatus) {
	p.buf = p.buf[:0]
	s := strings.TrimSpace(line)
	if s == "" {
		return nil, lineSkip
	}
	switch s[0] {
	case '!', '#', ';', '[':
		return nil, lineSkip
	}
	if isCosmetic(s) {
		return nil, lineSkip
	}
	if i := inlineComment(s); i >= 0 {
		if s = strings.TrimSpace(s[:i]); s == "" {
			return nil, lineSkip
		}
	}
	if !utf8.ValidString(s) || hasNonASCII(s) && !isRegexLine(s) {
		return nil, lineInvalid // domains must be A-labels
	}
	if isRegexLine(s) {
		return p.parseRegex(s)
	}
	s = strings.ToLower(s)
	if i := strings.IndexAny(s, " \t"); i > 0 {
		return p.parseHosts(s[:i], s[i+1:])
	}
	if _, err := netip.ParseAddr(s); err == nil {
		return nil, lineInvalid // an address without host names
	}
	return p.parseABP(s)
}

// parseHosts parses "IP host [host…]".
func (p *lineParser) parseHosts(first, rest string) ([]entry, lineStatus) {
	if _, err := netip.ParseAddr(first); err != nil {
		return nil, lineInvalid
	}
	invalid := false
	for tok := range strings.FieldsSeq(rest) {
		d := strings.TrimSuffix(tok, ".")
		if _, junk := junkHosts[d]; junk {
			continue
		}
		if !validDomain(d) || !strings.Contains(d, ".") {
			invalid = true
			continue
		}
		p.buf = append(p.buf, entry{kind: kindExact, domain: d})
	}
	switch {
	case len(p.buf) > 0:
		return p.buf, lineOK
	case invalid || strings.TrimSpace(rest) == "":
		return nil, lineInvalid
	}
	return nil, lineSkip // only junk host names
}

// parseRegex parses "[@@]/re/[$options]".
func (p *lineParser) parseRegex(s string) ([]entry, lineStatus) {
	e := entry{kind: kindPattern, regex: true}
	if strings.HasPrefix(s, "@@") {
		e.allow = true
		s = s[2:]
	}
	end := strings.LastIndexByte(s, '/')
	if end < 1 {
		return nil, lineInvalid
	}
	if end != len(s)-1 {
		if s[end+1] != '$' {
			return nil, lineUnsupported // a URL path rule such as /banner/ad.gif
		}
		if st := e.parseOptions(strings.ToLower(s[end+2:])); st != lineOK {
			return nil, st
		}
	}
	e.re = s[1:end]
	switch {
	case e.re == "":
		return nil, lineInvalid
	case len(e.re) > maxRegexLen:
		return nil, lineUnsupported
	}
	re, err := syntax.Parse("(?i)"+e.re, syntax.Perl)
	if err != nil {
		return nil, lineInvalid
	}
	if regexCost(re) > maxRegexCost {
		return nil, lineUnsupported // before Simplify, which expands repetitions
	}
	e.lit = requiredLiteral(re.Simplify())
	p.buf = append(p.buf, e)
	return p.buf, lineOK
}

// parseOptions applies "$important,badfilter"; any other option makes the
// rule unsupported (AdGuard Home semantics).
func (e *entry) parseOptions(opts string) lineStatus {
	if opts == "" {
		return lineInvalid
	}
	for o := range strings.SplitSeq(opts, ",") {
		switch strings.TrimSpace(o) {
		case "important":
			e.important = true
		case "badfilter":
			e.badfilter = true
		default:
			return lineUnsupported
		}
	}
	return lineOK
}

// parseABP parses plain domains, "*.d" and Adblock-style rules (lower-case).
func (p *lineParser) parseABP(s string) ([]entry, lineStatus) {
	var e entry
	if strings.HasPrefix(s, "@@") {
		e.allow = true
		s = s[2:]
	}
	if i := strings.LastIndexByte(s, '$'); i >= 0 {
		if st := e.parseOptions(s[i+1:]); st != lineOK {
			return nil, st
		}
		s = s[:i]
	}
	abp := e.allow || e.important || e.badfilter
	switch {
	case strings.HasPrefix(s, "||"):
		if d, ok := strings.CutSuffix(strings.TrimSuffix(s[2:], "|"), "^"); ok && validDomain(d) {
			e.kind, e.domain = kindSubtree, d
			return p.add(e)
		}
	case strings.HasPrefix(s, "|"):
		body, anchored := strings.CutSuffix(s[1:], "|")
		d, sep := strings.CutSuffix(body, "^")
		if (anchored || sep) && validDomain(d) {
			e.kind, e.domain = kindExact, d
			return p.add(e)
		}
	case strings.HasPrefix(s, "*.") && validDomain(s[2:]):
		e.kind, e.domain = kindSubtree, s[2:]
		return p.add(e)
	case !strings.ContainsAny(s, "*^|:/"):
		d := strings.TrimSuffix(s, ".")
		if _, junk := junkHosts[d]; junk && !abp {
			return nil, lineSkip
		}
		if !validDomain(d) || !strings.Contains(d, ".") {
			return nil, lineInvalid
		}
		e.kind, e.domain = kindExact, d
		if p.plainSubtree {
			e.kind = kindSubtree
		}
		return p.add(e)
	}
	re, lit, st := abpToRegex(s)
	if st != lineOK {
		return nil, st
	}
	e.kind, e.re, e.lit = kindPattern, re, lit
	return p.add(e)
}

func (p *lineParser) add(e entry) ([]entry, lineStatus) {
	p.buf = append(p.buf, e)
	return p.buf, lineOK
}

// abpToRegex converts an Adblock-style host pattern to RE2:
// "||" → ^(?:[^.]+\.)*, leading "|" or "://" → ^, "^" or trailing "|" → $,
// "*" → .*; an unanchored side matches a substring. It also returns the
// longest literal fragment (for the matcher's pre-check).
func abpToRegex(s string) (re, lit string, st lineStatus) {
	var b strings.Builder
	switch {
	case strings.HasPrefix(s, "||"):
		b.WriteString(`^(?:[^.]+\.)*`)
		s = s[2:]
	case strings.HasPrefix(s, "://"):
		b.WriteByte('^')
		s = s[3:]
	case strings.HasPrefix(s, "|"):
		b.WriteByte('^')
		s = s[1:]
	}
	anchorEnd := false
	if t, ok := strings.CutSuffix(s, "|"); ok {
		s, anchorEnd = t, true
	}
	if t, ok := strings.CutSuffix(s, "^"); ok {
		s, anchorEnd = t, true
	}
	alnum := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			alnum = true
		case c == '.', c == '-', c == '_', c == '*':
		case c == '^', c == '|':
			return "", "", lineInvalid // separator or anchor inside the pattern
		default:
			return "", "", lineUnsupported // URL rules (paths, ports, query strings)
		}
	}
	if !alnum {
		return "", "", lineInvalid // matches (almost) everything
	}
	for i, frag := range strings.Split(s, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(frag))
		if len(frag) > len(lit) {
			lit = frag
		}
	}
	if anchorEnd {
		b.WriteByte('$')
	}
	return b.String(), lit, lineOK
}

// requiredLiteral returns the longest literal string that every match of re
// must contain (lower-case), or "" if none is found.
func requiredLiteral(re *syntax.Regexp) string {
	switch re.Op {
	case syntax.OpLiteral:
		return strings.ToLower(string(re.Rune))
	case syntax.OpCapture:
		return requiredLiteral(re.Sub[0])
	case syntax.OpConcat:
		best := ""
		for _, sub := range re.Sub {
			if l := requiredLiteral(sub); len(l) > len(best) {
				best = l
			}
		}
		return best
	case syntax.OpPlus, syntax.OpRepeat:
		if re.Op == syntax.OpRepeat && re.Min < 1 {
			return ""
		}
		return requiredLiteral(re.Sub[0])
	}
	return ""
}

// isCosmetic reports whether s is a cosmetic (element hiding / scriptlet)
// rule, which DNS filtering ignores.
func isCosmetic(s string) bool {
	for _, m := range [...]string{"##", "#@#", "#$#", "#?#", "#%#"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// inlineComment returns the index of an inline " #…" comment, or -1.
func inlineComment(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return i
		}
	}
	return -1
}

// isRegexLine reports whether s is a "/re/" or "@@/re/" rule.
func isRegexLine(s string) bool {
	s = strings.TrimPrefix(s, "@@")
	return len(s) >= 2 && s[0] == '/' && strings.LastIndexByte(s, '/') > 0
}

func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

// parsedPattern is a compiled pattern entry of a list.
type parsedPattern struct {
	re   *regexp.Regexp
	lit  string
	cost int32 // estimated compiled size (regexCost)
	tier uint8
}

// parsed is the parse result of one list file. It is kept in memory so the
// matcher can be rebuilt without re-reading files.
type parsed struct {
	kind, plain string                // list configuration it was parsed with
	sets        [numTiers][2][]uint64 // [tier][exact|subtree]: sorted, unique hashes
	pats        []parsedPattern       // in file order, unique per tier
	entries     int                   // unique entries (domains + patterns)
	invalid     int                   // malformed lines
	unsupported int                   // unsupported rules (modifiers, URL rules, pattern caps)
}

// memory returns the approximate heap size of p in bytes.
func (p *parsed) memory() int64 {
	var n int64
	for t := range p.sets {
		for k := range p.sets[t] {
			n += int64(cap(p.sets[t][k])) * 8
		}
	}
	return n + int64(len(p.pats))*patternMemEstimate
}

// scanLines calls fn for every line of r (without terminator). It enforces
// the content rules: an optional UTF-8 BOM is stripped; control bytes other
// than tab, CR and LF reject the list (errBinary); an HTML page (first
// non-empty line starts with "<html" or "<!doctype") is rejected (errHTML);
// lines longer than maxLineLen are reported with long=true (and no content).
// The caller bounds the size of r.
func scanLines(ctx context.Context, r io.Reader, fn func(line []byte, long bool)) error {
	br := bufio.NewReaderSize(r, maxLineLen)
	first, sawContent := true, false
	for n := 0; ; n++ {
		if n%ctxCheckInterval == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		line, err := br.ReadSlice('\n')
		if first {
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
			first = false
		}
		if hasControl(line) {
			return errBinary
		}
		if !sawContent {
			if t := bytes.TrimSpace(line); len(t) > 0 {
				sawContent = true
				if hasPrefixFold(t, "<html") || hasPrefixFold(t, "<!doctype") {
					return errHTML
				}
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// line is only valid until the next read: report it as too long
			// and skip the rest of it.
			fn(nil, true)
			for errors.Is(err, bufio.ErrBufferFull) {
				var more []byte
				if more, err = br.ReadSlice('\n'); hasControl(more) {
					return errBinary
				}
			}
		} else if line = bytes.TrimRight(line, "\r\n"); len(line) > 0 {
			fn(line, false)
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

func hasControl(b []byte) bool {
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\r' && c != '\n' || c == 0x7f {
			return true
		}
	}
	return false
}

func hasPrefixFold(b []byte, prefix string) bool {
	return len(b) >= len(prefix) && strings.EqualFold(string(b[:len(prefix)]), prefix)
}

// badKey identifies an entry cancelled by a $badfilter rule.
type badKey struct {
	tier, kind uint8
	hash       uint64 // domain entries
	re         string // patterns
}

// parseList parses a list file. kind is the list kind (block|allow), plain
// the plainDomains flag (exact|subtree).
func parseList(ctx context.Context, r io.Reader, kind, plain string) (*parsed, error) {
	res := &parsed{kind: kind, plain: plain}
	lp := lineParser{plainSubtree: plain == "subtree"}
	allowList := kind == "allow"
	var bad map[badKey]struct{}
	seenPats := map[string]struct{}{} // tier + source
	var cost int64                    // of the compiled patterns (≤ maxPatternCost)
	err := scanLines(ctx, r, func(line []byte, long bool) {
		if long {
			res.invalid++
			return
		}
		entries, st := lp.parse(string(line))
		switch st {
		case lineInvalid:
			res.invalid++
			return
		case lineUnsupported:
			res.unsupported++
			return
		case lineSkip:
			return
		}
		for i := range entries {
			e := &entries[i]
			tier := e.tier(allowList)
			if e.badfilter {
				if bad == nil {
					bad = map[badKey]struct{}{}
				}
				bad[e.badKey(tier)] = struct{}{}
				continue
			}
			if e.kind != kindPattern {
				res.sets[tier][e.kind] = append(res.sets[tier][e.kind], hashName(e.domain))
				continue
			}
			key := string(rune('0'+tier)) + e.source()
			if _, dup := seenPats[key]; dup {
				continue
			}
			if len(res.pats) >= maxPatterns {
				res.unsupported++
				continue
			}
			re, c, err := compileRegex(e.source(), maxPatternCost-cost)
			switch {
			case errors.Is(err, errTooComplex):
				res.unsupported++ // too large alone, or beyond the list's budget
				continue
			case err != nil:
				res.invalid++
				continue
			}
			cost += c
			seenPats[key] = struct{}{}
			res.pats = append(res.pats, parsedPattern{re: re, lit: e.lit, cost: int32(c), tier: uint8(tier)})
		}
	})
	if err != nil {
		return nil, err
	}
	for t := range res.sets {
		for k := range res.sets[t] {
			hs := res.sets[t][k]
			slices.Sort(hs)
			hs = slices.Compact(hs)
			if len(bad) > 0 {
				hs = slices.DeleteFunc(hs, func(h uint64) bool {
					_, cancelled := bad[badKey{tier: uint8(t), kind: uint8(k), hash: h}]
					return cancelled
				})
			}
			res.sets[t][k] = slices.Clip(hs)
			res.entries += len(hs)
		}
	}
	if len(bad) > 0 {
		res.pats = slices.DeleteFunc(res.pats, func(p parsedPattern) bool {
			_, cancelled := bad[badKey{tier: p.tier, kind: kindPattern, re: patternSource(p)}]
			return cancelled
		})
	}
	res.pats = slices.Clip(res.pats)
	res.entries += len(res.pats)
	return res, nil
}

// badKey returns the key of the rule a $badfilter entry cancels.
func (e *entry) badKey(tier int) badKey {
	k := badKey{tier: uint8(tier), kind: e.kind}
	if e.kind == kindPattern {
		k.re = e.source()
	} else {
		k.hash = hashName(e.domain)
	}
	return k
}

// source returns the regexp source a pattern entry compiles to.
func (e *entry) source() string {
	if e.regex {
		return "(?i)" + e.re
	}
	return e.re
}

// patternSource returns the source of a compiled list pattern (see entry.source).
func patternSource(p parsedPattern) string { return p.re.String() }
