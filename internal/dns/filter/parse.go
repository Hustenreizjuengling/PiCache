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

	"golang.org/x/net/publicsuffix"

	"github.com/hustenreizjuengling/picache/internal/settings"
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
	// types ($dnstype) and deny ($denyallow: domains, sorted and unique)
	// restrict an exact or subtree entry (a modified entry).
	types typeSet
	deny  []string
}

// modified reports whether the entry has $dnstype or $denyallow.
func (e *entry) modified() bool { return !e.types.empty() || len(e.deny) > 0 }

// modKey is the canonical form of the modifiers ("" for none): the part of
// the $badfilter key that tells modified entries apart.
func (e *entry) modKey() string {
	if !e.modified() {
		return ""
	}
	return e.types.key() + ";" + strings.Join(e.deny, "|")
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
	lineBroad                         // a block of a whole TLD that the TLD guard refuses (counted as invalid)
)

// listFormat is the list configuration a parse result depends on.
type listFormat struct {
	kind, plain string // block | allow, exact | subtree
	// tldGuard counts subtree, wildcard and pattern blocks of a single
	// label or of an ICANN public suffix ("||com^", "*.co.uk", a plain
	// "co.uk" in subtree mode, "||*.com^", "/\.xyz$/"; broadDomain,
	// broadPattern) as invalid, so one broken or hostile list cannot block
	// a whole TLD. Every category but abused-tlds has it.
	tldGuard bool
	// ips: a list of answer addresses (format ips, parseIPLine) instead
	// of domain names.
	ips bool
}

// formatOf returns the parse format of a list.
func formatOf(kind, plain, category, format string) listFormat {
	return listFormat{kind: kind, plain: plain, tldGuard: category != CategoryAbusedTLDs, ips: format == FormatIPs}
}

// lineParser parses single list lines. It reuses its entry buffer, so the
// entries returned by parse are valid until the next call.
type lineParser struct {
	plainSubtree bool
	tldGuard     bool
	allowList    bool // every entry is an exception ($denyallow is unsupported)
	buf          []entry
}

func newLineParser(f listFormat) lineParser {
	return lineParser{plainSubtree: f.plain == "subtree", tldGuard: f.tldGuard, allowList: f.kind == "allow"}
}

// broadDomain reports whether a subtree block of d would cover a whole TLD
// or public suffix: d is a single label or an ICANN public suffix
// (golang.org/x/net/publicsuffix; private suffixes such as github.io are
// ordinary domains here).
func broadDomain(d string) bool {
	if !strings.Contains(d, ".") {
		return true
	}
	ps, icann := publicsuffix.PublicSuffix(d)
	return icann && ps == d
}

// probeLabel is a made-up label: a pattern that matches a name made of it
// directly below a public suffix blocks (about) every name there.
const probeLabel = "zz9probe"

// probeSuffixes are probed for every block pattern, besides the ICANN
// public suffixes its literals name, so a pattern without such a literal
// (/./, /^[a-z0-9-]+\.[a-z]+$/) is caught for the largest TLDs too.
var probeSuffixes = []string{"com", "net", "org", "de", "co.uk"}

// broadPattern reports whether the pattern entry e would block a whole TLD
// or ICANN public suffix: it matches "zz9probe.<suffix>" or
// "www.zz9probe.<suffix>" for one of probeSuffixes or for the ICANN public
// suffix that a literal of the pattern names ("*.co.uk^", "||*.com^",
// ".xyz^", /\.(top|xyz)$/, "||app*^"). The pattern is compiled only when a
// probe name contains its required literal. A pattern that does not
// compile is left to parseList, which refuses it anyway.
func broadPattern(e *entry) bool {
	tree, err := syntax.Parse(e.source(), syntax.Perl)
	if err != nil || regexCost(tree) > maxRegexCost {
		return false
	}
	var re *regexp.Regexp
	compiled := false
	matches := func(suffix string) bool {
		for _, name := range [...]string{probeLabel + "." + suffix, "www." + probeLabel + "." + suffix} {
			if !strings.Contains(name, e.lit) {
				continue // every match contains the literal
			}
			if !compiled {
				compiled = true
				re, _ = regexp.Compile(e.source())
			}
			if re != nil && re.MatchString(name) {
				return true
			}
		}
		return false
	}
	for _, s := range probeSuffixes {
		if matches(s) {
			return true
		}
	}
	return anyLiteral(tree, func(lit string) bool {
		s := icannSuffix(lit)
		return s != "" && matches(s)
	})
}

// anyLiteral reports whether fn holds for a literal of re (lower-case).
func anyLiteral(re *syntax.Regexp, fn func(string) bool) bool {
	if re.Op == syntax.OpLiteral {
		return fn(strings.ToLower(string(re.Rune)))
	}
	for _, sub := range re.Sub {
		if anyLiteral(sub, fn) {
			return true
		}
	}
	return false
}

