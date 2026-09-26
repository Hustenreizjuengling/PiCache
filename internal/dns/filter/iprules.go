package filter

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

const (
	maxIPRules        = 1000
	ipRuleColumns     = `id, action, pattern, enabled, comment, created_at, updated_at`
	ipRuleGroupsQuery = `SELECT rule_id, group_id FROM filter_ip_rule_groups ORDER BY rule_id, group_id`
)

// IPRule is a user rule for answer addresses (ARCHITECTURE 7.2, response
// addresses): an answer of the upstreams that contains an address in
// Pattern is blocked (with the global blocking mode) or, for an allow rule,
// never blocked by address.
type IPRule struct {
	ID        int64     `json:"id"`
	Action    string    `json:"action"`  // allow | block
	Pattern   string    `json:"pattern"` // an address or a masked CIDR (canonical)
	Enabled   bool      `json:"enabled"`
	GroupIDs  []int64   `json:"groupIds"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// IPRuleInput creates or updates an IP rule. Pattern is an address or a
// CIDR of at least /8 (IPv4) or /32 (IPv6), stored canonically (a single
// address without "/len", a CIDR masked, IPv4-mapped unmapped). GroupIDs
// as in ListInput.
type IPRuleInput struct {
	Action   string  `json:"action"`
	Pattern  string  `json:"pattern"`
	Enabled  bool    `json:"enabled"`
	GroupIDs []int64 `json:"groupIds"`
	Comment  string  `json:"comment"`
}

// IPRuleQuery filters IP rules: Search is a substring of the pattern or
// the comment.
type IPRuleQuery struct {
	Action string
	Search string
}

// IPRules returns the IP rules in id order.
func (e *Engine) IPRules(ctx context.Context, q IPRuleQuery) ([]IPRule, error) {
	q.Action = strings.ToLower(strings.TrimSpace(q.Action))
	q.Search = strings.ToLower(strings.TrimSpace(q.Search))
	if q.Action != "" && q.Action != "allow" && q.Action != "block" {
		return nil, apperr.Invalid("action", "must be allow or block")
	}
	if len(q.Search) > maxSearchLen {
		return nil, apperr.Invalid("search", "must be at most %d characters", maxSearchLen)
	}
	like := ""
	if q.Search != "" {
		like = "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Search) + "%"
	}
	rows, err := e.db.R.QueryContext(ctx, `SELECT `+ipRuleColumns+` FROM filter_ip_rules
		WHERE (?1 = '' OR action = ?1)
		  AND (?2 = '' OR lower(pattern) LIKE ?2 ESCAPE '\' OR lower(comment) LIKE ?2 ESCAPE '\')
		ORDER BY id LIMIT ?3`, q.Action, like, maxIPRules)
	if err != nil {
		return nil, fmt.Errorf("filter: query IP rules: %w", err)
	}
	out, err := scanIPRules(rows)
	if err != nil {
		return nil, err
	}
	groups, err := loadGroupMap(ctx, e.db.R, ipRuleGroupsQuery)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].GroupIDs = nonNil(groups[out[i].ID])
	}
	return out, nil
}

func scanIPRules(rows *sql.Rows) ([]IPRule, error) {
	defer rows.Close()
	out := []IPRule{}
	for rows.Next() {
		var r IPRule
		var created, updated int64
		if err := rows.Scan(&r.ID, &r.Action, &r.Pattern, &r.Enabled, &r.Comment, &created, &updated); err != nil {
			return nil, fmt.Errorf("filter: scan IP rule: %w", err)
		}
		r.CreatedAt, r.UpdatedAt = db.Time(created), db.Time(updated)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter: scan IP rules: %w", err)
	}
	return out, nil
}

// ipRule reads one IP rule with its groups.
func (e *Engine) ipRule(ctx context.Context, id int64) (IPRule, error) {
	rows, err := e.db.R.QueryContext(ctx, `SELECT `+ipRuleColumns+` FROM filter_ip_rules WHERE id = ?`, id)
	if err != nil {
		return IPRule{}, fmt.Errorf("filter: read IP rule: %w", err)
	}
	rs, err := scanIPRules(rows)
	if err != nil {
		return IPRule{}, err
	}
	if len(rs) == 0 {
		return IPRule{}, apperr.NotFound("IP rule", id)
	}
	groups, err := loadGroupMap(ctx, e.db.R, `SELECT rule_id, group_id FROM filter_ip_rule_groups WHERE rule_id = ? ORDER BY group_id`, id)
	if err != nil {
		return IPRule{}, err
	}
	rs[0].GroupIDs = nonNil(groups[id])
	return rs[0], nil
}

// normalizeIPRule validates an IP rule input and stores its pattern
// canonically.
func normalizeIPRule(in IPRuleInput) (IPRuleInput, error) {
	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	if in.Action != "allow" && in.Action != "block" {
		return in, apperr.Invalid("action", "must be allow or block")
	}
	p, ok := canonicalPrefix(strings.TrimSpace(in.Pattern))
	if !ok {
		return in, apperr.Invalid("pattern", "must be an IP address or a CIDR (e.g. 203.0.113.0/24)")
	}
	if p.Addr().Is4() && p.Bits() < minRuleBitsV4 || p.Addr().Is6() && p.Bits() < minRuleBitsV6 {
		return in, apperr.Invalid("pattern", "the network is too broad: at least /%d for IPv4 and /%d for IPv6", minRuleBitsV4, minRuleBitsV6)
	}
	in.Pattern = prefixText(p)
	in.Comment = strings.TrimSpace(in.Comment)
	if err := checkText("comment", in.Comment, maxCommentLen); err != nil {
		return in, err
	}
	var err error
	in.GroupIDs, err = normalizeGroups(in.GroupIDs)
	return in, err
}

// CreateIPRule adds an IP rule.
func (e *Engine) CreateIPRule(ctx context.Context, in IPRuleInput) (IPRule, error) {
	in, err := normalizeIPRule(in)
	if err != nil {
		return IPRule{}, err
	}
	if in.GroupIDs == nil {
		in.GroupIDs = []int64{defaultGroupID}
	}
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	var id int64
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM filter_ip_rules`).Scan(&n); err != nil {
			return err
		}
		if n >= maxIPRules {
			return apperr.Conflict("at most %d IP rules are supported", maxIPRules)
		}
		if err := checkGroups(ctx, tx, in.GroupIDs); err != nil {
			return err
		}
		now := db.NowMs()
		res, err := tx.ExecContext(ctx, `INSERT INTO filter_ip_rules (action, pattern, enabled, comment, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, in.Action, in.Pattern, in.Enabled, in.Comment, now, now)
		if isUniqueViolation(err) {
			return apperr.Conflict("this rule already exists")
		}
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return setGroups(ctx, tx, "filter_ip_rule_groups", "rule_id", id, in.GroupIDs)
	})
	if err != nil {
		return IPRule{}, err
	}
	if err := e.rebuildIPRulesLocked(ctx); err != nil {
		return IPRule{}, err
	}
	return e.ipRule(ctx, id)
}

// UpdateIPRule updates an IP rule (GroupIDs nil keeps its groups).
func (e *Engine) UpdateIPRule(ctx context.Context, id int64, in IPRuleInput) (IPRule, error) {
	in, err := normalizeIPRule(in)
	if err != nil {
		return IPRule{}, err
	}
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		if in.GroupIDs != nil {
			if err := checkGroups(ctx, tx, in.GroupIDs); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE filter_ip_rules SET action = ?, pattern = ?, enabled = ?, comment = ?, updated_at = ?
			WHERE id = ?`, in.Action, in.Pattern, in.Enabled, in.Comment, db.NowMs(), id)
		if isUniqueViolation(err) {
			return apperr.Conflict("this rule already exists")
		}
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, apperr.NotFound("IP rule", id))
		}
		if in.GroupIDs == nil {
			return nil
		}
		return setGroups(ctx, tx, "filter_ip_rule_groups", "rule_id", id, in.GroupIDs)
	})
	if err != nil {
		return IPRule{}, err
	}
	if err := e.rebuildIPRulesLocked(ctx); err != nil {
		return IPRule{}, err
	}
	return e.ipRule(ctx, id)
}

