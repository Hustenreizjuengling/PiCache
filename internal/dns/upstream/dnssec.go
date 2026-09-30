package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/miekg/dns"
	"golang.org/x/time/rate"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// DNSSEC timings and bounds (docs/ARCHITECTURE.md 7.6).
const (
	probeEvery        = 30 * time.Minute // probe interval per upstream
	probeRetry        = 30 * time.Second // while an upstream is unknown or its probes get no reply
	probeQueryTimeout = 2 * time.Second  // each of the two probe queries
	probeTriggerGap   = time.Minute      // re-probe after a root DNSKEY RRset without an anchor match, at most
	maxProbes         = 16               // probes at a time
	resumeAfter       = 60 * time.Second // time checks resume after this long without a reason to suspend them
	clockReadEvery    = time.Second      // the host clock state is read at most this often
	dnssecTick        = time.Second      // the maintenance loop (probes due, time checks)
	chainRate         = 50               // new chain exchanges per second (all routes)
	chainBurst        = 200
	chainClientRate   = 20               // new chain exchanges per second started for one client (its share of chainRate)
	chainClientBurst  = 100              // the burst of that share
	maxChainClients   = 4096             // clients whose share is tracked (the least recently seen evicted)
	chainClientIdle   = time.Minute      // a client's share is forgotten after this long without a chain exchange
	probeFailLimit    = 3                // probes in a row without a reply after which an upstream's root signatures no longer count
	bogusLogEvery     = 10 * time.Minute // one WARN per zone
	bogusLogZones     = 1024
	maxEDEClientText  = 128 // EDE texts of DNSSEC verdicts sent to clients
	maxDNSSECError    = 200 // UpstreamStat.dnssecError
)

// Indeterminate reasons set here (the validator adds "time checks
// suspended").
const (
	reasonStale          = "stale answer"
	reasonAnchorMismatch = "the trust anchors do not match the root zone"
)

// Probe states of an upstream (UpstreamStat.DNSSEC).
const (
	ProbeCapable        = dnssec.StateCapable
	ProbeNoDNSSEC       = dnssec.StateNoDNSSEC
	ProbeAnchorMismatch = dnssec.StateAnchorMismatch
	ProbeUnknown        = dnssec.StateUnknown
)

// Reasons of suspended time checks (GET /dns/upstreams dnssec.timeReason).
const (
	TimeReasonClockGuard     = "clock-guard"
	TimeReasonUnsynced       = "unsynced"
	TimeReasonRootSignatures = "root-signatures"
)

var (
	// errChainRate refuses a chain exchange beyond the global rate or the
	// client's share: a local refusal the validator never caches.
	errChainRate = dnssec.ErrRateLimited
	// ErrDNSSECTestRunning and ErrDNSSECTestTooSoon refuse a DNSSEC test
	// while one runs (409) and within 10 s of the last start (429).
	ErrDNSSECTestRunning = errors.New("a DNSSEC test is running")
	ErrDNSSECTestTooSoon = errors.New("wait 10 seconds between DNSSEC tests")
)

// Verdict is the DNSSEC verdict of a validated answer (Info.DNSSEC).
type Verdict struct {
	// Status is secure, insecure, bogus or indeterminate.
	Status string
	// Zone is the signer or the insecure cut: lower-case, without the
	// trailing dot ("." for the root; "" when unknown).
	Zone string
	// Reason is the failure phrase (bogus), the reason of an insecure or
	// indeterminate verdict, "" for secure.
	Reason string
	// EDE is the Extended DNS Error for clients: the code and "<zone>:
	// <failure or reason>" (at most 128 bytes, sanitised); nil if none.
	EDE *EDE
}

// Secure reports a secure verdict (false for nil).
func (v *Verdict) Secure() bool { return v != nil && v.Status == dnssec.Secure }

// statusOf returns the status of v ("" for nil).
func statusOf(v *Verdict) string {
	if v == nil {
		return ""
	}
	return v.Status
}

// verdictOf converts a validator result.
func verdictOf(res dnssec.Result) *Verdict {
	v := &Verdict{Status: res.Status, Zone: res.Zone, Reason: res.Reason}
	if res.EDE != dnssec.EDENone {
		text := res.Reason
		if res.Zone != "" {
			text = res.Zone + ": " + res.Reason
		}
		v.EDE = &EDE{Code: uint16(res.EDE), Text: cutText(SanitizeEDEText(text), maxEDEClientText)}
	}
	return v
}

