package logs

import (
	"context"
	"testing"
	"time"
)

func TestSessionGapLogic(t *testing.T) {
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }
	ev := func(sec int, durMs int64, client, group string) CacheEvent {
		return cacheEv(at(sec), client, "steam", group, 100, 60, 40, durMs)
	}
	for _, tc := range []struct {
		name     string
		events   []CacheEvent
		sessions int
	}{
		{"continuous", []CacheEvent{ev(0, 1000, "a", "g"), ev(60, 1000, "a", "g"), ev(170, 1000, "a", "g")}, 1},
		{"gap from the end of the last request", []CacheEvent{ev(0, 100_000, "a", "g"), ev(220, 0, "a", "g")}, 1},
		{"gap longer than 120 s", []CacheEvent{ev(0, 1000, "a", "g"), ev(122, 0, "a", "g")}, 2},
		{"exactly 120 s", []CacheEvent{ev(0, 0, "a", "g"), ev(120, 0, "a", "g")}, 1},
		{"late event joins", []CacheEvent{ev(100, 0, "a", "g"), ev(10, 50_000, "a", "g")}, 1},
		{"per client and group", []CacheEvent{ev(0, 0, "a", "g"), ev(1, 0, "b", "g"), ev(2, 0, "a", "h")}, 3},
		{"no group, no session", []CacheEvent{ev(0, 0, "a", "")}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := newSessionTracker(nil)
			for i := range tc.events {
				if err := tr.add(&tc.events[i]); err != nil {
					t.Fatal(err)
				}
			}
			if got := len(tr.open) + len(tr.retired); got != tc.sessions {
				t.Fatalf("sessions = %d, want %d", got, tc.sessions)
			}
		})
	}

	tr := newSessionTracker(nil)
	e1, e2 := ev(100, 2000, "a", "g"), ev(10, 1000, "a", "g")
	e2.Label = ""
	_ = tr.add(&e1)
	_ = tr.add(&e2)
	s := tr.open[sessionKey{"a", "steam", "g"}]
	if s.first != at(10).UnixMilli() || s.last != at(102).UnixMilli() || s.requests != 2 || s.sent != 200 ||
		s.hit != 120 || s.wan != 80 || s.label != "Label g" {
		t.Fatalf("session = %+v", s)
	}
}

func TestSessionsStoredAndQueried(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	old := now.Add(-time.Hour)
	for _, e := range []CacheEvent{
		cacheEv(old, "10.0.0.1", "steam", "steam:depot:1", 100, 100, 0, 1000),
		cacheEv(old.Add(30*time.Second), "10.0.0.1", "steam", "steam:depot:1", 50, 0, 50, 1000),
		cacheEv(now.Add(-10*time.Second), "10.0.0.1", "steam", "steam:depot:1", 10, 10, 0, 100), // new session
		cacheEv(now.Add(-5*time.Second), "10.0.0.2", "steam", "steam:depot:1", 20, 0, 20, 100),
		cacheEv(now.Add(-5*time.Second), "10.0.0.2", "epicgames", "epic:fortnite", 5, 5, 0, 100),
	} {
		s.w.addCache(e)
	}
	s.w.flush(now)

	page, err := s.Downloads(ctx, DownloadFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 4 || len(page.Items) != 4 {
		t.Fatalf("downloads = %d/%d, want 4", page.Total, len(page.Items))
	}
	first := page.Items[len(page.Items)-1]
	if first.Requests != 2 || first.BytesSent != 150 || first.BytesHit != 100 || first.BytesWAN != 50 || first.Active ||
		!first.FirstSeen.Equal(old) || first.Label != "Label steam:depot:1" {
		t.Fatalf("oldest session = %+v", first)
	}
	active, err := s.Downloads(ctx, DownloadFilter{ActiveOnly: true, Service: "steam"})
	if err != nil || active.Total != 2 || !active.Items[0].Active {
		t.Fatalf("active steam sessions = %+v, %v", active, err)
	}
	search, err := s.Downloads(ctx, DownloadFilter{Search: "fortn"})
	if err != nil || search.Total != 1 || search.Items[0].GroupKey != "epic:fortnite" {
		t.Fatalf("search = %+v, %v", search, err)
	}

	clients, err := s.GroupClients(ctx, "steam", "steam:depot:1")
	if err != nil || len(clients) != 2 {
		t.Fatalf("group clients = %+v, %v", clients, err)
	}
	if c := clients[1]; c.ClientIP != "10.0.0.1" || c.Sessions != 2 || c.BytesSent != 160 {
		t.Fatalf("group client = %+v", c)
	}
	counts, err := s.GroupClientCounts(ctx, []GroupRef{
		{"steam", "steam:depot:1"}, {"epicgames", "epic:fortnite"}, {"steam", "steam:depot:9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts[GroupRef{"steam", "steam:depot:1"}] != 2 || counts[GroupRef{"epicgames", "epic:fortnite"}] != 1 || len(counts) != 2 {
		t.Fatalf("counts = %v", counts)
	}

	// After a restart a running session continues (looked up in the database).
	s.w = newWriter(s, now)
	s.w.addCache(cacheEv(now, "10.0.0.2", "steam", "steam:depot:1", 1, 1, 0, 10))
	s.w.flush(now)
	if n := count(t, s, "logs_downloads"); n != 4 {
		t.Fatalf("sessions after restart = %d, want 4", n)
	}
	var req int64
	if err := s.d.R.QueryRow(`SELECT requests FROM logs_downloads WHERE client_ip = '10.0.0.2' AND service = 'steam'`).Scan(&req); err != nil || req != 2 {
		t.Fatalf("continued session requests = %d, %v", req, err)
	}
}
