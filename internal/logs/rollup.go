package logs

import (
	"context"
	"database/sql"
	"math/bits"
	"strings"
)

const (
	minuteMs = int64(60_000)
	hourMs   = int64(3_600_000)
)

// Query statuses as produced by the DNS server (ARCHITECTURE 7.1). They are
// repeated here because logs imports no other domain package.
const (
	statusForwarded = "forwarded"
	statusCached    = "cached"
	statusStale     = "stale"
	statusLocal     = "local"
	statusSpecial   = "special"
	statusOverride  = "override"
	// statusSafeSearch: safe search answered with the engine's restricted
	// host (7.1 step 7c); counted as allowed in the local column.
	statusSafeSearch = "safesearch"
	statusRefused    = "refused"
	statusError      = "error"
	blockedPrefix    = "blocked" // blocked-list, blocked-rule, blocked-regex, blocked-cname, blocked-special, blocked-schedule, blocked-service, blocked-upstream, blocked-rebind, blocked-ip
)

// blockedStatuses are the blocked statuses (7.1 steps 7a, 8, 10, 11, 13a, 14,
// 14c, 14d).
var blockedStatuses = []string{
	"blocked-list", "blocked-rule", "blocked-regex", "blocked-cname", "blocked-special", "blocked-schedule", "blocked-service",
	"blocked-upstream", "blocked-rebind", "blocked-ip",
}

// knownStatuses are the statuses accepted by query-log filters.
var knownStatuses = append(append([]string{
	statusForwarded, statusCached, statusStale, statusLocal, statusSpecial, statusOverride, statusSafeSearch},
	blockedStatuses...), statusRefused, statusError)

// statusClasses are the disjoint series classes; filters accept them as
// aliases for their statuses.
var statusClasses = map[string][]string{
	"allowed":  {statusForwarded, statusStale, statusLocal, statusSpecial, statusSafeSearch},
	"cached":   {statusCached},
	"override": {statusOverride},
	"blocked":  blockedStatuses,
	"other":    {statusRefused, statusError},
}

func isBlocked(status string) bool { return strings.HasPrefix(status, blockedPrefix) }

// floorTo returns the start of the bucket of size that contains ms.
func floorTo(ms, size int64) int64 {
	b := ms - ms%size
	if ms < 0 && ms%size != 0 {
		b -= size
	}
	return b
}

func hourStart(ms int64) int64 { return floorTo(ms, hourMs) }

// dnsCounts are the per-bucket DNS counters (columns of logs_dns_minute/hourly).
type dnsCounts struct {
	total, forwarded, cached, stale, local, special, override, blocked, other, durationUs int64
}

func (c *dnsCounts) add(status string, durationUs int64) {
	c.total++
	c.durationUs += durationUs
	switch {
	case status == statusForwarded:
		c.forwarded++
	case status == statusCached:
		c.cached++
	case status == statusStale:
		c.stale++
	case status == statusLocal, status == statusSafeSearch: // no column of its own (no logs.db migration)
		c.local++
	case status == statusSpecial:
		c.special++
	case status == statusOverride:
		c.override++
	case isBlocked(status):
		c.blocked++
	default:
		c.other++
	}
}

// cacheBucket keys the per-service cache rollups.
type cacheBucket struct {
	bucket  int64
	service string
}

// cacheCounts are the per-bucket, per-service cache counters.
type cacheCounts struct {
	requests, sent, hit, wan, stored, sniConns, sniBytes, evictions, evicted int64
}

// rollups accumulates counter deltas between two flushes.
type rollups struct {
	dnsMinute, dnsHour     map[int64]*dnsCounts
	cacheMinute, cacheHour map[cacheBucket]*cacheCounts
}

func newRollups() rollups {
	return rollups{
		dnsMinute:   map[int64]*dnsCounts{},
		dnsHour:     map[int64]*dnsCounts{},
		cacheMinute: map[cacheBucket]*cacheCounts{},
		cacheHour:   map[cacheBucket]*cacheCounts{},
	}
}

func (r *rollups) empty() bool {
	return len(r.dnsMinute) == 0 && len(r.dnsHour) == 0 && len(r.cacheMinute) == 0 && len(r.cacheHour) == 0
}

func (r *rollups) reset() {
	clear(r.dnsMinute)
	clear(r.dnsHour)
	clear(r.cacheMinute)
	clear(r.cacheHour)
}

func dnsAt(m map[int64]*dnsCounts, bucket int64) *dnsCounts {
	c := m[bucket]
	if c == nil {
		c = &dnsCounts{}
		m[bucket] = c
	}
	return c
}

func cacheAt(m map[cacheBucket]*cacheCounts, bucket int64, service string) *cacheCounts {
	k := cacheBucket{bucket, service}
	c := m[k]
	if c == nil {
		c = &cacheCounts{}
		m[k] = c
	}
	return c
}

func (r *rollups) addQuery(e *QueryEvent) {
	ts := e.Time.UnixMilli()
	dnsAt(r.dnsMinute, floorTo(ts, minuteMs)).add(e.Status, e.DurationUs)
	dnsAt(r.dnsHour, floorTo(ts, hourMs)).add(e.Status, e.DurationUs)
}

