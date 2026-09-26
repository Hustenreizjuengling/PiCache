package filter

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Import limits (POST /filter/rules/import; the body is bounded by the
// API's 1 MiB).
const (
	maxImportLines  = 20000
	maxImportErrors = 1000
)

// RuleImport is the body of POST /filter/rules/import: one rule per line
// in the common filter-list syntaxes (ImportRules). GroupIDs nil means the
// Default group, an empty non-nil slice no group.
type RuleImport struct {
	Text     string  `json:"text"`
	GroupIDs []int64 `json:"groupIds"`
	DryRun   bool    `json:"dryRun"`
}

// ImportError is the first error of one line of an import (line 0: the
// whole import). Field names the member ("syntax" for a line that is not a
// rule, "text" for the import as a whole).
type ImportError struct {
	Line    int    `json:"line"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// RuleImportResult says what an import did (Applied) or, after an error
// or a dry run, what the valid lines would do. Errors holds the first
// error of each line, sorted by line, at most 1 000 (never null);
// ErrorCount counts every line with an error.
type RuleImportResult struct {
	Applied    bool          `json:"applied"`
	Added      int           `json:"added"`
	Unchanged  int           `json:"unchanged"`
	Skipped    int           `json:"skipped"`
	Errors     []ImportError `json:"errors"`
	ErrorCount int           `json:"errorCount"`
}

// errImportNotApplied ends an import transaction without writing.
var errImportNotApplied = errors.New("import not applied")

// importedRule is a rule of an import line; rewrite is 4 or 6 for a rule
// made from a $dnsrewrite address of that family (two such lines for the
// same rule, one per family, become one rule).
type importedRule struct {
	in      RuleInput
	rewrite byte
}

// lineError is the error of an import line.
type lineError struct{ field, msg string }

func syntaxError(format string, args ...any) *lineError {
	return &lineError{field: "syntax", msg: fmt.Sprintf(format, args...)}
}

// ImportRules imports user rules from text, all or nothing (the contract of
// POST /dns/forwarders/import): any error writes nothing; a dry run only
// validates and counts; otherwise every new rule is written in one
// transaction and the rule matcher is rebuilt once. A line whose action,
// type and pattern an existing rule has is unchanged when its modifiers are
// identical (the rule's groups, enabled flag and comment stay) and an
// error otherwise. Imported rules are enabled, without comment, for
// in.GroupIDs. Request errors (line count, unknown groups) are
// apperr.Invalid.
func (e *Engine) ImportRules(ctx context.Context, in RuleImport) (RuleImportResult, error) {
	res := RuleImportResult{Errors: []ImportError{}}
	lines := strings.Split(strings.TrimSuffix(in.Text, "\n"), "\n")
	if len(lines) > maxImportLines {
		return res, apperr.Invalid("text", "at most %d lines", maxImportLines)
	}
	groups := []int64{defaultGroupID}
	if in.GroupIDs != nil {
		var err error
		if groups, err = normalizeGroups(in.GroupIDs); err != nil {
			return res, err
		}
	}
	failed := map[int]bool{}
	fail := func(line int, field, msg string) {
		if failed[line] && line != 0 {
			return
		}
		failed[line] = true
		res.ErrorCount++
		if len(res.Errors) < maxImportErrors {
			res.Errors = append(res.Errors, ImportError{Line: line, Field: field, Message: msg})
		}
	}
	type pending struct {
		line    int
		rule    Rule
		rewrite byte
	}
	var rules []pending
	byKey := map[string]int{} // action, type, pattern → index in rules
	for i, text := range lines {
		n := i + 1
		parsed, skip, lerr := parseImportLine(text)
		switch {
		case skip:
			res.Skipped++
			continue
		case lerr != nil:
			fail(n, lerr.field, lerr.msg)
			continue
		}
		var add []pending
		var merges []func()
		lineKeys := map[string]bool{}
		for _, ir := range parsed {
			ir.in.GroupIDs, ir.in.Enabled = groups, true
			sp, err := resolveRule(ir.in, nil)
			if err != nil {
				field, msg := fieldOf(err)
				fail(n, field, msg)
				break
			}
			key := sp.Action + "\x00" + sp.Type + "\x00" + sp.Pattern
			if lineKeys[key] {
				continue // the same name twice on one hosts line
			}
			lineKeys[key] = true
			if j, dup := byKey[key]; dup {
				prev := &rules[j]
				if canMerge(prev.rule, prev.rewrite, sp, ir.rewrite) {
					merges = append(merges, func() {
						prev.rule.ReplyIPv4 = cmpOr(prev.rule.ReplyIPv4, sp.ReplyIPv4)
						prev.rule.ReplyIPv6 = cmpOr(prev.rule.ReplyIPv6, sp.ReplyIPv6)
						prev.rewrite = 0 // merged: a third line is listed twice
					})
					continue
				}
				fail(n, "pattern", fmt.Sprintf("listed twice (line %d)", prev.line))
				break
			}
			add = append(add, pending{line: n, rule: sp, rewrite: ir.rewrite})
		}
		if failed[n] {
			continue
		}
		for _, m := range merges {
			m()
		}
		for _, p := range add {
			byKey[p.rule.Action+"\x00"+p.rule.Type+"\x00"+p.rule.Pattern] = len(rules)
			rules = append(rules, p)
		}
	}

	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	var toAdd []Rule
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkGroups(ctx, tx, groups); err != nil {
			return err
		}
		regexes, inverted := 0, 0
		for _, p := range rules {
			existing, found, err := ruleByKey(ctx, tx, p.rule.Action, p.rule.Type, p.rule.Pattern)
			switch {
			case err != nil:
				return err
			case found && sameOptions(existing, p.rule):
				res.Unchanged++
			case found:
				fail(p.line, "pattern", fmt.Sprintf("a rule for this pattern already exists (rule %d) with other options; edit it instead", existing.ID))
			default:
				toAdd = append(toAdd, p.rule)
				if p.rule.Type == "regex" {
					regexes++
				}
				if p.rule.Invert {
					inverted++
				}
			}
		}
		res.Added = len(toAdd)
		total, haveRegexes, haveInverted, err := ruleCounts(ctx, tx, 0)
		if err != nil {
			return err
		}
		switch {
		case total+len(toAdd) > maxRules:
			fail(0, "text", fmt.Sprintf("at most %d rules are supported (%d exist, %d would be added)", maxRules, total, len(toAdd)))
		case haveRegexes+regexes > maxRegexRules:
			fail(0, "text", fmt.Sprintf("at most %d regex rules are supported (%d exist, %d would be added)", maxRegexRules, haveRegexes, regexes))
		case haveInverted+inverted > maxInverted:
			fail(0, "text", fmt.Sprintf("at most %d inverted regular expressions are supported (%d exist, %d would be added)",
				maxInverted, haveInverted, inverted))
		}
		if res.ErrorCount > 0 || in.DryRun {
			return errImportNotApplied
		}
		now := db.NowMs()
		for _, r := range toAdd {
			ins, err := tx.ExecContext(ctx, `INSERT INTO filter_rules (action, type, pattern, enabled, comment, created_at, updated_at,
					qtypes, qtypes_negate, reply, reply_ipv4, reply_ipv6, denyallow, invert)
				VALUES (?, ?, ?, 1, '', ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.Action, r.Type, r.Pattern, now, now,
				jsonText(r.Qtypes), r.QtypesNegate, r.Reply, r.ReplyIPv4, r.ReplyIPv6, jsonText(r.Denyallow), r.Invert)
			if err != nil {
				return err
			}
			id, err := ins.LastInsertId()
			if err != nil {
				return err
			}
			if err := setGroups(ctx, tx, "filter_rule_groups", "rule_id", id, groups); err != nil {
				return err
			}
		}
		return nil
	})
	slices.SortStableFunc(res.Errors, func(a, b ImportError) int { return a.Line - b.Line })
	switch {
	case errors.Is(err, errImportNotApplied):
		return res, nil
	case err != nil:
		return RuleImportResult{Errors: []ImportError{}}, err
	}
	if len(toAdd) > 0 {
		if err := e.rebuildRulesLocked(ctx); err != nil {
			return RuleImportResult{Errors: []ImportError{}}, err
		}
	}
	res.Applied = true
	return res, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// canMerge reports whether rule b (from a $dnsrewrite address line of