// cutText cuts s to at most n bytes at a rune boundary.
func cutText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// probeState is the DNSSEC capability of one upstream (upstreamStats.mu).
type probeState struct {
	state     string    // dnssec.State*; "" = never answered (unknown)
	err       string    // why the state is not capable
	checkedAt time.Time // the last probe that answered
	lastTry   time.Time // the last probe started
	due       bool      // probe at the next tick (start, rebuild, switch to validate, a trigger)
	running   bool
	failures  int       // probes in a row that got no reply (retried every probeRetry)
	triggered time.Time // the last re-probe after an anchor mismatch in a query
	// rootInc and rootExp are the validity period of the anchored root
	// RRSIG that verified in the last answered probe of a capable upstream
	// (zero otherwise); the time checks judge it at the current time.
	rootInc, rootExp time.Time
}

func (p *probeState) stateOrUnknown() string {
	if p.state == "" {
		return dnssec.StateUnknown
	}
	return p.state
}

// validation is the DNSSEC state of a Resolver.
type validation struct {
	r       *Resolver
	v       *dnssec.Validator
	limiter *rate.Limiter        // new chain exchanges
	shares  *netutil.RateLimiter // each client's share of them (keyed like the DNS rate limit)
	probes  chan struct{}        // probes at a time

	clockMu     sync.Mutex
	clockRead   func() (synced, readable bool)
	clockAt     time.Time // when the host clock state was last read
	clockUnsync bool      // readable and not synchronised at clockAt
	clockSynced bool      // readable and synchronised at clockAt
	suspended   bool
	reason      string
	lastBad     time.Time // the last time a reason to suspend held

	logMu  sync.Mutex
	logged map[string]time.Time // zone → last bogus WARN

	testMu    sync.Mutex
	testLast  time.Time
	testing   atomic.Bool
	kickProbe chan struct{}
}

func newValidation(r *Resolver) *validation {
	val := &validation{r: r, limiter: rate.NewLimiter(chainRate, chainBurst), probes: make(chan struct{}, maxProbes),
		shares: netutil.NewRateLimiterMax(chainClientRate, chainClientBurst, maxChainClients),
		logged: map[string]time.Time{}, kickProbe: make(chan struct{}, 1)}
	val.v = dnssec.New(dnssec.Config{Base: r.life, Spawn: r.goTracked, Anchors: r.opts.anchors, NewRootKey: func() {
		r.log.Warn("a new DNSSEC root key is published: update PiCache before it is used")
	}})
	val.keyShares(r.set.Get().DNS)
	return val
}

// keyShares keys the clients' shares of the chain exchanges like the DNS
// rate limit (dns.rateLimitIpv4Prefix and rateLimitIpv6Prefix).
func (val *validation) keyShares(d settings.DNS) {
	val.shares.Reconfigure(chainClientRate, chainClientBurst, nil, d.RateLimitIPv4Prefix, d.RateLimitIPv6Prefix)
}

// validating reports DNSSEC mode validate.
func (r *Resolver) validating() bool { return r.set.Get().DNS.Validating() }

// SetClockReader sets how the host clock's state is read (app: the NTP
// server's adjtimex reader): synced, and whether the state could be read
// at all (EPERM in a container, systems other than Linux: false, which
// suspends nothing).
func (r *Resolver) SetClockReader(read func() (synced, readable bool)) {
	r.val.clockMu.Lock()
	defer r.val.clockMu.Unlock()
	r.val.clockRead = read
	r.val.clockAt = time.Time{}
}

// TimeChecks reports whether RRSIG validity periods are checked, and why
// not (TimeReason*).
func (r *Resolver) TimeChecks() (active bool, reason string) { return r.val.timeChecks(time.Now()) }

// timeChecks evaluates the time checks at now: suspended while the clock
// guard is active, the host clock is readable and not synchronised, or
// (while the host clock is not known to be synchronised) the probes agree
// that the root signatures are outside their period now; resumed when none
// of these held for resumeAfter, which empties the response, LookupIP, key
// and failure caches.
func (val *validation) timeChecks(now time.Time) (bool, string) {
	r := val.r
	reason := ""
	unsynced, synced := val.hostClock(now)
	switch {
	case r.clockBehind() && !r.opts.buildDate.IsZero():
		reason = TimeReasonClockGuard
	case unsynced:
		reason = TimeReasonUnsynced
	case !synced && val.rootSignaturesBad(now):
		reason = TimeReasonRootSignatures
	}
	val.clockMu.Lock()
	if reason != "" {
		val.lastBad = now
		if !val.suspended {
			r.log.Warn("DNSSEC time checks are suspended", slog.String("reason", reason))
		}
		val.suspended, val.reason = true, reason
		val.clockMu.Unlock()
		return false, reason
	}
	if !val.suspended {
		val.clockMu.Unlock()
		return true, ""
	}
	if now.Sub(val.lastBad) < r.opts.resumeAfter {
		reason = val.reason
		val.clockMu.Unlock()
		return false, reason
	}
	val.suspended, val.reason = false, ""
	val.clockMu.Unlock()
	r.log.Info("DNSSEC time checks resumed")
	r.flushResponses()
	val.v.DropSuspended()
	val.v.Flush()
	return true, ""
}

