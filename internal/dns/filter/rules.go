package filter

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

const (
	defaultGroupID  int64 = 1 // client_groups "Default"
	maxRules              = 20000
	maxRegexRules         = 1000
	maxSearchLen          = 256
	ruleColumns           = `id, action, type, pattern, enabled, comment, created_at, updated_at`
	ruleGroupsQuery       = `SELECT rule_id, group_id FROM filter_rule_groups ORDER BY rule_id, group_id`
)

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
		if err := rows.Scan(&r.ID, &r.Action, &r.Type, &r.Pattern, &r.Enabled, &r.Comment, &created, &updated); err != nil {
			return nil, fmt.Errorf("filter: scan rule: %w", err)
		}
		r.CreatedAt, r.UpdatedAt = db.Time(created), db.Time(updated)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter: scan rules: %w", err)
	}
	return out, nil
}

// rule reads one rule.
func (e *Engine) rule(ctx context.Context, id int64) (Rule, error) {
	rows, err := e.db.R.QueryContext(ctx, `SELECT `+ruleColumns+` FROM filter_rules WHERE id = ?`, id)
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
	groups, err := loadGroupMap(ctx, e.db.R, `SELECT rule_id, group_id FROM filter_rule_groups WHERE rule_id = ? ORDER BY group_id`, id)
	if err != nil {
		return Rule{}, err
	}
	rs[0].GroupIDs = nonNil(groups[id])
	return rs[0], nil
}

// CreateRule adds a rule.
func (e *Engine) CreateRule(ctx context.Context, in RuleInput) (Rule, error) {
	in, err := normalizeRule(in)
	if err != nil {
		return Rule{}, err
	}
	if in.GroupIDs == nil {
		in.GroupIDs = []int64{defaultGroupID}
	}
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	now := db.NowMs()
	var id int64
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		var total, regexes int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(type = 'regex'), 0) FROM filter_rules`).Scan(&total, &regexes); err != nil {
			return err
		}
		if total >= maxRules {
			return apperr.Conflict("at most %d rules are supported; use a local list for large sets", maxRules)
		}
		if in.Type == "regex" && regexes >= maxRegexRules {
			return apperr.Conflict("at most %d regex rules are supported", maxRegexRules)
		}
		if err := checkGroups(ctx, tx, in.GroupIDs); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO filter_rules (action, type, pattern, enabled, comment, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, in.Action, in.Type, in.Pattern, in.Enabled, in.Comment, now, now)
		if isUniqueViolation(err) {
			return apperr.Conflict("this rule already exists")
		}
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return setGroups(ctx, tx, "filter_rule_groups", "rule_id", id, in.GroupIDs)
	})
	if err != nil {
		return Rule{}, err
	}
	if err := e.rebuildRulesLocked(ctx); err != nil {
		return Rule{}, err
	}
	return e.rule(ctx, id)
}

// UpdateRule updates a rule.
func (e *Engine) UpdateRule(ctx context.Context, id int64, in RuleInput) (Rule, error) {
	in, err := normalizeRule(in)
	if err != nil {
		return Rule{}, err
	}
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		var oldType string
		if err := tx.QueryRowContext(ctx, `SELECT type FROM filter_rules WHERE id = ?`, id).Scan(&oldType); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return apperr.NotFound("rule", id)
			}
			return err
		}
		if in.Type == "regex" && oldType != "regex" {
			var regexes int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM filter_rules WHERE type = 'regex'`).Scan(&regexes); err != nil {
				return err
			}
			if regexes >= maxRegexRules {
				return apperr.Conflict("at most %d regex rules are supported", maxRegexRules)
			}
		}
		if in.GroupIDs != nil {
			if err := checkGroups(ctx, tx, in.GroupIDs); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `UPDATE filter_rules SET action = ?, type = ?, pattern = ?, enabled = ?, comment = ?, updated_at = ?
			WHERE id = ?`, in.Action, in.Type, in.Pattern, in.Enabled, in.Comment, db.NowMs(), id)
		if isUniqueViolation(err) {
			return apperr.Conflict("this rule already exists")
		}
		if err != nil || in.GroupIDs == nil {
			return err
		}
		return setGroups(ctx, tx, "filter_rule_groups", "rule_id", id, in.GroupIDs)
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

// normalizeRule validates a rule input and normalises its pattern.
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
		if len(p) > 2 && p[0] == '/' && p[len(p)-1] == '/' {
			p = p[1 : len(p)-1]
		}
		switch {
		case p == "":
			return in, apperr.Invalid("pattern", "is required")
		case len(p) > maxRegexLen:
			return in, apperr.Invalid("pattern", "must be at most %d characters", maxRegexLen)
		}
		if _, err := regexp.Compile("(?i)" + p); err != nil {
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
		re := ruleEntry{id: r.ID, action: ActionBlock, typ: r.Type, pattern: r.Pattern, groups: groups[r.ID]}
		if r.Action == "allow" {
			re.action = ActionAllow
		}
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
	e.publishLocked(nil, m)
	e.mu.Unlock()
	return nil
}

// reloadRuleGroups rebuilds the rule matcher if rule group memberships
// changed in the database (e.g. a group was deleted).
func (e *Engine) reloadRuleGroups(ctx context.Context) error {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	groups, err := loadGroupMap(ctx, e.db.R, ruleGroupsQuery)
	if err != nil {
		return err
	}
	for _, r := range e.snap.Load().rules.rules {
		if !slices.Equal(r.groups, groups[r.id]) {
			return e.rebuildRulesLocked(ctx)
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