// family bw) completes rule a (family aw): the other family, the same
// modifiers otherwise.
func canMerge(a Rule, aw byte, b Rule, bw byte) bool {
	if aw == 0 || bw == 0 || aw == bw {
		return false
	}
	c := b
	c.ReplyIPv4, c.ReplyIPv6 = a.ReplyIPv4, a.ReplyIPv6
	return sameOptions(a, c)
}

// fieldOf returns the member of a validation error without an index
// ("qtypes[1]" → "qtypes") and its message.
func fieldOf(err error) (string, string) {
	ae, ok := apperr.As(err)
	if !ok {
		return "syntax", err.Error()
	}
	field := ae.Field
	if i := strings.IndexByte(field, '['); i > 0 {
		field = field[:i]
	}
	if field == "" {
		field = "syntax"
	}
	return field, ae.Message
}

// parseImportLine maps one line of an import to rules (ARCHITECTURE 7.2,
// rule import): comments, cosmetic rules and hosts lines of junk names are
// skipped; hosts lines give an exact block per name; exact, subtree and
// regex lines (with "@@" as allow rules) take the modifiers $dnstype,
// $denyallow, $reply, $dnsrewrite (replies), $invert and $important (dropped)
// and, on regex lines without "$" modifiers, the suffixes ";querytype=",
// ";reply=" and ";invert"; other ABP shapes become regex rules. Anything
// else is an error.
func parseImportLine(line string) (rules []importedRule, skip bool, lerr *lineError) {
	s := strings.TrimSpace(line)
	if s == "" {
		return nil, true, nil
	}
	switch s[0] {
	case '!', '#', '[':
		return nil, true, nil
	}
	if isCosmetic(s) {
		return nil, true, nil
	}
	if !utf8Valid(s) {
		return nil, false, syntaxError("the line is not valid UTF-8")
	}
	if isRegexLine(s) {
		r, lerr := parseImportRegex(s)
		if lerr != nil {
			return nil, false, lerr
		}
		return []importedRule{r}, false, nil
	}
	if i := inlineComment(s); i >= 0 {
		if s = strings.TrimSpace(s[:i]); s == "" {
			return nil, true, nil
		}
	}
	allow := false
	body := s
	if rest, ok := strings.CutPrefix(s, "@@"); ok {
		allow, body = true, rest
	}
	if i := strings.IndexAny(body, " \t"); i > 0 {
		if _, err := netip.ParseAddr(body[:i]); err == nil {
			if allow {
				return nil, false, syntaxError("@@ cannot be used with hosts lines")
			}
			return importHosts(body[i+1:])
		}
		return nil, false, syntaxError("not a rule: %s", shorten(s))
	}
	base, opts := body, ""
	if i := strings.LastIndexByte(body, '$'); i >= 0 && i < len(body)-1 {
		base, opts = body[:i], body[i+1:]
	}
	base = strings.ToLower(base)
	if lerr := addressLine(base); lerr != nil {
		return nil, false, lerr
	}
	if strings.ContainsAny(base, `\()[]{}+?$`) {
		return nil, false, syntaxError("wrap regular expressions in /…/")
	}
	r := importedRule{in: RuleInput{Action: "block"}}
	if allow {
		r.in.Action = "allow"
	}
	switch {
	case strings.HasPrefix(base, "||"):
		d := strings.TrimSuffix(base[2:], "|")
		if d, ok := strings.CutSuffix(d, "^"); ok && validDomain(d) {
			r.in.Type, r.in.Pattern = "subtree", d
		}
	case strings.HasPrefix(base, "|"):
		body, anchored := strings.CutSuffix(base[1:], "|")
		d, sep := strings.CutSuffix(body, "^")
		if (anchored || sep) && validDomain(d) {
			r.in.Type, r.in.Pattern = "exact", d
		}
	case strings.HasPrefix(base, "*.") && validDomain(base[2:]):
		r.in.Type, r.in.Pattern = "subtree", base[2:]
	case !strings.ContainsAny(base, "*^|:/"):
		r.in.Type, r.in.Pattern = "exact", strings.TrimSuffix(base, ".")
	}
	if r.in.Type == "" {
		re, _, st := abpToRegex(base)
		switch st {
		case lineOK:
			r.in.Type, r.in.Pattern = "regex", re
		case lineUnsupported:
			return nil, false, syntaxError("URL rules are not supported: %s", shorten(s))
		default:
			return nil, false, syntaxError("not a rule: %s", shorten(s))
		}
	}
	if opts != "" {
		if lerr := applyImportOptions(&r, opts); lerr != nil {
			return nil, false, lerr
		}
	}
	return []importedRule{r}, false, nil
}

