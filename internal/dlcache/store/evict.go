package cachestore

import (
	"context"
	"log/slog"
	"math"
	"time"
)

// Evict applies retention and size limits once. Calls are serialised; a
// concurrent call waits and then runs its own pass.
//
// Order: failed removals are retried, pending index writes and statistics
// are flushed, then (1) objects inactive for longer than MaxAge are removed,
// (2) while cachedBytes > MaxBytes least-recently-used objects are removed
// down to 95 % of MaxBytes, (3) if FreeBytes() < MinFreeBytes, LRU objects
// are removed until the deficit MinFreeBytes·105/100 − free is covered. The
// free-space sample predates this run, so bytes freed by (1) and (2) count
// towards that deficit. Pinned objects and objects of pinned groups are
// never evicted; objects in use (Use) and objects used since the last
// statistics flush are skipped.
// Full is set when (2) or (3) could not be satisfied.
func (s *Store) Evict(ctx context.Context, p Policy) (EvictResult, error) {
	res := EvictResult{Reasons: map[string]int64{}}
	if !s.enter() {
		return res, ErrClosed
	}
	defer s.leave()
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	select {
	case s.evictSem <- struct{}{}:
	case <-ctx.Done():
		return res, ctx.Err()
	}
	defer func() { <-s.evictSem }()

	s.retryRemovals(ctx)
	if err := s.flush(ctx, true); err != nil {
		return res, err
	}
	if p.MaxAge > 0 {
		cutoff := time.Now().Add(-p.MaxAge).UnixMilli()
		if err := s.evictInactive(ctx, cutoff, &res); err != nil {
			return res, err
		}
	}
	if p.MaxBytes > 0 {
		if cached := s.usage.cached.Load(); cached > p.MaxBytes {
			deficit := cached - (p.MaxBytes - p.MaxBytes/20)
			freed, err := s.evictLRU(ctx, deficit, ReasonSize, &res)
			if err != nil {
				return res, err
			}
			res.Full = res.Full || freed < deficit
		}
	}
	if p.MinFreeBytes > 0 && p.FreeBytes != nil {
		free, err := p.FreeBytes()
		switch {
		case err != nil:
			s.log.Debug("free space unknown; skipping the min-free rule", slog.Any("err", err))
		case free < uint64(p.MinFreeBytes):
			target := p.MinFreeBytes
			if target <= math.MaxInt64/105 {
				target = target * 105 / 100
			}
			deficit := target - int64(free) - res.Bytes
			if deficit > 0 {
				freed, err := s.evictLRU(ctx, deficit, ReasonMinFree, &res)
				if err != nil {
					return res, err
				}
				res.Full = res.Full || freed < deficit
			}
		}
	}
	return res, nil
}

func (res *EvictResult) add(reason string, objects int, bytes int64) {
	if objects == 0 {
		return
	}
	res.Objects += int64(objects)
	res.Bytes += bytes
	res.Reasons[reason] += int64(objects)
}

// candidate is an eviction candidate in LRU order.
type candidate struct {
	id         string
	cached     int64
	lastAccess int64
}

// evictable excludes pinned objects and objects of pinned groups.
const evictable = `o.pinned = 0 AND NOT EXISTS (SELECT 1 FROM store_pinned_groups p
	WHERE p.service = o.service AND p.group_key = o.group_key)`

// candidates returns the next evictable objects after the keyset cursor
// (lastAccess, id) in LRU order.
func (s *Store) candidates(ctx context.Context, extra string, afterLA int64, afterID string, args ...any) ([]candidate, error) {
	q := `SELECT o.id, o.cached_bytes, o.last_access FROM store_objects o
		WHERE ` + evictable + extra + ` AND (o.last_access > ? OR (o.last_access = ? AND o.id > ?))
		ORDER BY o.last_access, o.id LIMIT ?`
	rows, err := s.db.R.QueryContext(ctx, q, append(args, afterLA, afterLA, afterID, removeBatch)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.cached, &c.lastAccess); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// evictInactive removes unpinned objects not accessed since cutoff.
func (s *Store) evictInactive(ctx context.Context, cutoff int64, res *EvictResult) error {
	keep := func(o *Object) bool {
		return o.Pinned || o.LastAccess.UnixMilli() >= cutoff || s.touched(o.ID) || s.used(o.ID)
	}
	la, id := int64(math.MinInt64), ""
	for {
		cs, err := s.candidates(ctx, ` AND o.last_access < ?`, la, id, cutoff)
		if err != nil || len(cs) == 0 {
			return err
		}
		ids := make([]string, len(cs))
		for i, c := range cs {
			ids[i] = c.id
		}
		objs, bytes, err := s.removeObjects(ctx, ids, ReasonInactive, keep)
		res.add(ReasonInactive, len(objs), bytes)
		if err != nil {
			return err
		}
		la, id = cs[len(cs)-1].lastAccess, cs[len(cs)-1].id
	}
}

// evictLRU removes least-recently-used unpinned objects until at least
// deficit bytes were freed or nothing evictable is left; returns the bytes
// freed.
func (s *Store) evictLRU(ctx context.Context, deficit int64, reason string, res *EvictResult) (int64, error) {
	keep := func(o *Object) bool { return o.Pinned || s.touched(o.ID) || s.used(o.ID) }
	var freed int64
	la, id := int64(math.MinInt64), ""
	for freed < deficit {
		cs, err := s.candidates(ctx, ` AND o.cached_bytes > 0`, la, id)
		if err != nil {
			return freed, err
		}
		if len(cs) == 0 {
			break
		}
		// Take just enough candidates to cover the remaining deficit.
		var ids []string
		var planned int64
		for _, c := range cs {
			ids = append(ids, c.id)
			la, id = c.lastAccess, c.id
			if planned += c.cached; freed+planned >= deficit {
				break
			}
		}
		objs, bytes, err := s.removeObjects(ctx, ids, reason, keep)
		freed += bytes
		res.add(reason, len(objs), bytes)
		if err != nil {
			return freed, err
		}
	}
	return freed, nil
}
