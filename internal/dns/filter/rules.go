package filter

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"unicode"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	defaultGroupID  int64 = 1 // client_groups "Default"
	maxRules              = 20000
	maxRegexRules         = 1000
	maxSearchLen          = 256
	ruleColumns           = `id, action, type, pattern, enabled, comment, created_at, updated_at, qtypes, qtypes_negate, reply, reply_ipv4, reply_ipv6, denyallow, invert`
	ruleGroupsQuery       = `SELECT rule_id, group_id FROM filter_rule_groups ORDER BY rule_id, group_id`
)

// Reply modes of a block rule (Rule.Reply; "" = filter.blockingMode).
const (
	ReplyNull     = "null"
	ReplyNXDomain = "nxdomain"
	ReplyNoData   = "nodata"
	ReplyRefused  = "refused"
	ReplyCustomIP = "custom_ip"
)

// errInvertCap is the conflict of the inverted-rule cap.
var errInvertCap = apperr.Conflict("at most %d inverted regular expressions are supported", maxInverted)

// Rules returns user rules.
func (e *Engine) Rules(ctx context.Context, q RuleQuery) ([]Rule, error) {
	q.Action = strings.ToLower(strings.TrimSpace(q.Action))
	q.Type = strings.ToLower(strings.TrimSpace(q.Type))
	q.Search = strings.ToLower(strings.TrimSpace(q.Search))
	if q.Action != "" && q.Action != "allow" && q.Action != "block" {
		return nil, apperr.Invalid("action", "must be allow or block")
	}
	if q.Type != "" && q.Type != "exact" && q.Type != "subtree" && q.Type != "regex" {
		return nil, apperr.Invalid("type", "must be exact, subtree or regex")
	}
	if len(q.Search) > maxSearchLen {
		return nil, apperr.Invalid("search", "must be at most %d characters", maxSearchLen)
	}
	like := ""
	if q.Search != "" {
		like = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Search) + "%"
	}
	rows, err := e.db.R.QueryContext(ctx, `SELECT `+ruleColumns+` FROM filter_rules
		WHERE (?1 = '' OR action = ?1) AND (?2 = '' OR type = ?2)
		  AND (?3 = '' OR lower(pattern) LIKE ?3 ESCAPE '\' OR lower(comment) LIKE ?3 ESCAPE '\')
		ORDER BY id LIMIT ?4`, q.Action, q.Type, like, maxRules)
	if err != nil {
		return nil, fmt.Errorf("filter: query rules: %w", err)
	}
	out, err := scanRules(rows)
	if err != nil {
		return nil, err
	}
	groups, err := loadGroupMap(ctx, e.db.R, ruleGroupsQuery)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].GroupIDs = nonNil(groups[out[i].ID])
	}
	return out, nil
}

func scanRules(rows *sql.Rows) ([]Rule, error) {
	defer rows.Close()
	out := []Rule{}
	for rows.Next() {
		var r Rule
		var created, updated int64
		var qtypes, deny string
		if err := rows.Scan(&r.ID, &r.Action, &r.Type, &r.Pattern, &r.Enabled, &r.Comment, &created, &updated,
			&qtypes, &r.QtypesNegate, &r.Reply, &r.ReplyIPv4, &r.ReplyIPv6, &deny, &r.Invert); err != nil {
			return nil, fmt.Errorf("filter: scan rule: %w", err)
		}
		r.CreatedAt, r.UpdatedAt = db.Time(created), db.Time(updated)
		r.Qtypes, r.Denyallow = jsonStrings(qtypes), jsonStrings(deny)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter: scan rules: %w", err)
	}
	return out, nil
}

