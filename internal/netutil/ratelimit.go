package netutil

import (
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimiter is a per-client-IP token bucket. It is safe for concurrent use.
// Idle buckets are removed by Sweep.
type RateLimiter struct {
	mu      sync.Mutex
	qps     rate.Limit
	burst   int
	exempt  []netip.Prefix
	buckets map[netip.Addr]*bucket
	dropped uint64
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewRateLimiter returns a limiter; qps <= 0 disables limiting.
func NewRateLimiter(qps, burst int, exempt []netip.Prefix) *RateLimiter {
	return &RateLimiter{qps: rate.Limit(qps), burst: burst, exempt: exempt, buckets: map[netip.Addr]*bucket{}}
}

// Allow reports whether a request from ip may proceed.
func (r *RateLimiter) Allow(ip netip.Addr) bool {
	if r == nil || r.qps <= 0 {
		return true
	}
	ip = ip.Unmap()
	if inAny(ip, r.exempt) {
		return true
	}
	now := time.Now()
	r.mu.Lock()
	b, ok := r.buckets[ip]
	if !ok {
		if len(r.buckets) >= 100_000 { // bound memory under spoofed-source floods
			r.dropped++
			r.mu.Unlock()
			return false
		}
		b = &bucket{lim: rate.NewLimiter(r.qps, r.burst)}
		r.buckets[ip] = b
	}
	b.seen = now
	ok = b.lim.AllowN(now, 1)
	if !ok {
		r.dropped++
	}
	r.mu.Unlock()
	return ok
}

// Dropped returns the number of rejected requests so far.
func (r *RateLimiter) Dropped() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

// Sweep removes buckets idle for longer than idle.
func (r *RateLimiter) Sweep(idle time.Duration) {
	if r == nil {
		return
	}
	cutoff := time.Now().Add(-idle)
	r.mu.Lock()
	defer r.mu.Unlock()
	for ip, b := range r.buckets {
		if b.seen.Before(cutoff) {
			delete(r.buckets, ip)
		}
	}
}