// icannSuffix returns the ICANN public suffix of the name a pattern literal
// ends with, "" if there is none (".co.uk" → "co.uk", "ads.example.com" →
// "com", "doubleclick" → "").
func icannSuffix(lit string) string {
	d := strings.Trim(lit, ".")
	if d == "" {
		return ""
	}
	if ps, icann := publicsuffix.PublicSuffix(d); icann {
		return ps
	}
	return ""
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
	return p.add(e)
}

// parseOptions applies "$important", "$badfilter", "$dnstype=…" and
// "$denyallow=…" (lower-case); any other option makes the rule unsupported
// (it would restrict the rule to requests a DNS filter never sees, so
// applying it to every query would over-block). Where the modifiers may
// be used is decided by the caller (add).
func (e *entry) parseOptions(opts string) lineStatus {
	if opts == "" {
		return lineInvalid
	}
	seenTypes, seenDeny := false, false
	for o := range strings.SplitSeq(opts, ",") {
		name, value, hasValue := strings.Cut(strings.TrimSpace(o), "=")
		switch {
		case name == "important" && !hasValue:
			e.important = true
		case name == "badfilter" && !hasValue:
			e.badfilter = true
		case name == "dnstype" && hasValue:
			if seenTypes {
				return lineInvalid
			}
			seenTypes = true
			types, negate, st := parseDNSType(value)
			if st != lineOK {
				return st
			}
			e.types = newTypeSet(types, negate)
		case name == "denyallow" && hasValue:
			if seenDeny {
				return lineInvalid
			}
			seenDeny = true
			deny, st := parseDenyallow(value)
			if st != lineOK {
				return st
			}
			e.deny = deny
		default:
			return lineUnsupported
		}
	}
	return lineOK
}

// parseDNSType parses the value of $dnstype: "A|AAAA" or "~A|~AAAA" (a "~"
// on every value negates the set). Mixing "~" and plain values, an unknown
// type or an empty value is invalid; more than maxEntryTypes types are
// unsupported.
func parseDNSType(v string) ([]uint16, bool, lineStatus) {
	var types []uint16
	negated := 0
	parts := strings.Split(v, "|")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, "~"); ok {
			negated++
			part = rest
		}
		if part == "" {
			return nil, false, lineInvalid
		}
		t, err := settings.ParseQType(part)
		if err != nil {
			return nil, false, lineInvalid
		}
		if !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	if negated != 0 && negated != len(parts) {
		return nil, false, lineInvalid
	}
	if len(types) > maxEntryTypes {
		return nil, false, lineUnsupported
	}
	return types, negated > 0, lineOK
}