func (r *rollups) addCache(e *CacheEvent) {
	start := e.Time.UnixMilli()
	end := start + e.DurationMs
	vals := [4]int64{e.BytesSent, e.BytesHit, e.BytesWAN, e.BytesStored}
	for _, res := range [...]struct {
		m    map[cacheBucket]*cacheCounts
		size int64
	}{{r.cacheMinute, minuteMs}, {r.cacheHour, hourMs}} {
		cacheAt(res.m, floorTo(start, res.size), e.Service).requests++
		spread(start, end, res.size, vals, func(b int64, p [4]int64) {
			c := cacheAt(res.m, b, e.Service)
			c.sent += p[0]
			c.hit += p[1]
			c.wan += p[2]
			c.stored += p[3]
		})
	}
}

func (r *rollups) addSNI(e *SNIEvent) {
	start := e.Time.UnixMilli()
	end := start + e.DurationMs
	vals := [4]int64{e.BytesUp + e.BytesDown}
	for _, res := range [...]struct {
		m    map[cacheBucket]*cacheCounts
		size int64
	}{{r.cacheMinute, minuteMs}, {r.cacheHour, hourMs}} {
		cacheAt(res.m, floorTo(start, res.size), e.Service).sniConns++
		spread(start, end, res.size, vals, func(b int64, p [4]int64) {
			cacheAt(res.m, b, e.Service).sniBytes += p[0]
		})
	}
}

func (r *rollups) addEviction(e *EvictionEvent) {
	ts := e.Time.UnixMilli()
	for _, c := range []*cacheCounts{
		cacheAt(r.cacheMinute, floorTo(ts, minuteMs), e.Service),
		cacheAt(r.cacheHour, floorTo(ts, hourMs), e.Service),
	} {
		c.evictions++
		c.evicted += e.Bytes
	}
}

// spread distributes vals over the buckets of size that [start, end)
// overlaps, proportionally to the overlap. The parts of each value add up to
// the value exactly. Buckets whose parts are all zero are skipped.
func spread(start, end, size int64, vals [4]int64, add func(bucket int64, parts [4]int64)) {
	if vals == [4]int64{} {
		return
	}
	first := floorTo(start, size)
	if end-start <= 0 || end <= first+size {
		add(first, vals)
		return
	}
	dur := end - start
	var done [4]int64
	for b := first; b < end; b += size {
		hi := min(b+size, end)
		var parts [4]int64
		for i, v := range vals {
			cum := mulDiv(v, hi-start, dur)
			parts[i] = cum - done[i]
			done[i] = cum
		}
		if parts != [4]int64{} {
			add(b, parts)
		}
	}
}

// mulDiv returns v*a/b for 0 <= v, 0 <= a <= b, b > 0 without overflow.
func mulDiv(v, a, b int64) int64 {
	hi, lo := bits.Mul64(uint64(v), uint64(a))
	q, _ := bits.Div64(hi, lo, uint64(b))
	return int64(q)
}

// writeRollups upserts the accumulated deltas.
func writeRollups(ctx context.Context, tx *sql.Tx, r *rollups) error {
	for table, m := range map[string]map[int64]*dnsCounts{"logs_dns_minute": r.dnsMinute, "logs_dns_hourly": r.dnsHour} {
		if len(m) == 0 {
			continue
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+table+`
			(bucket, total, forwarded, cached, stale, local, special, override, blocked, other, duration_us)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (bucket) DO UPDATE SET
				total = total + excluded.total, forwarded = forwarded + excluded.forwarded,
				cached = cached + excluded.cached, stale = stale + excluded.stale,
				local = local + excluded.local, special = special + excluded.special,
				override = override + excluded.override, blocked = blocked + excluded.blocked,
				other = other + excluded.other, duration_us = duration_us + excluded.duration_us`)
		if err != nil {
			return err
		}
		for b, c := range m {
			if _, err := stmt.ExecContext(ctx, b, c.total, c.forwarded, c.cached, c.stale, c.local,
				c.special, c.override, c.blocked, c.other, c.durationUs); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}
	for table, m := range map[string]map[cacheBucket]*cacheCounts{"logs_cache_minute": r.cacheMinute, "logs_cache_hourly": r.cacheHour} {
		if len(m) == 0 {
			continue
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO `+table+`
			(bucket, service, requests, bytes_sent, bytes_hit, bytes_wan, bytes_stored, sni_conns, sni_bytes, evictions, evicted_bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (bucket, service) DO UPDATE SET
				requests = requests + excluded.requests, bytes_sent = bytes_sent + excluded.bytes_sent,
				bytes_hit = bytes_hit + excluded.bytes_hit, bytes_wan = bytes_wan + excluded.bytes_wan,
				bytes_stored = bytes_stored + excluded.bytes_stored, sni_conns = sni_conns + excluded.sni_conns,
				sni_bytes = sni_bytes + excluded.sni_bytes, evictions = evictions + excluded.evictions,
				evicted_bytes = evicted_bytes + excluded.evicted_bytes`)
		if err != nil {
			return err
		}
		for k, c := range m {
			if _, err := stmt.ExecContext(ctx, k.bucket, k.service, c.requests, c.sent, c.hit, c.wan,
				c.stored, c.sniConns, c.sniBytes, c.evictions, c.evicted); err != nil {
				stmt.Close()
				return err
			}
		}
		stmt.Close()
	}
	return nil
}
