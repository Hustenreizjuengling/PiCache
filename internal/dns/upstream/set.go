package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const (
	maxUpstreamsPerSet = 16
	unhealthyAfter     = 3   // consecutive failures
	ewmaAlpha          = 0.2 // weight of a new RTT sample
	unmeasuredRTTMs    = 20  // optimistic estimate so new upstreams get tried
	maxFailPenalty     = 10
)

// upstream is one configured resolver.
type upstream struct {
	name string // the configured string (stats key)
	host string // UpstreamSpec.Host: names the upstream in block reasons (never a DoH path)
	t    transport
	st   *upstreamStats
}

// upstreamSet is an ordered list of upstreams plus the cache namespace for
// answers obtained through it.
type upstreamSet struct {
	id  string
	ups []*upstream
}

// buildSet parses list (at most maxUpstreamsPerSet entries) and creates the
// transports. Invalid entries are skipped and reported. Stats are taken
// from prev (keyed by upstream string); new ones are added to it.
func (r *Resolver) buildSet(kind string, list []string, boot *bootstrap, prev map[string]*upstreamStats) (*upstreamSet, []error) {
	s := &upstreamSet{}
	var errs []error
	for _, raw := range list {
		spec, err := settings.ParseUpstream(raw)
		if err != nil {
			errs = append(errs, fmt.Errorf("%q: %w", raw, err))
			continue
		}
		if len(s.ups) == maxUpstreamsPerSet {
			errs = append(errs, fmt.Errorf("%q: more than %d upstreams", raw, maxUpstreamsPerSet))
			continue
		}
		st := prev[spec.Raw]
		if st == nil {
			st = &upstreamStats{}
			if prev != nil {
				prev[spec.Raw] = st // shared with the other sets of this generation
			}
		}
		s.ups = append(s.ups, &upstream{name: spec.Raw, host: spec.Host, t: r.newTransport(spec, boot), st: st})
	}
	s.id = kind + "\x00" + joinKey(s.names())
	return s, errs
}

func (r *Resolver) newTransport(spec settings.UpstreamSpec, boot *bootstrap) transport {
	if r.opts.transport != nil {
		if t := r.opts.transport(spec); t != nil {
			return t
		}
	}
	switch spec.Proto {
	case "tls":
		return newDoT(spec, boot, r.opts.rootCAs)
	case "https":
		return newDoH(spec, boot, r.opts.rootCAs)
	}
	t := &plainTransport{addr: spec.Addr(), tcpOnly: spec.Proto == "tcp"}
	if !spec.IsIPLit {
		t.host, t.port, t.boot, t.filter = spec.Host, uint16(spec.Port), boot, r.opts.publicFilter
	}
	return t
}

func (s *upstreamSet) names() []string {
	out := make([]string, len(s.ups))
	for i, u := range s.ups {
		out[i] = u.name
	}
	return out
}

func (s *upstreamSet) close() {
	for _, u := range s.ups {
		u.t.close()
	}
}

// upstreamStats is shared by all sets that contain the same upstream.
type upstreamStats struct {
	queries atomic.Int64
	errors  atomic.Int64

	mu        sync.Mutex
	measured  bool    // ewmaMs holds at least one sample
	ewmaMs    float64 // smoothed RTT
	fails     int     // consecutive failures
	lastErr   string
	lastErrAt time.Time
}

// success records an answer; it reports whether the upstream recovered.
func (s *upstreamStats) success(rtt time.Duration) (recovered bool) {
	ms := float64(rtt) / float64(time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.measured {
		s.ewmaMs, s.measured = ms, true
	} else {
		s.ewmaMs += ewmaAlpha * (ms - s.ewmaMs)
	}
	recovered = s.fails >= unhealthyAfter
	s.fails = 0
	return recovered
}

// failure records an error; it reports whether the upstream just became
// unhealthy.
func (s *upstreamStats) failure(err error, now time.Time) (becameUnhealthy bool) {
	s.errors.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fails++
	s.lastErr = truncate(err.Error(), 256)
	s.lastErrAt = now
	return s.fails == unhealthyAfter
}

// score is the expected cost of asking this upstream (lower is better):
// the EWMA RTT multiplied by a penalty for recent consecutive failures.
func (s *upstreamStats) score() float64 {
	s.mu.Lock()
	rtt, fails := s.ewmaMs, s.fails
	if !s.measured {
		rtt = unmeasuredRTTMs
	}
	s.mu.Unlock()
	f := float64(min(fails, maxFailPenalty))
	return max(rtt, 1) * (1 + f) * (1 + f)
}

func (s *upstreamStats) snapshot(name string) UpstreamStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	return UpstreamStat{
		Upstream:    name,
		Queries:     s.queries.Load(),
		Errors:      s.errors.Load(),
		AvgRTTMs:    msFloat(time.Duration(s.ewmaMs * float64(time.Millisecond))),
		LastError:   s.lastErr,
		LastErrorAt: s.lastErrAt,
		Healthy:     s.fails < unhealthyAfter,
	}
}

