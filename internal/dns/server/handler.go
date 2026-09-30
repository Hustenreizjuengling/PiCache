package dnsserver

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	// maxInFlight bounds concurrently processed queries (miekg starts one
	// goroutine per UDP packet); beyond it UDP is dropped and TCP gets SERVFAIL.
	maxInFlight = 4096
	// queryTimeout bounds one query through the pipeline.
	queryTimeout = 10 * time.Second
	// maxSummaryLen bounds the answer summary in the query log.
	maxSummaryLen = 256
)

// Transport protocols of a query (logs.QueryEvent.protocol).
const (
	ProtoUDP = "udp"
	ProtoTCP = "tcp"
	ProtoDoT = "dot"
	ProtoDoH = "doh"
)

// dnsHandler adapts the server to miekg's dns.Handler.
type dnsHandler struct {
	s     *Server
	ctx   context.Context // cancelled when Serve ends
	proto string          // ProtoDoT on the DoT listeners; "" = UDP or TCP by the peer
}

// queryConn describes where a query came from: the transport protocol,
// the source (the transport peer; for DoH the effective client) and the
// ClientID it carried (DoT: the SNI, DoH: the path; "" = none). overload,
// when set, reports that too many queries are in flight instead of the
// SERVFAIL a TCP client gets (DoH answers 503).
type queryConn struct {
	proto    string
	source   netip.Addr
	clientID string
	overload func()
	// forwarded: the source is the effective client a trusted reverse
	// proxy forwarded (DoH on the web listeners), not the transport
	// source: iface: identifiers never apply.
	forwarded bool
	// zone: the zone the kernel reported with an IPv6 link-local transport
	// source, i.e. the interface it arrived on (netutil.LinkLocalZone);
	// source itself is canonical.
	zone string
}

// stream reports whether replies travel over a stream (TCP, DoT, DoH):
// never truncated, and a dropped query closes the connection (DoH: resets
// the stream).
func (c queryConn) stream() bool { return c.proto != ProtoUDP }

// encrypted reports DoT and DoH (EDNS padding, ClientIDs, no plain-DNS gate).
func (c queryConn) encrypted() bool { return c.proto == ProtoDoT || c.proto == ProtoDoH }

// ServeDNS runs the request pipeline (ARCHITECTURE 7.1) for one query over
// UDP, TCP or DoT.
func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	c := queryConn{proto: h.proto, source: netutil.AddrFromNet(w.RemoteAddr()), zone: netutil.PeerZone(w.RemoteAddr())}
	if c.proto == "" {
		c.proto = ProtoUDP
		if _, isTCP := w.RemoteAddr().(*net.TCPAddr); isTCP {
			c.proto = ProtoTCP
		}
	}
	if c.proto == ProtoDoT {
		// Checked again per query: switching DoT off closes the
		// connections of the listener too.
		if !h.s.d.Settings.Get().DNS.Encrypted.DoT {
			_ = w.Close()
			return
		}
		c.clientID = h.s.sniClientID(w)
	}
	h.s.serve(h.ctx, w, req, c)
}