// hostClock reads the host clock state (at most once a second): unsynced
// when it is readable and not synchronised, synced when it is readable and
// synchronised (neither when it cannot be read).
func (val *validation) hostClock(now time.Time) (unsynced, synced bool) {
	val.clockMu.Lock()
	defer val.clockMu.Unlock()
	if val.clockRead == nil {
		return false, false
	}
	if val.clockAt.IsZero() || now.Sub(val.clockAt) >= clockReadEvery || now.Before(val.clockAt) {
		ok, readable := val.clockRead()
		val.clockAt, val.clockUnsync, val.clockSynced = now, readable && !ok, readable && ok
	}
	return val.clockUnsync, val.clockSynced
}

// rootSignaturesBad reports whether the probes of the upstreams of the
// validated routes agree that the root zone's signatures are outside their
// validity period now: the last answered probe of at least one capable
// upstream found its anchored root RRSIG outside it (judged at now, so a
// corrected clock counts at once), and none found it inside. So old root
// signatures replayed to the probe of one plain upstream never suspend the
// time checks of every route while another upstream shows that the clock
// agrees with the root zone. An upstream whose probes keep failing no
// longer counts.
func (val *validation) rootSignaturesBad(now time.Time) bool {
	bad := false
	for _, u := range val.r.validatedUpstreams(false) {
		u.st.mu.Lock()
		p := u.st.probe
		u.st.mu.Unlock()
		if p.state != dnssec.StateCapable || p.rootExp.IsZero() || p.failures >= probeFailLimit {
			continue
		}
		if dnssec.RootValidAt(p.rootInc, p.rootExp, now) {
			return false
		}
		bad = true
	}
	return bad
}

// degraded returns the indeterminate reason of data from u: its probe
// state is no-dnssec or anchor-mismatch ("" otherwise, also for unknown).
func degraded(u *upstream) string {
	if u == nil {
		return ""
	}
	u.st.mu.Lock()
	state := u.st.probe.state
	u.st.mu.Unlock()
	switch state {
	case dnssec.StateNoDNSSEC:
		return u.display + " returns no DNSSEC data"
	case dnssec.StateAnchorMismatch:
		return reasonAnchorMismatch
	}
	return ""
}

// validateFetch validates a fetched answer of a validating fetch (rcode
// NOERROR or NXDOMAIN; not RRSIG, NSEC or NSEC3 queries): its verdict, or
// the upstream's own block of a chain reply (Info.Block). Its chain
// lookups draw on client's share of the chain exchanges (invalid: none).
func (r *Resolver) validateFetch(ctx context.Context, rt route, k cacheKey, res *exchangeResult, d settings.DNS, client netip.Addr) {
	switch k.qtype {
	case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3:
		return
	}
	if rc := res.msg.Rcode; rc != dns.RcodeSuccess && rc != dns.RcodeNameError {
		return
	}
	now := time.Now()
	active, _ := r.val.timeChecks(now)
	up := res.up
	out := r.val.v.Validate(ctx, &dnssec.Request{
		Route: rt.set.id, Name: k.name, Type: k.qtype, Msg: res.msg, Degraded: degraded(up),
		Lookup: r.chainLookup(chainRoute(rt, res), client), AnchorMismatch: func() { r.val.reprobe(up) },
		Now: now, TimeChecks: active, MaxTTL: d.CacheMaxTTL,
	})
	if b, ok := out.Block.(*BlockInfo); ok && b != nil {
		res.block = b
		return
	}
	if out.Status == "" {
		return
	}
	res.verdict = verdictOf(out)
	res.val = valMeta{lookupFailure: out.Lookup, local: out.Local, expires: out.Expires}
	if out.Status == dnssec.Bogus {
		r.val.logBogus(out, now)
	}
}

// chainRoute is the route of the chain lookups of a fetch through rt: rt,
// or only its fallbacks when they answered the fetch (none of the
// default upstreams replied to it; asking them first again would use up
// the validation's time before the fallbacks are reached). The key and
// failure caches stay those of rt (the same set).
func chainRoute(rt route, res *exchangeResult) route {
	rt.fallbackOnly = res.fallback
	return rt
}

