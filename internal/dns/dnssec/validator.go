package dnssec

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Global bounds of the chain lookups.
const (
	// maxChainFlights bounds the chain steps in progress (all routes).
	maxChainFlights = 1024
	// contextTimeout is the time of a validation context after the answer
	// arrived; a chain step runs detached for at most as long.
	contextTimeout = 4 * time.Second
)

var errClosed = errors.New("validator is closing")

// ErrRateLimited is the error of a chain lookup that a local rate limit
// refused (a LookupFunc returns it, possibly wrapped): the failure is
// local, so the failure cache never keeps it and Result.Local is set.
var ErrRateLimited = errors.New("chain lookup rate limit")

// LookupFunc sends a chain lookup (DS or DNSKEY, DO=1, CD=0) through the
// route of the validated answer. It is called from a detached chain step.
type LookupFunc func(ctx context.Context, name string, qtype uint16) (Reply, error)

// Reply is the answer to a chain lookup.
type Reply struct {
	Msg *dns.Msg
	// Degraded is set when the answering upstream is known not to return
	// usable DNSSEC data (its probe state): the verdict is indeterminate
	// with this reason.
	Degraded string
	// Block is the upstream's own block of the reply (non-nil ends the
	// validation: Result.Block).
	Block any
	// AnchorMismatch is called when the reply is a root DNSKEY RRset
	// without a key matching an anchor (the upstream re-probes it).
	AnchorMismatch func()
}

// Config configures a Validator.
type Config struct {
	// Base is the context the detached chain steps run under (cancelled on
	// shutdown); nil: context.Background().
	Base context.Context
	// Spawn runs fn in a goroutine its owner waits for on shutdown; it
	// returns false (without running fn) while the owner is closing. nil:
	// a plain goroutine.
	Spawn func(fn func()) bool
	// Anchors are the trust anchors of the root (nil: RootAnchors).
	Anchors []*dns.DS
	// NewRootKey is called when a validated root DNSKEY RRset holds a SEP
	// key that matches no anchor (a new root key is published).
	NewRootKey func()
}

// Validator validates DNS answers against the chain of trust from the root
// anchors. It keeps the validated key states and the chain failures per
// route and coalesces concurrent chain steps. Safe for concurrent use.
type Validator struct {
	cfg     Config
	anchors []*dns.DS
	sem     chan struct{} // concurrent signature verifications (GOMAXPROCS)

	keys  keyCache
	fails failCache

	mu      sync.Mutex
	flights map[stepKey]*stepCall

	newRootKey atomic.Bool
	verifies   atomic.Int64 // signature verifications since the start
}

// New returns a Validator.
func New(cfg Config) *Validator {
	if cfg.Base == nil {
		cfg.Base = context.Background()
	}
	if cfg.Spawn == nil {
		cfg.Spawn = func(fn func()) bool { go fn(); return true }
	}
	v := &Validator{cfg: cfg, anchors: cfg.Anchors, sem: make(chan struct{}, max(runtime.GOMAXPROCS(0), 1)),
		flights: map[stepKey]*stepCall{}}
	if v.anchors == nil {
		v.anchors = RootAnchors()
	}
	return v
}

// Flush empties the key and failure caches.
func (v *Validator) Flush() {
	v.keys.flush()
	v.fails.flush()
}

// DropSuspended removes the key states validated while time checks were
// suspended (called when they resume; the failure cache is emptied too).
func (v *Validator) DropSuspended() {
	v.keys.dropSuspended()
	v.fails.flush()
}

// Stats returns the validated zone states kept, their bytes and the chain
// failures kept.
func (v *Validator) Stats() (zones, bytes, failures int) {
	zones, bytes = v.keys.stats()
	return zones, bytes, v.fails.len()
}

// Flights returns the chain steps in progress.
func (v *Validator) Flights() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.flights)
}

// Verifications returns the signature verifications since the start.
func (v *Validator) Verifications() int64 { return v.verifies.Load() }

// NewRootKey reports whether a validated root DNSKEY RRset held a SEP key
// that matches no anchor since the start.
func (v *Validator) NewRootKey() bool { return v.newRootKey.Load() }