// serve runs the pipeline for one query from c.
func (s *Server) serve(ctx context.Context, w dns.ResponseWriter, req *dns.Msg, c queryConn) {
	// miekg does not recover handler panics; one bad query must not stop DNS.
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("panic while answering a DNS query", slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
		}
	}()
	stream := c.stream()
	ip := c.source

	// Health probes are answered like localhost before anything is counted,
	// limited, recorded or logged.
	if s.healthProbe(req, ip) {
		qc := newQuery(ctx, req, ip, c.proto, s.d.Settings.Get())
		_ = w.WriteMsg(s.shape(qc, s.addrAnswer(qc, localhostV4, localhostV6), stream))
		return
	}
	s.queries.Add(1)

	// 2. ACL first, so nothing is ever sent to disallowed sources (UDP is
	// additionally filtered before parsing, TCP and DoT at accept, DoH
	// before the pipeline).
	if !s.allowed(ip) {
		s.refused.Add(1)
		s.refusedSrc.add(ip, time.Now())
		if stream {
			_ = w.Close()
		}
		return
	}
	// 2a. Blocked sources and blocked ClientIDs (dns.blockedClients): no
	// answer, the connection is closed; not logged, not seen, not refused.
	_, blocked := s.blockedSource(ip)
	if !blocked && c.clientID != "" {
		_, blocked = s.blockedClientID(c.clientID)
	}
	if blocked {
		s.blockedClients.Add(1)
		if stream {
			_ = w.Close()
		}
		return
	}
	set := s.d.Settings.Get()

	// 1. Validation: one question (miekg answers FORMERR otherwise), opcode
	// QUERY, class IN, no server-identity queries, EDNS version 0.
	if len(req.Question) != 1 {
		s.refused.Add(1)
		m := new(dns.Msg).SetRcode(req, dns.RcodeFormatError)
		_ = w.WriteMsg(m)
		return
	}
	qc := newQuery(ctx, req, ip, c.proto, set)
	qc.clientID = c.clientID
	qc.forwarded = c.forwarded
	qc.zone = c.zone
	if rcode, reason := validate(req); rcode >= 0 {
		s.refused.Add(1)
		qc.id = s.identify(qc.peer())
		s.reply(w, qc, s.refusal(qc, rcode, reason), stream)
		return
	}

	// 3. Rate limit.
	if ok, first := s.limiter.Allow(ip); !ok {
		s.rateLimited.Add(1)
		if first {
			s.log.Warn("client exceeds the DNS rate limit; its queries are dropped (if it is a router or another DNS server forwarding to PiCache, add it to dns.rateLimitExempt)",
				slog.String("client", netutil.RateKey(ip, set.DNS.RateLimitIPv4Prefix, set.DNS.RateLimitIPv6Prefix).String()))
		}
		if stream {
			m := newReply(req)
			m.Rcode = dns.RcodeRefused
			_ = w.WriteMsg(s.shape(qc, result{msg: m}, true))
		}
		return
	}

	// 3a. Plain DNS closed (dns.plainDns off while DoT or DoH serves):
	// other devices get REFUSED over UDP and TCP, except for the names
	// that bootstrap encrypted DNS; this machine is exempt.
	if !c.encrypted() && s.plainClosed(set) && !s.plainExempt(ip) && !plainBootstrapName(set, qc.qname) {
		s.refused.Add(1)
		qc.id = s.identify(qc.peer())
		res := s.refusal(qc, dns.RcodeRefused, ReasonPlainDNSOff)
		res.plainOff = true
		s.reply(w, qc, res, stream)
		return
	}

	if s.inFlight.Add(1) > maxInFlight {
		s.inFlight.Add(-1)
		s.overloaded.Add(1)
		switch {
		case c.overload != nil:
			c.overload()
		case stream:
			m := newReply(req)
			m.Rcode = dns.RcodeServerFailure
			_ = w.WriteMsg(s.shape(qc, result{msg: m}, true))
		}
		return
	}
	defer s.inFlight.Add(-1)

	// 4. Hardening: ANY.
	if set.DNS.RefuseANY && qc.qtype == dns.TypeANY {
		s.refused.Add(1)
		qc.id = s.identify(qc.peer())
		s.reply(w, qc, s.refusal(qc, dns.RcodeNotImplemented, "ANY queries are refused"), stream)
		return
	}

	// 4a + 5. Identify: a trusted forwarder may name its client in EDNS;
	// a ClientID decides only when the source identifies no client.
	s.identifyClient(qc)
	// Blocked identities (a MAC, an address from EDNS): dropped like
	// blocked sources, before anything is recorded.
	if _, blocked := s.blockedIdentity(qc); blocked {
		s.blockedClients.Add(1)
		if stream {
			_ = w.Close()
		}
		return
	}
	// Clients excluded from the raw data (ignoreLogs) are not recorded as
	// seen; while client addresses are anonymised, or neither the query log
	// nor the statistics are kept, the activity is kept in memory only
	// (nothing is written to logs.db). ClientIDs stay in memory.
	if s.d.Clients != nil && !qc.id.IgnoreLogs {
		if lg := &set.Logs; lg.AnonymizeClientIPs || (!lg.QueryLogEnabled && !lg.StatsEnabled) {
			s.d.Clients.SeenTransient(qc.client)
		} else {
			s.d.Clients.Seen(qc.client)
		}
		if qc.clientID != "" {
			s.d.Clients.SeenDNSClientID(qc.client, qc.clientID)
		}
	}

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	if !s.limiter.Exempt(ip) {
		// A rate-limited source's DNSSEC chain lookups draw on its
		// device's share of the chain exchanges (7.6); exempt sources (a
		// router or forwarder with a whole LAN behind it) only on the
		// global rate.
		qctx = upstream.WithClient(qctx, chainClientKey(qc, ip), ip)
	}
	qc.ctx = qctx
	res := s.process(qc)
	if res.drop { // 7b: dns.droppedDomains
		s.dropped.Add(1)
		if stream {
			_ = w.Close()
		}
		return
	}
	s.reply(w, qc, res, stream)
}

