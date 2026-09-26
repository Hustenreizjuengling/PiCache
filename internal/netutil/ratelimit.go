package netutil

import (
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// RateLimiter is a token bucket per rate-limit key (RateKey with the
// configured prefix lengths; the DNS rate limit). It is safe for concurrent
// use and every operation on the query path is O(1): buckets
// are kept in least-recently-seen order, so when the table is full the
// oldest bucket is evicted (counted as overflow) instead of scanning the
// table. Call Sweep periodically (every ~10 s) to drop idle buckets.
// Reconfigure changes the limits in place, keeping buckets and drop
// statistics.
type RateLimiter struct {
	cfg atomic.Pointer[rateConfig]

	mu       sync.Mutex
	max      int // bucket table capacity (maxBuckets; lowered in tests)
	buckets  map[netip.Prefix]*bucket
	newest   *bucket // most recently seen (list head)
	oldest   *bucket // least recently seen (list tail)
	dropped  uint64
	overflow uint64
	limited  map[netip.Prefix]*RateLimited // drop stats, pruned after 1 h
}

// rateConfig is an immutable limiter configuration.
type rateConfig struct {
	qps    float64
	burst  float64
	exempt []netip.Prefix
	v4Bits int // RateKey prefix lengths of public sources
	v6Bits int
}

// RateLimited describes a client that was rate limited recently.
type RateLimited struct {
	Client  string    `json:"client"` // prefix string (e.g. "192.168.1.5/32")
	Dropped uint64    `json:"dropped"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
}

// bucket is the token bucket of one client key, linked into the
// least-recently-seen list.
type bucket struct {
	key          netip.Prefix
	tokens       float64
	burst        float64 // burst the bucket was last refilled with
	seen         time.Time
	newer, older *bucket
}

// maxBuckets bounds the bucket table (≈ 15 MB when full).
const maxBuckets = 100_000

// NewRateLimiter returns a limiter keyed by RateKey with the prefix lengths
// /32 and /64 (the keys of ClientKey); qps <= 0 disables limiting.
func NewRateLimiter(qps, burst int, exempt []netip.Prefix) *RateLimiter {
	r := &RateLimiter{max: maxBuckets, buckets: map[netip.Prefix]*bucket{}, limited: map[netip.Prefix]*RateLimited{}}
	r.cfg.Store(newRateConfig(qps, burst, exempt, 32, 64))
	return r
}

func newRateConfig(qps, burst int, exempt []netip.Prefix, v4Bits, v6Bits int) *rateConfig {
	if burst < 1 {
		burst = 1
	}
	return &rateConfig{qps: float64(qps), burst: float64(burst), exempt: slices.Clone(exempt), v4Bits: v4Bits, v6Bits: v6Bits}
}

// Reconfigure changes the limits, the exempt networks and the prefix
// lengths public sources are keyed by (RateKey) in place. Existing buckets
// and drop statistics are kept; a bucket picks up the new rate on its next
// request and gains the difference when the burst was raised, so a raised
// limit applies immediately (buckets of keys that no longer occur are
// swept). qps <= 0 disables limiting.
func (r *RateLimiter) Reconfigure(qps, burst int, exempt []netip.Prefix, v4Bits, v6Bits int) {
	if r == nil {
		return
	}
	r.cfg.Store(newRateConfig(qps, burst, exempt, v4Bits, v6Bits))
}

// Allow reports whether a request from ip may proceed. first reports whether
// this is the first drop for this client within the last hour (callers log once).
func (r *RateLimiter) Allow(ip netip.Addr) (ok, first bool) {
	if r == nil {
		return true, false
	}
	cfg := r.cfg.Load()
	if cfg.qps <= 0 || inAny(ip, cfg.exempt) {
		return true, false
	}
	key := RateKey(ip, cfg.v4Bits, cfg.v6Bits)
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, found := r.buckets[key]
	if found {
		r.unlink(b)
		elapsed := now.Sub(b.seen).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * cfg.qps
		}
		if cfg.burst > b.burst { // raised burst: grant the difference now
			b.tokens += cfg.burst - b.burst
		}
		b.tokens = min(b.tokens, cfg.burst)
	} else {
		if len(r.buckets) >= r.max {
			r.evictOldest()
			r.overflow++
		}
		b = &bucket{key: key, tokens: cfg.burst}
		r.buckets[key] = b
	}
	b.burst = cfg.burst
	b.seen = now
	r.pushNewest(b)
	if b.tokens >= 1 {
		b.tokens--
		return true, false
	}
	r.dropped++
	l, seen := r.limited[key]
	if !seen {
		if len(r.limited) >= 1024 {
			return false, false
		}
		l = &RateLimited{Client: key.String(), First: now}
		r.limited[key] = l
	}
	l.Dropped++
	l.Last = now
	return false, !seen
}

// pushNewest links b at the head (most recently seen).
func (r *RateLimiter) pushNewest(b *bucket) {
	b.newer, b.older = nil, r.newest
	if r.newest != nil {
		r.newest.newer = b
	}
	r.newest = b
	if r.oldest == nil {
		r.oldest = b
	}
}

// unlink removes b from the list (not from the map).
func (r *RateLimiter) unlink(b *bucket) {
	if b.newer != nil {
		b.newer.older = b.older
	} else {
		r.newest = b.older
	}
	if b.older != nil {
		b.older.newer = b.newer
	} else {
		r.oldest = b.newer
	}
	b.newer, b.older = nil, nil
}

// evictOldest drops the least recently seen bucket.
func (r *RateLimiter) evictOldest() {
	if b := r.oldest; b != nil {
		r.unlink(b)
		delete(r.buckets, b.key)
	}
}

// Dropped returns the number of rejected requests and of buckets evicted
// because the table was full.
func (r *RateLimiter) Dropped() (dropped, overflow uint64) {
	if r == nil {
		return 0, 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped, r.overflow
}

// Top returns up to n clients rate limited within the last hour, most drops first.
func (r *RateLimiter) Top(n int) []RateLimited {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	out := make([]RateLimited, 0, len(r.limited))
	for _, l := range r.limited {
		out = append(out, *l)
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b RateLimited) int {
		switch {
		case a.Dropped > b.Dropped:
			return -1
		case a.Dropped < b.Dropped:
			return 1
		}
		return 0
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Sweep removes buckets idle for longer than idle and drop stats older than
// 1 h. Idle buckets are found from the oldest end of the list, so the cost
// is proportional to the number of removed buckets.
func (r *RateLimiter) Sweep(idle time.Duration) {
	if r == nil {
		return
	}
	now := time.Now()
	cutoff := now.Add(-idle)
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.oldest != nil && r.oldest.seen.Before(cutoff) {
		r.evictOldest()
	}
	for k, l := range r.limited {
		if now.Sub(l.Last) > time.Hour {
			delete(r.limited, k)
		}
	}
}