// chainLookup returns the lookup function of the validator for rt: a
// fresh query (DO=1, CD=0, RD=1, a fresh ID, no client subnet) through the
// route that answered, never another; not cached, not filtered or logged,
// counted in the upstream statistics. At most chainClientRate new
// exchanges per second for the client the fetch was started for (a valid
// client: one its DNS rate limit applies to) and chainRate for all of
// them: a lookup beyond the client's share fails at once, one beyond the
// global rate waits for its turn within its context's deadline; both
// refusals are local (errChainRate, never cached). A reply of the default
// or a group set that the upstream classifies as its own block ends the
// validation.
func (r *Resolver) chainLookup(rt route, client netip.Addr) dnssec.LookupFunc {
	return func(ctx context.Context, name string, qtype uint16) (dnssec.Reply, error) {
		if client.IsValid() {
			if ok, _ := r.val.shares.Allow(client); !ok {
				return dnssec.Reply{}, errChainRate
			}
		}
		if r.val.limiter.Wait(ctx) != nil {
			return dnssec.Reply{}, errChainRate
		}
		q := newQuery(name, qtype, dns.ClassINET, true)
		res, err := r.exchangeRoute(ctx, rt, q, r.set.Get().DNS)
		if err != nil {
			return dnssec.Reply{}, chainError(err)
		}
		dedupRRs(res.msg)
		up := res.up
		rep := dnssec.Reply{Msg: res.msg, Degraded: degraded(up), AnchorMismatch: func() { r.val.reprobe(up) }}
		if rt.def {
			blocking, _ := parseEDE(res.msg)
			if b := classify(res.msg, qtype, res.host, blocking); b != nil {
				rep.Block = b
			}
		}
		return rep, nil
	}
}

type clientKey struct{}

// WithClient returns ctx carrying the address of the client a query is
// answered for (the DNS server sets it for clients its rate limit applies
// to): in DNSSEC mode validate the chain lookups of a fetch started for
// that query draw on the client's own share of the chain exchanges, so one
// client cannot use up the global rate (docs/ARCHITECTURE.md 7.6).
func WithClient(ctx context.Context, ip netip.Addr) context.Context {
	return context.WithValue(ctx, clientKey{}, ip)
}

// clientOf returns the client address of WithClient (invalid: none).
func clientOf(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(clientKey{}).(netip.Addr)
	return ip
}

// chainError shortens the error of a chain exchange for the failure
// phrase ("DS lookup failed (<error>)").
func chainError(err error) error {
	switch {
	case errors.Is(err, errTimeout), errors.Is(err, context.DeadlineExceeded):
		return errTimeout
	case errors.Is(err, errNoUpstreams):
		return errNoUpstreams
	}
	return errors.New("no reply")
}

// logBogus logs a bogus verdict at WARN, at most once per zone per 10
// minutes (1024 zones remembered). The reason is the failure phrase
// without names; the zone is the attribute "domain" (hidden while domains
// are hidden).
func (val *validation) logBogus(res dnssec.Result, now time.Time) {
	val.logMu.Lock()
	if last, ok := val.logged[res.Zone]; ok && now.Sub(last) < bogusLogEvery {
		val.logMu.Unlock()
		return
	}
	if len(val.logged) >= bogusLogZones {
		for z, t := range val.logged {
			if now.Sub(t) >= bogusLogEvery {
				delete(val.logged, z)
			}
		}
		if len(val.logged) >= bogusLogZones {
			val.logMu.Unlock()
			return
		}
	}
	val.logged[res.Zone] = now
	val.logMu.Unlock()
	reason := res.Reason
	if strings.HasPrefix(reason, "signer ") {
		reason = "signer is not the zone"
	}
	val.r.log.Warn("DNSSEC validation failed", slog.String("domain", res.Zone), slog.String("reason", reason), slog.Int("ede", res.EDE))
}

// --- probes ---

// validatedUpstreams returns the upstreams of the validated routes (each
// statistics object once): the default set, the fallbacks, the group sets
// and, unless withoutForwarders, the validating forwarders' targets; the
// clock-guard set and the group sets' guard parts only while the clock
// guard is active. Only then are they a route: otherwise no query goes to
// them, and probing them (plain DNS to the bootstrap servers) would only
// let someone on that path sway the time checks and the health check.
func (r *Resolver) validatedUpstreams(withoutForwarders bool) []*upstream {
	var out []*upstream
	seen := map[*upstreamStats]bool{}
	add := func(s *upstreamSet) {
		if s == nil {
			return
		}
		for _, u := range s.ups {
			if !seen[u.st] {
				seen[u.st] = true
				out = append(out, u)
			}
		}
	}
	guard := r.clockBehind()
	if ds := r.def.Load(); ds != nil {
		add(ds.normal)
		if guard {
			add(ds.guard)
		}
		add(ds.fallback)
	}
	if gs := r.groups.Load(); gs != nil {
		for _, s := range gs.sets {
			add(s.normal)
			if guard {
				add(s.guard)
			}
		}
	}
	if fs := r.fwds.Load(); fs != nil && !withoutForwarders {
		for _, s := range fs.sets {
			add(s)
		}
	}
	return out
}