// jsonStrings decodes a stored JSON array of strings (never nil; a value
// that does not decode, e.g. from an edited database, is empty).
func jsonStrings(s string) []string {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// jsonText encodes a list of strings as a JSON array ("[]" for none).
func jsonText(list []string) string {
	b, err := json.Marshal(nonNilStrings(list))
	if err != nil {
		return "[]"
	}
	return string(b)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// rule reads one rule.
func (e *Engine) rule(ctx context.Context, id int64) (Rule, error) {
	return readRule(ctx, e.db.R, id)
}

// readRule reads one rule with its groups (q: the read pool or a
// transaction).
func readRule(ctx context.Context, q querier, id int64) (Rule, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+ruleColumns+` FROM filter_rules WHERE id = ?`, id)
	if err != nil {
		return Rule{}, fmt.Errorf("filter: read rule: %w", err)
	}
	rs, err := scanRules(rows)
	if err != nil {
		return Rule{}, err
	}
	if len(rs) == 0 {
		return Rule{}, apperr.NotFound("rule", id)
	}
	groups, err := loadGroupMap(ctx, q, `SELECT rule_id, group_id FROM filter_rule_groups WHERE rule_id = ? ORDER BY group_id`, id)
	if err != nil {
		return Rule{}, err
	}
	rs[0].GroupIDs = nonNil(groups[id])
	return rs[0], nil
}

// ruleCounts returns the number of rules, of regex rules and of inverted
// regex rules (enabled or not), without the rule except (0 = none).
func ruleCounts(ctx context.Context, q querier, except int64) (total, regexes, inverted int, err error) {
	err = q.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(type = 'regex'), 0), COALESCE(SUM(type = 'regex' AND invert != 0), 0)
		FROM filter_rules WHERE id != ?`, except).Scan(&total, &regexes, &inverted)
	return total, regexes, inverted, err
}

// CreateRule adds a rule.
func (e *Engine) CreateRule(ctx context.Context, in RuleInput) (Rule, error) {
	sp, err := resolveRule(in, nil)
	if err != nil {
		return Rule{}, err
	}
	if sp.GroupIDs == nil {
		sp.GroupIDs = []int64{defaultGroupID}
	}
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	var id int64
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = insertRule(ctx, tx, sp)
		return err
	})
	if err != nil {
		return Rule{}, err
	}
	if err := e.rebuildRulesLocked(ctx); err != nil {
		return Rule{}, err
	}
	return e.rule(ctx, id)
}

// insertRule checks the caps and the groups and inserts a validated rule.
func insertRule(ctx context.Context, tx *sql.Tx, sp Rule) (int64, error) {
	total, regexes, inverted, err := ruleCounts(ctx, tx, 0)
	if err != nil {
		return 0, err
	}
	if total >= maxRules {
		return 0, apperr.Conflict("at most %d rules are supported; use a local list for large sets", maxRules)
	}
	if sp.Type == "regex" && regexes >= maxRegexRules {
		return 0, apperr.Conflict("at most %d regex rules are supported", maxRegexRules)
	}
	if sp.Invert && inverted >= maxInverted {
		return 0, errInvertCap
	}
	if err := checkGroups(ctx, tx, sp.GroupIDs); err != nil {
		return 0, err
	}
	now := db.NowMs()
	res, err := tx.ExecContext(ctx, `INSERT INTO filter_rules (action, type, pattern, enabled, comment, created_at, updated_at,
			qtypes, qtypes_negate, reply, reply_ipv4, reply_ipv6, denyallow, invert)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, sp.Action, sp.Type, sp.Pattern, sp.Enabled, sp.Comment, now, now,
		jsonText(sp.Qtypes), sp.QtypesNegate, sp.Reply, sp.ReplyIPv4, sp.ReplyIPv6, jsonText(sp.Denyallow), sp.Invert)
	if isUniqueViolation(err) {
		return 0, apperr.Conflict("this rule already exists")
	}
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, setGroups(ctx, tx, "filter_rule_groups", "rule_id", id, sp.GroupIDs)
}