// addressLine refuses a line that is an address or a network ("answer
// addresses are IP rules").
func addressLine(base string) *lineError {
	b := strings.TrimPrefix(strings.TrimPrefix(base, "|"), "|")
	b = strings.TrimSuffix(strings.TrimSuffix(b, "|"), "^")
	if _, ok := canonicalPrefix(b); ok {
		return syntaxError("answer addresses are IP rules (Filtering → Rules, answer addresses)")
	}
	return nil
}

// importHosts maps the names of a hosts line to exact block rules; junk
// host names are skipped (a line of junk names only is skipped), an
// invalid name is the line's error.
func importHosts(names string) ([]importedRule, bool, *lineError) {
	var out []importedRule
	for tok := range strings.FieldsSeq(strings.ToLower(names)) {
		d := strings.TrimSuffix(tok, ".")
		if _, junk := junkHosts[d]; junk {
			continue
		}
		if !validDomain(d) {
			return nil, false, &lineError{field: "pattern", msg: fmt.Sprintf("%s is not a valid domain name (A-labels, e.g. xn--… for IDNs)", shorten(tok))}
		}
		out = append(out, importedRule{in: RuleInput{Action: "block", Type: "exact", Pattern: d}})
	}
	if len(out) == 0 {
		return nil, true, nil
	}
	return out, false, nil
}