// probeAllSoon marks every upstream of a validated route to be probed at
// the next tick (start, rebuild, switch to validate).
func (val *validation) probeAllSoon() {
	for _, u := range val.r.validatedUpstreams(false) {
		u.st.mu.Lock()
		u.st.probe.due = true
		u.st.mu.Unlock()
	}
	select {
	case val.kickProbe <- struct{}{}:
	default:
	}
}

// reprobe probes u again (at most once a minute) after a query got a
// root DNSKEY RRset without an anchor match from it.
func (val *validation) reprobe(u *upstream) {
	if u == nil {
		return
	}
	now := time.Now()
	u.st.mu.Lock()
	if now.Sub(u.st.probe.triggered) >= probeTriggerGap {
		u.st.probe.triggered, u.st.probe.due = now, true
	}
	u.st.mu.Unlock()
	select {
	case val.kickProbe <- struct{}{}:
	default:
	}
}

// dnssecLoop runs the DNSSEC maintenance while ctx lasts: in validate mode
// the time checks (so that they resume without queries) and the probes
// that are due.
func (r *Resolver) dnssecLoop(ctx context.Context) {
	t := time.NewTicker(r.opts.dnssecTick)
	defer t.Stop()
	r.val.probeAllSoon() // the start
	for {
		if r.validating() {
			now := time.Now()
			r.val.timeChecks(now)
			r.probeDue(ctx, now)
			r.val.shares.Sweep(chainClientIdle)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.val.kickProbe:
		}
	}
}

// probeDue starts the probes that are due: marked, never tried, every
// probeEvery, every probeRetry while unknown or while its probes get no
// reply; at most maxProbes at a time.
func (r *Resolver) probeDue(ctx context.Context, now time.Time) {
	for _, u := range r.validatedUpstreams(false) {
		st := &u.st.probe
		u.st.mu.Lock()
		due := !st.running && (st.due || st.lastTry.IsZero() || now.Sub(st.lastTry) >= r.opts.probeEvery ||
			((st.state == "" || st.failures > 0) && now.Sub(st.lastTry) >= r.opts.probeRetry))
		if due {
			select {
			case r.val.probes <- struct{}{}:
				st.running, st.due, st.lastTry = true, false, now
			default:
				due = false // at most maxProbes at a time; the next tick continues
			}
		}
		u.st.mu.Unlock()
		if !due {
			continue
		}
		if !r.goTracked(func() {
			defer func() { <-r.val.probes }()
			r.probe(ctx, u)
		}) {
			<-r.val.probes
			u.st.mu.Lock()
			st.running = false
			u.st.mu.Unlock()
			return
		}
	}
}

// probe asks u for ". DNSKEY" and ". SOA" (DO=1, CD=0, 2 s each) straight
// through its transport (not counted, never cached) and records its state
// and, for a capable upstream, the validity period of the root signature.
// A probe that gets no reply leaves the state as it was (unknown before
// the first answer) and counts as a failure.
func (r *Resolver) probe(ctx context.Context, u *upstream) {
	defer func() {
		u.st.mu.Lock()
		u.st.probe.running = false
		u.st.mu.Unlock()
	}()
	check, err := r.probeRoot(ctx, u)
	now := time.Now()
	u.st.mu.Lock()
	st := &u.st.probe
	old := st.stateOrUnknown()
	if err != nil {
		st.err = cutText(SanitizeEDEText("no reply to the DNSSEC probe: "+err.Error()), maxDNSSECError)
		st.failures++
		u.st.mu.Unlock()
		return
	}
	st.state, st.err, st.checkedAt, st.failures = check.State, check.Error, now, 0
	st.rootInc, st.rootExp = time.Time{}, time.Time{}
	if check.State == dnssec.StateCapable {
		st.rootInc, st.rootExp = check.Inception, check.Expiration
	}
	u.st.mu.Unlock()
	if check.State == old {
		return
	}
	switch check.State {
	case dnssec.StateCapable:
		r.log.Info("upstream returns DNSSEC data", slog.String("upstream", u.display))
		r.cache.flush() // its answers were indeterminate
	case dnssec.StateNoDNSSEC:
		r.log.Warn("upstream does not return DNSSEC data: its answers are not validated", slog.String("upstream", u.display),
			slog.String("reason", check.Error))
	case dnssec.StateAnchorMismatch:
		r.log.Warn("the DNSSEC trust anchors of this version do not match the root zone: update PiCache",
			slog.String("upstream", u.display))
	}
}

