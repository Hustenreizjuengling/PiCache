package dnsserver

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// ourUDPSize is the EDNS buffer size we advertise and the UDP reply cap
// (DNS Flag Day 2020).
const ourUDPSize = 1232

var (
	errLoop       = errors.New("forwarding loop: the query came from the resolver it would be sent to")
	errNoUpstream = errors.New("no upstream resolver configured")
	errBadReply   = errors.New("malformed upstream reply")
)

// qctx is the state of one query travelling through the pipeline.
type qctx struct {
	ctx   context.Context
	start time.Time
	req   *dns.Msg
	q     dns.Question // as sent by the client (original case)
	qname string       // lower-case, no trailing dot
	qtype uint16
	// source is the transport address (ACL, blocked sources, rate limit,
	// loop guard, this server's names, ECS "client"); client is the
	// identity address: the source, or the address a trusted forwarder
	// named in EDNS (derived; groups, blocked identities, query log).
	source  netip.Addr
	client  netip.Addr
	derived bool
	// ednsMAC: the identity's MAC came from the EDNS options of a trusted
	// forwarder (otherwise from the neighbour table, the MAC of client).
	ednsMAC  bool
	proto    string // udp | tcp
	id       *clients.Identity
	set      *settings.All
	blocking bool            // blocking active (not disabled or paused, globally or for every group of the client)
	dec      filter.Decision // Filter.Check result with the filtering groups (steps 7c, 11; zero if blocking is off)
	decided  bool            // dec is computed
	steps    *[]string       // pipeline trace (Lookup only)
	// groups are the filtering groups: the identity's (enabled) groups
	// without those whose filtering is paused (Parental.FilterGroups; all
	// groups are qc.id.GroupIDs). The guard of step 7c, steps 8, 11, 14
	// and the blocked-name check of 14b use them; 7a, 7c, 10 and 14c use
	// all groups. scoped: groups and blocking are computed (scope).
	groups []int64
	scoped bool
	// recordsOnly: local records only, no DHCP lease names (wpad and
	// isatap in step 11a).
	recordsOnly bool
}

func newQuery(ctx context.Context, req *dns.Msg, source netip.Addr, proto string, set *settings.All) *qctx {
	q := req.Question[0]
	return &qctx{
		ctx:      ctx,
		start:    time.Now(),
		req:      req,
		q:        q,
		qname:    normalizeName(q.Name),
		qtype:    q.Qtype,
		source:   source,
		client:   source,
		proto:    proto,
		set:      set,
		blocking: set.Filter.BlockingActive(time.Now()),
	}
}

func (qc *qctx) tracing() bool { return qc.steps != nil }

// note appends a trace step (Lookup only).
func (qc *qctx) note(step string) {
	if qc.steps != nil {
		*qc.steps = append(*qc.steps, step)
	}
}

// result is the outcome of the pipeline.
type result struct {
	msg      *dns.Msg // reply before shaping; nil = no reply
	status   string
	reason   string
	listID   int64
	ruleID   int64
	service  string
	upstream string
	blocked  bool // blocked reply: EDE 15 for EDNS clients
	// edeText is the EDE 15 text when it must differ from reason (""
	// = reason): an upstream block never shows the upstream's host.
	edeText string
	// drop: no answer at all (dns.droppedDomains): UDP drop, TCP close.
	drop bool
	// def: the answer came from the default set (steps 13a, 13b, 14c);
	// block is the upstream's own block of it, ede the upstream reply's
	// EDE (any set).
	def   bool
	block *upstream.BlockInfo
	ede   *upstream.EDE
	// purpose is the statistics purpose of a list or rule decision (the
	// list's category or "rule"); the other purposes follow from the
	// status (purposeOf).
	purpose string
	// upstreamAnswer is the summary of the upstream's answer when the
	// steps after forwarding replaced or changed it (keepUpstreamAnswer);
	// the query log shows it when it differs from the final answer.
	upstreamAnswer string
}

// scope computes the filtering groups of the query once, after
// identification: the identity's groups without the paused ones. When the
// client has groups but all of them are paused, blocking is inactive for
// the query, like a global pause.
func (s *Server) scope(qc *qctx) {
	if qc.scoped {
		return
	}
	qc.scoped = true
	qc.groups = qc.id.GroupIDs
	if s.d.Parental != nil {
		qc.groups = s.d.Parental.FilterGroups(qc.id.GroupIDs, qc.start)
	}
	if len(qc.id.GroupIDs) > 0 && len(qc.groups) == 0 {
		qc.blocking = false
	}
}

