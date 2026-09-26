package dnsserver

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Encrypted DNS for clients (docs/ARCHITECTURE.md 19): DoT listeners
// (Serve), DoH (ServeDoH), ClientIDs, the plain-DNS switch (step 3a) and
// the discovery of designated resolvers (DDR, step 6).

// ReasonPlainDNSOff is the query-log reason of the REFUSED replies while
// plain DNS is closed (step 3a).
const ReasonPlainDNSOff = "plain-dns-off"

// ddrName is the name of the designated resolvers (RFC 9462).
const ddrName = "_dns.resolver.arpa"

// encState returns the current encrypted-DNS snapshot (never nil).
func (s *Server) encState() *EncryptedState {
	if s.d.Encrypted != nil {
		if st := s.d.Encrypted(); st != nil {
			return st
		}
	}
	return &EncryptedState{}
}

// EncryptedQueries returns the DoT and DoH queries answered since the
// start.
func (s *Server) EncryptedQueries() (dot, doh int64) {
	return s.dotAnswered.Load(), s.dohAnswered.Load()
}

// plainClosed reports whether plain DNS is closed: dns.plainDns is off and
// DoT or DoH is serving. With nothing serving plain DNS stays open (fail
// open, docs/ARCHITECTURE.md 19).
func (s *Server) plainClosed(set *settings.All) bool {
	return !set.DNS.PlainDNS && s.encState().Serving()
}

// plainExempt reports sources plain DNS always serves: loopback and this
// machine's own addresses (the rule of the health probe), so the host's
// resolver and `picache healthcheck` keep working.
func (s *Server) plainExempt(ip netip.Addr) bool {
	return ip.IsLoopback() || s.host.Load().isOwn(ip)
}

// plainBootstrapName reports the names plain DNS answers while it is
// closed: the server name, <ClientID>.<serverName> and _dns.resolver.arpa
// (the bootstrap of encrypted DNS; all answered at step 6).
func plainBootstrapName(set *settings.All, qname string) bool {
	if qname == ddrName {
		return true
	}
	_, ok := settings.ServerNameMatch(set.DNS.Encrypted.ServerName, qname)
	return ok
}

// sniClientID returns the ClientID of a DoT connection: the first label of
// an SNI <ClientID>.<serverName> with a valid label; "" otherwise (no
// SNI, the server name itself, another name).
func (s *Server) sniClientID(w dns.ResponseWriter) string {
	cs, ok := w.(dns.ConnectionStater)
	if !ok {
		return ""
	}
	st := cs.ConnectionState()
	if st == nil {
		return ""
	}
	return ClientIDFromSNI(s.d.Settings.Get().DNS.Encrypted.ServerName, st.ServerName)
}

// ClientIDFromSNI returns the ClientID of a DoT server name indication
// (see sniClientID).
func ClientIDFromSNI(serverName, sni string) string {
	id, ok := settings.ServerNameMatch(serverName, strings.TrimSuffix(strings.ToLower(sni), "."))
	if !ok {
		return ""
	}
	return id
}

// blockedClientID returns the clientid: entry of dns.blockedClients that
// matches a ClientID (step 2a; no safety net: a ClientID is chosen by the
// device).
func (s *Server) blockedClientID(id string) (string, bool) {
	l := s.lists.Load()
	if !l.blocked.HasClientIDs() {
		return "", false
	}
	return l.blocked.MatchClientID(id)
}

// applyClientID lets a ClientID decide the identity (end of step 5): only
// when the usual identification found no configured client does a known
// ClientID make the query its client's (groups, name and flags; the
// address and MAC stay the source's). A ClientID never changes the client
// of a device identified by its address or MAC.
func (s *Server) applyClientID(qc *qctx) {
	id := qc.clientID
	if id == "" || s.d.Clients == nil {
		return
	}
	known, ok := s.d.Clients.IdentifyDNSClientID(id)
	switch {
	case !ok:
		qc.note("ClientID " + id + ": no client has it; identified by the source")
	case qc.id.ClientID != 0 && known.ClientID != qc.id.ClientID:
		qc.note(fmt.Sprintf("ClientID %s ignored: the source is client %s", id, qc.id.Name))
	case qc.id.ClientID != 0:
		qc.note(fmt.Sprintf("ClientID %s: client %s", id, qc.id.Name))
	default:
		merged := *known
		merged.IP, merged.MAC = qc.id.IP, qc.id.MAC
		qc.id = &merged
		qc.note(fmt.Sprintf("ClientID %s: client %s", id, known.Name))
	}
}

// --- DDR (RFC 9462) ---

// DDR status reasons (GET /dns/encrypted ddr.reason).
const (
	DDROff          = "off"            // neither DoT nor DoH is enabled
	DDRNoServerName = "no-server-name" // dns.encrypted.serverName is not set
	DDRNotServing   = "not-serving"    // DoT and DoH are not serving
	DDRNoIPAddress  = "no-ip-address"  // the certificate has none of the answer addresses
)

