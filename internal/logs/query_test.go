package logs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestCursorPaginationIsStable(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	for i := range 250 {
		ts := now.Add(-time.Duration(i) * time.Second)
		if i >= 100 && i < 150 {
			ts = now.Add(-100 * time.Second) // 50 rows share one timestamp
		}
		s.w.addQuery(query(ts, "10.0.0.1", fmt.Sprintf("q%03d.example", i), "forwarded"))
	}
	s.w.flush(now)

	seen := map[int64]bool{}
	var prevTS time.Time
	var prevID int64
	cursor := ""
	for page := 0; ; page++ {
		p, err := s.QueryLog(ctx, QueryFilter{From: now.Add(-time.Hour), To: now.Add(time.Second), Limit: 100, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != -1 {
			t.Fatalf("total = %d, want -1 (unknown)", p.Total)
		}
		for _, e := range p.Items {
			if seen[e.ID] {
				t.Fatalf("duplicate id %d", e.ID)
			}
			seen[e.ID] = true
			if !prevTS.IsZero() && (e.Time.After(prevTS) || e.Time.Equal(prevTS) && e.ID >= prevID) {
				t.Fatalf("order broken at id %d", e.ID)
			}
			prevTS, prevID = e.Time, e.ID
		}
		if page == 0 {
			// Newer rows arriving between pages do not shift later pages.
			for range 10 {
				s.w.addQuery(query(now, "10.0.0.1", "new.example", "forwarded"))
			}
			s.w.flush(now)
		}
		if p.Next == "" {
			break
		}
		cursor = p.Next
	}
	if len(seen) != 250 {
		t.Fatalf("paged %d rows, want 250", len(seen))
	}
}

func TestQueryLogFilters(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for _, q := range []QueryEvent{
		{ClientIP: "10.0.0.1", ClientName: "laptop", QName: "www.example.com", QType: "A", Status: "forwarded", Upstream: "u1"},
		{ClientIP: "10.0.0.2", ClientName: "phone", QName: "example.com", QType: "AAAA", Status: "blocked-list"},
		{ClientIP: "10.0.0.2", ClientName: "phone", QName: "ads.example.net", QType: "A", Status: "blocked-cname"},
		{ClientIP: "10.0.0.3", QName: "steam.example", QType: "A", Status: "override"},
	} {
		q.Time = now.Add(-time.Minute)
		s.w.addQuery(q)
	}
	s.w.addQuery(query(now.Add(-2*time.Hour), "10.0.0.1", "old.example", "forwarded")) // outside the default hour
	s.w.flush(now)

	for _, tc := range []struct {
		name string
		f    QueryFilter
		want int
	}{
		{"default last hour", QueryFilter{}, 4},
		{"client ip", QueryFilter{Client: "10.0.0.2"}, 2},
		{"client name", QueryFilter{Client: "LAP"}, 1},
		{"domain substring", QueryFilter{Domain: "example.com"}, 2},
		{"domain exact", QueryFilter{Domain: `"example.com"`}, 1},
		{"status class", QueryFilter{Status: []string{"blocked"}}, 2},
		{"status list", QueryFilter{Status: []string{"blocked-cname", "override"}}, 2},
		{"qtype", QueryFilter{QType: "aaaa"}, 1},
		{"upstream", QueryFilter{Upstream: "u1"}, 1},
		{"explicit range", QueryFilter{From: now.Add(-3 * time.Hour), To: now}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := s.QueryLog(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Items) != tc.want {
				t.Fatalf("got %d items, want %d", len(p.Items), tc.want)
			}
		})
	}
	for _, f := range []QueryFilter{
		{Domain: "ex"},
		{Client: "ab"},
		{Status: []string{"bogus"}},
		{Cursor: "not-a-cursor"},
		{From: now, To: now.Add(-time.Minute)},
	} {
		_, err := s.QueryLog(ctx, f)
		wantKind(t, err, apperr.KindInvalid)
	}
}

func TestEventLists(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for i := range 5 {
		e := cacheEv(now.Add(-time.Duration(i)*time.Minute), "10.0.0.1", "steam", "steam:depot:1", 10, 10, 0, 5)
		e.Path = fmt.Sprintf("/depot/1/chunk/%d?secret=x", i)
		if i == 4 {
			e.CacheStatus, e.Service, e.Host = "MISS", "epicgames", "epic.example"
		}
		s.w.addCache(e)
		s.w.addSNI(SNIEvent{Time: now.Add(-time.Duration(i) * time.Minute), ClientIP: "10.0.0.2",
			SNI: fmt.Sprintf("s%d.example", i), Service: "riot", BytesDown: 5})
		s.w.addEviction(EvictionEvent{Time: now.Add(-time.Duration(i) * time.Minute), StoreID: "local",
			ObjectID: fmt.Sprintf("%032x", i), Service: "steam", GroupKey: "steam:depot:1", Bytes: 100,
			Reason: []string{"size", "inactive"}[i%2]})
	}
	s.w.flush(now)

	reqs, err := s.CacheRequests(ctx, EventFilter{Limit: 2})
	if err != nil || len(reqs.Items) != 2 || reqs.Next == "" || reqs.Items[0].Path != "/depot/1/chunk/0" {
		t.Fatalf("requests = %+v, %v", reqs, err)
	}
	reqs, err = s.CacheRequests(ctx, EventFilter{Cursor: reqs.Next, Limit: 10})
	if err != nil || len(reqs.Items) != 3 || reqs.Next != "" {
		t.Fatalf("second page = %+v, %v", reqs, err)
	}
	reqs, err = s.CacheRequests(ctx, EventFilter{Status: "miss", Service: "epicgames", Search: "epic.ex"})
	if err != nil || len(reqs.Items) != 1 {
		t.Fatalf("filtered requests = %+v, %v", reqs, err)
	}
	sni, err := s.SNIEvents(ctx, EventFilter{Search: "s3.exa", Client: "10.0.0.2"})
	if err != nil || len(sni.Items) != 1 || sni.Items[0].SNI != "s3.example" {
		t.Fatalf("sni = %+v, %v", sni, err)
	}
	ev, err := s.Evictions(ctx, EventFilter{Status: "inactive", From: now.Add(-time.Hour), To: now.Add(time.Second)})
	if err != nil || len(ev.Items) != 2 || ev.Items[0].Reason != "inactive" {
		t.Fatalf("evictions = %+v, %v", ev, err)
	}
	sum, err := s.Summary(ctx, now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil || sum.EvictedBytes != 500 {
		t.Fatalf("evicted bytes = %d, %v", sum.EvictedBytes, err)
	}
	_, err = s.SNIEvents(ctx, EventFilter{Search: "x"})
	wantKind(t, err, apperr.KindInvalid)
	_, err = s.Downloads(ctx, DownloadFilter{Offset: -1})
	wantKind(t, err, apperr.KindInvalid)
	_, err = s.GroupClients(ctx, "", "")
	wantKind(t, err, apperr.KindInvalid)
}

func TestCursorEncoding(t *testing.T) {
	c := encodeCursor(1_700_000_000_123, 42)
	ts, id, err := decodeCursor(c)
	if err != nil || ts != 1_700_000_000_123 || id != 42 {
		t.Fatalf("round trip = %d, %d, %v", ts, id, err)
	}
	for _, bad := range []string{"", "AAAA", encodeCursor(-1, 5), encodeCursor(5, 0), c + "A"} {
		if _, _, err := decodeCursor(bad); apperr.KindOf(err) != apperr.KindInvalid {
			t.Errorf("decodeCursor(%q) = %v", bad, err)
		}
	}
}
