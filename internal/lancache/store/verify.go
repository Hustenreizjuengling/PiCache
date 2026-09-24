package cachestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"time"
)

// Verify scans the slice tree and reconciles it with the index (checks
// headers, sizes and crc32c). With repair it fixes the index and deletes
// corrupt files; otherwise it only reports.
//
// The tree is walked shard by shard (slices/<h0h1>/<h2h3>); each shard is
// compared with the index rows of the same id prefix, so memory stays
// bounded by the size of one shard. Every change is re-checked under the
// object lock, so Verify runs safely next to normal traffic.
func (s *Store) Verify(ctx context.Context, repair bool, progress func(VerifyProgress)) (VerifyResult, error) {
	if !s.enter() {
		return VerifyResult{}, ErrClosed
	}
	defer s.leave()
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	return s.verify(ctx, repair, progress)
}

func (s *Store) verify(ctx context.Context, repair bool, progress func(VerifyProgress)) (VerifyResult, error) {
	start := time.Now()
	select {
	case s.verifySem <- struct{}{}:
	case <-ctx.Done():
		return VerifyResult{}, ctx.Err()
	}
	defer func() { <-s.verifySem }()
	v := &verifier{s: s, repair: repair, progress: progress, last: time.Now(), buf: make([]byte, 256<<10)}
	err := v.run(ctx)
	if progress != nil {
		progress(v.p)
	}
	return VerifyResult{VerifyProgress: v.p, Duration: time.Since(start)}, err
}

type verifier struct {
	s        *Store
	repair   bool
	progress func(VerifyProgress)
	p        VerifyProgress
	last     time.Time
	buf      []byte
}

// shardObj is the index state of one object of the shard being verified.
type shardObj struct {
	gen    uint64
	slices map[int64]bool // idx → a valid file was seen
}

func (v *verifier) run(ctx context.Context) error {
	if _, err := v.s.root.Stat(MarkerFile); err != nil {
		return fmt.Errorf("cachestore: store root unavailable: %w", err)
	}
	if err := v.s.flush(ctx, false); err != nil {
		return err
	}
	top, err := v.s.shardDirs(ctx, "slices")
	if err != nil {
		return err
	}
	indexed, err := v.s.indexPrefixes(ctx)
	if err != nil {
		return err
	}
	for a := range 256 {
		d1 := fmt.Sprintf("%02x", a)
		var sub map[string]bool
		if top[d1] {
			if sub, err = v.s.shardDirs(ctx, "slices/"+d1); err != nil {
				return err
			}
		}
		for b := range 256 {
			if err := ctx.Err(); err != nil {
				return err
			}
			d2 := fmt.Sprintf("%02x", b)
			if !sub[d2] && !indexed[d1+d2] {
				continue
			}
			if err := v.shard(ctx, d1, d2, sub[d2]); err != nil {
				return err
			}
		}
	}
	if v.repair {
		v.s.cleanTmp(ctx, time.Hour)
		return v.s.recompute(ctx)
	}
	return nil
}

