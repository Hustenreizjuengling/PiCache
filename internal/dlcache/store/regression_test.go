package cachestore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// blockingWriter blocks every Write until release is closed (a client that
// stopped reading).
type blockingWriter struct {
	release chan struct{}
	entered chan struct{}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return len(p), nil
}

// TestWriteRangeKeepsIOSlotsFree: clients that stop reading must not hold
// the filesystem I/O semaphore, or they starve every other store operation
// (cache hits of other clients, fills, removals).
func TestWriteRangeKeepsIOSlotsFree(t *testing.T) {
	e := newEnv(t)
	e.opt.IOConcurrency = 2
	s := e.open()
	ctx := context.Background()
	id, gen := putObject(t, s, "steam", "/big", "steam:b", 2*testSlice)

	// Twice as many stalled clients as I/O slots: the first ones take the
	// stream slots, the others go through the buffered path.
	release := make(chan struct{})
	var wg sync.WaitGroup
	defer func() { close(release); wg.Wait() }()
	readers := make([]SliceReader, 2*cap(s.sem))
	for i := range readers {
		r, err := s.ReadSlice(ctx, id, gen, 0)
		if err != nil {
			t.Fatal(err)
		}
		readers[i] = r
	}
	for _, r := range readers {
		w := &blockingWriter{release: release, entered: make(chan struct{}, 1)}
		wg.Go(func() {
			defer r.Close()
			if _, err := r.WriteRange(w, 0, testSlice); err != nil {
				t.Error(err)
			}
		})
		select {
		case <-w.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("WriteRange did not reach the client")
		}
	}

	tctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	r, err := s.ReadSlice(tctx, id, gen, 1)
	if err != nil {
		t.Fatalf("ReadSlice of another client while clients stall: %v", err)
	}
	var out bytes.Buffer
	if _, err := r.WriteRange(&out, 0, 100); err != nil || !bytes.Equal(out.Bytes(), payload(id, 1, 100)) {
		t.Fatalf("WriteRange of another client: %v", err)
	}
	r.Close()
	oid, m := testMeta("steam", "/fill", "steam:b", 1000)
	g, err := s.SetMeta(tctx, oid, m)
	if err == nil {
		err = s.WriteSlice(tctx, oid, g, 0, payload(oid, 0, 1000))
	}
	if err != nil {
		t.Fatalf("store write while clients stall: %v", err)
	}
	if len(s.sem) != 0 {
		t.Fatalf("%d I/O slots held by stalled clients", len(s.sem))
	}
}

// fillRetries fills the bounded retry list with unrelated entries.
func fillRetries(s *Store) {
	for i := range maxRetries {
		s.addRetry(retryRemoval{id: "ffffffffffffffffffffffffffffffff", idx: int64(i)})
	}
}

// TestSetMetaRemovesOldSlicesInBackground: the files of a discarded
// generation are deleted by the background remover, independent of the
// caller's deadline and of the bounded retry list, so none is orphaned.
func TestSetMetaRemovesOldSlicesInBackground(t *testing.T) {
	s, e := newStore(t)
	id, gen := putObject(t, s, "steam", "/resized", "steam:r", 8*testSlice)
	fillRetries(s)
	ended, cancel := context.WithCancel(context.Background())
	cancel() // the caller's budget is used up
	_, m := testMeta("steam", "/resized", "steam:r", 3*testSlice)
	gen2, err := s.SetMeta(ended, id, m)
	if err != nil || gen2 <= gen {
		t.Fatalf("SetMeta: gen %d → %d (%v)", gen, gen2, err)
	}
	waitRemovals(t, s)
	for i := range int64(8) {
		if fileExists(t, e, id, i) {
			t.Fatalf("slice file %d of the old generation left on disk", i)
		}
	}
	// The new generation is usable right away.
	if err := s.WriteSlice(context.Background(), id, gen2, 0, payload(id, 0, testSlice)); err != nil {
		t.Fatal(err)
	}
	checkAggregates(t, s)
}