// parseDenyallow parses the value of $denyallow: domains separated by "|"
// (A-labels). An invalid domain is invalid, more than maxDenyallow domains
// unsupported. It returns them sorted and unique.
func parseDenyallow(v string) ([]string, lineStatus) {
	var out []string
	for part := range strings.SplitSeq(v, "|") {
		d := normalizeName(part)
		if !validDomain(d) {
			return nil, lineInvalid
		}
		out = append(out, d)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxDenyallow {
		return nil, lineUnsupported
	}
	return out, lineOK
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
	abp := e.allow || e.important || e.badfilter || e.modified()
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

// add appends e unless the TLD guard refuses it: a subtree or pattern
// block (not an @@ or $badfilter entry) that covers a whole TLD or ICANN
// public suffix, judged without its denyallow set ("||com^$denyallow=x.com"
// stays refused). $dnstype and $denyallow are supported on exact and
// subtree entries, $denyallow only on subtree blocks; a pattern with
// either modifier is unsupported.
func (p *lineParser) add(e entry) ([]entry, lineStatus) {
	if p.tldGuard && !e.allow && !e.badfilter &&
		(e.kind == kindSubtree && broadDomain(e.domain) || e.kind == kindPattern && broadPattern(&e)) {
		return nil, lineBroad
	}
	if e.modified() && (e.kind == kindPattern || len(e.deny) > 0 && (e.allow || p.allowList || e.kind != kindSubtree)) {
		return nil, lineUnsupported
	}
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
	format listFormat            // list configuration it was parsed with
	sets   [numTiers][2][]uint64 // [tier][exact|subtree]: sorted, unique hashes
	mods   [numTiers]modTable    // modified entries ($dnstype, $denyallow), sorted by hash; src unset
	pats   []parsedPattern       // in file order, unique per tier
	// ips and ipBits hold the address entries of a list of format ips:
	// [allow|block][IPv4|IPv6] sorted, unique network keys (ipKey) and the
	// prefix lengths present, longest first.
	ips         [numIPTiers][2][]uint64
	ipBits      [numIPTiers][2][]uint8
	entries     int // unique entries (domains, modified entries, patterns, addresses)
	modified    int // of entries: modified entries
	ipEntries   int // of entries: address entries
	invalid     int // malformed lines and the blocks the TLD guard or the IP guard refused
	broad       int // of invalid: the blocks the TLD guard (or, format ips, the IP guard) refused
	unsupported int // unsupported rules (modifiers, URL rules, pattern and modified-entry caps)
}

// memory returns the approximate heap size of p in bytes.
func (p *parsed) memory() int64 {
	var n int64
	for t := range p.sets {
		for k := range p.sets[t] {
			n += int64(cap(p.sets[t][k])) * 8
		}
		n += p.mods[t].memory()
	}
	for t := range p.ips {
		for f := range p.ips[t] {
			n += int64(cap(p.ips[t][f])) * 8
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

// badKey identifies an entry cancelled by a $badfilter rule: the same
// kind, tier and domain (or pattern) and the same normalised modifiers
// ("||x^$dnstype=A,badfilter" cancels "||x^$dnstype=A", never "||x^").
type badKey struct {
	tier, kind uint8
	hash       uint64 // domain entries
	re         string // patterns
	mods       string // entry.modKey
}

// parseList parses a list file in format f (the list kind, the
// plainDomains flag, the TLD guard; a list of format ips with
// parseIPList).
func parseList(ctx context.Context, r io.Reader, f listFormat) (*parsed, error) {
	if f.ips {
		return parseIPList(ctx, r, f)
	}
	res := &parsed{format: f}
	lp := newLineParser(f)
	allowList := f.kind == "allow"
	var bad map[badKey]struct{}
	seenPats := map[string]struct{}{} // tier + source
	var cost int64                    // of the compiled patterns (≤ maxPatternCost)
	// Modified entries: unique by tier, kind, domain and modifiers; their
	// keys (parallel to res.mods) for $badfilter; one type set per
	// distinct set.
	seenMods := map[badKey]struct{}{}
	var modKeys [numTiers][]string
	typeSets := map[string]*typeSet{}
	modCount := 0
	// Rows per tier, kind and name (maxModsPerName): the matcher scans
	// every row of a name hash on each query below that name.
	type modName struct {
		tier, kind uint8
		hash       uint64
	}
	perName := map[modName]uint8{}
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
		case lineBroad:
			res.invalid++
			res.broad++
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
			if e.kind != kindPattern && e.modified() {
				key := e.badKey(tier)
				if _, dup := seenMods[key]; dup {
					continue
				}
				name := modName{uint8(tier), e.kind, key.hash}
				if modCount >= maxModified || perName[name] >= maxModsPerName {
					res.unsupported++
					continue
				}
				modCount++
				perName[name]++
				seenMods[key] = struct{}{}
				row := modRow{hash: key.hash, kind: e.kind, deny: denyHashes(e.deny)}
				if tk := e.types.key(); tk != "" {
					ts, ok := typeSets[tk]
					if !ok {
						ts = &typeSet{mask: e.types.mask, high: slices.Clone(e.types.high), negate: e.types.negate}
						typeSets[tk] = ts
					}
					row.types = ts
				}
				res.mods[tier] = append(res.mods[tier], row)
				modKeys[tier] = append(modKeys[tier], key.mods)
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
		mods := res.mods[t][:0]
		for i, row := range res.mods[t] {
			if _, cancelled := bad[badKey{tier: uint8(t), kind: row.kind, hash: row.hash, mods: modKeys[t][i]}]; !cancelled {
				mods = append(mods, row)
			}
		}
		res.mods[t] = slices.Clip(mods)
		sortMods(res.mods[t])
		res.modified += len(res.mods[t])
	}
	res.entries += res.modified
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

// parseIPList parses a list of answer addresses (format ips, parseIPLine):
// "@@" lines and every line of an allowlist are exceptions; a block the IP
// guard refuses counts as invalid and as broad.
func parseIPList(ctx context.Context, r io.Reader, f listFormat) (*parsed, error) {
	res := &parsed{format: f}
	var lens [numIPTiers][2][129]bool
	err := scanLines(ctx, r, func(line []byte, long bool) {
		if long {
			res.invalid++
			return
		}
		e, st := parseIPLine(string(line))
		switch st {
		case lineSkip:
			return
		case lineOK:
		default:
			res.invalid++
			return
		}
		tier := ipTierBlock
		if e.allow || f.kind == "allow" {
			tier = ipTierAllow
		}
		if tier == ipTierBlock && ipGuarded(e.prefix) {
			res.invalid++
			res.broad++
			return
		}
		fam := 1
		if e.prefix.Addr().Is4() {
			fam = 0
		}
		res.ips[tier][fam] = append(res.ips[tier][fam], ipKey(e.prefix.Addr(), e.prefix.Bits()))
		lens[tier][fam][e.prefix.Bits()] = true
	})
	if err != nil {
		return nil, err
	}
	for t := range res.ips {
		for fam := range res.ips[t] {
			keys := res.ips[t][fam]
			slices.Sort(keys)
			res.ips[t][fam] = slices.Clip(slices.Compact(keys))
			res.ipEntries += len(res.ips[t][fam])
			for b := 128; b >= 0; b-- {
				if lens[t][fam][b] {
					res.ipBits[t][fam] = append(res.ipBits[t][fam], uint8(b))
				}
			}
		}
	}
	res.entries = res.ipEntries
	return res, nil
}

// badKey returns the key of the rule a $badfilter entry cancels (and of
// the entry itself).
func (e *entry) badKey(tier int) badKey {
	k := badKey{tier: uint8(tier), kind: e.kind, mods: e.modKey()}
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
