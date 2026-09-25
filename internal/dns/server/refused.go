package dnsserver

import (
	"container/list"
	"net/netip"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// maxRefusedSources bounds the table of refused sources.
const maxRefusedSources = 256

// RefusedSource is a source address whose DNS queries the ACL dropped since
// the start.
type RefusedSource struct {
	Address string    `json:"address"`
	Count   int64     `json:"count"`
	Last    time.Time `json:"last"`
}

// refusedTable counts the sources dropped by the ACL: at most
// maxRefusedSources addresses (full IPv6 addresses, not prefixes), the
// least recently refused is evicted first. Memory only.
type refusedTable struct {
	mu    sync.Mutex
	order list.List // *refusedEntry, most recently refused first
	byIP  map[netip.Addr]*list.Element
}

type refusedEntry struct {
	ip    netip.Addr
	count int64
	last  time.Time
}

// add records a dropped query from ip (loopback and invalid addresses are
// ignored).
func (t *refusedTable) add(ip netip.Addr, now time.Time) {
	ip = netutil.Canon(ip)
	if !ip.IsValid() || ip.IsLoopback() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byIP == nil {
		t.byIP = make(map[netip.Addr]*list.Element)
	}
	if el, ok := t.byIP[ip]; ok {
		e := el.Value.(*refusedEntry)
		e.count++
		e.last = now
		t.order.MoveToFront(el)
		return
	}
	if t.order.Len() >= maxRefusedSources {
		oldest := t.order.Back()
		delete(t.byIP, oldest.Value.(*refusedEntry).ip)
		t.order.Remove(oldest)
	}
	t.byIP[ip] = t.order.PushFront(&refusedEntry{ip: ip, count: 1, last: now})
}

// list returns the sources, most recently refused first.
func (t *refusedTable) list() []RefusedSource {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RefusedSource, 0, t.order.Len())
	for el := t.order.Front(); el != nil; el = el.Next() {
		e := el.Value.(*refusedEntry)
		out = append(out, RefusedSource{Address: e.ip.String(), Count: e.count, Last: e.last.UTC()})
	}
	return out
}

// RefusedSources returns the source addresses whose queries the DNS ACL
// dropped since the start (at most 256, most recently refused first;
// loopback is never listed).
func (s *Server) RefusedSources() []RefusedSource { return s.refusedSrc.list() }