// HealthProbeName is the name `picache healthcheck` (the Docker
// HEALTHCHECK) asks for. From this machine it is answered like localhost;
// from anywhere else it is an ordinary name below "invalid" (NXDOMAIN).
const HealthProbeName = "healthcheck.picache.invalid."

// healthProbe reports whether req is a health probe: a class IN query for
// HealthProbeName from a loopback address or one of this machine's own
// addresses (a listener bound to a specific address) that the ACL allows.
// Probes are never counted, rate limited, recorded as client activity or
// logged. The name is answered locally and carries nothing, so no client
// can hide a real query this way.
func (s *Server) healthProbe(req *dns.Msg, ip netip.Addr) bool {
	if req.Opcode != dns.OpcodeQuery || len(req.Question) != 1 {
		return false
	}
	q := req.Question[0]
	return q.Qclass == dns.ClassINET && strings.EqualFold(q.Name, HealthProbeName) &&
		(ip.IsLoopback() || s.host.Load().isOwn(ip)) && s.allowed(ip)
}

// reply shapes, sends and logs the result; answered DoT and DoH queries
// are counted.
func (s *Server) reply(w dns.ResponseWriter, qc *qctx, res result, stream bool) {
	if res.msg == nil {
		return
	}
	m := s.shape(qc, res, stream)
	if err := w.WriteMsg(m); err != nil {
		s.log.Debug("write DNS reply", slog.String("client", qc.source.String()), slog.Any("err", err))
	}
	switch qc.proto {
	case ProtoDoT:
		s.dotAnswered.Add(1)
	case ProtoDoH:
		s.dohAnswered.Add(1)
	}
	s.dnssec.count(res.dnssec, time.Now())
	s.logQuery(qc, res, m)
}

// validate returns the rcode for requests the pipeline does not serve
// (-1 = serve).
func validate(req *dns.Msg) (int, string) {
	if req.Opcode != dns.OpcodeQuery {
		return dns.RcodeNotImplemented, "opcode " + opcodeString(req.Opcode) + " is not supported"
	}
	q := req.Question[0]
	if q.Qclass != dns.ClassINET {
		return dns.RcodeRefused, "class " + classString(q.Qclass) + " is refused"
	}
	switch normalizeName(q.Name) {
	case "version.bind", "version.server", "id.server", "hostname.bind", "authors.bind":
		return dns.RcodeRefused, "server identity queries are refused"
	}
	switch q.Qtype {
	case dns.TypeAXFR, dns.TypeIXFR, dns.TypeMAILA, dns.TypeMAILB, dns.TypeOPT:
		return dns.RcodeRefused, "query type " + typeString(q.Qtype) + " is refused"
	}
	if opt := req.IsEdns0(); opt != nil && opt.Version() != 0 {
		return dns.RcodeBadVers, "unsupported EDNS version"
	}
	return -1, ""
}

