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

// dnsHandler adapts the server to miekg's dns.Handler.
type dnsHandler struct {
	s   *Server
	ctx context.Context // cancelled when Serve ends
}

// ServeDNS runs the request pipeline (ARCHITECTURE 7.1) for one query.
func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	s := h.s
	// miekg does not recover handler panics; one bad query must not stop DNS.
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("panic while answering a DNS query", slog.Any("panic", v), slog.String("stack", string(debug.Stack())))
		}
	}()
	s.queries.Add(1)
	_, isTCP := w.RemoteAddr().(*net.TCPAddr)
	proto := "udp"
	if isTCP {
		proto = "tcp"
	}
	ip := netutil.AddrFromNet(w.RemoteAddr())

	// 2. ACL first, so nothing is ever sent to disallowed sources (UDP is
	// additionally filtered before parsing, TCP at accept).
	if !s.allowed(ip) {
		s.refused.Add(1)
		if isTCP {
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
	qc := newQuery(h.ctx, req, ip, proto, set)
	if rcode, reason := validate(req); rcode >= 0 {
		s.refused.Add(1)
		qc.id = s.identify(ip)
		s.reply(w, qc, s.refusal(qc, rcode, reason), isTCP)
		return
	}

	// 3. Rate limit.
	if ok, first := s.limiter.Allow(ip); !ok {
		s.rateLimited.Add(1)
		if first {
			s.log.Warn("client exceeds the DNS rate limit; its queries are dropped (if it is a router or another DNS server forwarding to PiCache, add it to dns.rateLimitExempt)",
				slog.String("client", netutil.ClientKey(ip).String()))
		}
		if isTCP {
			m := newReply(req)
			m.Rcode = dns.RcodeRefused
			_ = w.WriteMsg(s.shape(qc, result{msg: m}, true))
		}
		return
	}

	if s.inFlight.Add(1) > maxInFlight {
		s.inFlight.Add(-1)
		s.overloaded.Add(1)
		if isTCP {
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
		qc.id = s.identify(ip)
		s.reply(w, qc, s.refusal(qc, dns.RcodeNotImplemented, "ANY queries are refused"), isTCP)
		return
	}

	// 5. Identify. Clients excluded from logs are not recorded as seen, and
	// while client addresses are anonymised the activity is kept in memory
	// only (nothing is written to logs.db).
	qc.id = s.identify(ip)
	if s.d.Clients != nil && !qc.id.IgnoreLogs {
		if set.Logs.AnonymizeClientIPs {
			s.d.Clients.SeenTransient(ip)
		} else {
			s.d.Clients.Seen(ip)
		}
	}

	ctx, cancel := context.WithTimeout(h.ctx, queryTimeout)
	defer cancel()
	qc.ctx = ctx
	s.reply(w, qc, s.process(qc), isTCP)
}

// reply shapes, sends and logs the result.
func (s *Server) reply(w dns.ResponseWriter, qc *qctx, res result, isTCP bool) {
	if res.msg == nil {
		return
	}
	m := s.shape(qc, res, isTCP)
	if err := w.WriteMsg(m); err != nil {
		s.log.Debug("write DNS reply", slog.String("client", qc.client.String()), slog.Any("err", err))
	}
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
// DNSSEC records only for DO clients, AD only if requested, EDE 15 on
// blocked replies, truncation to the client's UDP size.
func (s *Server) shape(qc *qctx, res result, isTCP bool) *dns.Msg {
	m := res.msg
	m.Id = qc.req.Id
	m.Response = true
	m.RecursionAvailable = true
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
	if !isTCP {
		size = dns.MinMsgSize
	}
	if opt != nil {
		m.SetEdns0(ourUDPSize, do)
		if res.blocked {
			text := res.reason
			if len(text) > 128 {
				text = text[:128]
			}
			o := m.IsEdns0()
			o.Option = append(o.Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeBlocked, ExtraText: strings.ToValidUTF8(text, "?")})
		}
		if !isTCP {
			size = min(max(int(opt.UDPSize()), dns.MinMsgSize), ourUDPSize)
		}
	}
	m.Truncate(size)
	return m
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

// logQuery hands the query to the query log (async in the logs package),
// unless the client is excluded from logging.
func (s *Server) logQuery(qc *qctx, res result, reply *dns.Msg) {
	if s.d.Logs == nil || qc.id == nil || qc.id.IgnoreLogs || qc.tracing() {
		return
	}
	s.d.Logs.LogQuery(logs.QueryEvent{
		Time:       qc.start.UTC(),
		ClientIP:   qc.client.String(),
		ClientName: qc.id.Name,
		QName:      qc.qname,
		QType:      typeString(qc.qtype),
		Status:     res.status,
		RCode:      rcodeString(reply.Rcode),
		Reason:     res.reason,
		ListID:     res.listID,
		RuleID:     res.ruleID,
		Service:    res.service,
		Upstream:   res.upstream,
		DurationUs: time.Since(qc.start).Microseconds(),
		Answer:     summarize(reply.Answer),
		DNSSEC:     reply.AuthenticatedData,
		Protocol:   qc.proto,
	})
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

// aclReader drops UDP packets from sources outside the ACL before miekg
// parses them, so disallowed sources never get any reply (not even FORMERR).
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
		if err != nil || a.s.allowed(netutil.AddrFromNet(sess.RemoteAddr())) {
			return m, sess, err
		}
		a.s.refused.Add(1)
	}
}

func (a *aclReader) ReadPacketConn(conn net.PacketConn, timeout time.Duration) ([]byte, net.Addr, error) {
	for {
		m, addr, err := a.next.ReadPacketConn(conn, timeout)
		if err != nil || a.s.allowed(netutil.AddrFromNet(addr)) {
			return m, addr, err
		}
		a.s.refused.Add(1)
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