// shardDirs lists the shard subdirectories (two lowercase hex chars) of dir.
func (s *Store) shardDirs(ctx context.Context, dir string) (map[string]bool, error) {
	out := map[string]bool{}
	d, err := s.root.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer d.Close()
	for ctx.Err() == nil {
		ents, err := d.ReadDir(256)
		for _, de := range ents {
			if de.IsDir() && isShard(de.Name()) {
				out[de.Name()] = true
			}
		}
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, ctx.Err()
}

// shard verifies one leaf directory against the index rows of its prefix.
func (v *verifier) shard(ctx context.Context, d1, d2 string, exists bool) error {
	prefix := d1 + d2
	idx, err := v.s.indexShard(ctx, prefix)
	if err != nil {
		return err
	}
	if exists {
		dir := "slices/" + d1 + "/" + d2
		d, err := v.s.root.Open(dir)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err == nil {
			err = v.files(ctx, d, prefix, idx)
			d.Close()
			if err != nil {
				return err
			}
		}
	}
	for id, o := range idx {
		for i, seen := range o.slices {
			if !seen {
				if err := v.missing(ctx, id, o.gen, i); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (v *verifier) files(ctx context.Context, d *os.File, prefix string, idx map[string]*shardObj) error {
	for {
		ents, rerr := d.ReadDir(256)
		for _, de := range ents {
			if err := ctx.Err(); err != nil {
				return err
			}
			id, i, ok := parseSliceFile(de.Name())
			if !ok || id[:4] != prefix || !de.Type().IsRegular() {
				continue // not a slice file of this shard: left alone
			}
			if err := v.file(ctx, id, i, idx); err != nil {
				return err
			}
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// indexPrefixes returns the shard prefixes (first four id characters) that
// have objects in the index (at most 65 536).
func (s *Store) indexPrefixes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.R.QueryContext(ctx, `SELECT DISTINCT substr(id, 1, 4) FROM store_objects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// indexShard loads the index rows of all objects whose id starts with prefix.
func (s *Store) indexShard(ctx context.Context, prefix string) (map[string]*shardObj, error) {
	lo, hi := prefix, prefix+"g" // every id with this prefix sorts in [lo, hi)
	out := map[string]*shardObj{}
	rows, err := s.db.R.QueryContext(ctx, `SELECT id, gen FROM store_objects WHERE id >= ? AND id < ?`, lo, hi)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		var gen int64
		if err := rows.Scan(&id, &gen); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = &shardObj{gen: uint64(gen), slices: map[int64]bool{}}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.db.R.QueryContext(ctx, `SELECT object_id, idx FROM store_slices WHERE object_id >= ? AND object_id < ?`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var i int64
		if err := rows.Scan(&id, &i); err != nil {
			return nil, err
		}
		if o := out[id]; o != nil {
			o.slices[i] = false
		}
	}
	return out, rows.Err()
}

// file checks one slice file and reconciles it with the index.
func (v *verifier) file(ctx context.Context, id string, i int64, idx map[string]*shardObj) error {
	v.p.FilesScanned++
	v.report()
	if err := v.s.acquireIO(ctx); err != nil {
		return err
	}
	defer v.s.releaseIO()
	h, fi, err := v.check(id, i)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil // removed meanwhile
	case isCorrupt(err):
		v.p.Corrupt++
		if v.repair {
			return v.discard(ctx, id, i, fi, err)
		}
		return nil
	case err != nil:
		v.s.log.Warn("verify: cannot read slice file", slog.String("object", id), slog.Int64("slice", i), slog.Any("err", err))
		return nil
	}
	v.p.BytesScanned += fi.Size()
	if v.s.pendingRemoval(id, i) {
		return nil // removed from the index already; the file is deleted by the next Evict
	}
	if o := idx[id]; o != nil {
		if _, ok := o.slices[i]; ok {
			o.slices[i] = true
		}
	}
	return v.reconcile(ctx, id, i, &h, fi)
}

// check opens a slice file and validates its header, size and crc32c.
func (v *verifier) check(id string, i int64) (sliceHeader, os.FileInfo, error) {
	var h sliceHeader
	f, err := v.s.root.Open(sliceName(id, i))
	if err != nil {
		return h, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return h, nil, err
	}
	if !fi.Mode().IsRegular() {
		return h, fi, corrupt("not a regular file")
	}
	h, off, err := readHeader(f, fi.Size())
	if err != nil {
		return h, fi, err
	}
	if err := h.validate(id, i, v.s.sliceSize, fi.Size(), off); err != nil {
		return h, fi, err
	}
	sum := crc32.New(castagnoli)
	n, err := io.CopyBuffer(sum, io.NewSectionReader(f, off, fi.Size()-off), v.buf)
	if err != nil {
		return h, fi, err
	}
	if n != fi.Size()-off || sum.Sum32() != h.K {
		return h, fi, corrupt("crc32c mismatch")
	}
	return h, fi, nil
}

// reconcile brings the index in line with a valid slice file (under the
// object lock).
func (v *verifier) reconcile(ctx context.Context, id string, i int64, h *sliceHeader, fi os.FileInfo) error {
	s := v.s
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	cur, err := s.lookupLocked(ctx, id)
	switch {
	case err != nil:
		return err
	case cur != nil && cur.busy != nil:
		return nil // being removed or replaced
	case cur != nil && cur.total == h.T && cur.has(i):
		return nil // consistent
	case !v.sameFile(id, i, fi):
		return nil // replaced or removed since it was checked
	case cur != nil && cur.total != h.T:
		// A file of an older generation.
		v.p.Corrupt++
		if v.repair {
			v.removeIfSame(id, i, fi)
			if cur.has(i) {
				v.dropLocked(id, cur, i)
			}
		}
		return nil
	case cur == nil && (!validService(h.S) || !validHost(h.H) || !validPath(h.P)):
		v.p.Corrupt++
		if v.repair {
			v.removeIfSame(id, i, fi)
		}
		return nil
	}
	v.p.Added++
	if !v.repair {
		return nil
	}
	if s.pendingLen() >= maxPendingOps {
		return errBacklog
	}
	now := time.Now().UnixMilli()
	if cur == nil {
		// Unknown object: rebuild its record from the self-describing header.
		gk := s.groupKey(h.S, h.H, h.P)
		if !validGroupKey(gk) {
			gk = h.S + ":" + h.H
		}
		created := h.C
		if created <= 0 || created > now {
			created = now
		}
		hdr := rebuildHeader(h.M)
		_, hdrSize, _ := cleanHeader(hdr)
		cur = &entry{gen: s.gen.Add(1), total: h.T, service: h.S, host: h.H, path: h.P, groupKey: gk,
			header: hdr, hdrSize: hdrSize, present: newBitmap(h.T, s.sliceSize)}
		s.heads.set(id, cur)
		s.enqueue(indexOp{kind: opPut, id: id, e: cur, gen: cur.gen, created: created, lastAccess: now})
	}
	next := cur.withSlice(i, true)
	s.heads.set(id, next)
	s.enqueue(indexOp{kind: opSlice, id: id, e: next, gen: next.gen, idx: i, size: sliceLen(h.T, s.sliceSize, i),
		crc: h.K, created: h.C})
	if s.pendingLen() >= flushRows {
		s.kickFlush()
	}
	return nil
}

// missing handles an index entry whose file was not found in the listing.
func (v *verifier) missing(ctx context.Context, id string, gen uint64, i int64) error {
	s := v.s
	if err := s.acquireIO(ctx); err != nil {
		return err
	}
	defer s.releaseIO()
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	cur, err := s.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if cur == nil || cur.busy != nil || cur.gen != gen || !cur.has(i) {
		return nil
	}
	if _, err := s.root.Lstat(sliceName(id, i)); !errors.Is(err, fs.ErrNotExist) {
		return nil // written after the listing (or unreadable: left alone)
	}
	if _, err := s.root.Stat(MarkerFile); err != nil {
		return fmt.Errorf("cachestore: store root unavailable: %w", err)
	}
	v.p.Removed++
	if v.repair {
		v.dropLocked(id, cur, i)
	}
	return nil
}

// discard deletes a corrupt file and drops it from the index.
func (v *verifier) discard(ctx context.Context, id string, i int64, fi os.FileInfo, cause error) error {
	s := v.s
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	cur, err := s.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if cur != nil && cur.busy != nil {
		return nil
	}
	if fi != nil && !v.removeIfSame(id, i, fi) {
		return nil
	}
	if cur != nil && cur.has(i) {
		v.dropLocked(id, cur, i)
	}
	s.log.Warn("verify: removed corrupt slice file", slog.String("object", id), slog.Int64("slice", i), slog.Any("reason", cause))
	return nil
}

// dropLocked marks slice i absent (object lock held).
func (v *verifier) dropLocked(id string, cur *entry, i int64) {
	next := cur.withSlice(i, false)
	v.s.heads.set(id, next)
	v.s.enqueue(indexOp{kind: opDrop, id: id, e: next, gen: next.gen, idx: i})
}

func (v *verifier) sameFile(id string, i int64, fi os.FileInfo) bool {
	now, err := v.s.root.Lstat(sliceName(id, i))
	return err == nil && os.SameFile(fi, now)
}

// removeIfSame removes the file if it is still the one that was inspected.
func (v *verifier) removeIfSame(id string, i int64, fi os.FileInfo) bool {
	if !v.sameFile(id, i, fi) {
		return false
	}
	if err := v.s.root.Remove(sliceName(id, i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		v.s.addRetry(retryRemoval{id: id, idx: i})
	}
	return true
}

func (v *verifier) report() {
	if v.progress != nil && time.Since(v.last) >= time.Second {
		v.last = time.Now()
		v.progress(v.p)
	}
}

// recompute rebuilds per-object and per-group aggregates from the slice rows.
func (s *Store) recompute(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	if err := s.flushLocked(ctx, false); err != nil {
		return err
	}
	err := s.db.Tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []string{
			`DELETE FROM store_slices WHERE object_id NOT IN (SELECT id FROM store_objects)`,
			`UPDATE store_objects SET
				cached_bytes = COALESCE((SELECT SUM(size) FROM store_slices s WHERE s.object_id = store_objects.id), 0),
				slice_count = (SELECT COUNT(*) FROM store_slices s WHERE s.object_id = store_objects.id)`,
			`DELETE FROM store_groups`,
			`INSERT INTO store_groups (service, group_key, objects, slices, cached_bytes, total_bytes, bytes_served, hits,
				first_cached, last_access)
			SELECT service, group_key, COUNT(*), SUM(slice_count), SUM(cached_bytes), SUM(total), SUM(bytes_served),
				SUM(hits), MIN(created_at), MAX(last_access)
			FROM store_objects GROUP BY service, group_key`,
		} {
			if _, err := tx.ExecContext(ctx, q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.usage.load(ctx, s.db.W)
}