// probeRoot runs the two probe queries through u's transport.
func (r *Resolver) probeRoot(ctx context.Context, u *upstream) (dnssec.RootCheck, error) {
	ask := func(qtype uint16) (*dns.Msg, error) {
		qctx, cancel := context.WithTimeout(ctx, r.opts.probeTimeout)
		defer cancel()
		q := newQuery(".", qtype, dns.ClassINET, true)
		wire, err := q.Pack()
		if err != nil {
			return nil, err
		}
		m, err := u.t.exchange(qctx, q, wire)
		if err == nil && m.Id != q.Id {
			err = errIDMismatch
		}
		if err == nil {
			err = checkReply(q, m)
		}
		if err != nil {
			return nil, ctxErr(qctx, err)
		}
		if m.Rcode != dns.RcodeSuccess {
			return nil, fmt.Errorf("answered %s", dns.RcodeToString[m.Rcode])
		}
		return m, nil
	}
	k, err := ask(dns.TypeDNSKEY)
	if err != nil {
		return dnssec.RootCheck{}, err
	}
	s, err := ask(dns.TypeSOA)
	if err != nil {
		return dnssec.RootCheck{}, err
	}
	return r.val.v.CheckRoot(ctx, k, s, time.Now()), nil
}

// NewRootKey reports whether a new root key is published (a validated root
// DNSKEY RRset held a SEP key that matches no built-in anchor).
func (r *Resolver) NewRootKey() bool { return r.val.v.NewRootKey() }

// DNSSECHealth describes the DNSSEC state for the health check "dnssec".
type DNSSECHealth struct {
	// AnchorMismatch: an upstream of a validated route (group sets and
	// validating forwarders included) is anchor-mismatch.
	AnchorMismatch bool
	// NoDNSSEC are the display names of the no-dnssec upstreams of the
	// default set, the fallbacks and (while the clock guard is active) the
	// clock-guard set (group sets and forwarders are not included:
	// ForwarderStats has those).
	NoDNSSEC []string
}

// DNSSECHealth returns the probe states the health check reports.
func (r *Resolver) DNSSECHealth() DNSSECHealth {
	var h DNSSECHealth
	for _, u := range r.validatedUpstreams(false) {
		u.st.mu.Lock()
		if u.st.probe.state == dnssec.StateAnchorMismatch {
			h.AnchorMismatch = true
		}
		u.st.mu.Unlock()
	}
	seen := map[*upstreamStats]bool{}
	if ds := r.def.Load(); ds != nil {
		for _, s := range []*upstreamSet{ds.normal, ds.guard, ds.fallback} {
			if s == nil || (s == ds.guard && !r.clockBehind()) {
				continue
			}
			for _, u := range s.ups {
				u.st.mu.Lock()
				no := u.st.probe.state == dnssec.StateNoDNSSEC
				u.st.mu.Unlock()
				if no && !seen[u.st] {
					seen[u.st] = true
					h.NoDNSSEC = append(h.NoDNSSEC, u.display)
				}
			}
		}
	}
	return h
}

// --- validating forwarders ---

// fwdSets is the immutable registry of the validating forwarders' target
// sets, keyed by their upstream list.
type fwdSets struct {
	sets  []*upstreamSet
	byKey map[string]*upstreamSet
}

func (f *fwdSets) close() {
	for _, s := range f.sets {
		s.close()
	}
}

// SetValidatingForwarders builds one set per distinct target list of the
// forwarders with validate:true (app calls it whenever the forwarders
// change; rebuild does it again when the bootstrap servers change). The
// sets are pinned (never evicted like the ResolveVia sets), probed, and
// answer ResolveValidating; their statistics survive for lists that stay.
func (r *Resolver) SetValidatingForwarders(lists [][]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fwdCfg = nil
	for _, l := range lists {
		if len(l) > 0 && !slices.ContainsFunc(r.fwdCfg, func(o []string) bool { return slices.Equal(o, l) }) {
			r.fwdCfg = append(r.fwdCfg, slices.Clone(l))
		}
	}
	r.rebuildForwardersLocked(r.def.Load().boot)
	r.val.probeAllSoon()
}

