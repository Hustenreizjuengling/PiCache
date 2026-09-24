package logs

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

const (
	maxSeriesPoints     = 1500
	defaultSeriesPoints = 300
	maxStatsRange       = 400 * 24 * time.Hour
	defaultStatsRange   = 24 * time.Hour
	defaultTopLimit     = 10
	maxClientStats      = 1000
)

// niceSteps are the automatically chosen series steps.
var niceSteps = []time.Duration{
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
	2 * 24 * time.Hour, 7 * 24 * time.Hour,
}

// DNS and cache series keys.
var (
	dnsSeriesKeys   = []string{"allowed", "cached", "lancache", "blocked", "other"}
	cacheSeriesKeys = []string{"hit", "wan", "sni"}
)

// statsRange normalises [from, to): zero to = now, zero from = to - 24 h.
func statsRange(from, to time.Time) (time.Time, time.Time, error) {
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-defaultStatsRange)
	}
	if !from.Before(to) {
		return from, to, apperr.Invalid("from", "must be before to")
	}
	if to.Sub(from) > maxStatsRange {
		return from, to, apperr.Invalid("range", "must not exceed 400 days")
	}
	return from.UTC(), to.UTC(), nil
}

// rollupFor picks the rollup resolution for a range starting at from:
// minutes while they are retained (48 h), hours beyond.
func rollupFor(from, now time.Time) (suffix string, base time.Duration) {
	if now.Sub(from) <= minuteKeep {
		return "minute", time.Minute
	}
	return "hourly", time.Hour
}

// seriesPoints is the number of step buckets from floor(from) to to.
func seriesPoints(from, to time.Time, step time.Duration) int64 {
	s := step.Milliseconds()
	start := floorTo(from.UnixMilli(), s)
	return (to.UnixMilli() - start + s - 1) / s
}

// resolveStep validates or chooses the series step. step <= 0 chooses the
// smallest nice step with at most 300 points; an explicit step is rounded
// up to a multiple of the rollup resolution; more than 1500 points are
// rejected.
func resolveStep(from, to time.Time, step, base time.Duration) (time.Duration, error) {
	if step <= 0 {
		for _, c := range niceSteps {
			if c >= base && seriesPoints(from, to, c) <= defaultSeriesPoints {
				return c, nil
			}
		}
		return niceSteps[len(niceSteps)-1], nil
	}
	if step > maxStatsRange {
		return 0, apperr.Invalid("step", "must not exceed 400 days")
	}
	if r := step % base; r != 0 {
		step += base - r
	}
	if n := seriesPoints(from, to, step); n > maxSeriesPoints {
		return 0, apperr.Invalid("step", "the range and step give %d points; at most %d are allowed (use a larger step)", n, maxSeriesPoints)
	}
	return step, nil
}

// newSeries allocates a zero-filled series.
func newSeries(start, stepMs, n int64, keys []string) Series {
	s := Series{Step: stepMs / 1000, Timestamps: make([]int64, n), Values: make(map[string][]float64, len(keys))}
	for i := range s.Timestamps {
		s.Timestamps[i] = (start + int64(i)*stepMs) / 1000
	}
	for _, k := range keys {
		s.Values[k] = make([]float64, n)
	}
	return s
}

// DNSSeries returns DNS counts by status class per step.
func (s *Store) DNSSeries(ctx context.Context, from, to time.Time, step time.Duration) (Series, error) {
	return s.series(ctx, from, to, step, "", true)
}

// CacheSeries returns cache bytes (hit, wan, sni) per step, optionally for one service.
func (s *Store) CacheSeries(ctx context.Context, from, to time.Time, step time.Duration, service string) (Series, error) {
	service, err := exact("service", service, maxIDLen)
	if err != nil {
		return Series{}, err
	}
	return s.series(ctx, from, to, step, service, false)
}

