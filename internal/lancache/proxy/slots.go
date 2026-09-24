package proxy

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

const (
	// fillMemory bounds the memory of fill buffers: the global slot count
	// is min(maxConcurrentFills, fillMemory / slice size).
	fillMemory = 1 << 30
	// maxFreeBufferBytes bounds the free list of fill buffers.
	maxFreeBufferBytes = 64 << 20
)

// fillLimits are the slot limits in effect for one acquisition.
type fillLimits struct {
	global    int
	perClient int
}

// fillLimits derives the limits from the settings and the slice size.
func (s *Server) fillLimits(sliceSize int64) fillLimits {
	c := s.settings().Cache
	g := max(1, c.MaxConcurrentFills)
	if byMem := int(fillMemory / max(1, sliceSize)); byMem < g {
		g = max(1, byMem)
	}
	return fillLimits{global: g, perClient: min(g, max(1, c.MaxFillsPerClient))}
}

// fillSlots is a counting semaphore with a global and a per-client limit.
// A slot is held for as long as a fill buffer (or a captured slice buffer)
// is alive.
type fillSlots struct {
	mu        sync.Mutex
	used      int
	perClient map[netip.Prefix]int // ≤ global entries (removed at 0)
	wake      chan struct{}        // closed and replaced on every release
}

func (fs *fillSlots) takeLocked(k netip.Prefix, l fillLimits) bool {
	if fs.used >= l.global || fs.perClient[k] >= l.perClient {
		return false
	}
	if fs.perClient == nil {
		fs.perClient = make(map[netip.Prefix]int)
	}
	fs.used++
	fs.perClient[k]++
	return true
}

// tryAcquire takes a slot for client k if one is free.
func (fs *fillSlots) tryAcquire(k netip.Prefix, l fillLimits) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.takeLocked(k, l)
}

// acquire waits up to wait for a slot for client k.
func (fs *fillSlots) acquire(ctx context.Context, k netip.Prefix, l fillLimits, wait time.Duration) bool {
	if fs.tryAcquire(k, l) {
		return true
	}
	if wait <= 0 {
		return false
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		fs.mu.Lock()
		if fs.takeLocked(k, l) {
			fs.mu.Unlock()
			return true
		}
		if fs.wake == nil {
			fs.wake = make(chan struct{})
		}
		ch := fs.wake
		fs.mu.Unlock()
		select {
		case <-ch:
		case <-t.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// release returns a slot of client k.
func (fs *fillSlots) release(k netip.Prefix) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.used--
	if n := fs.perClient[k] - 1; n > 0 {
		fs.perClient[k] = n
	} else {
		delete(fs.perClient, k)
	}
	if fs.wake != nil {
		close(fs.wake)
		fs.wake = nil
	}
}

// bufPool is the free list of slice-sized buffers. The slice size of the
// last get is the current one (the active store's): a get of another size
// drops the list, and buffers of another size are not taken back.
type bufPool struct {
	mu   sync.Mutex
	size int64
	free [][]byte
}

// get returns a buffer of length size.
func (p *bufPool) get(size int64) []byte {
	p.mu.Lock()
	if p.size != size { // the store (slice size) changed
		p.size, p.free = size, nil
	}
	if len(p.free) > 0 {
		b := p.free[len(p.free)-1]
		p.free[len(p.free)-1] = nil
		p.free = p.free[:len(p.free)-1]
		p.mu.Unlock()
		return b[:size]
	}
	p.mu.Unlock()
	return make([]byte, size)
}

// put returns a buffer obtained from get; one of an outdated size is
// dropped.
func (p *bufPool) put(b []byte) {
	size := int64(cap(b))
	p.mu.Lock()
	defer p.mu.Unlock()
	if size != p.size {
		return
	}
	if int64(len(p.free)+1)*size <= maxFreeBufferBytes {
		p.free = append(p.free, b[:cap(b)])
	}
}
