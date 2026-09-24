package cachestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
)

// writeChunk is the amount WriteRange copies per I/O semaphore slot.
const writeChunk = 256 << 10

// ReadSlice opens a cached slice of generation gen. ErrSliceMissing if not
// cached, ErrStale if the object changed; a corrupt/mismatched file is
// deleted and reported as missing.
func (s *Store) ReadSlice(ctx context.Context, id string, gen uint64, idx int64) (SliceReader, error) {
	if !s.enter() {
		return nil, ErrClosed
	}
	defer s.leave()
	if !ValidObjectID(id) {
		return nil, errInvalidID
	}
	e, err := s.lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	if e == nil || e.busy != nil || e.gen != gen {
		return nil, ErrStale
	}
	if !e.has(idx) {
		return nil, ErrSliceMissing
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if err := s.acquireIO(ctx); err != nil {
		return nil, err
	}
	f, off, size, fi, err := s.openSlice(id, idx, e.total)
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			err = s.sliceGone(ctx, id, gen, idx, nil, err)
		case isCorrupt(err):
			err = s.sliceGone(ctx, id, gen, idx, fi, err)
		}
		s.releaseIO()
		return nil, err
	}
	s.releaseIO()
	// The object may have changed while the file was opened: a reader must
	// never hand out data of another generation.
	if cur, err := s.lookup(ctx, id); err != nil || cur == nil || cur.busy != nil || cur.gen != gen {
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		return nil, ErrStale
	}
	return &sliceReader{f: f, off: off, size: size, sem: s.sem}, nil
}

// openSlice opens and validates a slice file of an object of total bytes.
// On a validation error the file is closed and its FileInfo returned.
func (s *Store) openSlice(id string, idx, total int64) (f *os.File, off, size int64, fi os.FileInfo, err error) {
	f, err = s.root.Open(sliceName(id, idx))
	if err != nil {
		return nil, 0, 0, nil, err
	}
	fi, err = f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = corrupt("not a regular file")
	}
	if err == nil {
		var h sliceHeader
		h, off, err = readHeader(f, fi.Size())
		if err == nil {
			err = h.validate(id, idx, s.sliceSize, fi.Size(), off)
		}
		if err == nil && h.T != total {
			err = corrupt("total %d differs from the index (%d)", h.T, total)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, 0, 0, fi, err
	}
	return f, off, fi.Size() - off, fi, nil
}

// sliceGone handles a slice the index lists whose file is missing (fi nil)
// or corrupt: the file, if it is still the one inspected, is deleted and the
// slice dropped from the index. Called with an I/O slot held.
func (s *Store) sliceGone(ctx context.Context, id string, gen uint64, idx int64, fi os.FileInfo, cause error) error {
	if fi == nil {
		// An unmounted or vanished store root must not wipe the index.
		if _, err := s.root.Stat(MarkerFile); err != nil {
			return fmt.Errorf("cachestore: store root unavailable: %w", err)
		}
	}
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()
	cur, err := s.lookupLocked(ctx, id)
	if err != nil {
		return err
	}
	if cur == nil || cur.busy != nil || cur.gen != gen || !cur.has(idx) {
		return ErrSliceMissing
	}
	name := sliceName(id, idx)
	now, err := s.root.Lstat(name)
	switch {
	case err == nil && fi != nil && now.Mode()&fs.ModeSymlink != 0:
		// A link (hostile store content) is never a slice file.
		if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.addRetry(retryRemoval{id: id, idx: idx})
		}
	case err == nil && (fi == nil || !os.SameFile(fi, now)):
		return ErrSliceMissing // written again meanwhile
	case err == nil:
		if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.addRetry(retryRemoval{id: id, idx: idx})
		}
	}
	next := cur.withSlice(idx, false)
	s.heads.set(id, next)
	s.enqueue(indexOp{kind: opDrop, id: id, e: next, gen: gen, idx: idx})
	s.log.Warn("dropped unusable slice", slog.String("object", id), slog.Int64("slice", idx), slog.Any("reason", cause))
	return ErrSliceMissing
}

// sliceReader implements SliceReader on an open slice file.
type sliceReader struct {
	f      *os.File
	off    int64 // data offset in the file
	size   int64
	sem    chan struct{}
	mu     sync.Mutex // serialises WriteRange (it moves the file offset)
	closed atomic.Bool
}

func (r *sliceReader) Size() int64 { return r.size }

func (r *sliceReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("cachestore: negative offset")
	}
	if off >= r.size {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	want := p
	if rem := r.size - off; int64(len(p)) > rem {
		want = p[:rem]
	}
	r.sem <- struct{}{}
	n, err := r.f.ReadAt(want, r.off+off)
	<-r.sem
	switch {
	case errors.Is(err, io.EOF):
		err = io.ErrUnexpectedEOF // shorter than validated: truncated underneath
	case err == nil && len(want) < len(p):
		err = io.EOF
	}
	return n, err
}

// WriteRange copies n bytes starting at off to w. It seeks the *os.File and
// copies through io.LimitReader so net/http can use sendfile(2).
func (r *sliceReader) WriteRange(w io.Writer, off, n int64) (int64, error) {
	if off < 0 || n < 0 || off > r.size || n > r.size-off {
		return 0, fmt.Errorf("cachestore: range %d+%d outside slice of %d bytes", off, n, r.size)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var done int64
	for done < n {
		chunk := min(n-done, writeChunk)
		r.sem <- struct{}{}
		var m int64
		_, err := r.f.Seek(r.off+off+done, io.SeekStart)
		if err == nil {
			m, err = io.Copy(w, io.LimitReader(r.f, chunk))
		}
		<-r.sem
		done += m
		if err != nil {
			return done, err
		}
		if m < chunk {
			return done, io.ErrUnexpectedEOF
		}
	}
	return done, nil
}

func (r *sliceReader) Close() error {
	if r.closed.Swap(true) {
		return nil
	}
	return r.f.Close()
}
