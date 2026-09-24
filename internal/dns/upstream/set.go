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
		s.ups = append(s.ups, &upstream{name: spec.Raw, t: r.newTransport(spec, boot), st: st})
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
	case "tcp":
		return &plainTransport{addr: spec.Addr(), tcpOnly: true}
	case "tls":
		return newDoT(spec, boot, r.opts.rootCAs)
	case "https":
		return newDoH(spec, boot, r.opts.rootCAs)
	default:
		return &plainTransport{addr: spec.Addr()}
	}
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
	rtt      time.Duration
}

// exchangeSet sends q to the upstreams of set according to the mode, each
// attempt bounded by the attempt timeout and all of them by
// dns.upstreamTimeoutMs. SERVFAIL and REFUSED replies make the next upstream
// be tried; they are returned only if nothing better arrives.
func (r *Resolver) exchangeSet(ctx context.Context, set *upstreamSet, q *dns.Msg, d settings.DNS) (exchangeResult, error) {
	if len(set.ups) == 0 {
		return exchangeResult{}, errNoUpstreams
	}
	wire, err := q.Pack()
	if err != nil {
		return exchangeResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.UpstreamTimeoutMs)*time.Millisecond)
	defer cancel()
	if d.UpstreamMode == "parallel" && len(set.ups) > 1 {
		return r.exchangeParallel(ctx, set.ups, q, wire)
	}
	order := set.ups
	if d.UpstreamMode != "strict" {
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
		return exchangeResult{msg: m, upstream: u.name, rtt: rtt}, nil
	}
	if err != nil {
		r.recordFailure(u, err)
		return exchangeResult{}, fmt.Errorf("%s: %w", u.name, err)
	}
	if u.st.success(rtt) {
		r.log.Info("upstream recovered", slog.String("upstream", u.name))
	}
	return exchangeResult{msg: m, upstream: u.name, rtt: rtt}, nil
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