// process runs ARCHITECTURE 7.1 steps 5a–14c for a validated, admitted and
// identified query.
func (s *Server) process(qc *qctx) result {
	s.scope(qc)
	if r, ok := s.dns64PTR(qc); ok { // 5a
		return r
	}
	if r, ok := s.specialUse(qc); ok { // 6
		return r
	}
	if r, ok := s.localAnswer(qc); ok { // 7
		return r
	}
	if r, ok := s.parentalBlock(qc); ok { // 7a
		return r
	}
	if entry, ok := s.droppedDomain(qc); ok { // 7b
		qc.note("dropped by dns.droppedDomains (" + entry + ")")
		return result{drop: true, status: StatusDropped, reason: entry}
	}
	if r, ok := s.safeSearch(qc); ok { // 7c
		return r
	}
	if r, ok := s.downloadCacheOverride(qc); ok { // 8 (user rules) + 9
		return r
	}
	if r, ok := s.specialDomain(qc); ok { // 10
		return r
	}
	if qc.blocking && s.d.Filter != nil { // 11
		if !qc.decided {
			qc.dec, qc.decided = s.d.Filter.Check(qc.qname, qc.groups), true
		}
		if qc.dec.Action == filter.ActionAllow {
			if qc.tracing() {
				qc.note(fmt.Sprintf("allowed by %s %q", qc.dec.Source, qc.dec.Name))
			}
		}
		if qc.dec.Blocked() {
			return s.blocked(qc, qc.dec, statusFor(qc.dec))
		}
	}
	if r, ok := s.singleLabel(qc); ok { // 11a
		up := upstreamAnswerOf(qc, r.msg)
		s.inspectCNAMEs(qc, &r) // 14 (local answers are not inspected)
		stripIPv6Hints(qc, &r)  // 14a
		up.keep(&r)
		return r
	}
	return s.forward(qc)
}

// forward runs steps 12a–14c: AAAA disabled, conditional forwarding or the
// default upstreams, and the checks of their answer.
func (s *Server) forward(qc *qctx) result {
	if r, ok := s.aaaaDisabled(qc); ok { // 12a
		return r
	}
	var (
		r   result
		via []string
		ips []netip.Addr
	)
	f := s.fwd.Load().match(qc.qname, false)
	if f == nil && !qc.set.DNS.DomainNeeded && singleLabelQuery(qc) {
		f = s.fwd.Load().unqualified // single-label names while dns.domainNeeded is off
	}
	switch {
	case f != nil && f.def: // 12: the default upstreams (the step-13 path)
		r = s.resolveVia(qc, nil, nil, "conditional forwarder "+f.domain+": default upstreams")
	case f != nil: // 12
		via, ips = f.upstreams, f.ips
		r = s.resolveVia(qc, via, ips, "conditional forwarder "+f.domain)
	default: // 13
		r = s.resolveVia(qc, nil, nil, "upstreams")
	}
	up := upstreamAnswerOf(qc, r.msg)
	s.upstreamBlock(qc, &r)            // 13a
	s.bogusNXDomain(qc, &r)            // 13b
	s.inspectCNAMEs(qc, &r)            // 14
	stripIPv6Hints(qc, &r)             // 14a
	s.synthesizeAAAA(qc, &r, via, ips) // 14b
	s.rebindCheck(qc, &r)              // 14c
	up.keep(&r)
	return r
}

// upstreamAnswer is the upstream's reply before steps 13a–14c: msg as
// received (also a cached reply), and its summary taken before steps 14a
// and 14b can change it in place (HTTPS/SVCB answers while
// dns.disableAAAA is on, AAAA queries while DNS64 is on). Other replies
// are summarised only when a step replaced them, so the hot path costs
// nothing.
type upstreamAnswer struct {
	msg     *dns.Msg
	summary string
	early   bool
}

func upstreamAnswerOf(qc *qctx, msg *dns.Msg) upstreamAnswer {
	up := upstreamAnswer{msg: msg}
	if msg == nil {
		return up
	}
	d := &qc.set.DNS
	if (d.DNS64.Enabled && qc.qtype == dns.TypeAAAA) || (d.DisableAAAA && slices.ContainsFunc(msg.Answer, func(rr dns.RR) bool {
		t := rr.Header().Rrtype
		return t == dns.TypeHTTPS || t == dns.TypeSVCB
	})) {
		up.summary, up.early = summarize(msg.Answer), true
	}
	return up
}

// keep records the upstream's answer in r where the final answer is not
// the upstream's (docs/API.md logs.QueryEvent.upstreamAnswer): CNAME,
// upstream and rebind blocks, bogus NXDOMAIN, DNS64 and removed ipv6hint
// parameters. logQuery drops it when it equals the final answer.
func (up upstreamAnswer) keep(r *result) {
	if up.msg == nil {
		return
	}
	switch {
	case r.status == StatusBlockedCNAME, r.status == StatusBlockedUpstream, r.status == StatusBlockedRebind,
		r.reason == ReasonBogusNXDomain:
		if !up.early {
			up.summary = summarize(up.msg.Answer)
		}
		r.upstreamAnswer = up.summary
	case up.early: // DNS64 or a removed ipv6hint (unchanged answers are dropped by logQuery)
		r.upstreamAnswer = up.summary
	}
}

