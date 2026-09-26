package upstream

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// cacheReply is a cacheable answer of qtype for name.
func cacheReply(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.Response = true
	hdr := dns.RR_Header{Name: dns.Fqdn(name), Rrtype: qtype, Class: dns.ClassINET, Ttl: 60}
	switch qtype {
	case dns.TypeA:
		m.Answer = []dns.RR{&dns.A{Hdr: hdr, A: net.ParseIP("192.0.2.1").To4()}}
	default:
		m.Answer = []dns.RR{&dns.RFC3597{Hdr: hdr, Rdata: "00"}}
	}
	return m
}

// The cache counts insertions, evictions for the capacity, expired entries
// and the entries by record type.
func TestCacheCounters(t *testing.T) {
	var c respCache
	c.init()
	now := time.Now()
	p := cachePolicy{capacity: 3, staleWindow: time.Minute}
	put := func(name string, qtype uint16) {
		c.store(cacheKey{name: name, qtype: qtype, qclass: dns.ClassINET}, cacheReply(name, qtype), now, p, entryMeta{})
	}
	put("a.example", dns.TypeA)
	put("b.example", dns.TypeA)
	put("c.example", dns.TypeAAAA)
	put("c.example", dns.TypeAAAA) // a replacement: an insertion, no eviction
	put("d.example", dns.TypeTXT)  // evicts a.example
	if c.insertions.Load() != 5 || c.evictions.Load() != 1 || c.expired.Load() != 0 {
		t.Fatalf("insertions %d evictions %d expired %d", c.insertions.Load(), c.evictions.Load(), c.expired.Load())
	}
	if got := fmt.Sprint(c.typeStats()); got != "[{A 1} {AAAA 1} {TXT 1}]" {
		t.Fatalf("types %s", got)
	}
	// Past the TTL and the serve-stale window: removed as expired.
	if _, ok := c.get(cacheKey{name: "b.example", qtype: dns.TypeA, qclass: dns.ClassINET}, now.Add(2*time.Minute+time.Second), time.Minute); ok {
		t.Fatal("expired entry served")
	}
	if c.expired.Load() != 1 || fmt.Sprint(c.typeStats()) != "[{AAAA 1} {TXT 1}]" {
		t.Fatalf("expired %d, types %v", c.expired.Load(), c.typeStats())
	}
	c.flush()
	if st := c.typeStats(); len(st) != 0 || st == nil {
		t.Fatalf("types after a flush %v", st)
	}
}

// At most 64 record types are counted by name (the others as OTHER); the
// statistics list the 16 largest and OTHER for the rest.
func TestCacheTypeStatsBounded(t *testing.T) {
	var c respCache
	c.init()
	now := time.Now()
	p := cachePolicy{capacity: 10000, staleWindow: time.Minute}
	for i := range 80 {
		qtype := uint16(1000 + i)
		for j := range 1 + i%5 {
			name := fmt.Sprintf("n%d-%d.example", i, j)
			c.store(cacheKey{name: name, qtype: qtype, qclass: dns.ClassINET}, cacheReply(name, qtype), now, p, entryMeta{})
		}
	}
	c.mu.Lock()
	named, other := len(c.types), c.otherTypes
	c.mu.Unlock()
	if named != maxCacheTypes || other == 0 {
		t.Fatalf("%d named types, %d other entries", named, other)
	}
	st := c.typeStats()
	total := 0
	for _, s := range st {
		total += s.Entries
	}
	if len(st) != shownCacheTypes+1 || st[len(st)-1].Type != "OTHER" || total != c.len() || st[0].Entries != 5 {
		t.Fatalf("stats %v (total %d of %d)", st, total, c.len())
	}
	// Removing entries counted as OTHER keeps the counts exact.
	c.trim(0)
	c.mu.Lock()
	named, other = len(c.types), c.otherTypes
	c.mu.Unlock()
	if named != 0 || other != 0 || c.evictions.Load() != int64(total) {
		t.Fatalf("after eviction: %d named, %d other, %d evictions", named, other, c.evictions.Load())
	}
}