// rebuildForwardersLocked builds the sets of r.fwdCfg (r.mu held).
func (r *Resolver) rebuildForwardersLocked(boot *bootstrap) {
	r.lifeMu.Lock()
	closed := r.closed
	r.lifeMu.Unlock()
	if closed {
		return
	}
	prev := map[string]*upstreamStats{}
	old := r.fwds.Load()
	if old != nil {
		for key, s := range old.byKey {
			for _, u := range s.ups {
				prev[key+"\x00"+u.name] = u.st
			}
		}
	}
	next := &fwdSets{byKey: map[string]*upstreamSet{}}
	for _, list := range r.fwdCfg {
		key := joinKey(list)
		stats := map[string]*upstreamStats{}
		for k, st := range prev {
			if name, ok := strings.CutPrefix(k, key+"\x00"); ok {
				stats[name] = st
			}
		}
		s, errs := r.buildSet("fwd", list, boot, stats)
		for _, err := range errs {
			r.log.Warn("ignoring forwarder upstream", slog.Any("err", err))
		}
		if len(s.ups) == 0 {
			continue
		}
		next.sets = append(next.sets, s)
		next.byKey[key] = s
	}
	r.fwds.Store(next)
	if old != nil {
		old.close()
	}
}

// forwarderSet returns the registered set of a validating forwarder's
// targets, or (not registered yet) the ResolveVia set of the list.
func (r *Resolver) forwarderSet(upstreams []string) (*upstreamSet, error) {
	if fs := r.fwds.Load(); fs != nil {
		if s := fs.byKey[joinKey(upstreams)]; s != nil {
			return s, nil
		}
	}
	return r.viaSet(upstreams)
}

// ForwarderStats returns the statistics of a validating forwarder's
// targets with their DNSSEC probe state (validate mode; empty when the
// list is not registered).
func (r *Resolver) ForwarderStats(upstreams []string) []UpstreamStat {
	if fs := r.fwds.Load(); fs != nil {
		if s := fs.byKey[joinKey(upstreams)]; s != nil {
			return setStats(s, r.validating())
		}
	}
	return []UpstreamStat{}
}

// --- the DNSSEC test (POST /dns/dnssec/test) ---

const (
	dnssecTestTimeout = 10 * time.Second
	dnssecTestGap     = 10 * time.Second
)

// DNSSECTestNames are the fixed names of the DNSSEC test with the status
// each is expected to have (each checked on 2026-09-29 through a
// validating public resolver; a name that stops behaving is replaced in a
// release): example.com (IANA, signed), google.com (unsigned below the
// signed com), dnssec-failed.org (no DNSKEY matches the DS) and
// sigfail.ippacket.stream (a bad signature behind a CNAME).
var DNSSECTestNames = []struct{ Name, Expect string }{
	{"example.com", dnssec.Secure},
	{"google.com", dnssec.Insecure},
	{"dnssec-failed.org", dnssec.Bogus},
	{"sigfail.ippacket.stream", dnssec.Bogus},
}

// DNSSECTest is the result of POST /dns/dnssec/test.
type DNSSECTest struct {
	Mode       string         `json:"mode"`
	TimeChecks string         `json:"timeChecks"` // active | suspended
	TimeReason string         `json:"timeReason,omitempty"`
	Upstreams  []UpstreamStat `json:"upstreams"` // the default set, probed now
	Checks     []DNSSECCheck  `json:"checks"`
	DurationMs int64          `json:"durationMs"`
}

// DNSSECCheck is the result for one test name.
type DNSSECCheck struct {
	Name   string `json:"name"`
	Expect string `json:"expect"` // secure | insecure | bogus
	// Status is secure, insecure, bogus, indeterminate or error (no reply,
	// SERVFAIL to the CD=1 query, NXDOMAIN: the name is gone).
	Status string `json:"status"`
	RCode  string `json:"rcode,omitempty"`
	Reason string `json:"reason,omitempty"`
	EDE    *EDE   `json:"ede,omitempty"`
	// Upstream is the upstream that answered the CD=1 query.
	Upstream string `json:"upstream,omitempty"`
	// UpstreamRefused: the upstream answered the same query with CD=0 with
	// SERVFAIL (its own validator refuses it).
	UpstreamRefused bool   `json:"upstreamRefused"`
	Verdict         string `json:"verdict"` // pass | fail | inconclusive
}

// Verdicts of a DNSSEC test check.
const (
	TestPass         = "pass"
	TestFail         = "fail"
	TestInconclusive = "inconclusive"
	statusError      = "error"
)

