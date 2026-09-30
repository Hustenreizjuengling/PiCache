package filter

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// The follower sync (docs/ARCHITECTURE.md 15.4): the lists, rules and IP
// rules of a primary replace this follower's, IDs included, in the app's
// transaction. A list whose URL, kind and format stay under its id keeps
// its downloaded copy and status; new lists and lists whose URL, kind or
// format changed are fetched, the copies of removed lists deleted
// (SyncApplied, after the commit).

// SyncedLists are the validated lists and rules of a sync.
type SyncedLists struct {
	lists   []List
	rules   []Rule
	ipRules []IPRule
}

// ValidateSync checks the lists, rules and IP rules of a primary with the
// rules of the API (list URLs, kinds, categories, formats, group ids; the
// rule syntax; the caps: 100 lists, the rule caps and the 32 inverted
// regular expressions, the IP rule cap). The group ids are this
// follower's.
func (e *Engine) ValidateSync(lists []List, rules []Rule, ipRules []IPRule) (*SyncedLists, error) {
	if len(lists) > maxLists {
		return nil, apperr.Conflict("at most %d lists are supported", maxLists)
	}
	out := &SyncedLists{}
	ids, urls := map[int64]bool{}, map[string]bool{}
	for _, l := range lists {
		format := l.Format
		in, _, err := e.validateList(ListInput{Name: l.Name, URL: l.URL, Kind: l.Kind, PlainDomains: l.PlainDomains, Category: l.Category,
			Enabled: l.Enabled, GroupIDs: nonNil(l.GroupIDs), Comment: l.Comment, Format: &format})
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", l.Name, err)
		}
		if in.Category, err = resolveCategory(in.Category, CategoryOther, in.Kind); err != nil {
			return nil, fmt.Errorf("list %q: %w", l.Name, err)
		}
		if in.Category, err = ipListCategory(*in.Format, in.Category, false); err != nil {
			return nil, fmt.Errorf("list %q: %w", l.Name, err)
		}
		if l.ID <= 0 || ids[l.ID] || urls[in.URL] {
			return nil, apperr.Conflict("list %q is listed twice", l.Name)
		}
		ids[l.ID], urls[in.URL] = true, true
		v := l
		v.Name, v.URL, v.Kind, v.PlainDomains, v.Category, v.Comment, v.Format = in.Name, in.URL, in.Kind, in.PlainDomains, in.Category,
			in.Comment, *in.Format
		v.GroupIDs = in.GroupIDs
		out.lists = append(out.lists, v)
	}
	regexes, inverted := 0, 0
	keys := map[string]bool{}
	for _, r := range rules {
		negate, reply, v4, v6, invert := r.QtypesNegate, r.Reply, r.ReplyIPv4, r.ReplyIPv6, r.Invert
		sp, err := resolveRule(RuleInput{Action: r.Action, Type: r.Type, Pattern: r.Pattern, Enabled: r.Enabled, GroupIDs: nonNil(r.GroupIDs),
			Comment: r.Comment, Qtypes: r.Qtypes, QtypesNegate: &negate, Reply: &reply, ReplyIPv4: &v4, ReplyIPv6: &v6,
			Denyallow: r.Denyallow, Invert: &invert}, nil)
		if err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.Pattern, err)
		}
		key := sp.Action + "|" + sp.Type + "|" + sp.Pattern
		if r.ID <= 0 || keys[key] {
			return nil, apperr.Conflict("rule %q is listed twice", r.Pattern)
		}
		keys[key] = true
		if sp.Type == "regex" {
			regexes++
		}
		if sp.Invert {
			inverted++
		}
		sp.ID, sp.CreatedAt, sp.UpdatedAt = r.ID, r.CreatedAt, r.UpdatedAt
		out.rules = append(out.rules, sp)
	}
	switch {
	case len(rules) > maxRules:
		return nil, apperr.Conflict("at most %d rules are supported; use a local list for large sets", maxRules)
	case regexes > maxRegexRules:
		return nil, apperr.Conflict("at most %d regex rules are supported", maxRegexRules)
	case inverted > maxInverted:
		return nil, errInvertCap
	case len(ipRules) > maxIPRules:
		return nil, apperr.Conflict("at most %d IP rules are supported", maxIPRules)
	}
	ipKeys := map[string]bool{}
	for _, r := range ipRules {
		in, err := normalizeIPRule(IPRuleInput{Action: r.Action, Pattern: r.Pattern, Enabled: r.Enabled, GroupIDs: nonNil(r.GroupIDs),
			Comment: r.Comment})
		if err != nil {
			return nil, fmt.Errorf("IP rule %q: %w", r.Pattern, err)
		}
		if r.ID <= 0 || ipKeys[in.Action+"|"+in.Pattern] {
			return nil, apperr.Conflict("IP rule %q is listed twice", r.Pattern)
		}
		ipKeys[in.Action+"|"+in.Pattern] = true
		v := r
		v.Action, v.Pattern, v.Comment, v.GroupIDs = in.Action, in.Pattern, in.Comment, in.GroupIDs
		out.ipRules = append(out.ipRules, v)
	}
	return out, nil
}

