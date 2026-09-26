package logs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// The export reads keyset chunks of ExportChunk rows newest first; rows with
// equal timestamps are neither skipped nor repeated at a chunk boundary.
func TestExportChunks(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)
	total := 2*ExportChunk + 17
	for i := range total {
		// Groups of 7 queries share a timestamp, some groups span a boundary.
		s.w.addQuery(query(now.Add(-time.Duration(i/7)*time.Millisecond), "10.0.0.1", fmt.Sprintf("q%05d.example", i), "forwarded"))
		if s.w.rows() >= batchRows {
			s.w.flush(now)
		}
	}
	s.w.flush(now)
	seen := map[string]bool{}
	var chunks []int
	var lastTS time.Time
	var lastID int64
	err := s.ExportQueries(ctx, QueryFilter{From: now.Add(-time.Hour), To: now.Add(time.Second)}, func(c []QueryEvent) (bool, error) {
		chunks = append(chunks, len(c))
		for _, e := range c {
			if seen[e.QName] {
				t.Fatalf("%s exported twice", e.QName)
			}
			seen[e.QName] = true
			if !lastTS.IsZero() && (e.Time.After(lastTS) || (e.Time.Equal(lastTS) && e.ID >= lastID)) {
				t.Fatalf("not newest first at %s", e.QName)
			}
			lastTS, lastID = e.Time, e.ID
		}
		return true, nil
	})
	if err != nil || len(seen) != total || fmt.Sprint(chunks) != fmt.Sprint([]int{ExportChunk, ExportChunk, 17}) {
		t.Fatalf("exported %d in %v, %v", len(seen), chunks, err)
	}
	// fn stops the export.
	calls := 0
	if err := s.ExportQueries(ctx, QueryFilter{From: now.Add(-time.Hour)}, func([]QueryEvent) (bool, error) {
		calls++
		return false, nil
	}); err != nil || calls != 1 {
		t.Fatalf("stop: %d calls, %v", calls, err)
	}
}