// UpdateRule updates a rule. The members added in 0.13.0 keep their stored
// values when the input leaves them out (RuleInput).
func (e *Engine) UpdateRule(ctx context.Context, id int64, in RuleInput) (Rule, error) {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		old, err := readRule(ctx, tx, id)
		if err != nil {
			return err
		}
		sp, err := resolveRule(in, &old)
		if err != nil {
			return err
		}
		_, regexes, inverted, err := ruleCounts(ctx, tx, id)
		if err != nil {
			return err
		}
		if sp.Type == "regex" && old.Type != "regex" && regexes >= maxRegexRules {
			return apperr.Conflict("at most %d regex rules are supported", maxRegexRules)
		}
		if sp.Invert && !old.Invert && inverted >= maxInverted {
			return errInvertCap
		}
		if sp.GroupIDs != nil {
			if err := checkGroups(ctx, tx, sp.GroupIDs); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE filter_rules SET action = ?, type = ?, pattern = ?, enabled = ?, comment = ?, updated_at = ?,
				qtypes = ?, qtypes_negate = ?, reply = ?, reply_ipv4 = ?, reply_ipv6 = ?, denyallow = ?, invert = ?
			WHERE id = ?`, sp.Action, sp.Type, sp.Pattern, sp.Enabled, sp.Comment, db.NowMs(),
			jsonText(sp.Qtypes), sp.QtypesNegate, sp.Reply, sp.ReplyIPv4, sp.ReplyIPv6, jsonText(sp.Denyallow), sp.Invert, id)
		if isUniqueViolation(err) {
			return apperr.Conflict("this rule already exists")
		}
		if err != nil || sp.GroupIDs == nil {
			return err
		}
		return setGroups(ctx, tx, "filter_rule_groups", "rule_id", id, sp.GroupIDs)
	})
	if err != nil {
		return Rule{}, err
	}
	if err := e.rebuildRulesLocked(ctx); err != nil {
		return Rule{}, err
	}
	return e.rule(ctx, id)
}

// DeleteRule deletes a rule.
func (e *Engine) DeleteRule(ctx context.Context, id int64) error {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	res, err := e.db.W.ExecContext(ctx, `DELETE FROM filter_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, apperr.NotFound("rule", id))
	}
	return e.rebuildRulesLocked(ctx)
}

// ValidateRule validates a rule input as POST /filter/rules would (the
// groups are not checked against the database).
func ValidateRule(in RuleInput) error {
	_, err := resolveRule(in, nil)
	return err
}

// AddRuleForGroup makes the rule of in apply to group ("Only for this
// device", ARCHITECTURE 7.2): a rule with the same action, type and pattern
// and identical options gets the group added to its groups (created
// false); one with other options, or one that is disabled while in is
// enabled (or the reverse), is a conflict; otherwise a new rule for the
// group only is created (in.GroupIDs is ignored). The caps of CreateRule
// apply.
func (e *Engine) AddRuleForGroup(ctx context.Context, in RuleInput, group int64) (Rule, bool, error) {
	in.GroupIDs = nil
	sp, err := resolveRule(in, nil)
	if err != nil {
		return Rule{}, false, err
	}
	sp.GroupIDs = []int64{group}
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	var id int64
	created := false
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		existing, found, err := ruleByKey(ctx, tx, sp.Action, sp.Type, sp.Pattern)
		switch {
		case err != nil:
			return err
		case found && !sameOptions(existing, sp):
			return apperr.Conflict("a rule for this pattern already exists (rule %d) with other options; edit it instead", existing.ID)
		case found && !existing.Enabled && sp.Enabled:
			// Adding the group to a disabled rule would answer success and
			// block or allow nothing for the device.
			return apperr.Conflict("a rule for this pattern already exists (rule %d) but is disabled; enable or edit it", existing.ID)
		case found && existing.Enabled && !sp.Enabled:
			return apperr.Conflict("a rule for this pattern already exists (rule %d) and is enabled; edit it instead", existing.ID)
		case found:
			id = existing.ID
			if err := checkGroups(ctx, tx, sp.GroupIDs); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO filter_rule_groups (rule_id, group_id) VALUES (?, ?)`, id, group)
			return err
		}
		created = true
		id, err = insertRule(ctx, tx, sp)
		return err
	})
	if err != nil {
		return Rule{}, false, err
	}
	if err := e.rebuildRulesLocked(ctx); err != nil {
		return Rule{}, false, err
	}
	r, err := e.rule(ctx, id)
	return r, created, err
}

// ruleByKey returns the rule with the action, type and pattern (the
// UNIQUE key of filter_rules).
func ruleByKey(ctx context.Context, q querier, action, typ, pattern string) (Rule, bool, error) {
	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM filter_rules WHERE action = ? AND type = ? AND pattern = ?`, action, typ, pattern).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Rule{}, false, nil
	case err != nil:
		return Rule{}, false, err
	}
	r, err := readRule(ctx, q, id)
	return r, err == nil, err
}

// sameOptions reports whether two rules have the same modifiers (query
// types, reply, denyallow, invert).
func sameOptions(a, b Rule) bool {
	return slices.Equal(nonNilStrings(a.Qtypes), nonNilStrings(b.Qtypes)) && a.QtypesNegate == b.QtypesNegate &&
		a.Reply == b.Reply && a.ReplyIPv4 == b.ReplyIPv4 && a.ReplyIPv6 == b.ReplyIPv6 &&
		slices.Equal(nonNilStrings(a.Denyallow), nonNilStrings(b.Denyallow)) && a.Invert == b.Invert
}

