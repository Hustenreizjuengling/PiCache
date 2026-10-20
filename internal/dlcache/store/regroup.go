package cachestore

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
)

// Regrouping: an object keeps the group key it got when it was cached. When
// the grouping rules change (Options.GroupRules is raised), Open moves, in
// the background and once, every object whose key the current rules derive
// differently into its new group: the aggregates move with it as with
// SetMeta under a new key, and the new group of a pinned group is pinned
// too. The rules version is stored in store_meta ('group_rules') after all
// moves are committed, so an interrupted run starts again at the next Open.

const (
	regroupPage = 500 // objects read per query
	// regroupBacklog keeps the write backlog of a run small, so it never
	// makes SetMeta or WriteSlice fail with errBacklog.
	regroupBacklog = maxPendingOps / 4
)

// regroupOnce regroups when the stored rules version is older than
// Options.GroupRules, then stores the version.
func (s *Store) regroupOnce(ctx context.Context) {
	var v int64
	err := s.db.R.QueryRowContext(ctx, `SELECT num FROM store_meta WHERE key = 'group_rules'`).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Warn("cannot read the grouping rules version of the cache index", slog.Any("err", err))
		return
	}
	if v >= int64(s.opt.GroupRules) {
		return
	}
	start := time.Now()
	moved, err := s.regroup(ctx)
	if err == nil {
		err = s.flush(ctx, false) // the moves are committed before the version says so
	}
	if err == nil {
		_, err = s.db.W.ExecContext(ctx, `INSERT INTO store_meta (key, num) VALUES ('group_rules', ?)
			ON CONFLICT (key) DO UPDATE SET num = excluded.num`, s.opt.GroupRules)
	}
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.log.Warn("moving cached objects to the current content groups failed; retrying at the next start", slog.Any("err", err))
		}
		return
	}
	if moved > 0 {
		s.log.Info("cached objects moved to the current content groups", slog.Int64("objects", moved),
			slog.Duration("duration", time.Since(start)))
	}
}

// regroup moves every object whose group key the current rules derive
// differently and returns how many moved.
func (s *Store) regroup(ctx context.Context) (int64, error) {
	moves := map[groupMove]bool{}
	var moved int64
	after := ""
	for {
		page, err := s.regroupPage(ctx, after)
		if err != nil || len(page) == 0 {
			if err == nil {
				err = s.movePins(ctx, moves)
			}
			return moved, err
		}
		after = page[len(page)-1].id
		for _, o := range page {
			key := s.groupKey(o.service, o.host, o.path)
			if !validGroupKey(key) {
				key = o.service + ":" + o.host
			}
			if key == o.groupKey {
				continue
			}
			ok, err := s.moveObject(ctx, o.id, o.groupKey, key)
			if err != nil {
				return moved, err
			}
			if ok {
				moved++
				moves[groupMove{o.service, o.groupKey, key}] = true
			}
		}
		for s.pendingLen() >= regroupBacklog {
			s.kickFlush()
			select {
			case <-ctx.Done():
				return moved, ctx.Err()
			case <-time.After(flushEvery):
			}
		}
	}
}

type regroupRow struct{ id, service, host, path, groupKey string }

// groupMove is a move of objects of a service from one group to another.
type groupMove struct{ service, from, to string }

// regroupPage reads the next objects after the ID after.
func (s *Store) regroupPage(ctx context.Context, after string) ([]regroupRow, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, service, host, path, group_key FROM store_objects
		WHERE id > ? ORDER BY id LIMIT ?`, after, regroupPage)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []regroupRow
	for rows.Next() {
		var o regroupRow
		if err := rows.Scan(&o.id, &o.service, &o.host, &o.path, &o.groupKey); err != nil {
			return nil, err
		}
		page = append(page, o)
	}
	return page, rows.Err()
}

// moveObject gives an object the group key to (same generation) while it
// still has the key from; false when it changed or is gone meanwhile.
func (s *Store) moveObject(ctx context.Context, id, from, to string) (bool, error) {
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	cur, err := s.lookupLocked(ctx, id)
	if err != nil || cur == nil || cur.busy != nil || cur.groupKey != from {
		return false, err
	}
	next := &entry{gen: cur.gen, total: cur.total, service: cur.service, host: cur.host, path: cur.path, groupKey: to,
		header: cur.header, hdrSize: cur.hdrSize, noSlice: cur.noSlice, present: cur.present}
	s.heads.set(id, next)
	s.enqueue(indexOp{kind: opPut, id: id, e: next, gen: next.gen})
	return true, nil
}

// movePins pins the new group of a pinned group whose objects moved (with
// all its objects, as SetGroupPinned does) and unpins the old one once it
// has no objects left; the moved objects keep their pin.
func (s *Store) movePins(ctx context.Context, moves map[groupMove]bool) error {
	if len(moves) == 0 {
		return nil
	}
	if err := s.flush(ctx, false); err != nil {
		return err
	}
	return s.db.Tx(ctx, func(tx *sql.Tx) error {
		for m := range moves {
			res, err := tx.ExecContext(ctx, `INSERT INTO store_pinned_groups (service, group_key, created_at)
				SELECT service, ?, ? FROM store_pinned_groups WHERE service = ? AND group_key = ?
				ON CONFLICT DO NOTHING`, m.to, db.NowMs(), m.service, m.from)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE store_objects SET pinned = 1 WHERE service = ? AND group_key = ?`,
					m.service, m.to); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM store_pinned_groups WHERE service = ? AND group_key = ?
				AND NOT EXISTS (SELECT 1 FROM store_objects WHERE service = ? AND group_key = ?)`,
				m.service, m.from, m.service, m.from); err != nil {
				return err
			}
		}
		return nil
	})
}
