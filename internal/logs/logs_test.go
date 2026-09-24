package logs

import (
	"context"
	"net/netip"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestBatchingEveryFiveSecondsOrFullBatch(t *testing.T) {
	dir := t.TempDir()
	synctest.Test(t, func(t *testing.T) {
		ldb, cdb, set := openTestDBs(t, dir)
		defer cdb.Close()
		defer ldb.Close()
		s, err := New(t.Context(), ldb, set, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { s.Start(ctx); close(done) }()

		for i := range 10 {
			s.LogQuery(query(time.Now(), "192.168.1.10", "example.com", "forwarded"))
			s.LogCache(cacheEv(time.Now(), "192.168.1.10", "steam", "steam:depot:1", int64(i), 0, int64(i), 10))
		}
		time.Sleep(4 * time.Second)
		synctest.Wait()
		if n := count(t, s, "logs_queries"); n != 0 {
			t.Fatalf("rows before the 5 s flush = %d, want 0", n)
		}
		if m := s.Metrics(); m.QueueLength != 20 {
			t.Fatalf("queue length = %d, want 20", m.QueueLength)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n, c := count(t, s, "logs_queries"), count(t, s, "logs_cache_requests"); n != 10 || c != 10 {
			t.Fatalf("rows after flush = %d/%d, want 10/10", n, c)
		}
		if m := s.Metrics(); m.LastFlush.IsZero() || m.QueueLength != 0 {
			t.Fatalf("metrics after flush: %+v", m)
		}

		// A full batch is written at once, without waiting for the ticker.
		for range batchRows {
			s.LogQuery(query(time.Now(), "192.168.1.11", "example.org", "cached"))
		}
		synctest.Wait()
		if n := count(t, s, "logs_queries"); n != 10+batchRows {
			t.Fatalf("rows after a full batch = %d, want %d", n, 10+batchRows)
		}

		// Stop flushes what is pending and persists the hour's top lists.
		s.LogQuery(query(time.Now(), "192.168.1.12", "last.example", "blocked-list"))
		cancel()
		<-done
		if n := count(t, s, "logs_queries"); n != 11+batchRows {
			t.Fatalf("rows after stop = %d, want %d", n, 11+batchRows)
		}
		if n := count(t, s, "logs_dns_top_hourly"); n == 0 {
			t.Fatal("top lists not persisted at stop")
		}
		if s.Metrics().Dropped != 0 {
			t.Fatalf("dropped = %d", s.Metrics().Dropped)
		}
		if _, err := s.QueryLog(context.Background(), QueryFilter{}); apperr.KindOf(err) != apperr.KindUnavailable {
			t.Fatalf("query after stop: %v", err)
		}
	})
}

func TestFullQueuesDropAndCount(t *testing.T) {
	s, _ := newTestStore(t) // writer not running: queues fill up
	for range queryQueue + 7 {
		s.LogQuery(QueryEvent{})
	}
	for range sniQueue + 3 {
		s.LogSNI(SNIEvent{})
	}
	m := s.Metrics()
	if m.Dropped != 10 {
		t.Fatalf("dropped = %d, want 10", m.Dropped)
	}
	if m.QueueLength != queryQueue+sniQueue {
		t.Fatalf("queue length = %d", m.QueueLength)
	}
}

func TestAnonymizeIP(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"192.168.17.42", "192.168.0.0"},
		{"10.1.2.3", "10.1.0.0"},
		{"::ffff:10.1.2.3", "10.1.0.0"},
		{"2001:db8:abcd:12:34::1", "2001:db8:abcd::"},
		{"fe80::1%eth0", "fe80::"},
	} {
		if got := anonymizeIP(netip.MustParseAddr(tc.in)).String(); got != tc.want {
			t.Errorf("anonymizeIP(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		in   string
		anon bool
		want string
	}{
		{"::ffff:192.168.1.5", false, "192.168.1.5"},
		{"not-an-ip", false, "not-an-ip"},
		{"not-an-ip", true, ""},
	} {
		if got := cleanClientIP(tc.in, tc.anon); got != tc.want {
			t.Errorf("cleanClientIP(%q, %v) = %q, want %q", tc.in, tc.anon, got, tc.want)
		}
	}
}

func TestAnonymisedBeforeStorageAndLiveFeed(t *testing.T) {
	s, set := newTestStore(t)
	updateLogs(t, set, func(l *settings.Logs) { l.AnonymizeClientIPs = true })
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
	q := query(now, "192.168.44.55", "example.com", "forwarded")
	q.ClientName = "toms-phone"
	s.w.addQuery(q)
	c := cacheEv(now, "2001:db8:1:2:3::9", "steam", "steam:depot:7", 100, 50, 50, 20)
	c.ClientName = "toms-pc"
	s.w.addCache(c)
	s.w.addSNI(SNIEvent{Time: now, ClientIP: "10.9.8.7", ClientName: "x", SNI: "a.example", Service: "epic"})
	s.w.flush(now)

	if e := <-qch; e.ClientIP != "192.168.0.0" || e.ClientName != "" {
		t.Fatalf("live query not anonymised: %+v", e)
	}
	if e := <-cch; e.ClientIP != "2001:db8:1::" || e.ClientName != "" {
		t.Fatalf("live cache event not anonymised: %+v", e)
	}
	for _, tc := range []struct{ table, want string }{
		{"logs_queries", "192.168.0.0"},
		{"logs_cache_requests", "2001:db8:1::"},
		{"logs_sni", "10.9.0.0"},
		{"logs_downloads", "2001:db8:1::"},
	} {
		var ip, name string
		if err := s.d.R.QueryRow(`SELECT client_ip, client_name FROM `+tc.table).Scan(&ip, &name); err != nil {
			t.Fatal(err)
		}
		if ip != tc.want || name != "" {
			t.Errorf("%s stored %q/%q, want %q without name", tc.table, ip, name, tc.want)
		}
	}
	top, err := s.Top(context.Background(), TopClients, now.Add(-time.Hour), now.Add(time.Hour), 10)
	if err != nil || len(top) != 1 || top[0].Key != "192.168.0.0" {
		t.Fatalf("top clients = %+v, %v", top, err)
	}
}

func TestSubscriberLimitSlowSubscribersAndFilters(t *testing.T) {
	s, _ := newTestStore(t)
	var cancels []func()
	for range MaxSubscribers {
		_, cancel, err := s.SubscribeCache(nil)
		if err != nil {
			t.Fatal(err)
		}
		cancels = append(cancels, cancel)
	}
	_, _, err := s.SubscribeQueries(nil)
	wantKind(t, err, apperr.KindTooMany)
	cancels[0]()
	cancels[0]() // idempotent
	match, err := QueryMatcher("10.0.0.1", []string{"blocked"})
	if err != nil {
		t.Fatal(err)
	}
	ch, cancel, err := s.SubscribeQueries(match)
	if err != nil {
		t.Fatalf("subscribe after cancel: %v", err)
	}
	defer cancel()

	now := time.Now()
	s.w.addQuery(query(now, "10.0.0.2", "ads.example", "blocked-list")) // other client
	s.w.addQuery(query(now, "10.0.0.1", "ok.example", "forwarded"))     // not blocked
	for range liveBuffer + 44 {
		s.w.addQuery(query(now, "10.0.0.1", "ads.example", "blocked-regex"))
	}
	if len(ch) != liveBuffer {
		t.Fatalf("buffered = %d, want %d", len(ch), liveBuffer)
	}
	if got := s.Metrics().LiveDropped; got != 44 {
		t.Fatalf("live dropped = %d, want 44", got)
	}
	if e := <-ch; e.ClientIP != "10.0.0.1" || e.Status != "blocked-regex" {
		t.Fatalf("filter let through %+v", e)
	}

	s.Close()
	for range ch {
	}
	if _, _, err := s.SubscribeCache(nil); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("subscribe after close: %v", err)
	}
}

func TestQueryMatcherValidation(t *testing.T) {
	if m, err := QueryMatcher("", nil); m != nil || err != nil {
		t.Fatalf("empty matcher = %v, %v", m != nil, err)
	}
	_, err := QueryMatcher("ab", nil)
	wantKind(t, err, apperr.KindInvalid)
	_, err = QueryMatcher("", []string{"nonsense"})
	wantKind(t, err, apperr.KindInvalid)
	m, err := QueryMatcher("lap", []string{"allowed"})
	if err != nil {
		t.Fatal(err)
	}
	if !m(QueryEvent{ClientName: "Laptop", Status: "stale"}) || m(QueryEvent{ClientName: "Laptop", Status: "cached"}) {
		t.Fatal("name/class matching wrong")
	}
}

func TestQueryLogDisabledKeepsStatistics(t *testing.T) {
	s, set := newTestStore(t)
	updateLogs(t, set, func(l *settings.Logs) { l.QueryLogEnabled = false })
	_, _, err := s.SubscribeQueries(nil)
	wantKind(t, err, apperr.KindUnavailable)

	now := time.Now()
	for range 10 {
		s.w.addQuery(query(now, "10.0.0.1", "example.com", "forwarded"))
	}
	s.w.flush(now)
	if n := count(t, s, "logs_queries"); n != 0 {
		t.Fatalf("query rows = %d, want 0", n)
	}
	sum, err := s.Summary(context.Background(), now.Add(-time.Hour), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if sum.DNSQueries != 10 || sum.DNSForwarded != 10 || sum.ActiveClients != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	top, err := s.Top(context.Background(), TopDomains, now.Add(-time.Hour), now.Add(time.Hour), 5)
	if err != nil || len(top) != 1 || top[0].Count != 10 {
		t.Fatalf("top = %+v, %v", top, err)
	}
}

func TestLowDiskPausesRawInserts(t *testing.T) {
	s, _ := newTestStore(t)
	free := uint64(100 << 20)
	s.diskFree = func(string) (uint64, bool) { return free, true }
	now := time.Now()
	s.w.checkDisk(now)
	if !s.Metrics().RawPaused {
		t.Fatal("raw inserts not paused")
	}
	s.w.addQuery(query(now, "10.0.0.1", "example.com", "cached"))
	s.w.addCache(cacheEv(now, "10.0.0.1", "steam", "steam:depot:1", 10, 10, 0, 5))
	s.w.flush(now)
	if count(t, s, "logs_queries")+count(t, s, "logs_cache_requests") != 0 {
		t.Fatal("raw rows stored while paused")
	}
	if count(t, s, "logs_dns_minute") != 1 || count(t, s, "logs_downloads") != 1 {
		t.Fatal("rollups and sessions must continue while paused")
	}
	free = 10 << 30
	s.w.checkDisk(now.Add(diskCheckInterval))
	s.w.addQuery(query(now, "10.0.0.1", "example.com", "cached"))
	s.w.flush(now)
	if s.Metrics().RawPaused || count(t, s, "logs_queries") != 1 {
		t.Fatal("raw inserts did not resume")
	}
}

func TestDiscardStore(t *testing.T) {
	s := Discard("logs.db is broken", nil)
	s.LogQuery(QueryEvent{})
	s.LogCache(CacheEvent{})
	s.LogSNI(SNIEvent{})
	s.LogEviction(EvictionEvent{})
	if m := s.Metrics(); m.Disabled != "logs.db is broken" || m.Dropped != 0 {
		t.Fatalf("metrics = %+v", m)
	}
	ctx := context.Background()
	_, err := s.QueryLog(ctx, QueryFilter{})
	wantKind(t, err, apperr.KindUnavailable)
	_, err = s.Summary(ctx, time.Time{}, time.Time{})
	wantKind(t, err, apperr.KindUnavailable)
	_, err = s.Top(ctx, TopContent, time.Time{}, time.Time{}, 5)
	wantKind(t, err, apperr.KindUnavailable)
	_, _, err = s.SubscribeQueries(nil)
	wantKind(t, err, apperr.KindUnavailable)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	s.Start(cctx) // returns at once
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanEvents(t *testing.T) {
	now := time.Now()
	c := cleanCache(CacheEvent{
		Path:      "/depot/1/manifest/2?token=secret#frag",
		Host:      "CDN.Example.COM.",
		UserAgent: string(make([]byte, 300)) + "\xff",
		Label:     "bad \xff utf8",
		BytesSent: -5,
		// A duration beyond the SNI lifetime is clamped.
		DurationMs: 48 * time.Hour.Milliseconds(),
	}, false, now)
	if c.Path != "/depot/1/manifest/2" || c.Host != "cdn.example.com" || len(c.UserAgent) != maxTextLen ||
		c.Label != "bad � utf8" || c.BytesSent != 0 || c.DurationMs != maxEventSpan.Milliseconds() || c.Time.IsZero() {
		t.Fatalf("cleanCache = %+v", c)
	}
	q := cleanQuery(QueryEvent{QName: "WWW.Example.COM.", QType: "aaaa", Status: "Forwarded", Time: now}, false, now)
	if q.QName != "www.example.com" || q.QType != "AAAA" || q.Status != "forwarded" || !q.Time.Equal(now.Truncate(time.Millisecond)) {
		t.Fatalf("cleanQuery = %+v", q)
	}
	if e := cleanSNI(SNIEvent{Time: now.Add(time.Hour)}, false, now); !e.Time.Equal(now.Truncate(time.Millisecond)) {
		t.Fatalf("future timestamp kept: %v", e.Time)
	}
	if got := clean("äöü", 3); got != "ä" {
		t.Fatalf("clean cut inside a rune: %q", got)
	}
}
