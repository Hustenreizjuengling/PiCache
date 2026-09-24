package logs

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestSpreadIsExactAndProportional(t *testing.T) {
	var got []int64
	var sum [4]int64
	// 90 s starting 30 s into a minute: 30 s, 60 s → parts 1/3 and 2/3.
	spread(30_000, 120_000, minuteMs, [4]int64{900, 1, 0, 7}, func(b int64, p [4]int64) {
		got = append(got, b, p[0])
		for i := range p {
			sum[i] += p[i]
		}
	})
	if fmt.Sprint(got) != "[0 300 60000 600]" || sum != [4]int64{900, 1, 0, 7} {
		t.Fatalf("spread = %v, sums %v", got, sum)
	}
	// Large values and long spans do not overflow.
	var total int64
	spread(1, 25*hourMs, minuteMs, [4]int64{1 << 50}, func(_ int64, p [4]int64) { total += p[0] })
	if total != 1<<50 {
		t.Fatalf("total = %d", total)
	}
	calls := 0
	spread(5, 5, hourMs, [4]int64{3}, func(b int64, p [4]int64) { calls++ })
	if calls != 1 {
		t.Fatalf("zero-length span calls = %d", calls)
	}
}

func TestRollupsMatchRawEvents(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	rng := rand.New(rand.NewPCG(1, 2))
	now := time.Now()
	start := now.Add(-6 * time.Hour)
	statuses := append([]string{"weird"}, knownStatuses...)
	var sent, hit, wan, stored, sni int64
	for i := range 3000 {
		ts := start.Add(time.Duration(rng.Int64N(int64(5 * time.Hour))))
		s.w.addQuery(query(ts, fmt.Sprintf("10.0.%d.%d", i%3, i%7), fmt.Sprintf("d%d.example", i%50), statuses[rng.IntN(len(statuses))]))
		if i%3 == 0 {
			e := cacheEv(ts, "10.0.0.1", []string{"steam", "epicgames"}[i%2], "g", rng.Int64N(1<<30), rng.Int64N(1<<29), rng.Int64N(1<<29), rng.Int64N(600_000))
			e.BytesStored = e.BytesWAN / 2
			sent, hit, wan, stored = sent+e.BytesSent, hit+e.BytesHit, wan+e.BytesWAN, stored+e.BytesStored
			s.w.addCache(e)
		}
		if i%10 == 0 {
			up, down := rng.Int64N(1<<20), rng.Int64N(1<<30)
			sni += up + down
			s.w.addSNI(SNIEvent{Time: ts, ClientIP: "10.0.0.2", SNI: "x.example", Service: "epicgames",
				BytesUp: up, BytesDown: down, DurationMs: rng.Int64N(int64(3 * time.Hour / time.Millisecond))})
		}
		if s.w.rows() >= batchRows/2 {
			s.w.flush(now)
		}
	}
	s.w.flush(now)

	raw := map[string]int64{}
	rows, err := s.d.R.Query(`SELECT status, COUNT(*) FROM logs_queries GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			t.Fatal(err)
		}
		switch {
		case isBlocked(st):
			raw["blocked"] += n
		case st == "forwarded" || st == "stale" || st == "local" || st == "special":
			raw["allowed"] += n
		case st == "cached" || st == "lancache":
			raw[st] += n
		default:
			raw["other"] += n
		}
		raw["total"] += n
		if st == "forwarded" {
			raw["forwarded"] += n
		}
	}
	rows.Close()

	// Minute rollups (range within 48 h) and hourly rollups (older start).
	for _, from := range []time.Time{now.Add(-7 * time.Hour), now.Add(-72 * time.Hour)} {
		to := now.Add(4 * time.Hour) // covers transfers that end later
		sum, err := s.Summary(ctx, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if sum.DNSQueries != raw["total"] || sum.DNSBlocked != raw["blocked"] || sum.DNSCached != raw["cached"] ||
			sum.DNSLanCache != raw["lancache"] || sum.DNSForwarded != raw["forwarded"] || sum.AvgDNSDurationUs != 1000 {
			t.Fatalf("from %v: summary %+v, raw %v", now.Sub(from), sum, raw)
		}
		if sum.CacheRequests != 1000 || sum.CacheBytesSent != sent || sum.CacheBytesHit != hit ||
			sum.CacheBytesWAN != wan || sum.CacheBytesStored != stored || sum.SNIBytes != sni {
			t.Fatalf("from %v: cache summary %+v", now.Sub(from), sum)
		}
		dns, err := s.DNSSeries(ctx, from, to, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range dnsSeriesKeys {
			var total float64
			for _, v := range dns.Values[k] {
				total += v
			}
			if int64(total) != raw[k] {
				t.Fatalf("from %v: series %s = %v, raw %d", now.Sub(from), k, total, raw[k])
			}
		}
		cs, err := s.CacheSeries(ctx, from, to, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		var h, w, sn float64
		for i := range cs.Timestamps {
			h, w, sn = h+cs.Values["hit"][i], w+cs.Values["wan"][i], sn+cs.Values["sni"][i]
		}
		if int64(h) != hit || int64(w) != wan || int64(sn) != sni {
			t.Fatalf("from %v: cache series %v/%v/%v, want %d/%d/%d", now.Sub(from), h, w, sn, hit, wan, sni)
		}
	}
	svc, err := s.ServiceStats(ctx, now.Add(-7*time.Hour), now.Add(4*time.Hour))
	if err != nil || len(svc) != 2 {
		t.Fatalf("service stats = %+v, %v", svc, err)
	}
	if svc[0].Requests+svc[1].Requests != 1000 || svc[0].SNIBytes+svc[1].SNIBytes != sni || svc[0].SNIConnections+svc[1].SNIConnections != 300 {
		t.Fatalf("service stats = %+v", svc)
	}
}

func TestSeriesAlignmentZeroFillAndLimits(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	to := time.Now().Truncate(time.Minute)
	from := to.Add(-time.Hour)
	s.w.addQuery(query(from.Add(90*time.Second), "10.0.0.1", "a.example", "blocked-list"))
	s.w.addQuery(query(from.Add(100*time.Second), "10.0.0.1", "a.example", "cached"))
	s.w.addQuery(query(to.Add(-time.Second), "10.0.0.1", "a.example", "refused"))
	s.w.flush(to)

	ser, err := s.DNSSeries(ctx, from, to, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ser.Step != 60 || len(ser.Timestamps) != 60 || ser.Timestamps[0] != from.Unix() || ser.Timestamps[59] != to.Unix()-60 {
		t.Fatalf("series step %d, %d points, first %d", ser.Step, len(ser.Timestamps), ser.Timestamps[0])
	}
	for k, vals := range ser.Values {
		for i, v := range vals {
			want := 0.0
			if (k == "blocked" || k == "cached") && i == 1 || k == "other" && i == 59 {
				want = 1
			}
			if v != want {
				t.Fatalf("%s[%d] = %v, want %v", k, i, v, want)
			}
		}
	}
	// A 5-minute step aligns buckets on multiples of the step.
	ser, err = s.DNSSeries(ctx, from, to, 5*time.Minute)
	if err != nil || ser.Timestamps[0]%300 != 0 || ser.Timestamps[0] > from.Unix() {
		t.Fatalf("5-minute series = %+v, %v", ser, err)
	}
	if i := (from.Unix() + 90 - ser.Timestamps[0]) / 300; ser.Values["blocked"][i] != 1 {
		t.Fatalf("5-minute series blocked = %v", ser.Values["blocked"])
	}
	// Steps finer than the rollup are rounded up; old ranges use hours.
	ser, err = s.DNSSeries(ctx, from, to, 10*time.Second)
	if err != nil || ser.Step != 60 {
		t.Fatalf("step = %d, %v", ser.Step, err)
	}
	ser, err = s.CacheSeries(ctx, to.Add(-72*time.Hour), to, 90*time.Minute, "steam")
	if err != nil || ser.Step != 7200 || len(ser.Values["sni"]) != len(ser.Timestamps) {
		t.Fatalf("hourly series step = %d, %v", ser.Step, err)
	}

	for _, tc := range []struct {
		name     string
		from, to time.Time
		step     time.Duration
		want     time.Duration
		wantErr  bool
	}{
		{"auto 1h", to.Add(-time.Hour), to, 0, time.Minute, false},
		{"auto 24h", to.Add(-24 * time.Hour), to, 0, 5 * time.Minute, false},
		{"auto 7d", to.Add(-7 * 24 * time.Hour), to, 0, time.Hour, false},
		{"auto 90d", to.Add(-90 * 24 * time.Hour), to, 0, 12 * time.Hour, false},
		{"1500 points ok", to.Add(-25 * time.Hour), to, time.Minute, time.Minute, false},
		{"too many points", to.Add(-47 * time.Hour), to, time.Minute, 0, true},
		{"too many hourly points", to.Add(-90 * 24 * time.Hour), to, time.Hour, 0, true},
		{"from after to", to, to.Add(-time.Hour), 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ser, err := s.DNSSeries(ctx, tc.from, tc.to, tc.step)
			if tc.wantErr {
				wantKind(t, err, apperr.KindInvalid)
				return
			}
			if err != nil || time.Duration(ser.Step)*time.Second != tc.want {
				t.Fatalf("step = %ds, %v, want %v", ser.Step, err, tc.want)
			}
			if n := len(ser.Timestamps); n > maxSeriesPoints || n == 0 {
				t.Fatalf("points = %d", n)
			}
		})
	}
}

func TestTopListsLiveCheckpointAndRollover(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	from, to := now.Add(-2*time.Hour), now.Add(3*time.Hour)
	add := func(n int, client, qname, status, upstream string, dur int64) {
		for range n {
			q := query(now, client, qname, status)
			q.Upstream, q.DurationUs = upstream, dur
			q.ClientName = "name-" + client
			s.w.addQuery(q)
		}
	}
	add(5, "10.0.0.1", "a.example", "forwarded", "https://dns.quad9.net/dns-query", 2000)
	add(3, "10.0.0.2", "b.example", "cached", "", 0)
	add(4, "10.0.0.2", "ads.example", "blocked-list", "", 0)
	s.w.addCache(cacheEv(now, "10.0.0.3", "steam", "steam:depot:1", 1000, 900, 100, 10))
	s.w.addCache(cacheEv(now, "10.0.0.3", "steam", "steam:depot:2", 5000, 0, 5000, 10))
	s.w.addCache(cacheEv(now, "10.0.0.4", "epicgames", "steam:depot:1", 100, 0, 100, 10))
	s.w.flush(now)

	check := func(stage string) {
		t.Helper()
		for _, tc := range []struct {
			kind TopKind
			want string
		}{
			{TopDomains, "[{a.example  5 0 } {b.example  3 0 }]"},
			{TopBlockedDomains, "[{ads.example  4 0 }]"},
			{TopClients, "[{10.0.0.2 name-10.0.0.2 7 0 } {10.0.0.1 name-10.0.0.1 5 0 }]"},
			{TopUpstreams, "[{https://dns.quad9.net/dns-query  5 2000 }]"},
			{TopCacheClients, "[{10.0.0.3  2 6000 } {10.0.0.4  1 100 }]"},
			{TopContent, "[{steam:depot:2 Label steam:depot:2 1 5000 steam} {steam:depot:1 Label steam:depot:1 1 1000 steam} {steam:depot:1 Label steam:depot:1 1 100 epicgames}]"},
		} {
			items, err := s.Top(ctx, tc.kind, from, to, 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprint(items); got != tc.want {
				t.Errorf("%s: top %s = %s, want %s", stage, tc.kind, got, tc.want)
			}
		}
		stats, err := s.ClientStats(ctx, from, to)
		if err != nil || len(stats) != 4 || stats[0].ClientIP != "10.0.0.2" || stats[0].Blocked != 4 ||
			stats[2].ClientIP != "10.0.0.3" || stats[2].CacheBytes != 6000 || stats[2].CacheHitBytes != 900 {
			t.Errorf("%s: client stats = %+v, %v", stage, stats, err)
		}
	}
	check("in memory")
	s.w.checkpoint(now)
	check("after checkpoint")
	hour := s.top.currentHour()
	s.w.rollover(now, hour+hourMs)
	check("after rollover")
	if n := count(t, s, "logs_dns_top_hourly"); n != 6 {
		t.Fatalf("stored dns top rows = %d, want 6", n)
	}
	// A restart within the hour continues from the stored rows.
	if err := s.loadTop(ctx, hour); err != nil {
		t.Fatal(err)
	}
	add(1, "10.0.0.1", "a.example", "forwarded", "", 0)
	items, err := s.Top(ctx, TopDomains, from, to, 1)
	if err != nil || len(items) != 1 || items[0].Count != 6 {
		t.Fatalf("top after reload = %+v, %v", items, err)
	}
	_, err = s.Top(ctx, TopKind("nope"), from, to, 1)
	wantKind(t, err, apperr.KindInvalid)
}

func TestTopListsKeepTop1000PerHour(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for i := range 1200 {
		n := 1
		if i%100 == 0 {
			n = 3 + i/100
		}
		for range n {
			s.w.addQuery(query(now, "10.0.0.1", fmt.Sprintf("d%04d.example", i), "forwarded"))
		}
	}
	s.w.checkpoint(now)
	var n int
	if err := s.d.R.QueryRow(`SELECT COUNT(*) FROM logs_dns_top_hourly WHERE kind = 'domain'`).Scan(&n); err != nil || n != topKeysPerHour {
		t.Fatalf("stored domain rows = %d, %v", n, err)
	}
	items, err := s.Top(ctx, TopDomains, now.Add(-time.Hour), now.Add(time.Hour), 3)
	if err != nil || fmt.Sprint(items) != "[{d1100.example  14 0 } {d1000.example  13 0 } {d0900.example  12 0 }]" {
		t.Fatalf("top = %v, %v", items, err)
	}
}