// refusal builds an early error reply (status refused).
func (s *Server) refusal(qc *qctx, rcode int, reason string) result {
	m := newReply(qc.req)
	m.Rcode = rcode
	return result{msg: m, status: StatusRefused, reason: reason}
}

// shape applies ARCHITECTURE 7.1 step 15: our OPT only for EDNS clients,
// DNSSEC records only for DO clients, AD only if requested, the client's
// CD echoed (RFC 4035 3.2.2), EDE 15 on blocked replies (EDE 18 while
// plain DNS is closed; the EDE of a DNSSEC verdict on its SERVFAIL and on
// an insecure answer of an unsupported algorithm, digest or NSEC3
// parameters), truncation to the client's UDP size (never over a stream),
// EDNS padding for DoT and DoH.
func (s *Server) shape(qc *qctx, res result, stream bool) *dns.Msg {
	m := res.msg
	m.Id = qc.req.Id
	m.Response = true
	m.RecursionAvailable = true
	m.CheckingDisabled = qc.req.CheckingDisabled
	m.Extra = dropOPT(m.Extra)
	opt := qc.req.IsEdns0()
	do := opt != nil && opt.Do()
	if !do && !isDNSSECType(qc.qtype) {
		m.Answer = dropDNSSEC(m.Answer)
		m.Ns = dropDNSSEC(m.Ns)
		m.Extra = dropDNSSEC(m.Extra)
	}
	m.AuthenticatedData = m.AuthenticatedData && (qc.req.AuthenticatedData || do)
	if opt == nil && m.Rcode > 0xF {
		m.Rcode = dns.RcodeServerFailure // extended rcodes need EDNS
	}
	size := dns.MaxMsgSize
	if !stream {
		size = dns.MinMsgSize
	}
	if opt != nil {
		m.SetEdns0(ourUDPSize, do)
		if res.blocked {
			text := res.reason
			if res.edeText != "" {
				text = res.edeText
			}
			if len(text) > 128 {
				text = text[:128]
			}
			o := m.IsEdns0()
			o.Option = append(o.Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeBlocked, ExtraText: strings.ToValidUTF8(text, "?")})
		}
		if res.plainOff {
			o := m.IsEdns0()
			o.Option = append(o.Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeProhibited, ExtraText: plainOffText})
		}
		if v := res.dnssec; v != nil && v.EDE != nil && !res.blocked &&
			(res.dnssecFail || v.Status == dnssecInsecure && forwardedStatus(res.status)) {
			o := m.IsEdns0()
			o.Option = append(o.Option, &dns.EDNS0_EDE{InfoCode: v.EDE.Code, ExtraText: v.EDE.Text})
		}
		if !stream {
			size = min(max(int(opt.UDPSize()), dns.MinMsgSize), ourUDPSize)
		}
	}
	m.Truncate(size)
	if opt != nil && (qc.proto == ProtoDoT || qc.proto == ProtoDoH) && hasPadding(opt) {
		pad(m)
	}
	return m
}

// plainOffText is the EDE 18 text of the REFUSED replies while plain DNS
// is closed.
const plainOffText = "plain DNS is disabled on this server; use DoT or DoH"

// paddingBlock is the block size replies are padded to (RFC 8467 4.1).
const paddingBlock = 468

// hasPadding reports whether an OPT record carries the Padding option.
func hasPadding(opt *dns.OPT) bool {
	for _, o := range opt.Option {
		if o.Option() == dns.EDNS0PADDING {
			return true
		}
	}
	return false
}

// pad adds a Padding option (RFC 7830) to the reply's OPT record so that
// the reply is a multiple of 468 bytes (never beyond the largest message).
func pad(m *dns.Msg) {
	o := m.IsEdns0()
	if o == nil {
		return
	}
	n := m.Len() + 4 // the option header
	fill := (paddingBlock - n%paddingBlock) % paddingBlock
	if n+fill > dns.MaxMsgSize {
		return
	}
	o.Option = append(o.Option, &dns.EDNS0_PADDING{Padding: make([]byte, fill)})
}

