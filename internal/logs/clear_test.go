package logs

import (
	"context"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// runningStore starts the writer of a store on its own and returns a stop
// function.
func runningStore(t *testing.T) (*Store, func()) {
	t.Helper()
	s, _ := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Start(ctx); close(done) }()
	return s, func() { cancel(); <-done }
}

// fillEverything stores a bit of every kind of data (the current hour's
// pending rows and deltas included).
func fillEverything(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	for range 3 {
		q := query(now, "10.0.0.1", "a.example", "blocked-list")
		q.Purpose = "advertising"
		s.LogQuery(q)
	}
	s.LogCache(cacheEv(now, "10.0.0.2", "steam", "steam:depot:1", 100, 50, 50, 10))
	s.LogSNI(SNIEvent{Time: now, ClientIP: "10.0.0.2", SNI: "x.example", Service: "steam", BytesUp: 5})
	s.LogEviction(EvictionEvent{Time: now, StoreID: "local", ObjectID: "o1", Service: "steam", Bytes: 7, Reason: "size"})
	s.RecordEvent(EventRecord{Event: "health.warning", Severity: "warning", Title: "Health check warning: x"})
}

// Clearing the statistics: nothing reappears after a flush, a checkpoint,
// the hour rollover or the daily build; the raw data, the query log and the
// warning history stay.
func TestClearStats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, stop := runningStore(t)
		now := time.Now()
		// A finished day with hourly rows (the daily build would see them).
		insertHourly(t, s, dayStart(now.UnixMilli())-dayMs+hourMs, "domain", "old.example", 5, 0)
		if err := s.buildDay(context.Background(), dayStart(now.UnixMilli())-dayMs); err != nil {
			t.Fatal(err)
		}
		fillEverything(t, s, now)
		time.Sleep(6 * time.Second) // one flush
		synctest.Wait()
		fillEverything(t, s, now) // pending rows and deltas, and the in-memory hour
		synctest.Wait()
		for _, table := range statsTables[:5] {
			if count(t, s, table) == 0 && table != "logs_dns_top_hourly" {
				t.Fatalf("%s is empty before clearing", table)
			}
		}
		deleted, err := s.ClearStats(context.Background())
		if err != nil || deleted == 0 {
			t.Fatalf("deleted %d, %v", deleted, err)
		}
		time.Sleep(6 * time.Second)
		synctest.Wait()
		for _, table := range statsTables {
			if n := count(t, s, table); n != 0 {
				t.Errorf("%s has %d rows after clearing", table, n)
			}
		}
		for _, table := range []string{"logs_queries", "logs_cache_requests", "logs_sni", "logs_evictions", "logs_downloads", "logs_events"} {
			if count(t, s, table) == 0 {
				t.Errorf("%s was cleared", table)
			}
		}
		sum, err := s.Summary(context.Background(), now.Add(-2*time.Hour), now.Add(time.Minute))
		if err != nil || sum.DNSQueries != 0 || sum.ActiveClients != 0 || sum.UniqueDomains != 0 || sum.CacheRequests != 0 {
			t.Fatalf("summary after clearing %+v, %v", sum, err)
		}
		// The final flush and checkpoint, the next hour and the day change
		// bring nothing back (the writer is driven directly once stopped).
		stop()
		s.w.rollover(now.Add(time.Hour), hourStart(now.UnixMilli())+hourMs)
		s.w.dayChanged(now, hourStart(now.UnixMilli())-dayMs, hourStart(now.UnixMilli()))
		for range 10 {
			s.w.backfillStep()
		}
		for _, table := range statsTables {
			if n := count(t, s, table); n != 0 {
				t.Errorf("%s has %d rows after the rollover", table, n)
			}
		}
	})
}

// Clearing the query log deletes the stored and the pending rows and keeps
// the statistics.
func TestClearQueries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, stop := runningStore(t)
		defer stop()
		now := time.Now()
		fillEverything(t, s, now)
		time.Sleep(6 * time.Second)
		synctest.Wait()
		fillEverything(t, s, now) // pending
		synctest.Wait()
		deleted, err := s.ClearQueries(context.Background())
		if err != nil || deleted != 3 {
			t.Fatalf("deleted %d, %v", deleted, err)
		}
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if n := count(t, s, "logs_queries"); n != 0 {
			t.Fatalf("%d query rows after clearing", n)
		}
		sum, err := s.Summary(context.Background(), now.Add(-time.Hour), now.Add(time.Minute))
		if err != nil || sum.DNSQueries != 6 {
			t.Fatalf("statistics after clearing the query log %+v, %v", sum, err)
		}
		if count(t, s, "logs_cache_requests") == 0 || count(t, s, "logs_events") != 1 {
			t.Fatal("other data was cleared")
		}
	})
}

// Clearing needs a running writer on logs.db: 503 otherwise.
func TestClearUnavailable(t *testing.T) {
	d := Discard("broken", slog.New(slog.DiscardHandler))
	if _, err := d.ClearQueries(context.Background()); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("discard store: %v", err)
	}
	if _, err := d.ClearStats(context.Background()); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("discard store: %v", err)
	}
	s, stop := runningStore(t)
	stop()
	if _, err := s.ClearStats(context.Background()); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("stopped store: %v", err)
	}
}

// Events queued before a clear request are taken before the delete (run's
// select picks among ready channels at random): none of them is stored or
// counted after it. Clearing the query log keeps their statistics,
// resetting the statistics their query rows.
func TestClearTakesQueuedEvents(t *testing.T) {
	s, _ := newTestStore(t) // the writer is driven directly
	now := time.Now()
	fillEverything(t, s, now) // queued only
	if r := s.w.handleClear(clearQueries); r.err != nil {
		t.Fatal(r.err)
	}
	s.w.drainQueued() // what run would take next
	s.w.flush(now)
	if n := count(t, s, "logs_queries"); n != 0 {
		t.Fatalf("%d queued query rows reappeared after clearing the query log", n)
	}
	if count(t, s, "logs_dns_minute") == 0 || count(t, s, "logs_cache_requests") == 0 {
		t.Fatal("clearing the query log dropped the statistics or other rows of the queued events")
	}
	fillEverything(t, s, now)
	if r := s.w.handleClear(clearStats); r.err != nil {
		t.Fatal(r.err)
	}
	s.w.drainQueued()
	s.w.flush(now)
	s.w.checkpoint(now)
	for _, table := range statsTables {
		if n := count(t, s, table); n != 0 {
			t.Errorf("%s has %d rows of queued events after resetting the statistics", table, n)
		}
	}
	if n := count(t, s, "logs_queries"); n != 3 {
		t.Fatalf("%d query rows, want the 3 queued before the reset", n)
	}
}
