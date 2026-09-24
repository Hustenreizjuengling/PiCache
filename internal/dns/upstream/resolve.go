package upstream

import (
	"context"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Resolve answers req via the configured upstreams (with cache). req is not
// modified; see the package doc for the reply contract.
func (r *Resolver) Resolve(ctx context.Context, req *dns.Msg) (*dns.Msg, Info, error) {
	return r.resolve(ctx, req, r.defaultSet())
}

// ResolveVia answers req via the given upstreams (conditional forwarding,
// router resolver, local PTR resolvers). Cached separately per upstream set.
// The clock guard does not apply: these upstreams serve specific zones.
func (r *Resolver) ResolveVia(ctx context.Context, req *dns.Msg, upstreams []string) (*dns.Msg, Info, error) {
	set, err := r.viaSet(upstreams)
	if err != nil {
		return nil, Info{}, err
	}
	return r.resolve(ctx, req, set)
}

func (r *Resolver) resolve(ctx context.Context, req *dns.Msg, set *upstreamSet) (*dns.Msg, Info, error) {
	if req == nil || len(req.Question) != 1 {
		return nil, Info{}, errBadRequest
	}
	q := req.Question[0]
	d := r.set.Get().DNS
	do := d.DNSSEC
	if opt := req.IsEdns0(); opt != nil && opt.Do() {
		do = true
	}
	k := cacheKey{name: lowerASCII(q.Name), qtype: q.Qtype, qclass: q.Qclass, do: do, set: set.id}
	if pol := policy(d); pol.capacity > 0 {
		now := time.Now()
		if hit, ok := r.cache.get(k, now, pol.staleWindow); ok {
			if m, err := hit.reply(req, now); err == nil {
				if hit.stale {
					r.scheduleRefresh(k, set)
				}
				return m, Info{Cached: true, Stale: hit.stale}, nil
			}
		}
	}
	res, err := r.doFlight(ctx, k, func(fctx context.Context) (exchangeResult, error) {
		return r.fetch(fctx, k, set)
	})
	if err != nil {
		return nil, Info{}, err
	}
	m := res.msg.Copy()
	m.Id = req.Id
	m.Question = []dns.Question{q}
	m.Compress = true
	return m, Info{Upstream: res.upstream, RTT: res.rtt}, nil
}

// fetch queries the upstreams for k and caches the answer. It owns the
// reply until it returns; afterwards the reply is shared read-only.
func (r *Resolver) fetch(ctx context.Context, k cacheKey, set *upstreamSet) (exchangeResult, error) {
	d := r.set.Get().DNS
	q := newQuery(k.name, k.qtype, k.qclass, k.do)
	res, err := r.exchangeSet(ctx, set, q, d)
	if err != nil {
		return res, err
	}
	r.cache.store(k, res.msg, time.Now(), policy(d))
	return res, nil
}

func policy(d settings.DNS) cachePolicy {
	p := cachePolicy{minTTL: d.CacheMinTTL, maxTTL: d.CacheMaxTTL}
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
// others. Callers must copy res.msg before modifying it.
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
			c.res, c.err = fn(r.life)
			finish()
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
	set *upstreamSet
}

// scheduleRefresh queues one background refresh per stale key (dropped
// when the queue is full or the workers are not running).
func (r *Resolver) scheduleRefresh(k cacheKey, set *upstreamSet) {
	if !r.workers.Load() || !r.cache.beginRefresh(k, time.Now()) {
		return
	}
	select {
	case r.refreshQ <- refreshJob{key: k, set: set}:
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
				return r.fetch(fctx, j.key, j.set)
			})
			r.cache.endRefresh(j.key, time.Now())
		}
	}
}
