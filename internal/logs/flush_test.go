package logs

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// logs.flushSeconds: raw rows and count deltas are written every
// flushSeconds (or at 5000 pending rows) while the live feed is published
// at ingestion; a change applies without a restart.
func TestFlushSeconds(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ldb, cdb, set := openTestDBs(t, dir)
		defer cdb.Close()
		defer ldb.Close()
		updateLogs(t, set, func(g *settings.Logs) { g.FlushSeconds = 30 })
		s, err := New(t.Context(), ldb, set, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Start(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		live, stopLive, err := s.SubscribeQueries(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stopLive()

		s.LogQuery(query(time.Now(), "10.0.0.1", "a.example", "forwarded"))
		synctest.Wait()
		if len(live) != 1 {
			t.Fatal("the live feed waits for the flush")
		}
		time.Sleep(26 * time.Second)
		synctest.Wait()
		if n, m := count(t, s, "logs_queries"), count(t, s, "logs_dns_minute"); n != 0 || m != 0 {
			t.Fatalf("written before flushSeconds: %d rows, %d rollups", n, m)
		}
		time.Sleep(5 * time.Second) // the tick at 30 s
		synctest.Wait()
		if n, m := count(t, s, "logs_queries"), count(t, s, "logs_dns_minute"); n != 1 || m != 1 {
			t.Fatalf("after flushSeconds: %d rows, %d rollups", n, m)
		}

		// A full batch is written at once.
		for range batchRows {
			s.LogQuery(query(time.Now(), "10.0.0.2", "b.example", "forwarded"))
		}
		synctest.Wait()
		if n := count(t, s, "logs_queries"); n != 1+batchRows {
			t.Fatalf("rows after a full batch = %d", n)
		}

		// Back to 5 s without a restart (5 s after the full batch, at the
		// latest one tick later).
		updateLogs(t, set, func(g *settings.Logs) { g.FlushSeconds = 5 })
		s.LogQuery(query(time.Now(), "10.0.0.3", "c.example", "forwarded"))
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if n := count(t, s, "logs_queries"); n != 2+batchRows {
			t.Fatalf("rows after the change = %d", n)
		}
	})
}

// The maintenance tick runs every 5 s whatever flushSeconds is: the hour
// rollover checkpoints the finished hour.
func TestMaintenanceTickIndependentOfFlush(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ldb, cdb, set := openTestDBs(t, dir)
		defer cdb.Close()
		defer ldb.Close()
		updateLogs(t, set, func(g *settings.Logs) { g.FlushSeconds = 300 })
		s, err := New(t.Context(), ldb, set, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Start(ctx); close(done) }()
		defer func() { cancel(); <-done }()
		s.LogQuery(query(time.Now(), "10.0.0.1", "a.example", "forwarded"))
		synctest.Wait()
		first := s.top.currentHour()
		// Sleep past the next full hour: the rollover is at most one tick late.
		time.Sleep(time.Duration(first+hourMs-time.Now().UnixMilli())*time.Millisecond + 6*time.Second)
		synctest.Wait()
		if s.top.currentHour() == first {
			t.Fatal("no rollover")
		}
		var n int
		s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_hourly WHERE bucket = ?`, first).Scan(&n)
		if n == 0 {
			t.Fatal("the finished hour was not checkpointed")
		}
	})
}

// The flush cadence counts maintenance ticks, not clock readings: at the
// default 5 s every tick writes however late each one is handled (a tick
// handled sooner after it fired than the one before must not wait for the
// next), flushSeconds is rounded down to whole ticks, and a flush of a full
// batch in between does not move the cadence.
func TestFlushCadenceIgnoresTickJitter(t *testing.T) {
	s, set := newTestStore(t)
	now := time.Now()
	rows := 0
	step := func(tick int, delay time.Duration, want int) {
		t.Helper()
		rows++
		s.w.addQuery(query(now, "10.0.0.1", fmt.Sprintf("j%d.example", rows), "forwarded"))
		s.w.tick(now.Add(time.Duration(tick)*tickInterval + delay))
		if n := count(t, s, "logs_queries"); n != want {
			t.Fatalf("tick %d: %d rows written, want %d", tick, n, want)
		}
	}
	for i, delay := range []time.Duration{3 * time.Millisecond, time.Millisecond, 4 * time.Millisecond, 0} {
		step(i+1, delay, i+1)
	}
	// 12 s: every second tick (10 s, never later than 12 s).
	updateLogs(t, set, func(g *settings.Logs) { g.FlushSeconds = 12 })
	step(5, 2*time.Millisecond, 4)
	step(6, time.Millisecond, 6)
	step(7, 0, 6)
	s.w.flush(now.Add(7*tickInterval + time.Second)) // a full batch between two ticks
	step(8, 0, 8)
}