func (s *Store) series(ctx context.Context, from, to time.Time, step time.Duration, service string, dns bool) (Series, error) {
	from, to, err := statsRange(from, to)
	if err != nil {
		return Series{}, err
	}
	suffix, base := rollupFor(from, time.Now())
	if step, err = resolveStep(from, to, step, base); err != nil {
		return Series{}, err
	}
	stepMs := step.Milliseconds()
	start := floorTo(from.UnixMilli(), stepMs)
	n := seriesPoints(from, to, step)
	keys := cacheSeriesKeys
	q := `SELECT (bucket - ?) / ?, SUM(bytes_hit), SUM(bytes_wan), SUM(sni_bytes) FROM logs_cache_` + suffix +
		` WHERE bucket >= ? AND bucket < ?`
	args := []any{start, stepMs, start, to.UnixMilli()}
	if dns {
		keys = dnsSeriesKeys
		q = `SELECT (bucket - ?) / ?, SUM(forwarded + stale + local + special), SUM(cached), SUM(lancache),
			SUM(blocked), SUM(other) FROM logs_dns_` + suffix + ` WHERE bucket >= ? AND bucket < ?`
	} else if service != "" {
		q += ` AND service = ?`
		args = append(args, service)
	}
	out := newSeries(start, stepMs, n, keys)

	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return Series{}, err
	}
	defer release()
	rows, err := s.d.R.QueryContext(ctx, q+` GROUP BY 1`, args...)
	if err != nil {
		return Series{}, queryErr(ctx, err)
	}
	defer rows.Close()
	vals := make([]int64, len(keys))
	dst := make([]any, 1+len(keys))
	var i int64
	dst[0] = &i
	for k := range vals {
		dst[k+1] = &vals[k]
	}
	for rows.Next() {
		if err := rows.Scan(dst...); err != nil {
			return Series{}, err
		}
		if i < 0 || i >= n {
			continue
		}
		for k, key := range keys {
			out.Values[key][i] = float64(vals[k])
		}
	}
	return out, queryErr(ctx, rows.Err())
}

