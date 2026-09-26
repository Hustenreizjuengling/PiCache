package filter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// PresetState is the state of a category switch for one group.
type PresetState struct {
	On    bool   `json:"on"`    // every bound list is enabled and assigned to the group
	State string `json:"state"` // PresetOff | PresetActive | PresetPending | PresetFailed
}

// Category switch states.
const (
	PresetOff     = "off"     // not on
	PresetActive  = "active"  // on, every bound list has a copy (ok, unchanged, failed-cached)
	PresetPending = "pending" // on, a bound list has no copy yet
	PresetFailed  = "failed"  // on, a bound list could not be downloaded (failed-empty)
)

// boundTo reports whether a list with catalogKey is bound to the catalogue
// key: the same key, or an old key that catalogAliases maps to it.
func boundTo(catalogKey, key string) bool {
	return catalogKey != "" && (catalogKey == key || catalogAliases[catalogKey] == key)
}

// presetFor returns the category switch named sw.
func presetFor(sw string) (categoryPreset, bool) {
	i := slices.IndexFunc(categoryPresets, func(p categoryPreset) bool { return p.Switch == sw })
	if i < 0 {
		return categoryPreset{}, false
	}
	return categoryPresets[i], true
}

// Presets returns the state of every category switch for group. A bound
// list counts only while it is a blocklist of the switch's category, that
// is a protection list enforced like parental controls: a bound list that
// was given another category (e.g. security, which pauses with blocking)
// or turned into an allowlist leaves the switch off.
func (e *Engine) Presets(group int64) map[string]PresetState {
	e.mu.Lock()
	defer e.mu.Unlock()
	lists := sortedLists(e.lists)
	out := make(map[string]PresetState, len(categoryPresets))
	for _, p := range categoryPresets {
		st := PresetState{On: true, State: PresetActive}
		for _, key := range p.Keys {
			i := slices.IndexFunc(lists, func(rt *listRT) bool {
				return boundTo(rt.CatalogKey, key) && rt.Kind == "block" && rt.Category == p.Category &&
					rt.Enabled && slices.Contains(rt.GroupIDs, group)
			})
			if i < 0 {
				st = PresetState{State: PresetOff}
				break
			}
			switch lists[i].Status {
			case statusFailedEmpty:
				st.State = PresetFailed
			case statusPending:
				if st.State != PresetFailed {
					st.State = PresetPending
				}
			}
		}
		out[p.Switch] = st
	}
	return out
}

// PresetChange is what one SetPresets call did: the lists it created or
// changed (as they are now, for the audit) and what RevertPresets needs to
// undo exactly that.
type PresetChange struct {
	Lists   []List  // created or changed lists, in ID order (never nil)
	Created []int64 // IDs of the lists it created
	group   int64
	before  []List // the changed lists that existed, as they were before
}