// Request is the validation of one fetched answer.
type Request struct {
	// Route names the key and failure caches (a route of the upstream
	// package: its states never affect another route).
	Route string
	Name  string // question
	Type  uint16
	// Msg is the answer; the validator removes every record of the Answer
	// section that is not part of the answer to the question (scrub),
	// reduces the TTLs of the RRsets it authenticates and removes the
	// unvalidated RRsets of the Authority and Additional sections of a
	// secure answer (in place).
	Msg *dns.Msg
	// Degraded: the answering upstream is known not to return usable
	// DNSSEC data; the verdict is indeterminate with this reason.
	Degraded string
	Lookup   LookupFunc
	// AnchorMismatch is called when the answer is a root DNSKEY RRset
	// without a key matching an anchor (the answering upstream is
	// re-probed).
	AnchorMismatch func()
	Now            time.Time
	// TimeChecks: RRSIG validity periods are checked; false while they
	// are suspended (the verdict is then at best indeterminate).
	TimeChecks bool
	// MaxTTL is dns.cacheMaxTtl (0 = none): it caps the key states.
	MaxTTL uint32
}

// Validate validates req.Msg within a validation context of 4 s (ctx may
// end earlier: the chain steps continue detached and fill the key cache
// for a retry). Only NOERROR and NXDOMAIN answers are validated (others:
// Status ""); their Answer section is scrubbed first, whatever the verdict.
func (v *Validator) Validate(ctx context.Context, req *Request) Result {
	if req.Msg == nil || (req.Msg.Rcode != dns.RcodeSuccess && req.Msg.Rcode != dns.RcodeNameError) {
		return Result{EDE: EDENone} // not validated
	}
	scrub(req.Msg, req.Name, req.Type)
	if req.Degraded != "" {
		return Result{Status: Indeterminate, Reason: req.Degraded, EDE: EDENone}
	}
	ctx, cancel := context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	c := &vctx{
		sctx: sctx{ctx: ctx, v: v, b: &budget{}, now: req.Now, timeChecks: req.TimeChecks,
			onMismatch: req.AnchorMismatch},
		req:   req,
		steps: map[stepKey]stepResult{},
		zones: map[string]*zoneState{},
	}
	if req.Type == dns.TypeDS || req.Type == dns.TypeDNSKEY {
		c.seed = stepKey{req.Route, canon(req.Name), req.Type}
	}
	res := c.answer()
	if res.Status == Secure || res.Status == Insecure {
		if !req.TimeChecks {
			return Result{Status: Indeterminate, Zone: res.Zone, Reason: ReasonTimeSuspended, EDE: EDENone}
		}
	}
	if res.Status != Secure {
		res.Expires = time.Time{}
	}
	return res
}

// vctx is one validation context: its budget, the steps it made (a step is
// never repeated within a context) and the zone states it established.
type vctx struct {
	sctx
	req   *Request
	seed  stepKey // the client's own DS or DNSKEY query: a step for it is a chain loop
	steps map[stepKey]stepResult
	zones map[string]*zoneState
	kept  map[dns.RR]bool // authenticated records of the Authority section (trim)
}

// stepKey names a chain step: a DS or DNSKEY lookup of a name through a
// route.
type stepKey struct {
	route string
	name  string
	qtype uint16
}

// stepKind is the outcome of a chain step.
type stepKind int

const (
	stepBogus         stepKind = iota
	stepSecureDS               // DS: a signed DS RRset with supported entries (ds)
	stepKeys                   // DNSKEY: the zone's keys (keys)
	stepInsecure               // an insecure delegation (reason, ede)
	stepNotCut                 // DS: the name exists and is no zone cut
	stepCNAME                  // DS: a CNAME at the name (no zone cut; the walk stops)
	stepNXDomain               // DS: nothing exists at or below the name
	stepIndeterminate          // from a degraded upstream (degraded)
	stepBlocked                // the upstream's own block (block)
)

type stepResult struct {
	kind       stepKind
	ds         []*dns.DS
	keys       []key
	ttl        uint32
	expires    time.Time
	reason     string
	ede        int
	reportZone string
	fail       *failure
	degraded   string
	block      any
}

type stepCall struct {
	done chan struct{}
	res  stepResult
}

