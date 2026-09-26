package upstream

import (
	"container/list"
	"context"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/sync/semaphore"
	"golang.org/x/time/rate"
)

// Bounds of the fastest-address probes (upstream mode fastest_addr).
const (
	probeMaxTargets  = 8                      // addresses probed per answer
	probeBudget      = 300 * time.Millisecond // all probes of one answer
	probeCacheTTL    = 10 * time.Minute       // a probe result is reused this long
	probeCacheSize   = 4096                   // addresses with a cached result (LRU)
	probeMaxDials    = 32                     // probe dials at a time (all answers)
	probeRate        = 100                    // new probe targets per second (token bucket)
	probeBurst       = 100
	probePrimaryPort = 443
	probeOtherPort   = 80 // only after the connection to 443 failed
)

// prober orders the addresses of an answer by the time a TCP connection to
// them takes (443, else 80; connect and close, no data, no TLS). Only
// public unicast addresses that are not this machine are probed; results
// are cached per address. When an answer's probes cannot all get a dial
// slot and a token of the rate limit, none are started for it and it is
// ordered by cached results only (fail open, no waiting).
type prober struct {
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
	filter func(ctx context.Context, addrs []netip.Addr) ([]netip.Addr, error)
	slots  *semaphore.Weighted
	tokens *rate.Limiter

	mu    sync.Mutex
	order list.List // *probeResult, most recently used first
	byIP  map[netip.Addr]*list.Element
}

// probeResult is the cached outcome of probing one address.
type probeResult struct {
	ip      netip.Addr
	ok      bool
	rtt     time.Duration
	expires time.Time
}

func newProber(dial func(ctx context.Context, network, address string) (net.Conn, error),
	filter func(ctx context.Context, addrs []netip.Addr) ([]netip.Addr, error)) *prober {
	return &prober{
		dial:   dial,
		filter: filter,
		slots:  semaphore.NewWeighted(probeMaxDials),
		tokens: rate.NewLimiter(probeRate, probeBurst),
		byIP:   map[netip.Addr]*list.Element{},
	}
}

// reorder moves the answer's fastest address of type qtype (A or AAAA) to
// the front of its records; the other records keep their order. Only
// NOERROR answers with at least two addresses of qtype are considered;
// nothing happens when no connection succeeds.
func (p *prober) reorder(ctx context.Context, m *dns.Msg, qtype uint16) {
	if m.Rcode != dns.RcodeSuccess || (qtype != dns.TypeA && qtype != dns.TypeAAAA) {
		return
	}
	var idx []int
	var addrs []netip.Addr
	for i, rr := range m.Answer {
		if ip, ok := rrAddr(rr); ok && rr.Header().Rrtype == qtype {
			idx = append(idx, i)
			addrs = append(addrs, ip)
		}
	}
	if len(idx) < 2 {
		return
	}
	targets, err := p.filter(ctx, addrs)
	if err != nil {
		return
	}
	targets = uniqueAddrs(targets, probeMaxTargets)
	now := time.Now()
	results := map[netip.Addr]probeResult{}
	var fresh []netip.Addr
	for _, ip := range targets {
		if r, ok := p.cached(ip, now); ok {
			results[ip] = r
		} else {
			fresh = append(fresh, ip)
		}
	}
	if len(fresh) > 0 && p.acquire(len(fresh), now) {
		for ip, r := range p.probe(ctx, fresh) {
			results[ip] = r
		}
	}
	best, bestRTT := -1, time.Duration(0)
	for j, ip := range addrs {
		if r, ok := results[ip]; ok && r.ok && (best < 0 || r.rtt < bestRTT) {
			best, bestRTT = j, r.rtt
		}
	}
	if best <= 0 {
		return // nothing answered, or the first address is the fastest
	}
	moved := m.Answer[idx[best]]
	for j := best; j > 0; j-- {
		m.Answer[idx[j]] = m.Answer[idx[j-1]]
	}
	m.Answer[idx[0]] = moved
}

// acquire takes n dial slots and n tokens without waiting; false if either
// is not available (then nothing is held).
func (p *prober) acquire(n int, now time.Time) bool {
	if !p.slots.TryAcquire(int64(n)) {
		return false
	}
	if !p.tokens.AllowN(now, n) {
		p.slots.Release(int64(n))
		return false
	}
	return true
}

// probe connects to every address at once within probeBudget and caches
// the results; each goroutine releases its slot when done.
func (p *prober) probe(ctx context.Context, ips []netip.Addr) map[netip.Addr]probeResult {
	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()
	type outcome struct {
		ip  netip.Addr
		rtt time.Duration
		ok  bool
	}
	ch := make(chan outcome, len(ips))
	for _, ip := range ips {
		go func() {
			defer p.slots.Release(1)
			rtt, ok := p.connect(ctx, ip, probePrimaryPort)
			if !ok && ctx.Err() == nil {
				rtt, ok = p.connect(ctx, ip, probeOtherPort)
			}
			ch <- outcome{ip, rtt, ok}
		}()
	}
	out := make(map[netip.Addr]probeResult, len(ips))
	expires := time.Now().Add(probeCacheTTL)
	for range ips {
		o := <-ch
		r := probeResult{ip: o.ip, ok: o.ok, rtt: o.rtt, expires: expires}
		out[o.ip] = r
		p.store(r)
	}
	return out
}

// connect opens and closes one TCP connection; it reports the time the
// connection took.
func (p *prober) connect(ctx context.Context, ip netip.Addr, port uint16) (time.Duration, bool) {
	start := time.Now()
	c, err := p.dial(ctx, "tcp", netip.AddrPortFrom(ip, port).String())
	if err != nil {
		return 0, false
	}
	rtt := time.Since(start)
	_ = c.Close()
	return rtt, true
}

func (p *prober) cached(ip netip.Addr, now time.Time) (probeResult, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	el, ok := p.byIP[ip]
	if !ok {
		return probeResult{}, false
	}
	r := el.Value.(*probeResult)
	if !now.Before(r.expires) {
		p.order.Remove(el)
		delete(p.byIP, ip)
		return probeResult{}, false
	}
	p.order.MoveToFront(el)
	return *r, true
}

func (p *prober) store(r probeResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el, ok := p.byIP[r.ip]; ok {
		*el.Value.(*probeResult) = r
		p.order.MoveToFront(el)
		return
	}
	if p.order.Len() >= probeCacheSize {
		oldest := p.order.Back()
		delete(p.byIP, oldest.Value.(*probeResult).ip)
		p.order.Remove(oldest)
	}
	p.byIP[r.ip] = p.order.PushFront(&r)
}

// rrAddr returns the address of an A or AAAA record.
func rrAddr(rr dns.RR) (netip.Addr, bool) {
	switch v := rr.(type) {
	case *dns.A:
		return netip.AddrFromSlice(v.A.To4())
	case *dns.AAAA:
		return netip.AddrFromSlice(v.AAAA.To16())
	}
	return netip.Addr{}, false
}

// uniqueAddrs returns the first n distinct addresses of in.
func uniqueAddrs(in []netip.Addr, n int) []netip.Addr {
	out := make([]netip.Addr, 0, min(len(in), n))
	for _, ip := range in {
		if len(out) == n {
			break
		}
		dup := false
		for _, o := range out {
			if o == ip {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, ip)
		}
	}
	return out
}