func dropOPT(rrs []dns.RR) []dns.RR {
	out := rrs[:0]
	for _, rr := range rrs {
		if rr.Header().Rrtype != dns.TypeOPT {
			out = append(out, rr)
		}
	}
	return out
}

func isDNSSECType(t uint16) bool {
	return t == dns.TypeRRSIG || t == dns.TypeNSEC || t == dns.TypeNSEC3
}

func dropDNSSEC(rrs []dns.RR) []dns.RR {
	out := rrs[:0]
	for _, rr := range rrs {
		if !isDNSSECType(rr.Header().Rrtype) {
			out = append(out, rr)
		}
	}
	return out
}

// logQuery hands the query to the query log and the statistics (async in
// the logs package): not for a client excluded from both (ignoreLogs and
// ignoreStats), otherwise with NoLog and NoStats of its identity, and never
// for logs.ignoredDomains.
func (s *Server) logQuery(qc *qctx, res result, reply *dns.Msg) {
	if s.d.Logs == nil || qc.id == nil || (qc.id.IgnoreLogs && qc.id.IgnoreStats) || qc.tracing() || s.ignoredDomain(qc.qname) {
		return
	}
	answer := summarize(reply.Answer)
	e := logs.QueryEvent{
		Time:        qc.start.UTC(),
		ClientIP:    qc.client.String(),
		ClientName:  qc.id.Name,
		QName:       qc.qname,
		QType:       typeString(qc.qtype),
		Status:      res.status,
		RCode:       rcodeString(reply.Rcode),
		Reason:      res.reason,
		ListID:      res.listID,
		RuleID:      res.ruleID,
		Service:     res.service,
		Upstream:    res.upstream,
		DurationUs:  time.Since(qc.start).Microseconds(),
		Answer:      answer,
		DNSSEC:      reply.AuthenticatedData,
		Protocol:    qc.proto,
		ECS:         clientSubnet(qc.req),
		DNSClientID: qc.clientID,
		Purpose:     purposeOf(res),
		NoLog:       qc.id.IgnoreLogs,
		NoStats:     qc.id.IgnoreStats,
	}
	if res.dnssec != nil {
		e.DNSSECStatus = res.dnssec.Status
	}
	if res.upstreamAnswer != "" && res.upstreamAnswer != answer {
		e.UpstreamAnswer = res.upstreamAnswer
	}
	if res.ede != nil {
		e.UpstreamEDE = &logs.UpstreamEDE{Code: int(res.ede.Code), Text: res.ede.Text}
	}
	s.d.Logs.LogQuery(e)
}

// Statistics purposes that are not list categories (ARCHITECTURE 11).
const (
	PurposeRule       = "rule"
	PurposeService    = "service"
	PurposeSchedule   = "schedule"
	PurposeUpstream   = "upstream"
	PurposeRebind     = "rebind"
	PurposeSpecial    = "special"
	PurposeSafeSearch = "safesearch"
)

// purposeOf returns the statistics purpose of a logged query ("" = not
// counted): the list category or "rule" of a list or rule decision (set by
// the step), otherwise the mechanism of the status.
func purposeOf(res result) string {
	switch res.status {
	case StatusBlockedList, StatusBlockedRegex, StatusBlockedCNAME, StatusBlockedIP:
		if res.purpose != "" {
			return res.purpose
		}
		return filter.CategoryOther
	case StatusBlockedRule:
		return PurposeRule
	case StatusBlockedService:
		return PurposeService
	case StatusBlockedSchedule:
		return PurposeSchedule
	case StatusBlockedUpstream:
		return PurposeUpstream
	case StatusBlockedRebind:
		return PurposeRebind
	case StatusBlockedSpecial:
		return PurposeSpecial
	case StatusSafeSearch:
		return PurposeSafeSearch
	}
	return ""
}