// parseImportRegex parses "[@@]/re/" with "$" modifiers or ";" suffixes.
func parseImportRegex(s string) (importedRule, *lineError) {
	r := importedRule{in: RuleInput{Action: "block", Type: "regex"}}
	if rest, ok := strings.CutPrefix(s, "@@"); ok {
		r.in.Action, s = "allow", rest
	}
	end := strings.LastIndexByte(s, '/')
	r.in.Pattern = s[1:end]
	tail := s[end+1:]
	switch {
	case tail == "":
	case tail[0] == '$':
		if lerr := applyImportOptions(&r, tail[1:]); lerr != nil {
			return r, lerr
		}
	case tail[0] == ';':
		if lerr := applyRegexSuffixes(&r, tail[1:]); lerr != nil {
			return r, lerr
		}
	default:
		return r, syntaxError("URL rules are not supported: %s", shorten(s))
	}
	return r, nil
}

// applyImportOptions applies the "$" modifiers of a line.
func applyImportOptions(r *importedRule, opts string) *lineError {
	if strings.TrimSpace(opts) == "" {
		return syntaxError("empty modifier list")
	}
	seen := map[string]bool{}
	for o := range strings.SplitSeq(opts, ",") {
		o = strings.TrimSpace(o)
		name, value, hasValue := strings.Cut(o, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		key := name
		if name == "dnsrewrite" {
			key = "reply" // one reply per line
		}
		if seen[key] {
			return syntaxError("$%s is given twice", name)
		}
		seen[key] = true
		switch {
		case name == "important" && !hasValue:
		case name == "invert" && !hasValue:
			t := true
			r.in.Invert = &t
		case name == "dnstype" && hasValue:
			types, negate, lerr := importTypes(value, "|", "~")
			if lerr != nil {
				return lerr
			}
			r.in.Qtypes, r.in.QtypesNegate = types, &negate
		case name == "denyallow" && hasValue:
			r.in.Denyallow = strings.Split(value, "|")
		case name == "reply" && hasValue:
			if lerr := importReply(r, value); lerr != nil {
				return lerr
			}
		case name == "dnsrewrite" && hasValue:
			if lerr := importRewrite(r, value); lerr != nil {
				return lerr
			}
		case name == "client" || name == "ctag":
			return syntaxError("$%s is not supported; use groups: Only for this device in the query log", name)
		default:
			return syntaxError("the modifier $%s is not supported", shorten(o))
		}
	}
	return nil
}

// importTypes parses a list of query types separated by sep; a neg prefix
// on every value (or, with a single neg of "!", before the whole list)
// negates the set.
func importTypes(v, sep, neg string) ([]string, bool, *lineError) {
	negated, plain := 0, 0
	var types []string
	for part := range strings.SplitSeq(v, sep) {
		part = strings.TrimSpace(part)
		if rest, ok := strings.CutPrefix(part, neg); ok {
			negated++
			part = strings.TrimSpace(rest)
		} else {
			plain++
		}
		if part == "" {
			return nil, false, &lineError{field: "qtypes", msg: "a query type is empty"}
		}
		if _, err := settings.ParseQType(part); err != nil {
			return nil, false, &lineError{field: "qtypes", msg: fmt.Sprintf("unknown record type %s", shorten(part))}
		}
		types = append(types, part)
	}
	if negated > 0 && plain > 0 {
		return nil, false, &lineError{field: "qtypes", msg: "either every query type is negated (~) or none"}
	}
	return types, negated > 0, nil
}

// importReply applies "$reply=": a blocking mode, or
// "custom_ip|<IPv4, self or empty>|<IPv6, self or empty>".
func importReply(r *importedRule, v string) *lineError {
	parts := strings.Split(strings.TrimSpace(v), "|")
	mode := strings.ToLower(parts[0])
	switch {
	case mode == ReplyCustomIP && len(parts) == 3:
		v4, v6 := parts[1], parts[2]
		r.in.Reply, r.in.ReplyIPv4, r.in.ReplyIPv6 = &mode, &v4, &v6
	case len(parts) == 1 && (mode == ReplyNull || mode == ReplyNXDomain || mode == ReplyNoData || mode == ReplyRefused):
		r.in.Reply = &mode
	default:
		return &lineError{field: "reply", msg: "must be null, nxdomain, nodata, refused or custom_ip|<IPv4>|<IPv6>"}
	}
	return nil
}

// importRewrite applies "$dnsrewrite=": an rcode (NXDOMAIN, REFUSED,
// NOERROR, also with ";;") becomes the reply nxdomain, refused or nodata;
// an address (also "NOERROR;A;<IPv4>" or "NOERROR;AAAA;<IPv6>") the reply
// custom_ip. Rewrites to other names or records are local records.
func importRewrite(r *importedRule, v string) *lineError {
	v = strings.TrimSpace(v)
	upper := strings.ToUpper(strings.TrimSuffix(v, ";;"))
	set := func(mode string) *lineError {
		r.in.Reply = &mode
		return nil
	}
	switch upper {
	case "NXDOMAIN":
		return set(ReplyNXDomain)
	case "REFUSED":
		return set(ReplyRefused)
	case "NOERROR":
		return set(ReplyNoData)
	}
	addr, want := v, byte(0)
	if parts := strings.Split(v, ";"); len(parts) == 3 && strings.EqualFold(parts[0], "NOERROR") {
		switch strings.ToUpper(parts[1]) {
		case "A":
			addr, want = parts[2], 4
		case "AAAA":
			addr, want = parts[2], 6
		default:
			addr = ""
		}
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(addr))
	if err != nil || ip.Zone() != "" || ip.Is4In6() || (want == 4 && !ip.Is4()) || (want == 6 && !ip.Is6()) {
		return syntaxError("rewrites to other names are local records: create them on Local DNS")
	}
	mode, s := ReplyCustomIP, ip.String()
	r.in.Reply = &mode
	if ip.Is4() {
		r.in.ReplyIPv4, r.rewrite = &s, 4
	} else {
		r.in.ReplyIPv6, r.rewrite = &s, 6
	}
	return nil
}

// applyRegexSuffixes applies the ";" suffixes of a regex line:
// "querytype=A,AAAA" or "querytype=!A,AAAA" ("!" negates the list),
// "invert" and "reply=nodata|nxdomain|refused|<IPv4>|<IPv6>|ip" (an IPv4
// and an IPv6 reply may both be given; "ip" is this server's address for
// both families).
func applyRegexSuffixes(r *importedRule, suffixes string) *lineError {
	seen := map[string]bool{}
	var mode, v4, v6 string
	for sfx := range strings.SplitSeq(suffixes, ";") {
		sfx = strings.TrimSpace(sfx)
		if sfx == "" {
			continue
		}
		name, value, hasValue := strings.Cut(sfx, "=")
		name = strings.ToLower(strings.TrimSpace(name))
		switch {
		case name == "querytype" && hasValue:
			if seen[name] {
				return syntaxError(";querytype is given twice")
			}
			seen[name] = true
			value = strings.TrimSpace(value)
			negate := strings.HasPrefix(value, "!")
			var types []string
			for t := range strings.SplitSeq(strings.TrimPrefix(value, "!"), ",") {
				t = strings.TrimSpace(t)
				if t == "" {
					return &lineError{field: "qtypes", msg: "a query type is empty"}
				}
				if _, err := settings.ParseQType(t); err != nil {
					return &lineError{field: "qtypes", msg: fmt.Sprintf("unknown record type %s", shorten(t))}
				}
				types = append(types, t)
			}
			r.in.Qtypes, r.in.QtypesNegate = types, &negate
		case name == "invert" && !hasValue:
			t := true
			r.in.Invert = &t
		case name == "reply" && hasValue:
			value = strings.ToLower(strings.TrimSpace(value))
			if value == "none" {
				return syntaxError("dropping is configured in dns.droppedDomains")
			}
			var kind string
			switch value {
			case ReplyNoData, ReplyNXDomain, ReplyRefused:
				kind = value
			case "ip":
				kind, v4, v6 = ReplyCustomIP, cmpOr(v4, settings.SelfAddress), cmpOr(v6, settings.SelfAddress)
			default:
				ip, err := netip.ParseAddr(value)
				if err != nil || ip.Zone() != "" || ip.Is4In6() {
					return &lineError{field: "reply", msg: "must be nodata, nxdomain, refused, an address or ip"}
				}
				kind = ReplyCustomIP
				if ip.Is4() {
					v4 = cmpOr(v4, ip.String())
				} else {
					v6 = cmpOr(v6, ip.String())
				}
			}
			if mode != "" && (mode != ReplyCustomIP || kind != ReplyCustomIP) {
				return &lineError{field: "reply", msg: "the reply is given twice"}
			}
			mode = kind
		default:
			return syntaxError("the suffix ;%s is not supported", shorten(sfx))
		}
	}
	if mode != "" {
		r.in.Reply = &mode
		if mode == ReplyCustomIP {
			r.in.ReplyIPv4, r.in.ReplyIPv6 = &v4, &v6
		}
	}
	return nil
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "") == s }

