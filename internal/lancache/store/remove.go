package cachestore

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// Removal reasons (ARCHITECTURE 9.3).
const (
	ReasonInactive    = "inactive"
	ReasonSize        = "size"
	ReasonMinFree     = "min-free"
	ReasonManual      = "manual"
	ReasonCorrupt     = "corrupt"
	ReasonInvalidated = "invalidated"
)

const (
	removeBatch = 256  // objects per removal transaction
	maxRetries  = 4096 // bound of the failed-removal list
)

// retryRemoval is a slice file whose removal failed (Windows keeps open
// files); the next Evict retries it.
type retryRemoval struct {
	id  string
	idx int64
}

// removal is an object marked for removal (tombstone installed).
type removal struct {
	id   string
	tomb *entry
}

// removeObjects removes objects: tombstone (no new reads or writes) → flush
// pending index writes → delete the index rows (one transaction) → remove
// the files → drop the tombstones → OnEvict callbacks. keep (optional)
// re-checks each row inside the transaction; kept objects stay untouched.
func (s *Store) removeObjects(ctx context.Context, ids []string, reason string, keep func(*Object) bool) ([]Object, int64, error) {
	marked := make([]removal, 0, len(ids))
	for _, id := range ids {
		mu := s.lockFor(id)
		mu.Lock()
		e, err := s.lookupLocked(ctx, id)
		if err != nil {
			mu.Unlock()
			s.releaseTombs(marked, nil, false)
			return nil, 0, err
		}
		if e != nil && e.busy == nil {
			marked = append(marked, removal{id: id, tomb: s.heads.tomb(id, e)})
		}
		mu.Unlock()
	}
	if len(marked) == 0 {
		return nil, 0, nil
	}

	type gone struct {
		o    Object
		idxs []int64
		prev *entry
	}
	var (
		out []gone
		d   usageDelta
	)
	// Flush and delete under flushMu: all index writes made before the
	// tombstones are committed first, and the usage counters stay exact.
	s.flushMu.Lock()
	if err := s.flushLocked(ctx, false); err != nil {
		s.flushMu.Unlock()
		s.releaseTombs(marked, nil, false)
		return nil, 0, err
	}
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		w := s.newIndexTx(ctx, tx)
		for _, m := range marked {
			o, gen, found, err := w.readObject(m.id)
			if err != nil {
				return err
			}
			if !found || gen != m.tomb.gen || (keep != nil && keep(&o)) {
				continue
			}
			idxs, err := w.deleteObject(&o)
			if err != nil {
				return err
			}
			out = append(out, gone{o: o, idxs: idxs, prev: m.tomb.prev})
		}
		d = w.d
		return nil
	})
	if err == nil {
		s.usage.add(d)
	}
	s.flushMu.Unlock()
	if err != nil {
		s.releaseTombs(marked, nil, true)
		return nil, 0, err
	}

	removed := make(map[string]bool, len(out))
	objs := make([]Object, 0, len(out))
	var freed int64
	for _, g := range out {
		idxs := append(g.idxs, g.prev.indexes()...)
		slices.Sort(idxs)
		for _, idx := range slices.Compact(idxs) {
			s.removeFile(ctx, g.o.ID, idx)
		}
		removed[g.o.ID] = true
		objs = append(objs, g.o)
		freed += g.o.CachedBytes
	}
	s.releaseTombs(marked, removed, true)
	s.notifyRemoved(objs, reason)
	return objs, freed, nil
}

// releaseTombs drops the tombstones; objects not in removed become current
// again. flushed reports that all index writes before the tombstones were
// committed.
func (s *Store) releaseTombs(marked []removal, removed map[string]bool, flushed bool) {
	for _, m := range marked {
		mu := s.lockFor(m.id)
		mu.Lock()
		s.heads.untomb(m.id, m.tomb, !removed[m.id], !flushed && m.tomb.prevDirty)
		mu.Unlock()
		close(m.tomb.busy)
	}
}

// removeFile deletes a slice file that is no longer referenced by the index.
// If that fails (Windows: the file is still open) it is retried by the next
// Evict.
func (s *Store) removeFile(ctx context.Context, id string, idx int64) {
	if s.acquireIO(ctx) != nil {
		s.addRetry(retryRemoval{id: id, idx: idx})
		return
	}
	err := s.root.Remove(sliceName(id, idx))
	s.releaseIO()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.addRetry(retryRemoval{id: id, idx: idx})
	}
}

