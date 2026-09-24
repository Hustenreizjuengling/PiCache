package logs

import (
	"sync"
	"sync/atomic"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// hub fans events out from the writer to live subscribers. It holds at most
// MaxSubscribers subscriptions (all feeds together); each has a buffer of
// liveBuffer events and loses events while it is full.
type hub struct {
	mu      sync.RWMutex
	closed  bool
	n       int
	queries feed[QueryEvent]
	cache   feed[CacheEvent]
	dropped atomic.Uint64
}

type feed[T any] struct {
	subs map[*subscriber[T]]struct{}
}

type subscriber[T any] struct {
	ch     chan T
	filter func(T) bool
}

// subscribe adds a subscriber to f.
func subscribe[T any](h *hub, f *feed[T], filter func(T) bool) (<-chan T, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, func() {}, apperr.Unavailable("live feed not available")
	}
	if h.n >= MaxSubscribers {
		return nil, func() {}, apperr.TooMany("too many live streams (at most %d)", MaxSubscribers)
	}
	sub := &subscriber[T]{ch: make(chan T, liveBuffer), filter: filter}
	if f.subs == nil {
		f.subs = make(map[*subscriber[T]]struct{})
	}
	f.subs[sub] = struct{}{}
	h.n++
	cancel := sync.OnceFunc(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := f.subs[sub]; ok {
			delete(f.subs, sub)
			h.n--
			close(sub.ch)
		}
	})
	return sub.ch, cancel, nil
}

// publish delivers e to every matching subscriber without blocking.
func publish[T any](h *hub, f *feed[T], e T) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for sub := range f.subs {
		if sub.filter != nil && !sub.filter(e) {
			continue
		}
		select {
		case sub.ch <- e:
		default:
			h.dropped.Add(1)
		}
	}
}

// close ends all subscriptions (their channels are closed) and refuses new ones.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	closeFeed(&h.queries)
	closeFeed(&h.cache)
	h.n = 0
}

func closeFeed[T any](f *feed[T]) {
	for sub := range f.subs {
		close(sub.ch)
	}
	f.subs = nil
}
