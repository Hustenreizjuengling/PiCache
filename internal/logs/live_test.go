package logs

import (
	"context"
	"encoding/json/v2"
	"strings"
	"testing"
	"time"
)

// Live events carry a positive, unique sequence number so a UI can key its
// rows before the database assigns IDs; stored events do not.
func TestLiveEventsHaveSequenceNumbers(t *testing.T) {
	s, _ := newTestStore(t)
	qch, cancelQ, err := s.SubscribeQueries(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelQ()
	cch, cancelC, err := s.SubscribeCache(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelC()

	now := time.Now()
	s.w.addQuery(query(now, "10.0.0.1", "a.example", "forwarded"))
	s.w.addCache(cacheEv(now, "10.0.0.1", "steam", "steam:depot:1", 1, 1, 0, 1))
	s.w.addQuery(query(now, "10.0.0.1", "a.example", "forwarded")) // identical event
	s.w.flush(now)

	q1, c1, q2 := <-qch, <-cch, <-qch
	if q1.Seq == 0 || c1.Seq <= q1.Seq || q2.Seq <= c1.Seq {
		t.Fatalf("sequence numbers %d, %d, %d", q1.Seq, c1.Seq, q2.Seq)
	}
	b, err := json.Marshal(q1)
	if err != nil || !strings.Contains(string(b), `"seq":`) {
		t.Fatalf("live JSON %s %v", b, err)
	}
	page, err := s.QueryLog(context.Background(), QueryFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute)})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("query log %+v %v", page, err)
	}
	for _, e := range page.Items {
		if e.Seq != 0 || e.ID == 0 {
			t.Fatalf("stored event %+v", e)
		}
		if b, _ := json.Marshal(e); strings.Contains(string(b), `"seq"`) {
			t.Fatalf("stored JSON has a sequence number: %s", b)
		}
	}
}

// Top lists read hourly buckets; Summary tells where they really start.
func TestSummaryReportsTopListStart(t *testing.T) {
	s, _ := newTestStore(t)
	to := time.Date(2026, 9, 24, 10, 5, 0, 0, time.UTC)
	for _, tc := range []struct {
		from time.Time
		want time.Time
	}{
		{to.Add(-15 * time.Minute), time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)},
		{to.Add(-time.Hour), time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)},
		{time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC), time.Date(2026, 9, 24, 8, 0, 0, 0, time.UTC)},
	} {
		sum, err := s.Summary(context.Background(), tc.from, to)
		if err != nil {
			t.Fatal(err)
		}
		if !sum.TopFrom.Equal(tc.want) || !sum.From.Equal(tc.from) {
			t.Errorf("range from %v: topFrom %v, from %v", tc.from, sum.TopFrom, sum.From)
		}
		if got := TopFrom(tc.from, to); !got.Equal(tc.want) {
			t.Errorf("TopFrom(%v) = %v", tc.from, got)
		}
	}
}