// shorten cuts text quoted in an error message to 60 bytes (at a rune
// boundary).
func shorten(s string) string {
	if len(s) <= 60 {
		return s
	}
	cut := 60
	for cut > 0 && s[cut]&0xc0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// WriteRuleExport writes rules (Engine.Rules) as text that ImportRules
// reads back (GET /filter/rules/export): a title and the time, then the
// rules by id, each with its comment on the line before ("! <comment>");
// a disabled rule is written as a comment ("! [disabled] <line>"). Groups
// are not exported.
func WriteRuleExport(w io.Writer, rules []Rule, now time.Time) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "! Title: PiCache rules\n! Exported %s\n", now.UTC().Format(time.RFC3339))
	for _, r := range rules {
		if r.Comment != "" {
			fmt.Fprintf(bw, "! %s\n", r.Comment)
		}
		if !r.Enabled {
			bw.WriteString("! [disabled] ")
		}
		bw.WriteString(RuleLine(r))
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

// escapeControls writes the control characters of a regular expression as
// \x{…} escapes, so a rule stored before they were refused (normalizeRule)
// stays one export line.
func escapeControls(re string) string {
	if !strings.ContainsFunc(re, unicode.IsControl) {
		return re
	}
	var b strings.Builder
	for _, c := range re {
		if unicode.IsControl(c) {
			fmt.Fprintf(&b, `\x{%x}`, c)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

// RuleLine returns the export line of a rule: "@@" for allow rules; exact
// "|x^", subtree "||x^", regex "/re/"; then, if any, "$" and the
// modifiers dnstype, denyallow, reply and invert.
func RuleLine(r Rule) string {
	var b strings.Builder
	if r.Action == "allow" {
		b.WriteString("@@")
	}
	switch r.Type {
	case "exact":
		b.WriteString("|" + r.Pattern + "^")
	case "subtree":
		b.WriteString("||" + r.Pattern + "^")
	default:
		b.WriteString("/" + escapeControls(r.Pattern) + "/")
	}
	var mods []string
	if len(r.Qtypes) > 0 {
		types := slices.Clone(r.Qtypes)
		if r.QtypesNegate {
			for i := range types {
				types[i] = "~" + types[i]
			}
		}
		mods = append(mods, "dnstype="+strings.Join(types, "|"))
	}
	if len(r.Denyallow) > 0 {
		mods = append(mods, "denyallow="+strings.Join(r.Denyallow, "|"))
	}
	switch r.Reply {
	case "":
	case ReplyCustomIP:
		mods = append(mods, "reply=custom_ip|"+r.ReplyIPv4+"|"+r.ReplyIPv6)
	default:
		mods = append(mods, "reply="+r.Reply)
	}
	if r.Invert {
		mods = append(mods, "invert")
	}
	if len(mods) > 0 {
		b.WriteString("$" + strings.Join(mods, ","))
	}
	return b.String()
}