// resolveRule validates a rule input and returns the rule to store: the
// input merged with the stored rule old (nil on create; the result's
// GroupIDs are nil when the input leaves them out: the caller decides). A
// member the input leaves out keeps its stored value (the default on
// create); a stored member that does not fit the merged rule is reset when
// the input does not name it and refused (400 with its field) when it
// does.
func resolveRule(in RuleInput, old *Rule) (Rule, error) {
	in, err := normalizeRule(in)
	if err != nil {
		return Rule{}, err
	}
	sp := Rule{Action: in.Action, Type: in.Type, Pattern: in.Pattern, Enabled: in.Enabled, Comment: in.Comment,
		GroupIDs: in.GroupIDs, Qtypes: []string{}, Denyallow: []string{}}
	if old != nil {
		sp.Qtypes, sp.QtypesNegate = nonNilStrings(old.Qtypes), old.QtypesNegate
		sp.Reply, sp.ReplyIPv4, sp.ReplyIPv6 = old.Reply, old.ReplyIPv4, old.ReplyIPv6
		sp.Denyallow, sp.Invert = nonNilStrings(old.Denyallow), old.Invert
	}
	if in.Qtypes != nil {
		types, err := normalizeTypes(in.Qtypes)
		if err != nil {
			return Rule{}, err
		}
		sp.Qtypes = typeNames(types)
	}
	if in.QtypesNegate != nil {
		sp.QtypesNegate = *in.QtypesNegate
	}
	if in.Reply != nil {
		r := strings.ToLower(strings.TrimSpace(*in.Reply))
		if r != "" && !slices.Contains([]string{ReplyNull, ReplyNXDomain, ReplyNoData, ReplyRefused, ReplyCustomIP}, r) {
			return Rule{}, apperr.Invalid("reply", "must be empty (the blocking mode), null, nxdomain, nodata, refused or custom_ip")
		}
		sp.Reply = r
	}
	for _, a := range []struct {
		field string
		in    *string
		dst   *string
		v6    bool
	}{{"replyIpv4", in.ReplyIPv4, &sp.ReplyIPv4, false}, {"replyIpv6", in.ReplyIPv6, &sp.ReplyIPv6, true}} {
		if a.in == nil {
			continue
		}
		v, ok := normalizeReplyAddr(*a.in, a.v6)
		if !ok {
			kind := "an IPv4"
			if a.v6 {
				kind = "an IPv6"
			}
			return Rule{}, apperr.Invalid(a.field, "must be empty, %s address or self", kind)
		}
		*a.dst = v
	}
	if in.Denyallow != nil {
		if len(in.Denyallow) > maxDenyallow {
			return Rule{}, apperr.Invalid("denyallow", "at most %d domains", maxDenyallow)
		}
		deny := make([]string, 0, len(in.Denyallow))
		for i, d := range in.Denyallow {
			n, ok := normalizeSubtree(d)
			if !ok {
				return Rule{}, apperr.Invalid(fmt.Sprintf("denyallow[%d]", i), "must be a valid domain name (A-labels, e.g. xn--… for IDNs)")
			}
			if in.Type == "subtree" && (n == in.Pattern || !subtreeMatch(n, in.Pattern)) {
				return Rule{}, apperr.Invalid(fmt.Sprintf("denyallow[%d]", i), "must be a subdomain of %s", in.Pattern)
			}
			deny = append(deny, n)
		}
		slices.Sort(deny)
		sp.Denyallow = slices.Compact(deny)
	}
	if in.Invert != nil {
		sp.Invert = *in.Invert
	}

	// A stored member that does not fit the merged rule: reset unless named.
	fit := func(ok, named bool, field, msg string, reset func()) error {
		switch {
		case ok:
			return nil
		case named:
			return apperr.Invalid(field, "%s", msg)
		}
		reset()
		return nil
	}
	block := sp.Action == "block"
	checks := []func() error{
		func() error {
			return fit(!sp.QtypesNegate || len(sp.Qtypes) > 0, in.QtypesNegate != nil, "qtypesNegate", "needs at least one query type",
				func() { sp.QtypesNegate = false })
		},
		func() error {
			return fit(block || sp.Reply == "", in.Reply != nil, "reply", "allow rules have no reply", func() { sp.Reply = "" })
		},
		func() error {
			return fit(sp.Reply == ReplyCustomIP || sp.ReplyIPv4 == "", in.ReplyIPv4 != nil, "replyIpv4", "only with reply custom_ip",
				func() { sp.ReplyIPv4 = "" })
		},
		func() error {
			return fit(sp.Reply == ReplyCustomIP || sp.ReplyIPv6 == "", in.ReplyIPv6 != nil, "replyIpv6", "only with reply custom_ip",
				func() { sp.ReplyIPv6 = "" })
		},
		func() error {
			if sp.Reply == ReplyCustomIP && sp.ReplyIPv4 == "" && sp.ReplyIPv6 == "" {
				return apperr.Invalid("replyIpv4", "custom_ip needs replyIpv4 or replyIpv6")
			}
			return nil
		},
		func() error {
			return fit(len(sp.Denyallow) == 0 || block && (sp.Type == "subtree" || sp.Type == "regex"), in.Denyallow != nil,
				"denyallow", "only for subtree and regular-expression block rules", func() { sp.Denyallow = []string{} })
		},
		func() error {
			below := sp.Type != "subtree" || !slices.ContainsFunc(sp.Denyallow, func(d string) bool {
				return d == sp.Pattern || !subtreeMatch(d, sp.Pattern)
			})
			return fit(below, in.Denyallow != nil, "denyallow", "must be subdomains of "+sp.Pattern,
				func() { sp.Denyallow = []string{} })
		},
		func() error {
			return fit(!sp.Invert || block && sp.Type == "regex", in.Invert != nil, "invert", "only for regular-expression block rules",
				func() { sp.Invert = false })
		},
	}
	for _, check := range checks {
		if err := check(); err != nil {
			return Rule{}, err
		}
	}
	return sp, nil
}

