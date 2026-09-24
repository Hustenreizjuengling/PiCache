package netutil

import (
	"net/netip"
	"slices"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter is a token bucket per client key (/32 IPv4, /64 IPv6). It is
// safe for concurrent use. Call Sweep periodically (every ~10 s) to drop idle
// buckets. When the bucket table is full it fails open (the ACL already
// limits who can reach it) and counts the overflow.
type RateLimiter struct {
	mu       sync.Mutex
	qps      rate.Limit
	burst    int
	exempt   []netip.Prefix
	buckets  map[netip.Prefix]*bucket
	dropped  uint64
	overflow uint64
	limited  map[netip.Prefix]*RateLimited // drop stats, pruned after 1 h
}

// RateLimited describes a client that was rate limited recently.
type RateLimited struct {
	Client  string    `json:"client"` // prefix string (e.g. "192.168.1.5/32")
	Dropped uint64    `json:"dropped"`
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

const maxBuckets = 100_000

// NewRateLimiter returns a limiter; qps <= 0 disables limiting.
func NewRateLimiter(qps, burst int, exempt []netip.Prefix) *RateLimiter {
	return &RateLimiter{qps: rate.Limit(qps), burst: burst, exempt: exempt,
		buckets: map[netip.Prefix]*bucket{}, limited: map[netip.Prefix]*RateLimited{}}
}

// Allow reports whether a request from ip may proceed. first reports whether
// this is the first drop for this client within the last hour (callers log once).
func (r *RateLimiter) Allow(ip netip.Addr) (ok, first bool) {
	if r == nil || r.qps <= 0 {
		return true, false
	}
	if inAny(ip, r.exempt) {
		return true, false
	}
	key := ClientKey(ip)
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, found := r.buckets[key]
	if !found {
		if len(r.buckets) >= maxBuckets {
			r.sweepLocked(now.Add(-10 * time.Second))
			if len(r.buckets) >= maxBuckets {
				r.overflow++
				return true, false
			}
		}
		b = &bucket{lim: rate.NewLimiter(r.qps, r.burst)}
		r.buckets[key] = b
	}
	b.seen = now
	if b.lim.AllowN(now, 1) {
		return true, false
	}
	r.dropped++
	l, seen := r.limited[key]
	if !seen {
		if len(r.limited) < 1024 {
			l = &RateLimited{Client: key.String(), First: now}
			r.limited[key] = l
		} else {
			return false, false
		}
	}
	l.Dropped++
	l.Last = now
	return false, !seen
}

// Dropped returns the number of rejected requests and table overflows.
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

// Sweep removes buckets idle for longer than idle and drop stats older than 1 h.
func (r *RateLimiter) Sweep(idle time.Duration) {
	if r == nil {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now.Add(-idle))
	for k, l := range r.limited {
		if now.Sub(l.Last) > time.Hour {
			delete(r.limited, k)
		}
	}
}

func (r *RateLimiter) sweepLocked(cutoff time.Time) {
	for k, b := range r.buckets {
		if b.seen.Before(cutoff) {
			delete(r.buckets, k)
		}
	}
}