func (s *Store) addRetry(r retryRemoval) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	if len(s.retry) < maxRetries { // beyond, the file stays an orphan until Verify
		s.retry[r] = struct{}{}
	}
}

// pendingRemoval reports whether a slice file is waiting to be removed.
func (s *Store) pendingRemoval(id string, idx int64) bool {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	_, ok := s.retry[retryRemoval{id: id, idx: idx}]
	return ok
}

// retryRemovals retries failed removals. A file is only removed while its
// object does not reference that slice (checked under the object lock, so a
// slice written again in the meantime is kept).
func (s *Store) retryRemovals(ctx context.Context) {
	s.retryMu.Lock()
	list := make([]retryRemoval, 0, len(s.retry))
	for r := range s.retry {
		list = append(list, r)
	}
	clear(s.retry)
	s.retryMu.Unlock()
	for i, r := range list {
		if s.acquireIO(ctx) != nil {
			for _, rest := range list[i:] {
				s.addRetry(rest)
			}
			return
		}
		mu := s.lockFor(r.id)
		mu.Lock()
		e, err := s.lookupLocked(ctx, r.id)
		again := err != nil || e != nil && e.busy != nil
		if !again && (e == nil || !e.has(r.idx)) {
			if err := s.root.Remove(sliceName(r.id, r.idx)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				again = true
			}
		}
		mu.Unlock()
		s.releaseIO()
		if again {
			s.addRetry(r)
		}
	}
}

func validReason(r string) bool {
	switch r {
	case ReasonInactive, ReasonSize, ReasonMinFree, ReasonManual, ReasonCorrupt, ReasonInvalidated:
		return true
	}
	return false
}

// Invalidate deletes all slices of an object and its record.
func (s *Store) Invalidate(ctx context.Context, id string, reason string) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return errInvalidID
	}
	if !validReason(reason) {
		reason = ReasonInvalidated
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, _, err := s.removeObjects(ctx, []string{id}, reason, nil)
	return err
}

// DeleteObject purges one object (apperr.Invalid for a malformed id).
func (s *Store) DeleteObject(ctx context.Context, id string) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return errInvalidID
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	objs, _, err := s.removeObjects(ctx, []string{id}, ReasonManual, nil)
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return apperr.NotFound("object", id)
	}
	return nil
}

// DeleteGroup purges a content group; returns bytes freed.
func (s *Store) DeleteGroup(ctx context.Context, service, groupKey string) (int64, error) {
	if !s.enter() {
		return 0, ErrClosed
	}
	defer s.leave()
	if !validService(service) {
		return 0, apperr.Invalid("service", "invalid service id")
	}
	if !validGroupKey(groupKey) {
		return 0, apperr.Invalid("key", "invalid group key")
	}
	return s.purge(ctx, `service = ? AND group_key = ?`, service, groupKey)
}

// DeleteService purges all content of a service; returns bytes freed.
func (s *Store) DeleteService(ctx context.Context, service string) (int64, error) {
	if !s.enter() {
		return 0, ErrClosed
	}
	defer s.leave()
	if !validService(service) {
		return 0, apperr.Invalid("service", "invalid service id")
	}
	return s.purge(ctx, `service = ?`, service)
}

// purge removes every object matching where (a constant SQL fragment).
func (s *Store) purge(ctx context.Context, where string, args ...any) (int64, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if err := s.flush(ctx, false); err != nil { // make objects created just now visible
		return 0, err
	}
	var freed int64
	after := ""
	for {
		ids, err := s.queryIDs(ctx, `SELECT id FROM store_objects WHERE `+where+` AND id > ? ORDER BY id LIMIT ?`,
			append(args, after, removeBatch)...)
		if err != nil || len(ids) == 0 {
			return freed, err
		}
		_, n, err := s.removeObjects(ctx, ids, ReasonManual, nil)
		freed += n
		if err != nil {
			return freed, err
		}
		after = ids[len(ids)-1]
	}
}

func (s *Store) queryIDs(ctx context.Context, q string, args ...any) ([]string, error) {
	rows, err := s.db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