// normalizeReplyAddr returns the stored form of a reply address: "",
// settings.SelfAddress or a canonical address of the family.
func normalizeReplyAddr(s string, v6 bool) (string, bool) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", true
	case strings.EqualFold(s, settings.SelfAddress):
		return settings.SelfAddress, true
	case !settings.ValidReplyAddress(s, v6):
		return "", false
	}
	ip, _ := netip.ParseAddr(s)
	return ip.String(), true
}

// normalizeRule validates the action, type, pattern, comment and groups of
// a rule input and normalises its pattern.
func normalizeRule(in RuleInput) (RuleInput, error) {
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	if in.Action != "allow" && in.Action != "block" {
		return in, apperr.Invalid("action", "must be allow or block")
	}
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	var ok bool
	switch in.Type {
	case "exact":
		in.Pattern = normalizeName(in.Pattern)
		if !validDomain(in.Pattern) {
			return in, apperr.Invalid("pattern", "must be a valid domain name (A-labels, e.g. xn--… for IDNs)")
		}
	case "subtree":
		if in.Pattern, ok = normalizeSubtree(in.Pattern); !ok {
			return in, apperr.Invalid("pattern", `must be a domain name ("example.com", "*.example.com" or "||example.com^")`)
		}
	case "regex":
		p := strings.TrimSpace(in.Pattern)
		for len(p) > 2 && p[0] == '/' && p[len(p)-1] == '/' { // names never contain "/": the slashes enclose the expression
			p = strings.TrimSpace(p[1 : len(p)-1])
		}
		switch {
		case p == "":
			return in, apperr.Invalid("pattern", "is required")
		case len(p) > maxRegexLen:
			return in, apperr.Invalid("pattern", "must be at most %d characters", maxRegexLen)
		case strings.ContainsFunc(p, unicode.IsControl):
			// A line break would split the rule's export line into several
			// rules; the expression can write it as \n or \x{0a}.
			return in, apperr.Invalid("pattern", `must not contain control characters (write them as \n, \t or \x{…})`)
		}
		switch _, _, err := compileRegex("(?i)"+p, maxRegexCost); {
		case errors.Is(err, errTooComplex):
			return in, apperr.Invalid("pattern", "is too complex: its repetitions expand to more than about %d instructions", maxRegexCost)
		case err != nil:
			return in, apperr.Invalid("pattern", "invalid regular expression: %s", strings.TrimPrefix(err.Error(), "error parsing regexp: "))
		}
		in.Pattern = p
	default:
		return in, apperr.Invalid("type", "must be exact, subtree or regex")
	}
	in.Comment = strings.TrimSpace(in.Comment)
	if err := checkText("comment", in.Comment, maxCommentLen); err != nil {
		return in, err
	}
	var err error
	in.GroupIDs, err = normalizeGroups(in.GroupIDs)
	return in, err
}

