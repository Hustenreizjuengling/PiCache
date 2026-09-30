package upstream

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Resolve answers req via the default set (with cache): the configured
// upstreams, the clock-guard set while the clock guard is active, and the
// fallbacks when no default upstream replied. ecs is the client subnet to
// send (invalid = none); it is part of the cache key. The answer is
// classified (Info.Block) when the upstream blocked the name itself. req
// is not modified; see the package doc for the reply contract.
func (r *Resolver) Resolve(ctx context.Context, req *dns.Msg, ecs netip.Prefix) (*dns.Msg, Info, error) {
	return r.resolve(ctx, req, r.defaultRoute(), ecs)
}

// ResolveValidating answers req via the target set of a forwarder that
// validates DNSSEC (validate:true, registered with
// SetValidatingForwarders): like ResolveVia, but in DNSSEC mode validate
// its answers are validated through these upstreams only. In the other
// modes it is ResolveVia.
func (r *Resolver) ResolveValidating(ctx context.Context, req *dns.Msg, upstreams []string) (*dns.Msg, Info, error) {
	if !r.validating() {
		return r.ResolveVia(ctx, req, upstreams)
	}
	set, err := r.forwarderSet(upstreams)
	if err != nil {
		return nil, Info{}, err
	}
	return r.resolve(ctx, req, route{set: set, validate: true}, netip.Prefix{})
}

// ResolveVia answers req via the given upstreams (conditional forwarding,
// router resolver, local PTR resolvers). Cached separately per upstream set.
// The clock guard, the fallbacks, the client subnet, the classification of
// blocked answers and the fastest-address order do not apply: these
// upstreams serve specific zones.
func (r *Resolver) ResolveVia(ctx context.Context, req *dns.Msg, upstreams []string) (*dns.Msg, Info, error) {
	set, err := r.viaSet(upstreams)
	if err != nil {
		return nil, Info{}, err
	}
	return r.resolve(ctx, req, route{set: set}, netip.Prefix{})
}

// route is the path of a fetch: the upstream set, the fallbacks asked when
// none of them replied, whether it is the default set or a group set
// (classification of blocked answers, fastest-address order) and whether
// its answers are validated in DNSSEC mode validate (the default route,
// the group sets, the validating forwarders). The key and failure caches
// of the validator are kept per set (set.id).
type route struct {
	set      *upstreamSet
	fallback *upstreamSet // nil: none (ResolveVia, clock guard, not configured)
	def      bool
	validate bool
	// fallbackOnly asks the fallbacks at once: the chain lookups of a
	// fetch they answered (no upstream of set replied to it, so asking
	// set first again would use up the validation's time).
	fallbackOnly bool
}

func (r *Resolver) resolve(ctx context.Context, req *dns.Msg, rt route, ecs netip.Prefix) (*dns.Msg, Info, error) {
	if req == nil || len(req.Question) != 1 {
		return nil, Info{}, errBadRequest
	}
	q := req.Question[0]
	d := r.set.Get().DNS
	do := d.DNSSEC
	if opt := req.IsEdns0(); opt != nil && opt.Do() {
		do = true
	}
	if ecs.IsValid() {
		ecs = ecs.Masked()
	}
	validate := d.Validating()
	k := cacheKey{name: lowerASCII(q.Name), qtype: q.Qtype, qclass: q.Qclass, do: do, set: rt.set.id, ecs: ecs,
		val: validate && rt.validate}
	if pol := policy(d); pol.capacity > 0 {
		now := time.Now()
		if hit, ok := r.cache.get(k, now, pol.staleWindow); ok {
			if m, err := hit.reply(req, now); err == nil {
				if hit.stale {
					r.scheduleRefresh(k, rt)
				}
				info := Info{Cached: true, Stale: hit.stale, Block: hit.meta.block, EDE: hit.meta.ede, Fallback: hit.meta.fallback,
					DNSSEC: hit.meta.verdict}
				if hit.stale && info.DNSSEC != nil {
					info.DNSSEC = &Verdict{Status: dnssec.Indeterminate, Reason: reasonStale}
				}
				if validate {
					m.AuthenticatedData = info.DNSSEC.Secure()
				}
				return m, info, nil
			}
		}
	}
	client := clientOf(ctx)
	res, err := r.doFlight(ctx, k, func(fctx context.Context) (exchangeResult, error) {
		return r.fetch(fctx, k, rt, client)
	})
	if err != nil {
		return nil, Info{}, err
	}
	m := res.msg.Copy()
	m.Id = req.Id
	m.Question = []dns.Question{q}
	m.Compress = true
	if validate {
		m.AuthenticatedData = res.verdict.Secure()
	}
	return m, Info{Upstream: res.upstream, RTT: res.rtt, Block: res.block, EDE: res.ede, Fallback: res.fallback, DNSSEC: res.verdict}, nil
}