// TestRemovalAfterDeadlineIsQueued: a removal whose deadline ended is
// queued for the background remover instead of the bounded retry list.
func TestRemovalAfterDeadlineIsQueued(t *testing.T) {
	s, e := newStore(t)
	id, _ := putObject(t, s, "steam", "/stray", "steam:s", 1000)
	stray := filepath.Join(e.root, filepath.FromSlash(sliceName(id, 7))) // not referenced by the index
	if err := os.WriteFile(stray, []byte("stray"), 0o640); err != nil {
		t.Fatal(err)
	}
	fillRetries(s)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	s.removeFile(ended, id, 7)
	waitRemovals(t, s)
	if fileExists(t, e, id, 7) {
		t.Fatal("file orphaned after its removal deadline ended")
	}
	if !fileExists(t, e, id, 0) {
		t.Fatal("background remover deleted a referenced slice")
	}
}

// TestRemoverKeepsRewrittenSlice: a queued removal never deletes a slice
// that the object references again.
func TestRemoverKeepsRewrittenSlice(t *testing.T) {
	s, e := newStore(t)
	ctx := context.Background()
	id, gen := putObject(t, s, "steam", "/again", "steam:a", 1000)
	s.queueRemoval(id, 0)
	waitRemovals(t, s)
	if !fileExists(t, e, id, 0) || !s.HasSlice(ctx, id, gen, 0) {
		t.Fatal("live slice removed by a queued removal")
	}
}

// TestAcquireIOHonoursEndedContext: an ended context fails even when a slot
// is free (select would pick at random).
func TestAcquireIOHonoursEndedContext(t *testing.T) {
	s, _ := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 50 {
		if err := s.acquireIO(ctx); err == nil {
			s.releaseIO()
			t.Fatal("acquireIO succeeded with an ended context")
		}
	}
}

// TestEvictSkipsObjectsInUse: an object being served is never the LRU
// victim, however old its last access.
func TestEvictSkipsObjectsInUse(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ids := lruObjects(t, s, 3) // ids[0] is least recently used
	release := s.Use(ids[0])
	res, err := s.Evict(ctx, Policy{MaxBytes: 5 * testSlice / 2})
	if err != nil || res.Objects != 1 {
		t.Fatalf("evict: %+v %v", res, err)
	}
	if !present(t, s, ids[0]) || present(t, s, ids[1]) {
		t.Fatal("the object in use was evicted instead of the next LRU object")
	}
	setLastAccess(t, s, ids[2], time.Now())
	if _, err := s.Evict(ctx, Policy{MaxAge: 30 * time.Minute}); err != nil || !present(t, s, ids[0]) {
		t.Fatalf("inactive expiry removed an object in use: %v", err)
	}
	release()
	release() // idempotent
	if s.used(ids[0]) {
		t.Fatal("still in use after release")
	}
	if res, err := s.Evict(ctx, Policy{MaxBytes: testSlice}); err != nil || present(t, s, ids[0]) {
		t.Fatalf("released object not evictable: %+v %v", res, err)
	}
}

// TestTouchCountsHitsOnly: a request that served nothing from the cache
// updates the last access but is no hit and serves no cached bytes.
func TestTouchCountsHitsOnly(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, _ := putObject(t, s, "steam", "/miss", "steam:m", 1000)
	setLastAccess(t, s, id, time.Now().Add(-time.Hour))
	before := time.Now().Add(-time.Second)
	s.Touch(id, 0) // a MISS download
	mustFlush(t, s)
	page, err := s.Objects(ctx, ObjectQuery{GroupKey: "steam:m"})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("objects: %v", err)
	}
	if o := page.Items[0]; o.Hits != 0 || o.BytesServed != 0 || o.LastAccess.Before(before) {
		t.Fatalf("after a miss: hits=%d served=%d lastAccess=%v", o.Hits, o.BytesServed, o.LastAccess)
	}
	s.Touch(id, 600)
	mustFlush(t, s)
	page, _ = s.Objects(ctx, ObjectQuery{GroupKey: "steam:m"})
	if o := page.Items[0]; o.Hits != 1 || o.BytesServed != 600 {
		t.Fatalf("after a hit: hits=%d served=%d", o.Hits, o.BytesServed)
	}
	checkAggregates(t, s)
}