// normalizeSubtree accepts "example.com", "*.example.com", ".example.com"
// and "||example.com^" and returns "example.com".
func normalizeSubtree(s string) (string, bool) {
	s = normalizeName(s)
	if t, ok := strings.CutPrefix(s, "||"); ok {
		t = strings.TrimSuffix(t, "|")
		if s, ok = strings.CutSuffix(t, "^"); !ok {
			return "", false
		}
	} else {
		s = strings.TrimPrefix(strings.TrimPrefix(s, "*"), ".")
	}
	s = strings.TrimSuffix(s, ".")
	return s, validDomain(s)
}

// rebuildRules reloads the enabled rules and publishes a new rule matcher.
func (e *Engine) rebuildRules(ctx context.Context) error {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	return e.rebuildRulesLocked(ctx)
}

func (e *Engine) rebuildRulesLocked(ctx context.Context) error {
	rows, err := e.db.R.QueryContext(ctx, `SELECT `+ruleColumns+` FROM filter_rules WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("filter: load rules: %w", err)
	}
	rules, err := scanRules(rows)
	if err != nil {
		return err
	}
	groups, err := loadGroupMap(ctx, e.db.R, ruleGroupsQuery)
	if err != nil {
		return err
	}
	entries := make([]ruleEntry, 0, len(rules))
	for _, r := range rules {
		re := compileRuleEntry(r, groups[r.ID])
		if r.Type == "regex" {
			if re.re, err = regexp.Compile("(?i)" + r.Pattern); err != nil {
				e.log.Warn("skipping invalid regex rule", slog.Int64("id", r.ID), slog.Any("err", err))
				continue
			}
		}
		entries = append(entries, re)
	}
	m := buildRuleMatcher(entries)
	e.mu.Lock()
	e.publishLocked(nil, m, nil)
	e.mu.Unlock()
	return nil
}

// compileRuleEntry converts a stored rule into its matcher entry (without
// the compiled expression). Stored type names that no longer parse are
// ignored; an allow rule never has a reply, denyallow or invert.
func compileRuleEntry(r Rule, groups []int64) ruleEntry {
	re := ruleEntry{id: r.ID, action: ActionBlock, typ: r.Type, pattern: r.Pattern, groups: groups, invert: r.Invert && r.Type == "regex",
		reply: r.Reply, replyV4: r.ReplyIPv4, replyV6: r.ReplyIPv6}
	if r.Action == "allow" {
		re.action = ActionAllow
		re.invert, re.reply, re.replyV4, re.replyV6 = false, "", "", ""
	}
	var types []uint16
	for _, name := range r.Qtypes {
		if t, err := settings.ParseQType(name); err == nil {
			types = append(types, t)
		}
	}
	re.types = newTypeSet(types, r.QtypesNegate)
	if re.action == ActionBlock {
		re.deny, re.denyall = denyHashes(r.Denyallow), nonNilStrings(r.Denyallow)
	}
	return re
}

// reloadRuleGroups rebuilds the rule and IP rule matchers if their group
// memberships changed in the database (e.g. a group was deleted).
func (e *Engine) reloadRuleGroups(ctx context.Context) error {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	groups, err := loadGroupMap(ctx, e.db.R, ruleGroupsQuery)
	if err != nil {
		return err
	}
	snap := e.snap.Load()
	for _, r := range snap.rules.rules {
		if !slices.Equal(r.groups, groups[r.id]) {
			if err := e.rebuildRulesLocked(ctx); err != nil {
				return err
			}
			break
		}
	}
	ipGroups, err := loadGroupMap(ctx, e.db.R, ipRuleGroupsQuery)
	if err != nil {
		return err
	}
	for _, r := range snap.ipRules.rules {
		if !slices.Equal(r.groups, ipGroups[r.id]) {
			return e.rebuildIPRulesLocked(ctx)
		}
	}
	return nil
}

// regexLiteral returns the literal every match of a (case-insensitive)
// user regex must contain.
func regexLiteral(pattern string) string {
	re, err := syntax.Parse("(?i)"+pattern, syntax.Perl)
	if err != nil {
		return ""
	}
	return requiredLiteral(re.Simplify())
}