// TestDNSSEC runs the DNSSEC test (every mode; in off and passthrough it
// shows whether validate would work): it probes the default set's
// upstreams now, then for each test name asks "A" with DO=1 and CD=1
// through the default route (the response cache neither read nor written)
// and validates the answer locally with the normal chain path, and asks
// again with CD=0 to see whether the upstream's own validator refuses it.
// One test at a time (ErrDNSSECTestRunning), at most one start per 10 s
// (ErrDNSSECTestTooSoon), at most 10 s in total (unfinished checks end as
// error).
func (r *Resolver) TestDNSSEC(ctx context.Context) (DNSSECTest, error) {
	if !r.val.testing.CompareAndSwap(false, true) {
		return DNSSECTest{}, ErrDNSSECTestRunning
	}
	defer r.val.testing.Store(false)
	start := time.Now()
	r.val.testMu.Lock()
	if !r.val.testLast.IsZero() && start.Sub(r.val.testLast) < dnssecTestGap {
		r.val.testMu.Unlock()
		return DNSSECTest{}, ErrDNSSECTestTooSoon
	}
	r.val.testLast = start
	r.val.testMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, dnssecTestTimeout)
	defer cancel()
	d := r.set.Get().DNS
	out := DNSSECTest{Mode: d.DNSSECMode, TimeChecks: "active", Checks: []DNSSECCheck{}}
	rt := r.defaultRoute()
	var wg sync.WaitGroup
	for _, u := range rt.set.ups {
		wg.Go(func() { r.probe(ctx, u) })
	}
	wg.Wait()
	out.Upstreams = setStats(rt.set, true)
	active, reason := r.val.timeChecks(time.Now())
	if !active {
		out.TimeChecks, out.TimeReason = "suspended", reason
	}
	// A check that is still running when the test time is up ends as
	// error; its goroutine only ever sends to the buffered channel, so a
	// late result never touches the returned checks.
	type checkResult struct {
		i int
		c DNSSECCheck
	}
	checks := make([]DNSSECCheck, len(DNSSECTestNames))
	done := make(chan checkResult, len(DNSSECTestNames))
	started := 0
	for i, tn := range DNSSECTestNames {
		checks[i] = DNSSECCheck{Name: tn.Name, Expect: tn.Expect, Status: statusError, Reason: "no reply within the test time"}
		if r.goTracked(func() { done <- checkResult{i, r.testName(ctx, rt, tn.Name, tn.Expect, active)} }) {
			started++
		}
	}
collect:
	for range started {
		select {
		case res := <-done:
			checks[res.i] = res.c
		case <-ctx.Done():
			break collect
		}
	}
	for i := range checks {
		checks[i].Verdict = testVerdict(checks[i])
	}
	out.Checks = checks
	out.DurationMs = time.Since(start).Milliseconds()
	return out, nil
}

// testName runs one check of the DNSSEC test.
func (r *Resolver) testName(ctx context.Context, rt route, name, expect string, timeChecks bool) DNSSECCheck {
	c := DNSSECCheck{Name: name, Expect: expect}
	d := r.set.Get().DNS
	fq := dns.Fqdn(name)
	q := newQuery(fq, dns.TypeA, dns.ClassINET, true)
	q.CheckingDisabled = true
	res, err := r.exchangeRoute(ctx, rt, q, d)
	if err != nil {
		c.Status, c.Reason = statusError, "no reply: "+chainError(err).Error()
		return c
	}
	dedupRRs(res.msg)
	c.Upstream, c.RCode = res.upstream, rcodeName(res.msg.Rcode)
	switch res.msg.Rcode {
	case dns.RcodeSuccess:
	case dns.RcodeNameError:
		c.Status, c.Reason = statusError, "the test name does not exist any more"
		return c
	default:
		c.Status, c.Reason = statusError, "the upstream answered "+c.RCode+" with checking disabled"
		return c
	}
	up := res.up
	out := r.val.v.Validate(ctx, &dnssec.Request{
		Route: rt.set.id, Name: fq, Type: dns.TypeA, Msg: res.msg, Degraded: degraded(up),
		Lookup: r.chainLookup(chainRoute(rt, &res), netip.Addr{}), AnchorMismatch: func() { r.val.reprobe(up) },
		Now: time.Now(), TimeChecks: timeChecks, MaxTTL: d.CacheMaxTTL,
	})
	if out.Status == "" {
		c.Status, c.Reason = statusError, "the upstream blocked a chain lookup"
	} else {
		v := verdictOf(out)
		c.Status, c.Reason, c.EDE = v.Status, v.Reason, v.EDE
	}
	q2 := newQuery(fq, dns.TypeA, dns.ClassINET, true)
	if res2, err := r.exchangeRoute(ctx, rt, q2, d); err == nil && res2.msg.Rcode == dns.RcodeServerFailure {
		c.UpstreamRefused = true
	}
	return c
}

// testVerdict judges a check: pass when the status is the expected one,
// inconclusive for indeterminate and error, fail otherwise.
func testVerdict(c DNSSECCheck) string {
	switch {
	case c.Status == c.Expect:
		return TestPass
	case c.Status == dnssec.Indeterminate || c.Status == statusError:
		return TestInconclusive
	}
	return TestFail
}

func rcodeName(rc int) string {
	if s, ok := dns.RcodeToString[rc]; ok {
		return s
	}
	return fmt.Sprintf("RCODE%d", rc)
}
