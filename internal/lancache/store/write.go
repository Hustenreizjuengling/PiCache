package cachestore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"time"
)

// SetMeta creates or updates the object record and returns its generation.
// A new record or a different Total increments the generation and discards
// existing slices. New objects of a pinned group are pinned.
func (s *Store) SetMeta(ctx context.Context, id string, m Meta) (gen uint64, err error) {
	if !s.enter() {
		return 0, ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return 0, errInvalidID
	}
	if err := validateMeta(id, &m); err != nil {
		return 0, err
	}
	hdr, hdrSize, err := cleanHeader(m.Header)
	if err != nil {
		return 0, err
	}
	if s.pendingLen() >= maxPendingOps {
		s.kickFlush()
		return 0, errBacklog
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	mu := s.lockFor(id)
	for {
		mu.Lock()
		cur, err := s.lookupLocked(ctx, id)
		if err != nil {
			mu.Unlock()
			return 0, err
		}
		if cur != nil && cur.busy != nil { // being removed or replaced: wait and look again
			busy := cur.busy
			mu.Unlock()
			select {
			case <-busy:
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		now := time.Now().UnixMilli()
		next := &entry{total: m.Total, service: m.Service, host: m.Host, path: m.Path, groupKey: m.GroupKey,
			header: hdr, hdrSize: hdrSize, noSlice: m.NoSlice}
		op := indexOp{kind: opPut, id: id, e: next, created: now, lastAccess: now}
		if cur == nil {
			next.gen = s.gen.Add(1)
			next.present = newBitmap(m.Total, s.sliceSize)
			op.gen = next.gen
			s.heads.set(id, next)
			s.enqueue(op)
			mu.Unlock()
			return next.gen, nil
		}
		if cur.total == m.Total {
			if cur.host == m.Host && cur.groupKey == m.GroupKey && cur.noSlice == m.NoSlice && headerEqual(cur.header, hdr) {
				mu.Unlock()
				return cur.gen, nil
			}
			next.gen, next.present = cur.gen, cur.present
			op.gen = next.gen
			s.heads.set(id, next)
			s.enqueue(op)
			mu.Unlock()
			return next.gen, nil
		}
		// The total changed: new generation with no slices. The old slice
		// files are deleted by the background remover (not within the
		// caller's deadline, and not forgotten when that ends). No old file
		// can pass for a slice of the new generation meanwhile: a slice is
		// only read when the new generation wrote it (renamed over the old
		// file), the remover never deletes such a slice, and a file's
		// header must carry the new total.
		next.gen = s.gen.Add(1)
		next.present = newBitmap(m.Total, s.sliceSize)
		op.gen = next.gen
		s.heads.set(id, next)
		s.enqueue(op)
		s.queueRemovals(id, cur.present)
		mu.Unlock()
		return next.gen, nil
	}
}

// WriteSlice stores slice idx of generation gen (temp → close → rename →
// index). It returns ErrStale if the record is gone or gen differs, and an
// error unless len(data) == min(SliceSize, Total−idx·SliceSize). crc32c of
// data is recorded in the slice header.
func (s *Store) WriteSlice(ctx context.Context, id string, gen uint64, idx int64, data []byte) error {
	if !s.enter() {
		return ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return errInvalidID
	}
	e, err := s.lookup(ctx, id)
	if err != nil {
		return err
	}
	if e == nil || e.busy != nil || e.gen != gen {
		return ErrStale
	}
	if idx < 0 || idx >= slicesFor(e.total, s.sliceSize) {
		return fmt.Errorf("cachestore: slice index %d out of range", idx)
	}
	if want := sliceLen(e.total, s.sliceSize, idx); int64(len(data)) != want {
		return fmt.Errorf("cachestore: slice %d has %d bytes, want %d", idx, len(data), want)
	}
	if s.pendingLen() >= maxPendingOps {
		s.kickFlush()
		return errBacklog
	}
	now := time.Now().UnixMilli()
	crc := crc32c(data)
	prefix, err := encodePrefix(&sliceHeader{O: id, I: idx, S: e.service, H: e.host, P: e.path, T: e.total,
		Z: s.sliceSize, C: now, K: crc, M: e.header})
	if err != nil {
		return err
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if err := s.acquireIO(ctx); err != nil {
		return err
	}
	defer s.releaseIO()
	tmp, err := s.writeTemp(prefix, data)
	if err != nil {
		return err
	}
	dir, name := sliceDir(id), sliceName(id, idx)
	if err := s.ensureDir(dir); err != nil {
		_ = s.root.Remove(tmp)
		return err
	}

	mu := s.lockFor(id)
	mu.Lock()
	cur, err := s.lookupLocked(ctx, id)
	if err == nil && (cur == nil || cur.busy != nil || cur.gen != gen) {
		err = ErrStale
	}
	if err == nil {
		err = s.root.Rename(tmp, name)
		if errors.Is(err, fs.ErrNotExist) { // shard directory removed underneath us
			s.forgetDir(dir)
			if err = s.ensureDir(dir); err == nil {
				err = s.root.Rename(tmp, name)
			}
		}
	}
	if err != nil {
		mu.Unlock()
		_ = s.root.Remove(tmp)
		return err
	}
	next := cur.withSlice(idx, true)
	s.heads.set(id, next)
	s.enqueue(indexOp{kind: opSlice, id: id, e: next, gen: gen, idx: idx, size: int64(len(data)), crc: crc, created: now})
	mu.Unlock()
	return nil
}

// writeTemp writes a complete slice file into tmp/ and closes it.
func (s *Store) writeTemp(prefix, data []byte) (string, error) {
	name := "tmp/" + strconv.FormatUint(rand.Uint64(), 16) + ".tmp"
	f, err := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if errors.Is(err, fs.ErrNotExist) { // tmp/ removed underneath us
		if err = s.root.MkdirAll("tmp", 0o750); err == nil {
			f, err = s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
		}
	}
	if err != nil {
		return "", fmt.Errorf("cachestore: create temp file: %w", err)
	}
	_, err = f.Write(prefix)
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = s.root.Remove(name)
		return "", fmt.Errorf("cachestore: write temp file: %w", err)
	}
	return name, nil
}

// ensureDir creates a shard directory once.
func (s *Store) ensureDir(dir string) error {
	s.dirMu.Lock()
	_, ok := s.dirs[dir]
	s.dirMu.Unlock()
	if ok {
		return nil
	}
	if err := s.root.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("cachestore: create %s: %w", dir, err)
	}
	s.dirMu.Lock()
	s.dirs[dir] = struct{}{}
	s.dirMu.Unlock()
	return nil
}

func (s *Store) forgetDir(dir string) {
	s.dirMu.Lock()
	delete(s.dirs, dir)
	s.dirMu.Unlock()
}

// cleanTmp removes temp files older than olderThan (left behind by crashes).
func (s *Store) cleanTmp(ctx context.Context, olderThan time.Duration) {
	d, err := s.root.Open("tmp")
	if err != nil {
		return
	}
	defer d.Close()
	cutoff := time.Now().Add(-olderThan)
	for ctx.Err() == nil {
		ents, err := d.ReadDir(256)
		for _, de := range ents {
			if !de.Type().IsRegular() || !strings.HasSuffix(de.Name(), ".tmp") {
				continue
			}
			if fi, err := de.Info(); err != nil || fi.ModTime().After(cutoff) {
				continue
			}
			if s.acquireIO(ctx) != nil {
				return
			}
			_ = s.root.Remove("tmp/" + de.Name())
			s.releaseIO()
		}
		if err != nil {
			return
		}
	}
}
