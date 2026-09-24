package cachestore

import (
	"math/bits"
	"net/http"
	"sync"
)

// entry is the compact in-memory state of one object. Entries are immutable:
// every change creates a new entry (copy-on-write), so readers never need the
// object lock.
type entry struct {
	gen      uint64
	total    int64
	service  string
	host     string
	path     string
	groupKey string
	header   http.Header // filtered response headers (read-only)
	hdrSize  int
	noSlice  bool
	present  []uint64 // bitmap of cached slices (read-only)

	// busy is non-nil for a tombstone: the object is being removed. It is
	// closed when that has finished.
	busy chan struct{}
	// prev is the entry a tombstone replaced (restored if the removal is
	// skipped); prevDirty reports that prev had unflushed index writes.
	prev      *entry
	prevDirty bool
}

func (e *entry) has(idx int64) bool {
	if idx < 0 || idx/64 >= int64(len(e.present)) {
		return false
	}
	return e.present[idx/64]&(1<<(uint(idx)%64)) != 0
}

// indexes returns the indexes of all cached slices.
func (e *entry) indexes() []int64 {
	n := 0
	for _, w := range e.present {
		n += bits.OnesCount64(w)
	}
	out := make([]int64, 0, n)
	for i, w := range e.present {
		for w != 0 {
			b := bits.TrailingZeros64(w)
			out = append(out, int64(i*64+b))
			w &^= 1 << uint(b)
		}
	}
	return out
}

// clone returns a shallow copy that shares the immutable fields.
func (e *entry) clone() *entry {
	c := *e
	c.busy, c.prev, c.prevDirty = nil, nil, false
	return &c
}

// withSlice returns a copy with slice idx marked present or absent.
func (e *entry) withSlice(idx int64, present bool) *entry {
	c := e.clone()
	c.present = append([]uint64(nil), e.present...)
	if present {
		c.present[idx/64] |= 1 << (uint(idx) % 64)
	} else {
		c.present[idx/64] &^= 1 << (uint(idx) % 64)
	}
	return c
}

// cost approximates the memory held by the entry.
func (e *entry) cost() int64 {
	return int64(200 + len(e.service) + len(e.host) + len(e.path) + len(e.groupKey) + e.hdrSize + 8*len(e.present))
}

func newBitmap(total, sliceSize int64) []uint64 {
	return make([]uint64, (slicesFor(total, sliceSize)+63)/64)
}

// heads holds the in-memory object state:
//   - overlay: entries with index changes not yet flushed to the DB and
//     tombstones. It is authoritative and bounded by the pending-op limit.
//   - an LRU of clean entries, bounded by count and approximate bytes.
type heads struct {
	mu       sync.Mutex
	overlay  map[string]*entry
	nodes    map[string]*lruNode
	root     lruNode // sentinel: root.next is the most recently used
	maxN     int
	maxBytes int64
	bytes    int64
}

type lruNode struct {
	id         string
	e          *entry
	cost       int64
	prev, next *lruNode
}

func newHeads(maxN int, maxBytes int64) *heads {
	h := &heads{overlay: map[string]*entry{}, nodes: map[string]*lruNode{}, maxN: maxN, maxBytes: maxBytes}
	h.root.next, h.root.prev = &h.root, &h.root
	return h
}

// get returns the current entry (tombstones included).
func (h *heads) get(id string) *entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e, ok := h.overlay[id]; ok {
		return e
	}
	if n, ok := h.nodes[id]; ok {
		h.unlink(n)
		h.pushFront(n)
		return n.e
	}
	return nil
}

// set records a changed entry: authoritative in the overlay until the index
// write is flushed, and cached in the LRU.
func (h *heads) set(id string, e *entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.overlay[id] = e
	h.putLocked(id, e)
}

// setClean caches an entry loaded from the index unless the overlay has a
// newer state.
func (h *heads) setClean(id string, e *entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.overlay[id]; !ok {
		h.putLocked(id, e)
	}
}

// tomb replaces the entry of id with a tombstone and returns it.
func (h *heads) tomb(id string, prev *entry) *entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, dirty := h.overlay[id]
	t := &entry{gen: prev.gen, total: prev.total, busy: make(chan struct{}), prev: prev, prevDirty: dirty}
	h.overlay[id] = t
	h.removeLocked(id)
	return t
}

// untomb removes tombstone t. With restore the previous entry becomes
// current again (back in the overlay if its index writes are still
// pending); otherwise the object is gone.
func (h *heads) untomb(id string, t *entry, restore, pending bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.overlay[id] != t {
		return
	}
	delete(h.overlay, id)
	if restore {
		if pending {
			h.overlay[id] = t.prev
		}
		h.putLocked(id, t.prev)
	}
}

// flushed drops the overlay entry of id if it is still e (its index write
// has been committed).
func (h *heads) flushed(id string, e *entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.overlay[id] == e {
		delete(h.overlay, id)
	}
}

func (h *heads) putLocked(id string, e *entry) {
	c := e.cost()
	if c > h.maxBytes/8 {
		h.removeLocked(id) // too large to cache; loaded from the index on demand
		return
	}
	if n, ok := h.nodes[id]; ok {
		h.bytes += c - n.cost
		n.e, n.cost = e, c
		h.unlink(n)
		h.pushFront(n)
	} else {
		n := &lruNode{id: id, e: e, cost: c}
		h.nodes[id] = n
		h.bytes += c
		h.pushFront(n)
	}
	for (len(h.nodes) > h.maxN || h.bytes > h.maxBytes) && h.root.prev != &h.root {
		h.removeNode(h.root.prev)
	}
}

func (h *heads) removeLocked(id string) {
	if n, ok := h.nodes[id]; ok {
		h.removeNode(n)
	}
}

func (h *heads) removeNode(n *lruNode) {
	h.unlink(n)
	delete(h.nodes, n.id)
	h.bytes -= n.cost
}

func (h *heads) unlink(n *lruNode) {
	n.prev.next = n.next
	n.next.prev = n.prev
	n.prev, n.next = nil, nil
}

func (h *heads) pushFront(n *lruNode) {
	n.prev = &h.root
	n.next = h.root.next
	h.root.next.prev = n
	h.root.next = n
}
