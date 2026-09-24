package cachestore

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func countRows(t *testing.T, s *Store, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.R.QueryRowContext(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBatchedWrites checks the flush timing: index writes within 250 ms,
// access statistics every 30 s.
func TestBatchedWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newStore(t)
		ctx := context.Background()
		id, m := testMeta("steam", "/batched", "steam:b", 1000)
		gen, err := s.SetMeta(ctx, id, m)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WriteSlice(ctx, id, gen, 0, payload(id, 0, 1000)); err != nil {
			t.Fatal(err)
		}
		// Visible to readers immediately, in the index only after the batch.
		if !s.HasSlice(ctx, id, gen, 0) {
			t.Fatal("slice not visible before the flush")
		}
		if n := countRows(t, s, `SELECT COUNT(*) FROM store_slices`); n != 0 {
			t.Fatalf("index written before the batch interval (%d rows)", n)
		}
		time.Sleep(flushEvery + 10*time.Millisecond)
		synctest.Wait()
		if n := countRows(t, s, `SELECT COUNT(*) FROM store_slices`); n != 1 {
			t.Fatalf("slice rows after the batch interval: %d", n)
		}

		s.Touch(id, 500)
		s.Touch(id, 500)
		time.Sleep(statsEvery - time.Second)
		synctest.Wait()
		if n := countRows(t, s, `SELECT hits FROM store_objects WHERE id = ?`, id); n != 0 {
			t.Fatalf("statistics flushed early: hits=%d", n)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := countRows(t, s, `SELECT hits FROM store_objects WHERE id = ?`, id); n != 2 {
			t.Fatalf("hits after the statistics interval: %d", n)
		}
		if n := countRows(t, s, `SELECT bytes_served FROM store_groups WHERE group_key = 'steam:b'`); n != 1000 {
			t.Fatalf("group bytes served: %d", n)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

// TestBacklog: writers are refused while too many index writes are pending,
// and accepted again once they are committed.
func TestBacklog(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, m := testMeta("steam", "/backlog", "steam:b", 1000)
	s.flushMu.Lock() // no flush may run while the queue is full
	s.pendMu.Lock()
	for range maxPendingOps {
		s.pending = append(s.pending, indexOp{kind: opDrop, id: id, e: &entry{}})
	}
	s.pendMu.Unlock()
	_, err := s.SetMeta(ctx, id, m)
	s.flushMu.Unlock()
	if err != errBacklog {
		t.Fatalf("SetMeta with a full queue: %v", err)
	}
	mustFlush(t, s)
	gen, err := s.SetMeta(ctx, id, m)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteSlice(ctx, id, gen, 0, payload(id, 0, 1000)); err != nil {
		t.Fatal(err)
	}
	checkAggregates(t, s)
}

// TestFlushBatchSize: a full batch is written without waiting for the timer.
func TestFlushBatchSize(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, _ := newStore(t)
		ctx := context.Background()
		for i := range flushRows {
			id, m := testMeta("steam", "/many/"+string(rune('a'+i%26))+string(rune('a'+i/26)), "steam:m", 10)
			if _, err := s.SetMeta(ctx, id, m); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait() // the kicked flusher runs; no time passes
		if n := countRows(t, s, `SELECT COUNT(*) FROM store_objects`); n != flushRows {
			t.Fatalf("objects in the index: %d", n)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