// SyncChange is what a sync changed about the lists (SyncApplied).
type SyncChange struct {
	kept    map[int64]bool // same URL, kind and format under the same id
	removed []int64
}

// ReplaceSynced replaces the lists, rules and IP rules in tx (IDs of the
// primary). A list that keeps its URL, kind and format keeps its stored
// download state.
func (e *Engine) ReplaceSynced(ctx context.Context, tx *sql.Tx, s *SyncedLists) (*SyncChange, error) {
	type state struct {
		url, kind, format                                string
		status, lastError, etag, lastModified, hash      string
		lastUpdated, lastChecked, lastSuccess, sizeBytes int64
		entries, invalid, unsupported                    int
	}
	old := map[int64]state{}
	rows, err := tx.QueryContext(ctx, `SELECT id, url, kind, format, status, last_error, etag, last_modified, content_hash,
		last_updated, last_checked, last_success, size_bytes, entries, invalid, unsupported FROM filter_lists`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var st state
		if err := rows.Scan(&id, &st.url, &st.kind, &st.format, &st.status, &st.lastError, &st.etag, &st.lastModified, &st.hash,
			&st.lastUpdated, &st.lastChecked, &st.lastSuccess, &st.sizeBytes, &st.entries, &st.invalid, &st.unsupported); err != nil {
			rows.Close()
			return nil, err
		}
		old[id] = st
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, q := range []string{`DELETE FROM filter_lists`, `DELETE FROM filter_rules`, `DELETE FROM filter_ip_rules`} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return nil, err
		}
	}
	ch := &SyncChange{kept: map[int64]bool{}}
	now := db.NowMs()
	for _, l := range s.lists {
		if err := checkGroups(ctx, tx, l.GroupIDs); err != nil {
			return nil, fmt.Errorf("list %q: %w", l.Name, err)
		}
		st, had := old[l.ID]
		keep := had && st.url == l.URL && st.kind == l.Kind && st.format == l.Format
		created := db.Ms(l.CreatedAt)
		if created == 0 {
			created = now
		}
		if keep {
			ch.kept[l.ID] = true
			_, err = tx.ExecContext(ctx, `INSERT INTO filter_lists (id, name, url, kind, plain_domains, category, catalog_key, enabled, comment,
					created_at, format, name_auto, status, last_error, etag, last_modified, content_hash, last_updated, last_checked,
					last_success, size_bytes, entries, invalid, unsupported)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				l.ID, l.Name, l.URL, l.Kind, l.PlainDomains, l.Category, l.CatalogKey, l.Enabled, l.Comment, created, l.Format, l.NameAuto,
				st.status, st.lastError, st.etag, st.lastModified, st.hash, st.lastUpdated, st.lastChecked, st.lastSuccess, st.sizeBytes,
				st.entries, st.invalid, st.unsupported)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO filter_lists (id, name, url, kind, plain_domains, category, catalog_key, enabled, comment,
					created_at, format, name_auto) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				l.ID, l.Name, l.URL, l.Kind, l.PlainDomains, l.Category, l.CatalogKey, l.Enabled, l.Comment, created, l.Format, l.NameAuto)
		}
		if err != nil {
			return nil, fmt.Errorf("list %q: %w", l.Name, err)
		}
		if err := setGroups(ctx, tx, "filter_list_groups", "list_id", l.ID, l.GroupIDs); err != nil {
			return nil, err
		}
	}
	for id := range old {
		if !slices.ContainsFunc(s.lists, func(l List) bool { return l.ID == id }) {
			ch.removed = append(ch.removed, id)
		}
	}
	for _, r := range s.rules {
		if err := checkGroups(ctx, tx, r.GroupIDs); err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.Pattern, err)
		}
		created, updated := db.Ms(r.CreatedAt), db.Ms(r.UpdatedAt)
		if created == 0 {
			created, updated = now, now
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO filter_rules (id, action, type, pattern, enabled, comment, created_at, updated_at,
				qtypes, qtypes_negate, reply, reply_ipv4, reply_ipv6, denyallow, invert)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Action, r.Type, r.Pattern, r.Enabled, r.Comment, created, updated,
			jsonText(r.Qtypes), r.QtypesNegate, r.Reply, r.ReplyIPv4, r.ReplyIPv6, jsonText(r.Denyallow), r.Invert); err != nil {
			return nil, fmt.Errorf("rule %q: %w", r.Pattern, err)
		}
		if err := setGroups(ctx, tx, "filter_rule_groups", "rule_id", r.ID, r.GroupIDs); err != nil {
			return nil, err
		}
	}
	for _, r := range s.ipRules {
		if err := checkGroups(ctx, tx, r.GroupIDs); err != nil {
			return nil, fmt.Errorf("IP rule %q: %w", r.Pattern, err)
		}
		created, updated := db.Ms(r.CreatedAt), db.Ms(r.UpdatedAt)
		if created == 0 {
			created, updated = now, now
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO filter_ip_rules (id, action, pattern, enabled, comment, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Action, r.Pattern, r.Enabled, r.Comment, created, updated); err != nil {
			return nil, fmt.Errorf("IP rule %q: %w", r.Pattern, err)
		}
		if err := setGroups(ctx, tx, "filter_ip_rule_groups", "rule_id", r.ID, r.GroupIDs); err != nil {
			return nil, err
		}
	}
	return ch, nil
}

// SyncApplied brings the engine in line with the committed sync: the
// lists are reloaded from the database (a kept list keeps its parsed copy;
// a new or changed one is downloaded; the copies of removed lists and of
// changed ones are deleted), the rules and IP rules rebuilt. It compares
// with what the engine holds, not only with ch: when the reload of an
// earlier run failed after its commit, the next run finds every row kept
// and none removed although the engine still holds the lists from before.
func (e *Engine) SyncApplied(ctx context.Context, ch *SyncChange) error {
	e.listMu.Lock()
	defer e.listMu.Unlock()
	fresh, err := loadLists(ctx, e.db.R)
	if err != nil {
		return err
	}
	e.mu.Lock()
	for _, id := range ch.removed {
		if _, ok := e.lists[id]; ok {
			delete(e.lists, id)
			e.parseGen++
		}
		e.removeFiles(id)
	}
	next := map[int64]*listRT{}
	for _, f := range fresh {
		l := f.List
		rt, had := e.lists[l.ID]
		if had && ch.kept[l.ID] && rt.URL == l.URL && rt.Kind == l.Kind && rt.Format == l.Format {
			oldFormat := rt.format()
			wasEnabled := rt.Enabled
			rt.Name, rt.NameAuto, rt.PlainDomains, rt.Category, rt.CatalogKey = l.Name, l.NameAuto, l.PlainDomains, l.Category, l.CatalogKey
			rt.Enabled, rt.Comment, rt.GroupIDs = l.Enabled, l.Comment, l.GroupIDs
			switch {
			case !rt.Enabled:
				rt.wantDownload, rt.wantReparse = false, false
				if rt.parsed != nil {
					rt.parsed = nil
					e.parseGen++
				}
			case !wasEnabled || rt.format() != oldFormat:
				if e.hasCache(l.ID) {
					rt.wantReparse = true
				} else {
					rt.wantDownload = true
				}
			}
			next[l.ID] = rt
			continue
		}
		if had {
			e.removeFiles(l.ID)
			if rt.parsed != nil {
				e.parseGen++
			}
		}
		f.Status, f.jitter, f.wantDownload = statusPending, newJitter(), l.Enabled
		next[l.ID] = f
	}
	for id := range e.lists {
		if _, ok := next[id]; !ok { // removed by a run whose reload failed
			e.removeFiles(id)
		}
	}
	e.lists = next
	e.parseGen++
	e.publishLocked(nil, nil, nil)
	e.mu.Unlock()
	e.ruleMu.Lock()
	err = e.rebuildRulesLocked(ctx)
	if err == nil {
		err = e.rebuildIPRulesLocked(ctx)
	}
	e.ruleMu.Unlock()
	if err != nil {
		return err
	}
	if err := e.reloadListGroups(ctx); err != nil {
		return err
	}
	e.requestCompile()
	e.signal()
	return nil
}

// ListIDs returns the ids of the synced lists (tests).
func (s *SyncedLists) ListIDs() []int64 {
	out := make([]int64, 0, len(s.lists))
	for _, l := range s.lists {
		out = append(out, l.ID)
	}
	return out
}