// SetPresets switches the category switches in want (switch name → on;
// switches not in want are left alone) for group in one transaction and
// returns what it changed. Switching on makes every bound catalogue key
// have a list that is a protection list for the group: a missing list is
// created from the catalogue entry (enabled, only this group); an existing
// one gets the entry's category and catalogue key back if they differ (it
// was recategorised, or found by its URL), the group, and is enabled again
// (for its other groups too). Switching off removes the group from the
// bound lists, which stay. apperr.Conflict when a bound URL holds an
// allowlist or the list limit (100) would be exceeded; nothing is changed
// then.
//
// Memory is updated from the rows of the transaction, so nothing is read
// after the commit; the bound lists of the switches in want are brought in
// line with the database even when nothing changed.
func (e *Engine) SetPresets(ctx context.Context, group int64, want map[string]bool) (PresetChange, error) {
	change := PresetChange{Lists: []List{}, group: group}
	for sw := range want {
		if _, ok := presetFor(sw); !ok {
			return change, apperr.Invalid("categories."+sw, "unknown category switch")
		}
	}
	e.listMu.Lock()
	defer e.listMu.Unlock()
	now := e.now()
	var rows []*listRT
	before := map[int64]List{}
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM client_groups WHERE id = ?`, group).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return apperr.NotFound("group", group)
			}
			return err
		}
		var err error
		if rows, err = loadLists(ctx, tx); err != nil {
			return err
		}
		// touch keeps the state of an existing list before its first change.
		touch := func(r *listRT) {
			if _, ok := before[r.ID]; !ok {
				before[r.ID] = r.copy()
			}
		}
		for _, p := range categoryPresets { // a fixed order: the same input always gives the same changes
			on, ok := want[p.Switch]
			if !ok {
				continue
			}
			for _, key := range p.Keys {
				entry, ok := catalogEntry(key)
				if !ok {
					return fmt.Errorf("filter: category switch %s: no catalogue entry %s", p.Switch, key)
				}
				if !on {
					for _, r := range rows {
						if boundTo(r.CatalogKey, key) && slices.Contains(r.GroupIDs, group) {
							if _, err := tx.ExecContext(ctx, `DELETE FROM filter_list_groups WHERE list_id = ? AND group_id = ?`, r.ID, group); err != nil {
								return err
							}
							touch(r)
							r.GroupIDs = slices.DeleteFunc(r.GroupIDs, func(g int64) bool { return g == group })
						}
					}
					continue
				}
				r := pickBound(rows, key, entry.URL, group)
				if r == nil {
					if len(rows) >= maxLists {
						return apperr.Conflict("at most %d lists are supported", maxLists)
					}
					res, err := tx.ExecContext(ctx, `INSERT INTO filter_lists (name, url, kind, plain_domains, category, catalog_key, enabled, comment, created_at)
						VALUES (?, ?, ?, ?, ?, ?, 1, '', ?)`, entry.Name, entry.URL, entry.Kind, entry.PlainDomains, entry.Category, entry.Key, db.Ms(now))
					if err != nil {
						return err
					}
					id, err := res.LastInsertId()
					if err != nil {
						return err
					}
					if _, err := tx.ExecContext(ctx, `INSERT INTO filter_list_groups (list_id, group_id) VALUES (?, ?)`, id, group); err != nil {
						return err
					}
					rows = append(rows, &listRT{List: List{
						ID: id, Name: entry.Name, URL: entry.URL, Kind: entry.Kind, Format: FormatDomains, PlainDomains: entry.PlainDomains,
						Category: entry.Category, CatalogKey: entry.Key, Enabled: true, GroupIDs: []int64{group},
						Status: statusPending, CreatedAt: db.Time(db.Ms(now)),
					}})
					change.Created = append(change.Created, id)
					continue
				}
				if r.Kind == "allow" {
					return apperr.Conflict("the list %q at the address of this category switch is an allowlist", r.Name)
				}
				if r.Category != entry.Category || !boundTo(r.CatalogKey, key) {
					touch(r)
					r.Category = entry.Category
					if !boundTo(r.CatalogKey, key) {
						r.CatalogKey = entry.Key
					}
					if _, err := tx.ExecContext(ctx, `UPDATE filter_lists SET category = ?, catalog_key = ? WHERE id = ?`, r.Category, r.CatalogKey, r.ID); err != nil {
						return err
					}
				}
				if !r.Enabled {
					touch(r)
					if _, err := tx.ExecContext(ctx, `UPDATE filter_lists SET enabled = 1 WHERE id = ?`, r.ID); err != nil {
						return err
					}
					r.Enabled = true
				}
				if !slices.Contains(r.GroupIDs, group) {
					touch(r)
					if _, err := tx.ExecContext(ctx, `INSERT INTO filter_list_groups (list_id, group_id) VALUES (?, ?)`, r.ID, group); err != nil {
						return err
					}
					r.GroupIDs = append(r.GroupIDs, group)
					slices.Sort(r.GroupIDs)
				}
			}
		}
		return nil
	})
	if err != nil {
		return PresetChange{Lists: []List{}}, err
	}
	var sync []*listRT
	var ids []int64
	for _, r := range rows {
		_, changed := before[r.ID]
		if changed || slices.Contains(change.Created, r.ID) {
			ids = append(ids, r.ID)
			if changed {
				change.before = append(change.before, before[r.ID])
			}
		}
		if changed || slices.Contains(change.Created, r.ID) || boundToAny(r.CatalogKey, want) {
			sync = append(sync, r)
		}
	}
	change.Lists = e.syncLists(sync, ids)
	return change, nil
}

// boundToAny reports whether a list with catalogKey is bound to a key of
// one of the switches in want.
func boundToAny(catalogKey string, want map[string]bool) bool {
	for _, p := range categoryPresets {
		if _, ok := want[p.Switch]; ok && slices.ContainsFunc(p.Keys, func(k string) bool { return boundTo(catalogKey, k) }) {
			return true
		}
	}
	return false
}

// pickBound returns the list a switch uses for key: a bound list that is
// already enabled for group, else the first bound list, else the list at
// the entry's URL (a list the user added before it had a catalogue key).
func pickBound(rows []*listRT, key, url string, group int64) *listRT {
	var first *listRT
	for _, r := range rows {
		if !boundTo(r.CatalogKey, key) {
			continue
		}
		if r.Enabled && slices.Contains(r.GroupIDs, group) {
			return r
		}
		if first == nil {
			first = r
		}
	}
	if first != nil {
		return first
	}
	for _, r := range rows {
		if r.URL == url {
			return r
		}
	}
	return nil
}

// RevertPresets undoes the list changes of c, a SetPresets call that must
// not stand (the parental configuration could not be saved): it deletes
// the lists c created and gives the lists c changed back their enabled
// flag, category, catalogue key and membership of c's group (their other
// groups are left alone). It returns the IDs of the deleted lists and the
// restored lists; a list deleted meanwhile is skipped.
func (e *Engine) RevertPresets(ctx context.Context, c PresetChange) (deleted []int64, restored []List, err error) {
	if len(c.Created) == 0 && len(c.before) == 0 {
		return nil, []List{}, nil
	}
	e.listMu.Lock()
	defer e.listMu.Unlock()
	var rows []*listRT
	var ids []int64
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range c.Created {
			res, err := tx.ExecContext(ctx, `DELETE FROM filter_lists WHERE id = ?`, id)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n > 0 {
				deleted = append(deleted, id)
			}
		}
		for _, b := range c.before {
			res, err := tx.ExecContext(ctx, `UPDATE filter_lists SET enabled = ?, category = ?, catalog_key = ? WHERE id = ?`,
				b.Enabled, b.Category, b.CatalogKey, b.ID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				continue // deleted meanwhile
			}
			query := `DELETE FROM filter_list_groups WHERE list_id = ? AND group_id = ?`
			if slices.Contains(b.GroupIDs, c.group) {
				query = `INSERT OR IGNORE INTO filter_list_groups (list_id, group_id) SELECT ?, id FROM client_groups WHERE id = ?`
			}
			if _, err := tx.ExecContext(ctx, query, b.ID, c.group); err != nil {
				return err
			}
			ids = append(ids, b.ID)
		}
		all, err := loadLists(ctx, tx)
		if err != nil {
			return err
		}
		for _, r := range all {
			if slices.Contains(ids, r.ID) {
				rows = append(rows, r)
			}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(deleted) > 0 {
		e.mu.Lock()
		for _, id := range deleted {
			if _, ok := e.lists[id]; ok {
				delete(e.lists, id)
				e.parseGen++
			}
			e.removeFiles(id)
		}
		e.publishLocked(nil, nil, nil)
		e.mu.Unlock()
		e.requestCompile()
	}
	return deleted, e.syncLists(rows, ids), nil
}

// syncLists brings the in-memory lists in line with rows, read and changed
// in a transaction that has committed (so a cancelled request cannot leave
// memory behind the database, and nothing is read again). A missing list
// is added; a changed one takes the row's enabled flag, category,
// catalogue key and groups, and is parsed or downloaded again when it was
// enabled or its parse format changed. It returns the lists ids as they
// are now. The caller holds e.listMu.
func (e *Engine) syncLists(rows []*listRT, ids []int64) []List {
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	for _, row := range rows {
		rt, ok := e.lists[row.ID]
		switch {
		case !ok:
			rt = row
			rt.jitter = newJitter()
			e.lists[rt.ID] = rt
			if rt.Enabled {
				e.wantCopyLocked(rt)
			}
		case rt.Enabled == row.Enabled && rt.Category == row.Category && rt.CatalogKey == row.CatalogKey &&
			slices.Equal(rt.GroupIDs, row.GroupIDs):
			continue
		default:
			oldFormat, wasEnabled := rt.format(), rt.Enabled
			rt.Enabled, rt.Category, rt.CatalogKey, rt.GroupIDs = row.Enabled, row.Category, row.CatalogKey, nonNil(row.GroupIDs)
			switch {
			case !rt.Enabled:
				rt.wantDownload, rt.wantReparse = false, false
				if rt.parsed != nil {
					rt.parsed = nil
					e.parseGen++
				}
			case !wasEnabled || rt.format() != oldFormat:
				e.wantCopyLocked(rt)
			}
		}
		changed = true
	}
	if changed {
		e.publishLocked(nil, nil, nil)
		e.requestCompile()
		e.signal()
	}
	out := make([]List, 0, len(ids))
	for _, id := range ids {
		if rt, ok := e.lists[id]; ok {
			out = append(out, rt.copy())
		}
	}
	return out
}

// wantCopyLocked asks the update loop to parse the cached copy of rt, or to
// download one when there is none. e.mu must be held.
func (e *Engine) wantCopyLocked(rt *listRT) {
	if e.hasCache(rt.ID) {
		rt.wantReparse = true
	} else {
		rt.wantDownload = true
	}
}