// rcode and dnssec filter the query log and the export.
func TestQueryFilterRCodeAndDNSSEC(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Second) // before the default end of the query-log range
	for i, rc := range []string{"NOERROR", "NXDOMAIN", "SERVFAIL", "NOERROR"} {
		q := query(now, "10.0.0.1", fmt.Sprintf("r%d.example", i), "forwarded")
		q.RCode, q.DNSSEC = rc, i%2 == 0
		s.w.addQuery(q)
	}
	s.w.flush(now)
	yes, no := true, false
	for _, tc := range []struct {
		f    QueryFilter
		want int
	}{
		{QueryFilter{RCode: []string{"nxdomain"}}, 1},
		{QueryFilter{RCode: []string{"NXDOMAIN", " servfail "}}, 2},
		{QueryFilter{RCode: []string{"NOERROR"}, DNSSEC: &yes}, 1},
		{QueryFilter{DNSSEC: &no}, 2},
		{QueryFilter{RCode: []string{"REFUSED"}}, 0},
	} {
		page, err := s.QueryLog(ctx, tc.f)
		if err != nil || len(page.Items) != tc.want {
			t.Errorf("%+v: %d rows, %v", tc.f, len(page.Items), err)
		}
		n := 0
		if err := s.ExportQueries(ctx, tc.f, func(c []QueryEvent) (bool, error) { n += len(c); return true, nil }); err != nil || n != tc.want {
			t.Errorf("export %+v: %d rows, %v", tc.f, n, err)
		}
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = fmt.Sprintf("RCODE%d", i)
	}
	for _, bad := range [][]string{{"NO ERROR"}, {"nx-domain"}, {"ABCDEFGHIJKLMNOPQ"}, {"é"}, many} {
		_, err := s.QueryLog(ctx, QueryFilter{RCode: bad})
		if e, ok := apperr.As(err); !ok || e.Field != "rcode" {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// The rcode filter stops at the 17th distinct value, so a huge list costs
// linear time; repeated values count once.
func TestRCodeFilterBounded(t *testing.T) {
	huge := make([]string, 200_000)
	for i := range huge {
		huge[i] = fmt.Sprintf("C%d", i)
	}
	start := time.Now()
	if _, err := rcodeFilter(huge); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("huge list: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("huge list took %v", d)
	}
	same := make([]string, 1000)
	for i := range same {
		same[i] = []string{"noerror", "NXDOMAIN", " servfail"}[i%3]
	}
	if got, err := rcodeFilter(same); err != nil || fmt.Sprint(got) != "[NOERROR NXDOMAIN SERVFAIL]" {
		t.Fatalf("repeated values: %v, %v", got, err)
	}
}

// Client series: allowed, blocked and cache bytes per step for the
// addresses of a client, from the hourly (and daily) client rows and the
// in-memory hour.
func TestClientSeries(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	hour := hourStart(now.UnixMilli())
	insertHourly(t, s, hour-2*hourMs, "client", "10.0.0.1", 10, 4)
	insertHourly(t, s, hour-2*hourMs, "client", "fd00::1", 5, 0)
	insertHourly(t, s, hour-2*hourMs, "client", "10.0.0.9", 99, 99)
	if _, err := s.d.W.Exec(`INSERT INTO logs_cache_top_hourly (bucket, kind, key, bytes_sent) VALUES (?, 'client', '10.0.0.1', 1000)`,
		hour-hourMs); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"forwarded", "blocked-list", "forwarded"} {
		s.w.addQuery(query(now, "fd00::1", "x.example", st))
	}
	// Whole hours, so that the bucket count does not depend on the minute.
	ser, err := s.ClientSeries(ctx, []string{"10.0.0.1", "fd00::1"}, time.UnixMilli(hour-3*hourMs), time.UnixMilli(hour+hourMs), 0)
	if err != nil || ser.Step != 3600 || len(ser.Timestamps) != 4 {
		t.Fatalf("series %+v, %v", ser, err)
	}
	if got := fmt.Sprint(ser.Values["allowed"], ser.Values["blocked"], ser.Values["cacheBytes"]); got != "[0 11 0 2] [0 4 0 1] [0 0 1000 0]" {
		t.Fatalf("values %s (timestamps %v)", got, ser.Timestamps)
	}
	if fmt.Sprint(ser.Addresses) != "[10.0.0.1 fd00::1]" {
		t.Fatalf("addresses %v", ser.Addresses)
	}
	// Unknown keys give zeros; the step bounds.
	ser, err = s.ClientSeries(ctx, []string{"10.9.9.9"}, time.Time{}, time.Time{}, 0)
	if err != nil || ser.Step != 3600 || len(ser.Values["allowed"]) != 25 {
		t.Fatalf("empty series %d points, step %d, %v", len(ser.Values["allowed"]), ser.Step, err)
	}
	for _, tc := range []struct {
		step  time.Duration
		from  time.Duration
		field string
	}{
		{time.Minute, 24 * time.Hour, "step"},
		{59 * time.Minute, 24 * time.Hour, "step"},
		{time.Hour, 70 * 24 * time.Hour, "step"}, // 1680 points
	} {
		_, err := s.ClientSeries(ctx, []string{"10.0.0.1"}, now.Add(-tc.from), now, tc.step)
		if e, ok := apperr.As(err); !ok || e.Field != tc.field {
			t.Errorf("step %v over %v: %v", tc.step, tc.from, err)
		}
	}
	ser, err = s.ClientSeries(ctx, []string{"10.0.0.1"}, now.Add(-24*time.Hour), now, 90*time.Minute)
	if err != nil || ser.Step != 7200 {
		t.Fatalf("rounded step %d, %v", ser.Step, err)
	}
	// Day steps read the daily rows of complete days.
	today := dayStart(now.UnixMilli())
	if _, err := s.d.W.Exec(`INSERT INTO logs_dns_top_daily (bucket, kind, key, count, blocked) VALUES (?, 'client', '10.0.0.1', 50, 5)`,
		today-3*dayMs); err != nil {
		t.Fatal(err)
	}
	ser, err = s.ClientSeries(ctx, []string{"10.0.0.1"}, now.Add(-5*24*time.Hour), now, 25*time.Hour)
	if err != nil || ser.Step != 2*86400 {
		t.Fatalf("day series step %d, %v", ser.Step, err)
	}
	var sum float64
	for _, v := range ser.Values["allowed"] {
		sum += v
	}
	// The daily row (50 − 5) and, while it is in today, the hourly row of
	// two hours ago (10 − 4); yesterday has no daily rows.
	want := 45.0
	if hour-2*hourMs >= today {
		want += 6
	}
	if sum != want {
		t.Fatalf("day series allowed %v (sum %v, want %v)", ser.Values["allowed"], sum, want)
	}
	if _, err := s.ClientSeries(ctx, make([]string, MaxSeriesAddresses+1), now.Add(-time.Hour), now, 0); apperr.KindOf(err) != apperr.KindInvalid {
		t.Fatalf("too many addresses: %v", err)
	}
}