// exchangeResult is a verified upstream reply.
type exchangeResult struct {
	msg      *dns.Msg
	upstream string
	host     string // UpstreamSpec.Host of the answering upstream
	rtt      time.Duration
	fallback bool       // answered by a fallback upstream
	block    *BlockInfo // the default set's own block (classify)
	ede      *EDE       // the reply's EDE (parseEDE)
}

// Fallback budgets (ARCHITECTURE 7.4): with fallbacks configured the
// default upstreams get at most primaryBudget, then all fallbacks are
// asked at once for at most fallbackBudget, so one fallback round ends
// within the 10 s a client query may take.
const (
	primaryBudget  = 7 * time.Second
	fallbackBudget = 2500 * time.Millisecond
)

// exchangeRoute sends q through rt: the upstreams of rt.set according to
// the mode and, when every attempt ended without any reply (transport
// errors, timeouts) and the route has fallbacks, all fallbacks in
// parallel. A reply of any rcode never leads to the fallbacks. Mode
// fastest_addr asks the default set like parallel and ResolveVia sets like
// load_balance.
func (r *Resolver) exchangeRoute(ctx context.Context, rt route, q *dns.Msg, d settings.DNS) (exchangeResult, error) {
	wire, err := q.Pack()
	if err != nil {
		return exchangeResult{}, err
	}
	mode := d.UpstreamMode
	if mode == "fastest_addr" {
		mode = "load_balance"
		if rt.def {
			mode = "parallel"
		}
	}
	timeout := time.Duration(d.UpstreamTimeoutMs) * time.Millisecond
	if rt.fallback == nil || len(rt.fallback.ups) == 0 {
		return r.exchangeSet(ctx, rt.set, q, wire, mode, timeout)
	}
	res, err := r.exchangeSet(ctx, rt.set, q, wire, mode, min(timeout, primaryBudget))
	if err == nil || ctx.Err() != nil || errors.Is(err, errClosed) {
		return res, err
	}
	fctx, cancel := context.WithTimeout(ctx, fallbackBudget)
	defer cancel()
	fres, ferr := r.exchangeParallel(fctx, rt.fallback.ups, q, wire)
	if ferr != nil {
		return exchangeResult{}, fmt.Errorf("%w; fallback: %w", err, ferr)
	}
	fres.fallback = true
	r.lastFallback.Store(time.Now().UnixNano())
	return fres, nil
}

