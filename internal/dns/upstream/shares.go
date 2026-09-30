package upstream

import (
	"container/list"
	"context"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// chainShares are the clients' shares of the DNSSEC chain exchanges
// (docs/ARCHITECTURE.md 7.6): a token bucket per client key (the DNS
// server's device key, WithClient), at most max keys (the least recently
// used one is dropped), a key unused for chainClientIdle forgotten
// (sweep). A lookup waits for its share's turn within its context's
// deadline, like the global rate: a burst is delayed, not refused.
type chainShares struct {
	limit rate.Limit
	burst int
	max   int

	mu  sync.Mutex
	m   map[string]*list.Element // of *chainShare
	lru list.List                // most recently used first
}

type chainShare struct {
	key  string
	lim  *rate.Limiter
	used time.Time
}

func newChainShares(perSecond float64, burst, max int) *chainShares {
	return &chainShares{limit: rate.Limit(perSecond), burst: burst, max: max, m: map[string]*list.Element{}}
}

// wait takes one chain exchange from key's share, waiting for it within
// ctx's deadline; it fails at once when the wait would outlast the
// deadline (nothing is taken then).
func (s *chainShares) wait(ctx context.Context, key string) error {
	return s.limiter(key, time.Now()).Wait(ctx)
}

// limiter returns key's bucket (a new, full one for a new key).
func (s *chainShares) limiter(key string, now time.Time) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.m[key]; ok {
		sh := el.Value.(*chainShare)
		sh.used = now
		s.lru.MoveToFront(el)
		return sh.lim
	}
	if s.lru.Len() >= s.max {
		if old := s.lru.Back(); old != nil {
			s.lru.Remove(old)
			delete(s.m, old.Value.(*chainShare).key)
		}
	}
	sh := &chainShare{key: key, lim: rate.NewLimiter(s.limit, s.burst), used: now}
	s.m[key] = s.lru.PushFront(sh)
	return sh.lim
}

// sweep forgets the shares unused for idle (their buckets are full again
// by then).
func (s *chainShares) sweep(idle time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for el := s.lru.Back(); el != nil; {
		sh := el.Value.(*chainShare)
		if now.Sub(sh.used) < idle {
			return
		}
		prev := el.Prev()
		s.lru.Remove(el)
		delete(s.m, sh.key)
		el = prev
	}
}

// len returns the number of shares kept.
func (s *chainShares) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lru.Len()
}
