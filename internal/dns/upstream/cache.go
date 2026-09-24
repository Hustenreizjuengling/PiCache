package upstream

import (
	"container/list"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

const (
	staleTTL       = 30              // TTL of answers served stale (RFC 8767)
	servfailTTL    = 5               // seconds a SERVFAIL is cached (RFC 2308 7.1 allows ≤ 5 min)
	maxNegativeTTL = 3600            // RFC 2308 5: negative answers ≤ 1 h (we use it as the cap)
	maxEntryBytes  = 16 << 10        // larger responses are answered but not cached
	maxCacheBytes  = 64 << 20        // total wire bytes in the cache
	refreshBackoff = 5 * time.Second // after a stale refresh, before the next one for the key
	hardMaxTTL     = 7 * 86400       // cap for every cached TTL (RFC 8767 4 suggests 7 days)
)

// cacheKey identifies a cached answer. set is the upstream-set namespace.
type cacheKey struct {
	name   string // lower-case qname
	qtype  uint16
	qclass uint16
	do     bool // DO bit sent upstream
	set    string
}

// cacheEntry is an answer in packed wire form: exact memory accounting and
// every hit unpacks its own copy.
type cacheEntry struct {
	key         cacheKey
	wire        []byte
	stored      time.Time
	expires     time.Time
	servfail    bool
	refreshing  bool
	nextRefresh time.Time
}

// cachePolicy is the part of the settings that shapes what is cached.
type cachePolicy struct {
	capacity    int
	minTTL      uint32
	maxTTL      uint32 // 0 = no cap
	staleWindow time.Duration
}

// respCache is an LRU of upstream answers, bounded by entry count and total
// wire bytes.
type respCache struct {
	mu    sync.Mutex
	m     map[cacheKey]*list.Element // values: *cacheEntry
	lru   list.List                  // front = most recently used
	bytes int

	hits, misses, staleHits atomic.Int64
}

func (c *respCache) init() { c.m = map[cacheKey]*list.Element{} }

// cacheHit is a snapshot of an entry taken under the lock.
type cacheHit struct {
	wire   []byte // immutable
	stored time.Time
	stale  bool
}

// get returns the entry for k. Expired entries are returned as stale while
// they are within staleWindow (never SERVFAIL entries); older ones are
// removed.
func (c *respCache) get(k cacheKey, now time.Time, staleWindow time.Duration) (cacheHit, bool) {
	c.mu.Lock()
	el := c.m[k]
	if el == nil {
		c.mu.Unlock()
		c.misses.Add(1)
		return cacheHit{}, false
	}
	e := el.Value.(*cacheEntry)
	stale := !now.Before(e.expires)
	if stale && (e.servfail || now.Sub(e.expires) >= staleWindow) {
		c.removeLocked(el)
		c.mu.Unlock()
		c.misses.Add(1)
		return cacheHit{}, false
	}
	c.lru.MoveToFront(el)
	h := cacheHit{wire: e.wire, stored: e.stored, stale: stale}
	c.mu.Unlock()
	c.hits.Add(1)
	if stale {
		c.staleHits.Add(1)
	}
	return h, true
}

// store caches m under k if it is cacheable. It may rewrite TTLs in m (the
// TTL clamp and the negative TTL on the SOA), so the caller must own m. A
// SERVFAIL never replaces an entry that can still be served (fresh or
// stale): RFC 8767 prefers stale data over failures.
func (c *respCache) store(k cacheKey, m *dns.Msg, now time.Time, p cachePolicy) {
	if p.capacity <= 0 {
		return
	}
	ttl, servfail, ok := prepareForCache(m, k.qtype, p)
	if !ok {
		return
	}
	compress := m.Compress
	m.Compress = true
	wire, err := m.Pack()
	m.Compress = compress
	if err != nil || len(wire) > maxEntryBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el := c.m[k]; el != nil {
		old := el.Value.(*cacheEntry)
		if servfail && !old.servfail && now.Sub(old.expires) < p.staleWindow {
			return
		}
		c.removeLocked(el)
	}
	e := &cacheEntry{key: k, wire: wire, stored: now, expires: now.Add(time.Duration(ttl) * time.Second), servfail: servfail}
	c.m[k] = c.lru.PushFront(e)
	c.bytes += len(wire)
	c.evictLocked(p.capacity)
}

// prepareForCache classifies m, the reply to a query of type qtype, and
// returns its cache lifetime. All TTLs are clamped to [minTTL, maxTTL].
//   - NOERROR with an answer of qtype: the smallest answer TTL;
//   - negative answers with an SOA (RFC 2308): NXDOMAIN and NODATA, also at
//     the end of a CNAME/DNAME chain in the answer section (RFC 2308 5): the
//     negative TTL min(SOA TTL, SOA MINIMUM), clamped and at most 1 h (and
//     at most the smallest answer TTL of the chain); the SOA carries it;
//   - SERVFAIL: 5 s;
//   - anything else (truncated, other rcodes, negative without SOA, TTL 0):
//     not cached.
func prepareForCache(m *dns.Msg, qtype uint16, p cachePolicy) (ttl uint32, servfail, ok bool) {
	if m.Truncated {
		return 0, false, false
	}
	switch m.Rcode {
	case dns.RcodeServerFailure:
		return servfailTTL, true, true
	case dns.RcodeSuccess, dns.RcodeNameError:
	default:
		return 0, false, false
	}
	clampTTLs(m, p)
	ttl = ^uint32(0)
	for _, rr := range m.Answer {
		ttl = min(ttl, rr.Header().Ttl)
	}
	if m.Rcode == dns.RcodeSuccess && answersType(m.Answer, qtype) {
		return ttl, false, ttl > 0
	}
	var soa *dns.SOA
	for _, rr := range m.Ns {
		if s, isSOA := rr.(*dns.SOA); isSOA {
			soa = s
			break
		}
	}
	if soa == nil {
		return 0, false, false
	}
	neg := min(clamp(min(soa.Hdr.Ttl, soa.Minttl), p), maxNegativeTTL)
	for _, rr := range m.Ns {
		rr.Header().Ttl = min(rr.Header().Ttl, neg)
	}
	soa.Hdr.Ttl = neg
	ttl = min(ttl, neg)
	return ttl, false, ttl > 0
}

// answersType reports whether answer holds data of type qtype, i.e. is a
// positive answer rather than only a CNAME/DNAME chain ending in NODATA.
func answersType(answer []dns.RR, qtype uint16) bool {
	for _, rr := range answer {
		if t := rr.Header().Rrtype; t == qtype || qtype == dns.TypeANY && t != dns.TypeRRSIG {
			return true
		}
	}
	return false
}

func clampTTLs(m *dns.Msg, p cachePolicy) {
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			if h := rr.Header(); h.Rrtype != dns.TypeOPT {
				h.Ttl = clamp(h.Ttl, p)
			}
		}
	}
}