// fetch queries the upstreams of rt for k, removes duplicate records,
// classifies the answer (default and group sets), validates it (a
// validating fetch: k.val), orders its addresses (mode fastest_addr,
// default and group sets, not for a bogus answer) and caches it with its
// verdict. The chain lookups of the validation draw on client's share of
// the chain exchanges (WithClient; zero: none). It owns the reply until
// it returns; afterwards the reply is shared read-only.
func (r *Resolver) fetch(ctx context.Context, k cacheKey, rt route, client chainClient) (exchangeResult, error) {
	d := r.set.Get().DNS
	q := newQuery(k.name, k.qtype, k.qclass, k.do)
	addECS(q, k.ecs)
	res, err := r.exchangeRoute(ctx, rt, q, d)
	if err != nil {
		return res, err
	}
	dedupRRs(res.msg)
	blocking, logged := parseEDE(res.msg)
	res.ede = logged
	if rt.def {
		res.block = classify(res.msg, k.qtype, res.host, blocking)
	}
	if k.val && res.block == nil {
		r.validateFetch(ctx, rt, k, &res, d, client)
	}
	if rt.def && res.block == nil && d.UpstreamMode == "fastest_addr" && statusOf(res.verdict) != dnssec.Bogus {
		r.prober.reorder(ctx, res.msg, k.qtype)
	}
	r.cache.store(k, res.msg, time.Now(), policy(d), entryMeta{block: res.block, ede: res.ede, fallback: res.fallback,
		verdict: res.verdict, val: res.val})
	return res, nil
}

// maxDedupRRs bounds the pairwise duplicate check per section; larger
// sections are passed on unchanged.
const maxDedupRRs = 256

// dedupRRs removes duplicate records (same owner name, class, type and
// RDATA; RFC 2181 5) from every section of m in place, e.g. from resolvers
// such as Docker's embedded DNS that repeat every A/AAAA record. The first
// copy is kept with the lowest TTL of its copies (RFC 2181 5.2).
func dedupRRs(m *dns.Msg) {
	m.Answer = dedupSection(m.Answer)
	m.Ns = dedupSection(m.Ns)
	m.Extra = dedupSection(m.Extra)
}

func dedupSection(rrs []dns.RR) []dns.RR {
	if len(rrs) < 2 || len(rrs) > maxDedupRRs {
		return rrs
	}
	out := rrs[:1]
next:
	for _, rr := range rrs[1:] {
		for _, kept := range out {
			if dns.IsDuplicate(kept, rr) {
				kept.Header().Ttl = min(kept.Header().Ttl, rr.Header().Ttl)
				continue next
			}
		}
		out = append(out, rr)
	}
	return out
}

func policy(d settings.DNS) cachePolicy {
	p := cachePolicy{minTTL: d.CacheMinTTL, maxTTL: d.CacheMaxTTL, blockedTTL: uint32(max(d.UpstreamBlockedTTL, 0))}
	if d.CacheEnabled {
		p.capacity = d.CacheSize
	}
	if d.ServeStale {
		p.staleWindow = time.Duration(d.ServeStaleMaxAgeSec) * time.Second
	}
	return p
}

// flightGroup de-duplicates identical in-flight queries.
type flightGroup struct {
	mu sync.Mutex
	m  map[cacheKey]*call
}

type call struct {
	done chan struct{}
	res  exchangeResult
	err  error
}

// doFlight runs fn once per key at a time; concurrent callers wait for the
// same result. fn runs detached from any caller (bounded by the upstream
// timeout, cancelled on shutdown) so one impatient client cannot fail the
// others; if it panics, the waiters get errInternal. Callers must copy
// res.msg before modifying it.
func (r *Resolver) doFlight(ctx context.Context, k cacheKey, fn func(context.Context) (exchangeResult, error)) (exchangeResult, error) {
	g := &r.flight
	g.mu.Lock()
	c, ok := g.m[k]
	if !ok {
		if len(g.m) >= maxInflight {
			g.mu.Unlock()
			return exchangeResult{}, errBusy
		}
		c = &call{done: make(chan struct{})}
		g.m[k] = c
		g.mu.Unlock()
		finish := func() {
			g.mu.Lock()
			delete(g.m, k)
			g.mu.Unlock()
			close(c.done)
		}
		if !r.goTracked(func() {
			defer finish()
			c.err = errInternal // replaced unless fn panics
			c.res, c.err = fn(r.life)
		}) {
			c.err = errClosed
			finish()
		}
	} else {
		g.mu.Unlock()
	}
	select {
	case <-c.done:
		return c.res, c.err
	case <-ctx.Done():
		return exchangeResult{}, ctx.Err()
	}
}

// refreshJob asks for a background refresh of a stale entry.
type refreshJob struct {
	key cacheKey
	rt  route
}

// scheduleRefresh queues one background refresh per stale key (dropped
// when the queue is full or the workers are not running).
func (r *Resolver) scheduleRefresh(k cacheKey, rt route) {
	if !r.workers.Load() || !r.cache.beginRefresh(k, time.Now()) {
		return
	}
	select {
	case r.refreshQ <- refreshJob{key: k, rt: rt}:
	default:
		r.cache.endRefresh(k, time.Now())
	}
}

func (r *Resolver) refreshLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-r.refreshQ:
			_, _ = r.doFlight(ctx, j.key, func(fctx context.Context) (exchangeResult, error) {
				return r.fetch(fctx, j.key, j.rt, chainClient{})
			})
			r.cache.endRefresh(j.key, time.Now())
		}
	}
}