// DDRReason says why nothing is announced to a client whose hint
// addresses are hints ("" when something is): the first matching reason.
func DDRReason(e settings.EncryptedDNS, st *EncryptedState, hints []netip.Addr) string {
	switch {
	case !e.Enabled():
		return DDROff
	case e.ServerName == "":
		return DDRNoServerName
	case !(e.DoT && st.DoT) && !(e.DoH && st.DoH):
		return DDRNotServing
	}
	for _, ip := range hints {
		if slices.Contains(st.LeafIPs, ip) {
			return ""
		}
	}
	return DDRNoIPAddress
}

// ddrAnswer answers _dns.resolver.arpa SVCB with the designated resolvers:
// while DoH serves one record per distinct port of the DoH and HTTPS
// listeners (alpn=h2, dohpath), while DoT serves one per DoT port (alpn=dot),
// DoH first, each with the hints of this server's names for the client.
// Nothing is announced (NODATA) unless the served certificate contains
// one of the hint addresses (verified discovery: the client checks that
// the resolver's certificate has the address it asked).
func (s *Server) ddrAnswer(qc *qctx) result {
	v4, v6 := s.serverNameAddrs(qc)
	st := s.encState()
	e := qc.set.DNS.Encrypted
	if why := DDRReason(e, st, slices.Concat(v4, v6)); why != "" {
		qc.note("DDR: not announced (" + why + ")")
		return s.negative(qc, dns.RcodeSuccess, StatusSpecial, "resolver.arpa")
	}
	target := dns.Fqdn(e.ServerName)
	hints := func(kv []dns.SVCBKeyValue) []dns.SVCBKeyValue {
		if len(v4) > 0 {
			kv = append(kv, &dns.SVCBIPv4Hint{Hint: ipSlices(v4)})
		}
		if len(v6) > 0 {
			kv = append(kv, &dns.SVCBIPv6Hint{Hint: ipSlices(v6)})
		}
		return kv
	}
	var rrs []dns.RR
	add := func(kv []dns.SVCBKeyValue) {
		rrs = append(rrs, &dns.SVCB{Hdr: rrHeader(qc.q.Name, dns.TypeSVCB, specialTTL), Priority: 1, Target: target, Value: kv})
	}
	if e.DoH && st.DoH {
		for _, p := range st.DoHPorts {
			kv := []dns.SVCBKeyValue{&dns.SVCBAlpn{Alpn: []string{"h2"}}, &dns.SVCBPort{Port: p}}
			add(append(hints(kv), &dns.SVCBDoHPath{Template: "/dns-query{?dns}"}))
		}
	}
	if e.DoT && st.DoT {
		for _, p := range st.DoTPorts {
			add(hints([]dns.SVCBKeyValue{&dns.SVCBAlpn{Alpn: []string{"dot"}}, &dns.SVCBPort{Port: p}}))
		}
	}
	if len(rrs) == 0 {
		qc.note("DDR: not announced (" + DDRNotServing + ")")
		return s.negative(qc, dns.RcodeSuccess, StatusSpecial, "resolver.arpa")
	}
	qc.note(fmt.Sprintf("DDR: %d designated resolvers", len(rrs)))
	m := newReply(qc.req)
	m.Authoritative = true
	m.Answer = rrs
	return result{msg: m, status: StatusSpecial}
}

func ipSlices(ips []netip.Addr) []net.IP {
	out := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		out = append(out, net.IP(ip.AsSlice()))
	}
	return out
}

// --- DoT connections ---

// newDoTServer serves DoT on a TLS listener: the queries of a connection
// are read and answered one after another, in order (pipelining without
// reordering; at most one in flight, queries sent ahead wait in the
// socket); the handshake and the first message within 10 s, a started
// message within the idle time of 30 s, every reply within 10 s, 1024
// queries per connection.
func newDoTServer(ln net.Listener, h dns.Handler) *dns.Server {
	return &dns.Server{Listener: dotDeadlines(ln), Handler: h, MaxTCPQueries: maxDoTQueries,
		ReadTimeout: dotTimeout, IdleTimeout: func() time.Duration { return dotIdle }}
}

// dotDeadlines wraps a DoT (TLS) listener: every connection gets a 10 s
// deadline for the handshake, which runs in the connection's goroutine at
// the first read (never in Accept), and every reply is written within
// 10 s. miekg sets the read deadlines of the messages.
func dotDeadlines(ln net.Listener) net.Listener { return &dotListener{Listener: ln} }

type dotListener struct{ net.Listener }

func (l *dotListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(dotTimeout))
	return &dotConn{Conn: c}, nil
}

// dotConn bounds every write and exposes the TLS state (the SNI of
// ClientIDs) to the DNS handler.
type dotConn struct{ net.Conn }

func (c *dotConn) Write(b []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(dotTimeout))
	return c.Conn.Write(b)
}

// ConnectionState returns the TLS state of the connection (empty for a
// connection that is not TLS).
func (c *dotConn) ConnectionState() tls.ConnectionState {
	if tc, ok := c.Conn.(interface{ ConnectionState() tls.ConnectionState }); ok {
		return tc.ConnectionState()
	}
	return tls.ConnectionState{}
}