// clamp applies dns.cacheMinTtl/cacheMaxTtl and the hard 7-day cap.
func clamp(ttl uint32, p cachePolicy) uint32 {
	ttl = max(ttl, p.minTTL)
	if p.maxTTL > 0 {
		ttl = min(ttl, p.maxTTL)
	}
	return min(ttl, hardMaxTTL)
}

// reply unpacks a private copy of the entry and adapts it to req: the
// request's Id and question (original case), TTLs reduced by the entry's
// age (minimum 0) or set to 30 for stale answers.
func (h cacheHit) reply(req *dns.Msg, now time.Time) (*dns.Msg, error) {
	m := new(dns.Msg)
	if err := m.Unpack(h.wire); err != nil {
		return nil, err
	}
	age := uint32(max(now.Sub(h.stored), 0) / time.Second)
	for _, sec := range [][]dns.RR{m.Answer, m.Ns, m.Extra} {
		for _, rr := range sec {
			hdr := rr.Header()
			switch {
			case hdr.Rrtype == dns.TypeOPT:
			case h.stale:
				hdr.Ttl = staleTTL
			case hdr.Ttl > age:
				hdr.Ttl -= age
			default:
				hdr.Ttl = 0
			}
		}
	}
	m.Id = req.Id
	m.Question = []dns.Question{req.Question[0]}
	m.Compress = true
	return m, nil
}

// beginRefresh marks k as being refreshed; false if a refresh is already
// running or was attempted less than refreshBackoff ago.
func (c *respCache) beginRefresh(k cacheKey, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.m[k]
	if el == nil {
		return false
	}
	e := el.Value.(*cacheEntry)
	if e.refreshing || now.Before(e.nextRefresh) {
		return false
	}
	e.refreshing = true
	return true
}

func (c *respCache) endRefresh(k cacheKey, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el := c.m[k]; el != nil {
		e := el.Value.(*cacheEntry)
		e.refreshing = false
		e.nextRefresh = now.Add(refreshBackoff)
	}
}

func (c *respCache) trim(capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictLocked(capacity)
}

func (c *respCache) evictLocked(capacity int) {
	for c.lru.Len() > 0 && (c.lru.Len() > capacity || c.bytes > maxCacheBytes) {
		c.removeLocked(c.lru.Back())
	}
}

func (c *respCache) removeLocked(el *list.Element) {
	e := c.lru.Remove(el).(*cacheEntry)
	delete(c.m, e.key)
	c.bytes -= len(e.wire)
}

func (c *respCache) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.m)
	c.lru.Init()
	c.bytes = 0
}

func (c *respCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
