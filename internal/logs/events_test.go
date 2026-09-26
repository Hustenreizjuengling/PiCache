package logs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// eventStores returns the logs.db history and the in-memory one.
func eventStores(t *testing.T) map[string]*Store {
	t.Helper()
	s, _ := newTestStore(t)
	return map[string]*Store{"logs.db": s, "memory": Discard("disabled", slog.New(slog.DiscardHandler))}
}

func TestEventHistoryMergeAndAck(t *testing.T) {
	ctx := context.Background()
	for name, s := range eventStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			s.RecordEvent(EventRecord{Event: "health.warning", Severity: "warning", Title: "Health check warning: host",
				Message: "first\nline", Time: now.Add(-time.Minute)})
			s.RecordEvent(EventRecord{Event: "health.warning", Severity: "warning", Title: "Health check warning: host",
				Message: "second\x00 \u202eline", Time: now})
			s.RecordEvent(EventRecord{Event: "security.lockout", Severity: "warning", Title: "Sign-in lockout: 10.0.0.1"})
			s.RecordEvent(EventRecord{Event: "backup.succeeded", Severity: "info", Title: "Scheduled backup written"})
			s.RecordEvent(EventRecord{Event: "notify.test", Title: "never"})
			s.RecordEvent(EventRecord{Event: "bogus", Title: "never"})
			if c := s.EventCounts(); c.All != 2 || c.Security != 1 {
				t.Fatalf("counts %+v", c)
			}
			page, err := s.Events(ctx, EventQuery{Security: true})
			if err != nil || len(page.Items) != 3 {
				t.Fatalf("history %+v, %v", page, err)
			}
			var h Event
			for _, e := range page.Items {
				if e.Event == "health.warning" {
					h = e
				}
			}
			if h.Count != 2 || h.Message != "second line" || !h.Time.Before(h.LastTime) || h.AcknowledgedAt != nil {
				t.Fatalf("merged entry %+v", h)
			}
			viewer, err := s.Events(ctx, EventQuery{})
			if err != nil || len(viewer.Items) != 2 {
				t.Fatalf("without security %+v, %v", viewer, err)
			}
			acked, err := s.AckEvent(ctx, h.ID, "admin")
			if err != nil || acked.AcknowledgedAt == nil || acked.AcknowledgedBy != "admin" {
				t.Fatalf("ack %+v, %v", acked, err)
			}
			again, err := s.AckEvent(ctx, h.ID, "other")
			if err != nil || again.AcknowledgedBy != "admin" || !again.AcknowledgedAt.Equal(*acked.AcknowledgedAt) {
				t.Fatalf("ack is not idempotent: %+v, %v", again, err)
			}
			if _, err := s.AckEvent(ctx, 9999, "admin"); apperr.KindOf(err) != apperr.KindNotFound {
				t.Fatalf("unknown id: %v", err)
			}
			if c := s.EventCounts(); c.All != 1 || c.Security != 1 {
				t.Fatalf("counts after ack %+v", c)
			}
			// An acknowledged entry is not merged into: a new one starts.
			s.RecordEvent(EventRecord{Event: "health.warning", Severity: "warning", Title: "Health check warning: host"})
			unacked := true
			open, _ := s.Events(ctx, EventQuery{Security: true, Unacknowledged: &unacked})
			if len(open.Items) != 3 { // the new warning, the lockout and the info entry
				t.Fatalf("unacknowledged %+v", open.Items)
			}
			n, err := s.AckAllEvents(ctx, "admin")
			if err != nil || n != 3 {
				t.Fatalf("ack all %d, %v", n, err)
			}
			if c := s.EventCounts(); c.All != 0 {
				t.Fatalf("counts after ack all %+v", c)
			}
			acked2 := false
			done, _ := s.Events(ctx, EventQuery{Security: true, Unacknowledged: &acked2})
			if len(done.Items) != 4 {
				t.Fatalf("acknowledged %+v", done.Items)
			}
		})
	}
}

// At most 200 entries per area (the oldest last time dropped first) in
// logs.db, 200 in all in memory; entries older than 90 days are removed.
func TestEventHistoryBounds(t *testing.T) {
	ctx := context.Background()
	for name, s := range eventStores(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			for i := range 230 {
				s.RecordEvent(EventRecord{Event: "storage.offline", Severity: "warning", Title: fmt.Sprintf("offline %d", i),
					Time: now.Add(time.Duration(i-300) * time.Second)})
			}
			s.RecordEvent(EventRecord{Event: "update.failed", Severity: "error", Title: "old", Time: now.Add(-91 * 24 * time.Hour)})
			var all []Event
			for cursor := ""; ; {
				page, err := s.Events(ctx, EventQuery{Security: true, Limit: 7, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				all = append(all, page.Items...)
				if cursor = page.Next; cursor == "" {
					break
				}
			}
			storage := 0
			for i, e := range all {
				if e.Event == "storage.offline" {
					storage++
				}
				if i > 0 && e.LastTime.After(all[i-1].LastTime) {
					t.Fatalf("not newest first at %d", i)
				}
			}
			if storage != 200 || all[0].Title != "offline 229" {
				t.Fatalf("%d storage entries, newest %q", storage, all[0].Title)
			}
			if s.events.d != nil {
				// The 90-day rule applies at the next pruning.
				s.w.prune(now)
				page, _ := s.Events(ctx, EventQuery{Security: true, Limit: 200})
				for _, e := range page.Items {
					if e.Title == "old" {
						t.Fatal("an entry older than 90 days was kept")
					}
				}
			} else if len(all) != 200 || strings.Contains(fmt.Sprint(all), "old") {
				t.Fatalf("memory ring holds %d", len(all))
			}
		})
	}
}

func TestEventHistoryBadCursor(t *testing.T) {
	s, _ := newTestStore(t)
	if _, err := s.Events(context.Background(), EventQuery{Cursor: "nope"}); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("bad cursor: %v", err)
	}
}

// The history survives clearing (see TestClearStats) and a reopen.
func TestEventHistoryPersists(t *testing.T) {
	dir := t.TempDir()
	ldb, cdb, set := openTestDBs(t, dir)
	s, err := New(context.Background(), ldb, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.RecordEvent(EventRecord{Event: "backup.failed", Severity: "error", Title: "Scheduled backup failed"})
	ldb.Close()
	cdb.Close()
	ldb, cdb, set = openTestDBs(t, dir)
	defer ldb.Close()
	defer cdb.Close()
	s, err = New(context.Background(), ldb, set, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := s.EventCounts(); c.All != 1 {
		t.Fatalf("counts after reopen %+v", c)
	}
}
