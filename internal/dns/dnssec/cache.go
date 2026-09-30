package dnssec

import (
	"container/list"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Bounds of the key and failure caches (docs/ARCHITECTURE.md 7.6).
const (
	maxKeyEntries   = 16384
	maxKeyBytes     = 8 << 20  // packed wire size of the kept DNSKEY RRsets plus the names
	maxKeyRRsetSize = 16 << 10 // a larger DNSKEY RRset is not kept (validated on each use)
	maxFailEntries  = 4096
	maxKeyLifetime  = 24 * time.Hour
	cryptoFailTTL   = 30 * time.Second
	lookupFailTTL   = 5 * time.Second
)

// cacheKey names a zone of a route.
type cacheKey struct {
	route string
	zone  string // canonical
}

// zoneState is the validated key state of a zone for a route: secure with
// the keys that may sign its data, or insecure (an insecure cut) with the
// reason.
type zoneState struct {
	zone   string // canonical: the zone (secure) or the insecure cut
	secure bool
	keys   []key
	reason string // insecure
	ede    int    // insecure
	// reportZone is the zone an insecure verdict names ("" = zone): the
	// zone of the proof for NSEC3 parameters above the limit.
	reportZone string
}

type keyEntry struct {
	k         cacheKey
	st        *zoneState
	expires   time.Time
	suspended bool // validated while time checks were suspended
	bytes     int
}

// keyCache holds validated zone states per (route, zone): an LRU of at
// most maxKeyEntries entries and maxKeyBytes bytes.
type keyCache struct {
	mu    sync.Mutex
	m     map[cacheKey]*list.Element // values: *keyEntry
	lru   list.List                  // front = most recently used
	bytes int
}

func (c *keyCache) get(k cacheKey, now time.Time) *zoneState {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.m[k]
	if el == nil {
		return nil
	}
	e := el.Value.(*keyEntry)
	if !now.Before(e.expires) {
		c.removeLocked(el)
		return nil
	}
	c.lru.MoveToFront(el)
	return e.st
}

// put stores a state for lifetime (nothing for a lifetime of zero or a
// secure state whose DNSKEY RRset is above maxKeyRRsetSize).
func (c *keyCache) put(k cacheKey, st *zoneState, lifetime time.Duration, now time.Time, suspended bool) {
	if lifetime <= 0 {
		return
	}
	size := len(k.zone) + 64 // the entry, its name and the list element
	for _, key := range st.keys {
		size += dns.Len(key.k)
	}
	if st.secure && size > maxKeyRRsetSize {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[cacheKey]*list.Element{}
	}
	if el := c.m[k]; el != nil {
		c.removeLocked(el)
	}
	e := &keyEntry{k: k, st: st, expires: now.Add(lifetime), suspended: suspended, bytes: size}
	c.m[k] = c.lru.PushFront(e)
	c.bytes += size
	for c.lru.Len() > maxKeyEntries || c.bytes > maxKeyBytes {
		c.removeLocked(c.lru.Back())
	}
}

func (c *keyCache) removeLocked(el *list.Element) {
	e := c.lru.Remove(el).(*keyEntry)
	delete(c.m, e.k)
	c.bytes -= e.bytes
}

func (c *keyCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.m)
	c.lru.Init()
	c.bytes = 0
}

// dropSuspended removes the states validated while time checks were
// suspended.
func (c *keyCache) dropSuspended() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for el := c.lru.Front(); el != nil; {
		next := el.Next()
		if el.Value.(*keyEntry).suspended {
			c.removeLocked(el)
		}
		el = next
	}
}

func (c *keyCache) stats() (entries, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len(), c.bytes
}

type failEntry struct {
	k       cacheKey
	f       *failure
	expires time.Time
}

// failCache holds chain failures per (route, zone) for 30 s (cryptographic)
// or 5 s (lookup failures, exhausted limits): an LRU of maxFailEntries.
type failCache struct {
	mu  sync.Mutex
	m   map[cacheKey]*list.Element // values: *failEntry
	lru list.List
}

func (c *failCache) get(k cacheKey, now time.Time) *failure {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.m[k]
	if el == nil {
		return nil
	}
	e := el.Value.(*failEntry)
	if !now.Before(e.expires) {
		c.lru.Remove(el)
		delete(c.m, k)
		return nil
	}
	c.lru.MoveToFront(el)
	return e.f
}

func (c *failCache) put(k cacheKey, f *failure, now time.Time) {
	ttl := cryptoFailTTL
	if f.lookup {
		ttl = lookupFailTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[cacheKey]*list.Element{}
	}
	if el := c.m[k]; el != nil {
		c.lru.Remove(el)
	}
	c.m[k] = c.lru.PushFront(&failEntry{k: k, f: f, expires: now.Add(ttl)})
	for c.lru.Len() > maxFailEntries {
		old := c.lru.Remove(c.lru.Back()).(*failEntry)
		delete(c.m, old.k)
	}
}

func (c *failCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.m)
	c.lru.Init()
}

func (c *failCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

// lifetime returns how long a validated state may be kept: the smallest of
// the TTLs (RRSIG original TTLs included), the time until the earliest
// RRSIG expiration (time checks active), dns.cacheMaxTtl (when set) and
// 24 h; dns.cacheMinTtl never raises it.
func lifetime(ttl uint32, expires time.Time, maxTTL uint32, now time.Time, timeChecks bool) time.Duration {
	life := min(time.Duration(ttl)*time.Second, maxKeyLifetime)
	if maxTTL > 0 {
		life = min(life, time.Duration(maxTTL)*time.Second)
	}
	if timeChecks && !expires.IsZero() {
		life = min(life, expires.Sub(now))
	}
	return life
}
