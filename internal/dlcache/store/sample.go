package cachestore

import (
	"context"
	"errors"
	"io"
	"io/fs"
)

// maxSample bounds SampleSlices.
const maxSample = 1024

// SliceRef names one cached slice.
type SliceRef struct {
	ID  string // object id
	Idx int64  // slice index
}

// SampleSlices returns up to n cached slices picked uniformly at random from
// the index, for the storage speed test (reads of old content, not only of
// what was cached last). Changes of the last flush interval may be missing.
// It scans the slice table once; ctx bounds it.
func (s *Store) SampleSlices(ctx context.Context, n int) ([]SliceRef, error) {
	if !s.enter() {
		return nil, ErrClosed
	}
	defer s.leave()
	n = min(max(n, 0), maxSample)
	if n == 0 {
		return nil, nil
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.db.R.QueryContext(ctx, `SELECT object_id, idx FROM store_slices ORDER BY random() LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SliceRef, 0, n)
	for rows.Next() {
		var r SliceRef
		if err := rows.Scan(&r.ID, &r.Idx); err != nil {
			return nil, err
		}
		if ValidObjectID(r.ID) && r.Idx >= 0 {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// ReadSliceUncached reads a cached slice the way a cache hit does on a cold
// page cache, for the storage speed test: it opens and validates the slice
// file like ReadSlice, drops the file from the page cache (DropPageCache)
// and reads all of its data in chunks of len(buf). It returns the data bytes
// read and whether the page cache was dropped. It takes no I/O slot and
// changes nothing: a slice that is gone or damaged is reported
// (ErrSliceMissing or the validation error), not dropped from the index.
func (s *Store) ReadSliceUncached(ctx context.Context, ref SliceRef, buf []byte) (n int64, dropped bool, err error) {
	if !s.enter() {
		return 0, false, ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(ref.ID) {
		return 0, false, errInvalidID
	}
	if len(buf) == 0 {
		return 0, false, errors.New("cachestore: empty read buffer")
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	e, err := s.lookup(ctx, ref.ID)
	if err != nil {
		return 0, false, err
	}
	if e == nil || e.busy != nil || !e.has(ref.Idx) {
		return 0, false, ErrSliceMissing
	}
	f, off, size, _, err := s.openSlice(ref.ID, ref.Idx, e.total)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, ErrSliceMissing
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	dropped = DropPageCache(f)
	for n < size {
		if err := ctx.Err(); err != nil {
			return n, dropped, err
		}
		k, err := f.ReadAt(buf[:min(int64(len(buf)), size-n)], off+n)
		n += int64(k)
		if errors.Is(err, io.EOF) {
			return n, dropped, io.ErrUnexpectedEOF // shorter than validated: truncated underneath
		}
		if err != nil {
			return n, dropped, err
		}
	}
	return n, dropped, nil
}
