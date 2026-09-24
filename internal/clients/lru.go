package clients

// lru is a fixed-capacity least-recently-used map. It is not safe for
// concurrent use; callers hold their own mutex.
type lru[K comparable, V any] struct {
	max   int
	items map[K]*lruNode[K, V]
	head  *lruNode[K, V] // most recently used
	tail  *lruNode[K, V] // least recently used
}

type lruNode[K comparable, V any] struct {
	key        K
	val        V
	prev, next *lruNode[K, V]
}

func newLRU[K comparable, V any](max int) *lru[K, V] {
	return &lru[K, V]{max: max, items: make(map[K]*lruNode[K, V])}
}

// get returns the value for k and marks it as recently used.
func (c *lru[K, V]) get(k K) (V, bool) {
	n, ok := c.items[k]
	if !ok {
		var zero V
		return zero, false
	}
	c.moveToFront(n)
	return n.val, true
}

// peek returns the value for k without changing its recency.
func (c *lru[K, V]) peek(k K) (V, bool) {
	n, ok := c.items[k]
	if !ok {
		var zero V
		return zero, false
	}
	return n.val, true
}

// put inserts or replaces k, evicting the least recently used entry when
// full. It reports whether k was newly inserted.
func (c *lru[K, V]) put(k K, v V) (inserted bool) {
	if n, ok := c.items[k]; ok {
		n.val = v
		c.moveToFront(n)
		return false
	}
	if len(c.items) >= c.max && c.tail != nil {
		c.remove(c.tail)
	}
	n := &lruNode[K, V]{key: k, val: v}
	c.items[k] = n
	c.pushFront(n)
	return true
}

// delete removes k if present.
func (c *lru[K, V]) delete(k K) {
	if n, ok := c.items[k]; ok {
		c.remove(n)
	}
}

// clear removes all entries.
func (c *lru[K, V]) clear() {
	clear(c.items)
	c.head, c.tail = nil, nil
}

func (c *lru[K, V]) len() int { return len(c.items) }

// each calls fn for every entry from most to least recently used until fn
// returns false.
func (c *lru[K, V]) each(fn func(K, V) bool) {
	for n := c.head; n != nil; n = n.next {
		if !fn(n.key, n.val) {
			return
		}
	}
}

func (c *lru[K, V]) pushFront(n *lruNode[K, V]) {
	n.prev, n.next = nil, c.head
	if c.head != nil {
		c.head.prev = n
	}
	c.head = n
	if c.tail == nil {
		c.tail = n
	}
}

func (c *lru[K, V]) unlink(n *lruNode[K, V]) {
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		c.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		c.tail = n.prev
	}
	n.prev, n.next = nil, nil
}

func (c *lru[K, V]) moveToFront(n *lruNode[K, V]) {
	if c.head == n {
		return
	}
	c.unlink(n)
	c.pushFront(n)
}

func (c *lru[K, V]) remove(n *lruNode[K, V]) {
	c.unlink(n)
	delete(c.items, n.key)
}