// DeleteIPRule deletes an IP rule.
func (e *Engine) DeleteIPRule(ctx context.Context, id int64) error {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	res, err := e.db.W.ExecContext(ctx, `DELETE FROM filter_ip_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, apperr.NotFound("IP rule", id))
	}
	return e.rebuildIPRulesLocked(ctx)
}

// rebuildIPRules reloads the enabled IP rules and publishes their matcher.
func (e *Engine) rebuildIPRules(ctx context.Context) error {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	return e.rebuildIPRulesLocked(ctx)
}

func (e *Engine) rebuildIPRulesLocked(ctx context.Context) error {
	rows, err := e.db.R.QueryContext(ctx, `SELECT `+ipRuleColumns+` FROM filter_ip_rules WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return fmt.Errorf("filter: load IP rules: %w", err)
	}
	rules, err := scanIPRules(rows)
	if err != nil {
		return err
	}
	groups, err := loadGroupMap(ctx, e.db.R, ipRuleGroupsQuery)
	if err != nil {
		return err
	}
	entries := make([]ipRuleEntry, 0, len(rules))
	for _, r := range rules {
		a := ActionBlock
		if r.Action == "allow" {
			a = ActionAllow
		}
		entries = append(entries, ipRuleEntry{id: r.ID, action: a, pattern: r.Pattern, groups: groups[r.ID]})
	}
	m := buildIPRuleMatcher(entries)
	e.mu.Lock()
	e.publishLocked(nil, nil, m)
	e.mu.Unlock()
	return nil
}