// exchangeSet sends q (packed: wire) to the upstreams of set according to
// mode, each attempt bounded by the attempt timeout and all of them by
// timeout. SERVFAIL and REFUSED replies make the next upstream be tried;
// they are returned only if nothing better arrives. An error means that no
// upstream replied at all.
func (r *Resolver) exchangeSet(ctx context.Context, set *upstreamSet, q *dns.Msg, wire []byte, mode string, timeout time.Duration) (exchangeResult, error) {
	if len(set.ups) == 0 {
		return exchangeResult{}, errNoUpstreams
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if mode == "parallel" && len(set.ups) > 1 {
		return r.exchangeParallel(ctx, set.ups, q, wire)
	}
	order := set.ups
	if mode != "strict" {
		order = loadBalanceOrder(set.ups)
	}
	var fallback exchangeResult
	var errs []error
	for _, u := range order {
		if ctx.Err() != nil {
			break
		}
		res, err := r.attempt(ctx, u, q, wire)
		switch {
		case err != nil:
			errs = append(errs, err)
		case softFailure(res.msg):
			if fallback.msg == nil {
				fallback = res
			}
		default:
			return res, nil
		}
	}
	if fallback.msg != nil {
		return fallback, nil
	}
	return exchangeResult{}, allFailed(errs)
}

// exchangeParallel asks all upstreams at once; the first good reply wins
// and the other attempts are cancelled.
func (r *Resolver) exchangeParallel(ctx context.Context, ups []*upstream, q *dns.Msg, wire []byte) (exchangeResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outcome struct {
		res exchangeResult
		err error
	}
	ch := make(chan outcome, len(ups))
	started := 0
	for _, u := range ups {
		if !r.goTracked(func() {
			res, err := r.attempt(ctx, u, q, wire)
			ch <- outcome{res, err}
		}) {
			break
		}
		started++
	}
	if started == 0 {
		return exchangeResult{}, errClosed
	}
	var fallback exchangeResult
	var errs []error
	for range started {
		o := <-ch
		switch {
		case o.err != nil:
			errs = append(errs, o.err)
		case softFailure(o.res.msg):
			if fallback.msg == nil {
				fallback = o.res
			}
		default:
			return o.res, nil
		}
	}
	if fallback.msg != nil {
		return fallback, nil
	}
	return exchangeResult{}, allFailed(errs)
}

// attempt performs one exchange with u, verifies the reply and records
// statistics. Attempts cancelled by the caller (parallel losers, shutdown)
// are not counted.
func (r *Resolver) attempt(ctx context.Context, u *upstream, q *dns.Msg, wire []byte) (exchangeResult, error) {
	actx, cancel := context.WithTimeout(ctx, r.opts.attempt)
	defer cancel()
	start := time.Now()
	m, err := u.t.exchange(actx, q, wire)
	rtt := time.Since(start)
	if err == nil {
		if m.Id != q.Id {
			err = errIDMismatch
		} else {
			err = checkReply(q, m)
		}
	}
	if err != nil && errors.Is(ctx.Err(), context.Canceled) {
		return exchangeResult{}, context.Canceled
	}
	u.st.queries.Add(1)
	if err == nil && m.Rcode == dns.RcodeRefused {
		r.recordFailure(u, errRefused)
		return exchangeResult{msg: m, upstream: u.name, host: u.host, rtt: rtt}, nil
	}
	if err != nil {
		r.recordFailure(u, err)
		return exchangeResult{}, fmt.Errorf("%s: %w", u.name, err)
	}
	if u.st.success(rtt) {
		r.log.Info("upstream recovered", slog.String("upstream", u.name))
	}
	return exchangeResult{msg: m, upstream: u.name, host: u.host, rtt: rtt}, nil
}

func (r *Resolver) recordFailure(u *upstream, err error) {
	if u.st.failure(err, time.Now()) {
		r.log.Warn("upstream is failing", slog.String("upstream", u.name), slog.Any("err", err))
	}
}

// softFailure reports replies after which another upstream is worth asking.
func softFailure(m *dns.Msg) bool {
	return m.Rcode == dns.RcodeServerFailure || m.Rcode == dns.RcodeRefused
}

func allFailed(errs []error) error {
	if len(errs) == 0 {
		return fmt.Errorf("upstream: %w", errTimeout)
	}
	return fmt.Errorf("upstream: all upstreams failed: %w", errors.Join(errs...))
}

// loadBalanceOrder picks the first upstream at random, weighted by the
// inverse of its score (EWMA RTT and recent failures); the others follow by
// ascending score for fail-over.
func loadBalanceOrder(ups []*upstream) []*upstream {
	if len(ups) == 1 {
		return ups
	}
	scores := make([]float64, len(ups))
	total := 0.0
	for i, u := range ups {
		scores[i] = u.st.score()
		total += 1 / scores[i]
	}
	pick := len(ups) - 1
	x := rand.Float64() * total
	for i := range ups {
		x -= 1 / scores[i]
		if x < 0 {
			pick = i
			break
		}
	}
	idx := make([]int, 0, len(ups))
	for i := range ups {
		if i != pick {
			idx = append(idx, i)
		}
	}
	slices.SortStableFunc(idx, func(a, b int) int {
		switch {
		case scores[a] < scores[b]:
			return -1
		case scores[a] > scores[b]:
			return 1
		}
		return 0
	})
	order := make([]*upstream, 0, len(ups))
	order = append(order, ups[pick])
	for _, i := range idx {
		order = append(order, ups[i])
	}
	return order
}
