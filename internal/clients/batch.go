package clients

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
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
func requireIDs(ctx context.Context, tx *sql.Tx, table, what string, ids []int64) error {
	in, args := inList(ids)
	rows, err := tx.QueryContext(ctx, `SELECT id FROM `+table+` WHERE id`+in, args...)
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

// BatchGroups deletes, enables or disables the groups ids ("delete",
// "enable", "disable") in one transaction, all or nothing: an unknown id is
// apperr.NotFound, the Default group in a delete apperr.Forbidden. Deleting
// moves the clients left without a group into the Default group, like
// DeleteGroup. The change listeners run once. It returns the number of
// groups whose state changed (deleting counts every id).
func (r *Registry) BatchGroups(ctx context.Context, action string, ids []int64) (int, error) {
	if action == "delete" && slices.Contains(ids, DefaultGroupID) {
		return 0, errDefaultGroup
	}
	changed := 0
	err := r.write(ctx, func(tx *sql.Tx) error {
		if err := requireIDs(ctx, tx, "client_groups", "group", ids); err != nil {
			return err
		}
		in, args := inList(ids)
		switch action {
		case "delete":
			if _, err := tx.ExecContext(ctx, `DELETE FROM client_groups WHERE id`+in, args...); err != nil {
				return err
			}
			changed = len(ids)
			return adoptOrphans(ctx, tx)
		case "enable", "disable":
			on := action == "enable"
			res, err := tx.ExecContext(ctx, `UPDATE client_groups SET enabled = ? WHERE enabled != ? AND id`+in,
				append([]any{on, on}, args...)...)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			changed = int(n)
			return err
		}
		return apperr.Invalid("action", "must be delete, enable or disable")
	})
	if err != nil {
		return 0, err
	}
	return changed, nil
}

// BatchDeleteClients deletes the clients ids in one transaction, all or
// nothing (an unknown id is apperr.NotFound); the change listeners run once.
func (r *Registry) BatchDeleteClients(ctx context.Context, ids []int64) (int, error) {
	err := r.write(ctx, func(tx *sql.Tx) error {
		if err := requireIDs(ctx, tx, "client_clients", "client", ids); err != nil {
			return err
		}
		in, args := inList(ids)
		_, err := tx.ExecContext(ctx, `DELETE FROM client_clients WHERE id`+in, args...)
		return err
	})
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}