// summarize returns a compact answer summary (≤ 256 bytes), e.g.
// "CNAME cdn.example.net, 192.0.2.1, 192.0.2.2".
func summarize(rrs []dns.RR) string {
	var b strings.Builder
	for _, rr := range rrs {
		var part string
		switch v := rr.(type) {
		case *dns.A:
			part = v.A.String()
		case *dns.AAAA:
			part = v.AAAA.String()
		case *dns.CNAME:
			part = "CNAME " + strings.TrimSuffix(v.Target, ".")
		case *dns.RRSIG, *dns.NSEC, *dns.NSEC3:
			continue
		default:
			part = typeString(rr.Header().Rrtype) + " " + strings.TrimPrefix(rr.String(), rr.Header().String())
		}
		if b.Len() > 0 {
			b.WriteString(", ")
		}
		b.WriteString(part)
		if b.Len() > maxSummaryLen {
			break
		}
	}
	out := strings.ToValidUTF8(b.String(), "?")
	if len(out) > maxSummaryLen {
		out = out[:maxSummaryLen-3] + "..."
	}
	return out
}

// identify returns the client identity (Default group if unknown).
func (s *Server) identify(ip netip.Addr) *clients.Identity {
	if s.d.Clients != nil {
		if id := s.d.Clients.Identify(ip); id != nil {
			return id
		}
	}
	return &clients.Identity{IP: ip, GroupIDs: []int64{clients.DefaultGroupID}}
}

// allowed reports whether ip may use the DNS service.
func (s *Server) allowed(ip netip.Addr) bool { return s.d.ACL.Get().Allowed(ip) }

// admitUDP reports whether a UDP packet from ip may be parsed: the source
// is allowed by the ACL and not a blocked client (step 2a). Refused and
// blocked packets are counted.
func (s *Server) admitUDP(ip netip.Addr) bool {
	if !s.allowed(ip) {
		s.refused.Add(1)
		s.refusedSrc.add(ip, time.Now())
		return false
	}
	if _, blocked := s.blockedSource(ip); blocked {
		s.blockedClients.Add(1)
		return false
	}
	return true
}

// aclReader drops UDP packets from sources outside the ACL and from
// blocked sources before miekg parses them, so they never get any reply
// (not even FORMERR).
type aclReader struct {
	next dns.PacketConnReader
	s    *Server
}

func (s *Server) decorateReader(r dns.Reader) dns.Reader {
	pr, ok := r.(dns.PacketConnReader)
	if !ok {
		return r
	}
	return &aclReader{next: pr, s: s}
}

func (a *aclReader) ReadTCP(conn net.Conn, timeout time.Duration) ([]byte, error) {
	return a.next.ReadTCP(conn, timeout)
}

func (a *aclReader) ReadUDP(conn *net.UDPConn, timeout time.Duration) ([]byte, *dns.SessionUDP, error) {
	for {
		m, sess, err := a.next.ReadUDP(conn, timeout)
		if err != nil {
			return m, sess, err
		}
		if a.s.admitUDP(netutil.AddrFromNet(sess.RemoteAddr())) {
			return m, sess, nil
		}
	}
}

func (a *aclReader) ReadPacketConn(conn net.PacketConn, timeout time.Duration) ([]byte, net.Addr, error) {
	for {
		m, addr, err := a.next.ReadPacketConn(conn, timeout)
		if err != nil {
			return m, addr, err
		}
		if a.s.admitUDP(netutil.AddrFromNet(addr)) {
			return m, addr, nil
		}
	}
}

func typeString(t uint16) string {
	if s, ok := dns.TypeToString[t]; ok {
		return s
	}
	return "TYPE" + strconv.Itoa(int(t))
}

func rcodeString(rc int) string {
	if s, ok := dns.RcodeToString[rc]; ok {
		return s
	}
	return "RCODE" + strconv.Itoa(rc)
}

func classString(c uint16) string {
	if s, ok := dns.ClassToString[c]; ok {
		return s
	}
	return "CLASS" + strconv.Itoa(int(c))
}

func opcodeString(op int) string {
	if s, ok := dns.OpcodeToString[op]; ok {
		return s
	}
	return strconv.Itoa(op)
}
