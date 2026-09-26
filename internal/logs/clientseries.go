package logs

import (
	"context"
	"encoding/json/v2"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// MaxSeriesAddresses bounds the addresses of one client series (the
// addresses of a device, most recently seen first).
const MaxSeriesAddresses = 256

// Client series keys: DNS queries allowed and blocked (kind client of the
// DNS top tables: count − blocked, blocked) and download cache bytes sent
// (kind client of the cache top tables).
var clientSeriesKeys = []string{"allowed", "blocked", "cacheBytes"}

// ClientSeries returns the activity of addrs (an address, or the addresses
// of a device) per step over [from, to) from the client rows of the top
// tables and the in-memory hour. step must be at least an hour (0 chooses
// at most 300 points); it is rounded up to a multiple of an hour, from a
// day on to a multiple of a day (such series read the daily top tables for
// complete days). More than 1500 points are refused. Unknown addresses
// give a series of zeros.
func (s *Store) ClientSeries(ctx context.Context, addrs []string, from, to time.Time, step time.Duration) (ClientSeries, error) {
	from, to, err := statsRange(from, to)
	if err != nil {
		return ClientSeries{}, err
	}
	if len(addrs) > MaxSeriesAddresses {
		return ClientSeries{}, apperr.Invalid("key", "at most %d addresses", MaxSeriesAddresses)
	}
	if step < 0 || (step > 0 && step < time.Hour) {
		return ClientSeries{}, apperr.Invalid("step", "must be at least 3600 seconds")
	}
	base := time.Hour
	if step >= 24*time.Hour {
		base = 24 * time.Hour
	}
	if step, err = resolveStep(from, to, step, base); err != nil {
		return ClientSeries{}, err
	}
	stepMs := step.Milliseconds()
	start := floorTo(from.UnixMilli(), stepMs)
	n := seriesPoints(from, to, step)
	ser := newSeries(start, stepMs, n, clientSeriesKeys)
	out := ClientSeries{Step: ser.Step, Timestamps: ser.Timestamps, Values: ser.Values, Addresses: slices.Clone(addrs)}
	if out.Addresses == nil {
		out.Addresses = []string{}
	}
	if len(addrs) == 0 {
		return out, nil
	}

	// Snapshots of the in-memory hour first (see rollover).
	hour, dnsMem := s.top.dnsSnapshot(dnsTopClient)
	_, cacheMem := s.top.cacheSnapshot(cacheTopClient)
	var sp topSpan
	if stepMs%dayMs == 0 { // day-aligned buckets: the daily rows of the complete days
		last := dayStart(to.UnixMilli())
		hLo, hHi, mem := topWindow(time.UnixMilli(last), to, hour)
		sp = topSpan{start: start, dayLo: start, dayHi: last, hourLo: hLo, hourHi: hHi, mem: mem}
	} else {
		lo, hi, mem := topWindow(time.UnixMilli(start), to, hour)
		sp = topSpan{start: lo, dayLo: lo, dayHi: lo, hourLo: lo, hourHi: hi, mem: mem}
	}
	keys, err := json.Marshal(addrs)
	if err != nil {
		return ClientSeries{}, err
	}
	add := func(bucket int64, vals ...int64) {
		i := (bucket - start) / stepMs
		if bucket < start || i >= n {
			return
		}
		for k, v := range vals {
			out.Values[clientSeriesKeys[k]][i] += float64(v)
		}
	}

	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return ClientSeries{}, err
	}
	defer release()
	args := append(sp.args(), string(keys))
	rows, err := s.d.R.QueryContext(ctx, spanCTE+`SELECT b, SUM(c) - SUM(bl), SUM(bl) FROM (
			SELECT t.bucket AS b, t.count AS c, t.blocked AS bl FROM days CROSS JOIN logs_dns_top_daily t
			WHERE t.bucket = days.b AND t.kind = 'client' AND t.key IN (SELECT value FROM json_each(?5))
			UNION ALL
			SELECT t.bucket, t.count, t.blocked FROM hours CROSS JOIN logs_dns_top_hourly t
			WHERE t.bucket = hours.b AND t.kind = 'client' AND t.key IN (SELECT value FROM json_each(?5))
		) GROUP BY b`, args...)
	if err != nil {
		return ClientSeries{}, queryErr(ctx, err)
	}
	for rows.Next() {
		var b, allowed, blocked int64
		if err := rows.Scan(&b, &allowed, &blocked); err != nil {
			rows.Close()
			return ClientSeries{}, err
		}
		add(b, allowed, blocked)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ClientSeries{}, queryErr(ctx, err)
	}
	rows, err = s.d.R.QueryContext(ctx, spanCTE+`SELECT b, SUM(sent) FROM (
			SELECT t.bucket AS b, t.bytes_sent AS sent FROM days CROSS JOIN logs_cache_top_daily t
			WHERE t.bucket = days.b AND t.kind = 'client' AND t.key IN (SELECT value FROM json_each(?5))
			UNION ALL
			SELECT t.bucket, t.bytes_sent FROM hours CROSS JOIN logs_cache_top_hourly t
			WHERE t.bucket = hours.b AND t.kind = 'client' AND t.key IN (SELECT value FROM json_each(?5))
		) GROUP BY b`, args...)
	if err != nil {
		return ClientSeries{}, queryErr(ctx, err)
	}
	defer rows.Close()
	for rows.Next() {
		var b, sent int64
		if err := rows.Scan(&b, &sent); err != nil {
			return ClientSeries{}, err
		}
		add(b, 0, 0, sent)
	}
	if err := rows.Err(); err != nil {
		return ClientSeries{}, queryErr(ctx, err)
	}
	if sp.mem {
		for _, r := range dnsMem {
			if slices.Contains(addrs, r.Key) {
				add(hour, r.Count-r.Blocked, r.Blocked)
			}
		}
		for _, r := range cacheMem {
			if slices.Contains(addrs, r.Key) {
				add(hour, 0, 0, r.Sent)
			}
		}
	}
	return out, nil
}
