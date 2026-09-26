package filter

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// Batch actions (POST …/batch).
const (
	BatchDelete  = "delete"
	BatchEnable  = "enable"
	BatchDisable = "disable"
)

// inList returns " IN (?,?,…)" and the arguments for ids.
func inList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return " IN (?" + strings.Repeat(",?", len(ids)-1) + ")", args
}

// requireIDs returns apperr.NotFound naming the first id of ids that table
// (a constant) does not hold.
func requireIDs(ctx context.Context, q querier, table, what string, ids []int64) error {
	in, args := inList(ids)
	rows, err := q.QueryContext(ctx, `SELECT id FROM `+table+` WHERE id`+in, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	have := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		have[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if !have[id] {
			return apperr.NotFound(what, id)
		}
	}
	return nil
}

// batchRows deletes, enables or disables the rows ids of table (a
// constant) in tx, all or nothing (an unknown id is apperr.NotFound), and
// returns how many rows changed (deleting counts every id). The caller
// validated action and ids.
func batchRows(ctx context.Context, tx *sql.Tx, table, what, action string, ids []int64, timestamps bool) (int, error) {
	if err := requireIDs(ctx, tx, table, what, ids); err != nil {
		return 0, err
	}
	in, args := inList(ids)
	var query string
	switch action {
	case BatchDelete:
		query = `DELETE FROM ` + table + ` WHERE id` + in
	case BatchEnable, BatchDisable:
		on := 0
		if action == BatchEnable {
			on = 1
		}
		set := fmt.Sprintf("enabled = %d", on)
		if timestamps {
			set += fmt.Sprintf(", updated_at = %d", db.NowMs())
		}
		query = `UPDATE ` + table + ` SET ` + set + ` WHERE enabled != ` + fmt.Sprint(on) + ` AND id` + in
	default:
		return 0, apperr.Invalid("action", "must be delete, enable or disable")
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	if action == BatchDelete {
		return len(ids), nil
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// BatchRules deletes, enables or disables the rules ids in one
// transaction (all or nothing) and rebuilds the rule matcher once.
func (e *Engine) BatchRules(ctx context.Context, action string, ids []int64) (int, error) {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	var changed int
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = batchRows(ctx, tx, "filter_rules", "rule", action, ids, true)
		return err
	})
	if err != nil {
		return 0, err
	}
	return changed, e.rebuildRulesLocked(ctx)
}

// BatchIPRules deletes, enables or disables the IP rules ids in one
// transaction (all or nothing) and rebuilds their matcher once.
func (e *Engine) BatchIPRules(ctx context.Context, action string, ids []int64) (int, error) {
	e.ruleMu.Lock()
	defer e.ruleMu.Unlock()
	var changed int
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = batchRows(ctx, tx, "filter_ip_rules", "IP rule", action, ids, true)
		return err
	})
	if err != nil {
		return 0, err
	}
	return changed, e.rebuildIPRulesLocked(ctx)
}

// errBudget is the conflict of enabling lists beyond the entry budget
// (field force: resend with force to enable them anyway).
func errBudget(adds, total int) error {
	return &apperr.Error{Kind: apperr.KindConflict, Field: "force", Message: fmt.Sprintf(
		"enabling these lists adds about %d entries; the blocklists would hold about %d, more than %d; a small host may run short of memory",
		adds, total, EntryBudget)}
}

// listEstimate is the entry estimate of a list that is about to be
// enabled: its stored entry count when it has one, else the catalogue
// entry's for the same URL, else 0.
func listEstimate(rt *listRT) int {
	if rt.Entries > 0 {
		return rt.Entries
	}
	if c, ok := catalogByURL(rt.URL); ok {
		return c.Entries
	}
	return 0
}

// BatchLists deletes, enables or disables the lists ids in one transaction
// (all or nothing). Enabling is refused (apperr.Conflict with field force)
// when the compiled entries plus the estimates of the lists being enabled
// exceed EntryBudget, unless force. Deleted lists lose their cached copies
// after the commit; the matcher is recompiled once.
func (e *Engine) BatchLists(ctx context.Context, action string, ids []int64, force bool) (int, error) {
	e.listMu.Lock()
	defer e.listMu.Unlock()
	if action == BatchEnable && !force {
		adds := 0
		e.mu.Lock()
		for _, id := range ids {
			if rt, ok := e.lists[id]; ok && !rt.Enabled {
				adds += listEstimate(rt)
			}
		}
		e.mu.Unlock()
		if total := e.snap.Load().lists.entries + adds; adds > 0 && total > EntryBudget {
			return 0, errBudget(adds, total)
		}
	}
	var changed int
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = batchRows(ctx, tx, "filter_lists", "list", action, ids, false)
		return err
	})
	if err != nil {
		return 0, err
	}
	e.mu.Lock()
	for _, id := range ids {
		rt, ok := e.lists[id]
		if !ok {
			continue
		}
		switch action {
		case BatchDelete:
			delete(e.lists, id)
			if rt.parsed != nil {
				e.parseGen++
			}
			e.removeFiles(id)
		case BatchDisable:
			rt.Enabled, rt.wantDownload, rt.wantReparse = false, false, false
			if rt.parsed != nil {
				rt.parsed = nil
				e.parseGen++
			}
		case BatchEnable:
			if !rt.Enabled {
				rt.Enabled = true
				e.wantCopyLocked(rt)
			}
		}
	}
	e.parseGen++ // the set of enabled lists changed
	e.publishLocked(nil, nil, nil)
	e.mu.Unlock()
	e.requestCompile()
	e.signal()
	e.log.Info("lists changed", slog.String("action", action), slog.Int("lists", len(ids)), slog.Int("changed", changed))
	return changed, nil
}