// Summary returns dashboard totals for [from, to).
func (s *Store) Summary(ctx context.Context, from, to time.Time) (Summary, error) {
	from, to, err := statsRange(from, to)
	if err != nil {
		return Summary{}, err
	}
	now := time.Now()
	suffix, base := rollupFor(from, now)
	lo, hi := floorTo(from.UnixMilli(), base.Milliseconds()), to.UnixMilli()
	sum := Summary{From: from, To: to, TopFrom: TopFrom(from), DroppedLogEvents: s.dropped.Load()}

	// Snapshots of the in-memory hour first (see rollover).
	hour, dnsClients := s.top.dnsSnapshot(dnsTopClient)
	_, cacheClients := s.top.cacheSnapshot(cacheTopClient)

	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return Summary{}, err
	}
	defer release()
	var durUs int64
	if err := s.d.R.QueryRowContext(ctx, `SELECT COALESCE(SUM(total), 0), COALESCE(SUM(blocked), 0),
		COALESCE(SUM(cached), 0), COALESCE(SUM(lancache), 0), COALESCE(SUM(forwarded), 0), COALESCE(SUM(duration_us), 0)
		FROM logs_dns_`+suffix+` WHERE bucket >= ? AND bucket < ?`, lo, hi).
		Scan(&sum.DNSQueries, &sum.DNSBlocked, &sum.DNSCached, &sum.DNSLanCache, &sum.DNSForwarded, &durUs); err != nil {
		return Summary{}, queryErr(ctx, err)
	}
	if err := s.d.R.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests), 0), COALESCE(SUM(bytes_sent), 0),
		COALESCE(SUM(bytes_hit), 0), COALESCE(SUM(bytes_wan), 0), COALESCE(SUM(bytes_stored), 0),
		COALESCE(SUM(evicted_bytes), 0), COALESCE(SUM(sni_bytes), 0)
		FROM logs_cache_`+suffix+` WHERE bucket >= ? AND bucket < ?`, lo, hi).
		Scan(&sum.CacheRequests, &sum.CacheBytesSent, &sum.CacheBytesHit, &sum.CacheBytesWAN, &sum.CacheBytesStored,
			&sum.EvictedBytes, &sum.SNIBytes); err != nil {
		return Summary{}, queryErr(ctx, err)
	}
	if sum.DNSQueries > 0 {
		sum.BlockedPercent = float64(sum.DNSBlocked) * 100 / float64(sum.DNSQueries)
		sum.AvgDNSDurationUs = durUs / sum.DNSQueries
	}
	if t := sum.CacheBytesHit + sum.CacheBytesWAN; t > 0 {
		sum.ByteHitRatio = float64(sum.CacheBytesHit) / float64(t)
	}

	tlo, thi, mem := topWindow(from, to, hour)
	if !mem {
		dnsClients, cacheClients = nil, nil
	}
	dnsJSON, err := json.Marshal(nonNil(dnsClients))
	if err != nil {
		return Summary{}, err
	}
	cacheJSON, err := json.Marshal(nonNil(cacheClients))
	if err != nil {
		return Summary{}, err
	}
	if err := s.d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
			SELECT key FROM logs_dns_top_hourly WHERE kind = 'client' AND bucket >= ? AND bucket < ?
			UNION SELECT key FROM logs_cache_top_hourly WHERE kind = 'client' AND bucket >= ? AND bucket < ?
			UNION SELECT json_extract(value, '$.k') FROM json_each(?)
			UNION SELECT json_extract(value, '$.k') FROM json_each(?))`,
		tlo, thi, tlo, thi, string(dnsJSON), string(cacheJSON)).Scan(&sum.ActiveClients); err != nil {
		return Summary{}, queryErr(ctx, err)
	}
	if err := s.d.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs_downloads WHERE last_seen >= ?`,
		now.Add(-sessionActive).UnixMilli()).Scan(&sum.ActiveDownloads); err != nil {
		return Summary{}, queryErr(ctx, err)
	}
	return sum, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// TopFrom returns the effective start of the top lists, client statistics
// and Summary.ActiveClients for a range starting at from: they are read
// from hourly top tables, so the range starts at the full hour.
func TopFrom(from time.Time) time.Time {
	return time.UnixMilli(hourStart(from.UnixMilli())).UTC()
}

// topWindow returns the stored hourly buckets [lo, hi) to read for [from,
// to) and whether the in-memory hour (not yet or only partially stored)
// belongs to the range. Stored rows of the in-memory hour are excluded:
// memory holds that hour completely.
func topWindow(from, to time.Time, memHour int64) (lo, hi int64, mem bool) {
	lo, hi = hourStart(from.UnixMilli()), to.UnixMilli()
	mem = memHour >= lo && memHour < hi
	if hi > memHour {
		hi = max(memHour, lo)
	}
	return lo, hi, mem
}

// Top returns a top list for [TopFrom(from), to).
func (s *Store) Top(ctx context.Context, kind TopKind, from, to time.Time, limit int) ([]TopItem, error) {
	from, to, err := statsRange(from, to)
	if err != nil {
		return nil, err
	}
	limit = min(max(limit, 0), topKeysPerHour)
	if limit == 0 {
		limit = defaultTopLimit
	}
	switch kind {
	case TopDomains, TopBlockedDomains, TopClients, TopUpstreams:
		k := map[TopKind]dnsKind{TopDomains: dnsTopDomain, TopBlockedDomains: dnsTopBlocked,
			TopClients: dnsTopClient, TopUpstreams: dnsTopUpstream}[kind]
		rows, err := s.dnsTop(ctx, k, from, to, limit)
		if err != nil {
			return nil, err
		}
		out := make([]TopItem, 0, len(rows))
		for _, r := range rows {
			it := TopItem{Key: r.Key, Count: r.Count}
			switch k {
			case dnsTopClient:
				it.Label = r.Label
			case dnsTopUpstream:
				if r.Count > 0 {
					it.Bytes = r.DurationUs / r.Count
				}
			}
			out = append(out, it)
		}
		return out, nil
	case TopCacheClients, TopContent:
		k := cacheTopClient
		if kind == TopContent {
			k = cacheTopContent
		}
		rows, err := s.cacheTop(ctx, k, from, to, limit)
		if err != nil {
			return nil, err
		}
		out := make([]TopItem, 0, len(rows))
		for _, r := range rows {
			out = append(out, TopItem{Key: r.Key, Label: r.Label, Count: r.Requests, Bytes: r.Sent, Extra: r.Service})
		}
		return out, nil
	}
	return nil, apperr.Invalid("kind", "must be one of domains, blocked, clients, cache-clients, content, upstreams")
}

// dnsTop aggregates the hourly DNS top rows of kind k over [from, to),
// merged with the in-memory current hour, highest count first.
func (s *Store) dnsTop(ctx context.Context, k dnsKind, from, to time.Time, limit int) ([]dnsTopRow, error) {
	hour, mem := s.top.dnsSnapshot(k) // before reading the database (see rollover)
	lo, hi, inRange := topWindow(from, to, hour)
	if !inRange {
		mem = nil
	}
	memJSON, err := json.Marshal(nonNil(mem))
	if err != nil {
		return nil, err
	}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// label comes from the row with the newest last_seen (bare column of MAX()).
	rows, err := s.d.R.QueryContext(ctx, `SELECT key, label, SUM(count) AS c, SUM(blocked), SUM(duration_us), MAX(last_seen)
		FROM (
			SELECT key, label, count, blocked, duration_us, last_seen FROM logs_dns_top_hourly
			WHERE kind = ? AND bucket >= ? AND bucket < ?
			UNION ALL
			SELECT json_extract(value, '$.k'), json_extract(value, '$.l'), json_extract(value, '$.c'),
				json_extract(value, '$.b'), json_extract(value, '$.d'), json_extract(value, '$.t')
			FROM json_each(?)
		) GROUP BY key ORDER BY c DESC, key LIMIT ?`, dnsKindNames[k], lo, hi, string(memJSON), limit)
	if err != nil {
		return nil, queryErr(ctx, err)
	}
	defer rows.Close()
	out := []dnsTopRow{}
	for rows.Next() {
		var r dnsTopRow
		if err := rows.Scan(&r.Key, &r.Label, &r.Count, &r.Blocked, &r.DurationUs, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, queryErr(ctx, rows.Err())
}

// cacheTop aggregates the hourly cache top rows of kind k over [from, to),
// merged with the in-memory current hour, most bytes sent first.
func (s *Store) cacheTop(ctx context.Context, k cacheKind, from, to time.Time, limit int) ([]cacheTopRow, error) {
	hour, mem := s.top.cacheSnapshot(k)
	lo, hi, inRange := topWindow(from, to, hour)
	if !inRange {
		mem = nil
	}
	memJSON, err := json.Marshal(nonNil(mem))
	if err != nil {
		return nil, err
	}
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := s.d.R.QueryContext(ctx, `SELECT service, key, label, SUM(requests), SUM(bytes_sent) AS b, SUM(bytes_hit),
			SUM(bytes_wan), MAX(last_seen)
		FROM (
			SELECT service, key, label, requests, bytes_sent, bytes_hit, bytes_wan, last_seen FROM logs_cache_top_hourly
			WHERE kind = ? AND bucket >= ? AND bucket < ?
			UNION ALL
			SELECT json_extract(value, '$.s'), json_extract(value, '$.k'), json_extract(value, '$.l'),
				json_extract(value, '$.r'), json_extract(value, '$.b'), json_extract(value, '$.h'),
				json_extract(value, '$.w'), json_extract(value, '$.t')
			FROM json_each(?)
		) GROUP BY service, key ORDER BY b DESC, service, key LIMIT ?`, cacheKindNames[k], lo, hi, string(memJSON), limit)
	if err != nil {
		return nil, queryErr(ctx, err)
	}
	defer rows.Close()
	out := []cacheTopRow{}
	for rows.Next() {
		var r cacheTopRow
		if err := rows.Scan(&r.Service, &r.Key, &r.Label, &r.Requests, &r.Sent, &r.Hit, &r.WAN, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, queryErr(ctx, rows.Err())
}

// ServiceStats aggregates traffic per service for [from, to).
func (s *Store) ServiceStats(ctx context.Context, from, to time.Time) ([]ServiceStat, error) {
	from, to, err := statsRange(from, to)
	if err != nil {
		return nil, err
	}
	suffix, base := rollupFor(from, time.Now())
	ctx, release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	rows, err := s.d.R.QueryContext(ctx, `SELECT service, SUM(requests), SUM(bytes_sent), SUM(bytes_hit), SUM(bytes_wan),
			SUM(sni_conns), SUM(sni_bytes)
		FROM logs_cache_`+suffix+` WHERE bucket >= ? AND bucket < ?
		GROUP BY service HAVING SUM(requests) > 0 OR SUM(sni_conns) > 0
		ORDER BY SUM(bytes_sent) + SUM(sni_bytes) DESC, service`,
		floorTo(from.UnixMilli(), base.Milliseconds()), to.UnixMilli())
	if err != nil {
		return nil, queryErr(ctx, err)
	}
	defer rows.Close()
	out := []ServiceStat{}
	for rows.Next() {
		var st ServiceStat
		if err := rows.Scan(&st.Service, &st.Requests, &st.BytesSent, &st.BytesHit, &st.BytesWAN,
			&st.SNIConnections, &st.SNIBytes); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, queryErr(ctx, rows.Err())
}

// ClientStats aggregates per client for [from, to) (at most 1000 clients,
// most queries first).
func (s *Store) ClientStats(ctx context.Context, from, to time.Time) ([]ClientStat, error) {
	from, to, err := statsRange(from, to)
	if err != nil {
		return nil, err
	}
	dnsRows, err := s.dnsTop(ctx, dnsTopClient, from, to, maxClientStats)
	if err != nil {
		return nil, err
	}
	cacheRows, err := s.cacheTop(ctx, cacheTopClient, from, to, maxClientStats)
	if err != nil {
		return nil, err
	}
	byIP := make(map[string]*ClientStat, len(dnsRows)+len(cacheRows))
	for _, r := range dnsRows {
		byIP[r.Key] = &ClientStat{ClientIP: r.Key, ClientName: r.Label, Queries: r.Count, Blocked: r.Blocked,
			LastSeen: db.Time(r.LastSeen)}
	}
	for _, r := range cacheRows {
		c := byIP[r.Key]
		if c == nil {
			c = &ClientStat{ClientIP: r.Key}
			byIP[r.Key] = c
		}
		c.CacheBytes, c.CacheHitBytes = r.Sent, r.Hit
		if c.ClientName == "" {
			c.ClientName = r.Label
		}
		if t := db.Time(r.LastSeen); t.After(c.LastSeen) {
			c.LastSeen = t
		}
	}
	out := make([]ClientStat, 0, len(byIP))
	for _, c := range byIP {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b ClientStat) int {
		return cmp.Or(cmp.Compare(b.Queries, a.Queries), cmp.Compare(b.CacheBytes, a.CacheBytes),
			cmp.Compare(a.ClientIP, b.ClientIP))
	})
	return out[:min(len(out), maxClientStats)], nil
}