// resolveVia forwards the query to specific resolvers (via == nil: the
// default set) and converts the reply into a result.
func (s *Server) resolveVia(qc *qctx, via []string, ips []netip.Addr, what string) result {
	resp, info, err := s.exchange(qc, qc.q, via, ips)
	if err != nil {
		if qc.tracing() {
			qc.note(fmt.Sprintf("%s failed: %s", what, errText(err)))
		}
		r := s.servfail(qc, errText(err))
		r.upstream = info.Upstream
		return r
	}
	status := StatusForwarded
	switch {
	case info.Stale:
		status = StatusStale
	case info.Cached:
		status = StatusCached
	}
	if qc.tracing() {
		qc.note(fmt.Sprintf("answered via %s (%s, %s)", what, orDash(info.Upstream), status))
	}
	r := result{msg: resp, status: status, upstream: info.Upstream, def: len(via) == 0, block: info.Block, ede: info.EDE}
	if info.Fallback {
		qc.note("fallback: answered by " + orDash(info.Upstream))
		r.reason = ReasonFallback
	}
	return r
}

// exchange sends a fresh query for q upstream: via == nil asks the default
// set with the client subnet of dns.ecs. A query from one of the target
// resolvers themselves (the source address) is refused with errLoop (loop
// guard).
func (s *Server) exchange(qc *qctx, q dns.Question, via []string, ips []netip.Addr) (*dns.Msg, upstream.Info, error) {
	if s.d.Upstream == nil {
		return nil, upstream.Info{}, errNoUpstream
	}
	if slices.Contains(ips, qc.source) {
		return nil, upstream.Info{}, errLoop
	}
	req := upstreamRequest(qc.req, q)
	var (
		resp *dns.Msg
		info upstream.Info
		err  error
	)
	if len(via) == 0 {
		resp, info, err = s.d.Upstream.Resolve(qc.ctx, req, s.ecsFor(qc))
	} else {
		resp, info, err = s.d.Upstream.ResolveVia(qc.ctx, req, via)
	}
	if err == nil && (resp == nil || len(resp.Question) != 1) {
		err = errBadReply
	}
	return resp, info, err
}

// upstreamRequest builds the query handed to the upstream package: the
// question, RD, the client's CD and AD bits and DO; no client EDNS options.
func upstreamRequest(client *dns.Msg, q dns.Question) *dns.Msg {
	m := new(dns.Msg)
	m.Id = client.Id
	m.RecursionDesired = true
	m.CheckingDisabled = client.CheckingDisabled
	m.AuthenticatedData = client.AuthenticatedData
	m.Question = []dns.Question{q}
	if opt := client.IsEdns0(); opt != nil && opt.Do() {
		m.SetEdns0(ourUDPSize, true)
	}
	return m
}

// inspectCNAMEs replaces a forwarded answer by the blocking reply if any
// CNAME target in it is blocked for the client (step 14).
func (s *Server) inspectCNAMEs(qc *qctx, r *result) {
	if !qc.set.Filter.CNAMEInspection || !qc.blocking || s.d.Filter == nil || r.msg == nil ||
		qc.dec.Action == filter.ActionAllow {
		return
	}
	switch r.status {
	case StatusForwarded, StatusCached, StatusStale:
	default:
		return
	}
	for _, rr := range r.msg.Answer {
		c, ok := rr.(*dns.CNAME)
		if !ok {
			continue
		}
		target := normalizeName(c.Target)
		d := s.d.Filter.Check(target, qc.groups)
		if !d.Blocked() {
			continue
		}
		qc.note("CNAME target " + target + " is blocked")
		upstreamName, def, ede := r.upstream, r.def, r.ede
		*r = s.blocked(qc, d, StatusBlockedCNAME)
		r.reason = d.Name + " (CNAME " + target + ")"
		r.upstream, r.def, r.ede = upstreamName, def, ede
		return
	}
}

// servfail returns SERVFAIL with status error.
func (s *Server) servfail(qc *qctx, reason string) result {
	m := newReply(qc.req)
	m.Rcode = dns.RcodeServerFailure
	return result{msg: m, status: StatusError, reason: reason}
}

// newReply creates a reply to req (question, ID, RD and CD copied; RA set).
func newReply(req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = true
	return m
}

func rrHeader(name string, t uint16, ttl uint32) dns.RR_Header {
	return dns.RR_Header{Name: name, Rrtype: t, Class: dns.ClassINET, Ttl: ttl}
}

// errText returns a short, log-safe error description.
func errText(err error) string {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return strings.ToValidUTF8(msg, "?")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