// step runs (or joins) a chain step and counts it against the context's
// lookups (32).
func (c *vctx) step(qtype uint16, name string, run func(sc *sctx, lookup LookupFunc) stepResult) stepResult {
	k := stepKey{c.req.Route, canon(name), qtype}
	if r, ok := c.steps[k]; ok {
		return r
	}
	if k == c.seed {
		return stepResult{kind: stepBogus, fail: fail(name, FailChainLoop, EDEBogus)}
	}
	if c.b.lookups >= maxChainLookups {
		return stepResult{kind: stepBogus, fail: limitFail(name)}
	}
	c.b.lookups++
	r := c.v.flight(c.ctx, k, c.req, run)
	c.steps[k] = r
	return r
}

// flight coalesces concurrent identical chain steps (all routes: at most
// maxChainFlights at a time; beyond that the step fails as a local
// refusal). The step runs detached with its own budget for at most
// contextTimeout; a waiter waits at most until its own context ends (then
// the step fails for it as a lookup failure, and the step's result still
// fills the caches). A step that panics ends as a lookup failure for its
// waiters; the panic goes on to the goroutine of Config.Spawn.
func (v *Validator) flight(ctx context.Context, k stepKey, req *Request, run func(sc *sctx, lookup LookupFunc) stepResult) stepResult {
	v.mu.Lock()
	call := v.flights[k]
	if call == nil {
		if len(v.flights) >= maxChainFlights {
			v.mu.Unlock()
			r := lookupFailure(k, "too many chain lookups")
			r.fail.local = true
			return r
		}
		call = &stepCall{done: make(chan struct{})}
		v.flights[k] = call
		v.mu.Unlock()
		now, timeChecks, lookup := req.Now, req.TimeChecks, req.Lookup
		finish := func() {
			v.mu.Lock()
			delete(v.flights, k)
			v.mu.Unlock()
			close(call.done)
		}
		if !v.cfg.Spawn(func() {
			defer finish()
			call.res = lookupFailure(k, "internal error") // replaced unless run panics
			fctx, cancel := context.WithTimeout(v.cfg.Base, contextTimeout)
			defer cancel()
			call.res = run(&sctx{ctx: fctx, v: v, b: &budget{}, now: now, timeChecks: timeChecks}, lookup)
		}) {
			call.res = lookupFailure(k, errClosed.Error())
			finish()
		}
	} else {
		v.mu.Unlock()
	}
	select {
	case <-call.done:
		return call.res
	case <-ctx.Done():
		return lookupFailure(k, "timeout")
	}
}

// lookupFailure is the result of a chain lookup that failed.
func lookupFailure(k stepKey, why string) stepResult {
	if k.qtype == dns.TypeDNSKEY {
		return stepResult{kind: stepBogus, fail: lookupFail(k.name, "DNSKEY lookup failed ("+why+")", EDEDNSKEYMissing)}
	}
	return stepResult{kind: stepBogus, fail: lookupFail(k.name, "DS lookup failed ("+why+")", EDEBogus)}
}

// cacheStep stores the outcome of a step for the route: an insecure or
// secure zone state in the key cache, a failure in the failure cache (not
// a local refusal: it says nothing about the zone).
func (v *Validator) cacheStep(sc *sctx, route, name string, r stepResult, maxTTL uint32) {
	k := cacheKey{route, canon(name)}
	switch r.kind {
	case stepBogus:
		if !r.fail.local {
			v.fails.put(k, r.fail, sc.now)
		}
	case stepInsecure:
		st := &zoneState{zone: k.zone, reason: r.reason, ede: r.ede, reportZone: r.reportZone}
		v.keys.put(k, st, lifetime(r.ttl, r.expires, maxTTL, sc.now, sc.timeChecks), sc.now, !sc.timeChecks)
	case stepKeys:
		st := &zoneState{zone: k.zone, secure: true, keys: r.keys}
		v.keys.put(k, st, lifetime(r.ttl, r.expires, maxTTL, sc.now, sc.timeChecks), sc.now, !sc.timeChecks)
	}
}

// replyProblem describes a chain reply that is not an answer ("" for
// NOERROR and NXDOMAIN).
func replyProblem(m *dns.Msg) string {
	if m == nil {
		return "no reply"
	}
	switch m.Rcode {
	case dns.RcodeSuccess, dns.RcodeNameError:
		return ""
	}
	if s, ok := dns.RcodeToString[m.Rcode]; ok {
		return s
	}
	return "rcode " + strconv.Itoa(m.Rcode)
}
