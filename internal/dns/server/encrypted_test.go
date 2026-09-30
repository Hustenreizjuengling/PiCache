package dnsserver

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// encEnv is a test server with encrypted DNS on (server name
// dns.lan) and a settable encrypted-DNS snapshot.
func encEnv(t *testing.T, mutate func(*settings.All)) (*testEnv, *atomic.Pointer[EncryptedState]) {
	t.Helper()
	e := newEnv(t, func(a *settings.All) {
		a.DNS.Encrypted = settings.EncryptedDNS{DoT: true, DoH: true, ServerName: "dns.lan"}
		if mutate != nil {
			mutate(a)
		}
	})
	var st atomic.Pointer[EncryptedState]
	st.Store(&EncryptedState{DoT: true, DoH: true, DoTPorts: []uint16{853}, DoHPorts: []uint16{4443, 8443},
		LeafIPs: []netip.Addr{testCacheIP}})
	e.srv.d.Encrypted = st.Load
	return e, &st
}

// serveVia runs m through the pipeline as a query of c with a fake writer.
func (e *testEnv) serveVia(c queryConn, m *dns.Msg) *fakeWriter {
	w := &fakeWriter{remote: &net.TCPAddr{IP: net.IP(c.source.AsSlice()), Port: 40000}}
	if c.proto == ProtoUDP {
		w.remote = &net.UDPAddr{IP: net.IP(c.source.AsSlice()), Port: 40000}
	}
	e.srv.serve(context.Background(), w, m, c)
	return w
}

func question(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func ede(m *dns.Msg) *dns.EDNS0_EDE {
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if e, ok := o.(*dns.EDNS0_EDE); ok {
				return e
			}
		}
	}
	return nil
}

// --- plain-DNS switch ---

func TestPlainDNSGate(t *testing.T) {
	e, st := encEnv(t, func(a *settings.All) { a.DNS.PlainDNS = false })
	lan := netip.MustParseAddr("192.168.1.50")

	// Other devices get REFUSED with EDE 18 over UDP and TCP; it is logged,
	// not recorded as seen.
	for _, proto := range []string{ProtoUDP, ProtoTCP} {
		m := question("gated.example", dns.TypeA)
		m.SetEdns0(1232, false)
		w := e.serveVia(queryConn{proto: proto, source: lan}, m)
		if len(w.msgs) != 1 || w.msgs[0].Rcode != dns.RcodeRefused || w.closed {
			t.Fatalf("%s: %+v closed=%v", proto, w.msgs, w.closed)
		}
		x := ede(w.msgs[0])
		if x == nil || x.InfoCode != dns.ExtendedErrorCodeProhibited || x.ExtraText != "plain DNS is disabled on this server; use DoT or DoH" {
			t.Fatalf("%s: EDE %+v", proto, x)
		}
		if len(w.msgs[0].Answer) != 0 {
			t.Fatal("answer in a refusal")
		}
	}
	ev := e.logs.waitEvent(t, "gated.example", 0)
	if ev.Status != StatusRefused || ev.Reason != ReasonPlainDNSOff || ev.RCode != "REFUSED" {
		t.Fatalf("logged %+v", ev)
	}
	if e.cl.seen[lan] != 0 || e.cl.transient[lan] != 0 || len(e.up.callsFor("gated.example")) != 0 {
		t.Fatal("a refused query was recorded or forwarded")
	}
	// Without EDNS: REFUSED without an OPT record.
	if w := e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question("gated.example", dns.TypeA)); w.msgs[0].IsEdns0() != nil {
		t.Fatal("OPT for a client without EDNS")
	}

	// The names that bootstrap encrypted DNS are answered.
	for _, q := range []struct {
		name  string
		qtype uint16
	}{{"dns.lan", dns.TypeA}, {"phone.dns.lan", dns.TypeAAAA}, {"dns.lan", dns.TypeTXT}, {"_dns.resolver.arpa", dns.TypeSVCB}} {
		w := e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question(q.name, q.qtype))
		if len(w.msgs) != 1 || w.msgs[0].Rcode != dns.RcodeSuccess {
			t.Fatalf("%s: %+v", q.name, w.msgs)
		}
	}

	// This machine is exempt (loopback and own addresses).
	for _, src := range []netip.Addr{netip.MustParseAddr("127.0.0.1"), testCacheIP} {
		w := e.serveVia(queryConn{proto: ProtoUDP, source: src}, question("host.example", dns.TypeA))
		if len(w.msgs) != 1 || w.msgs[0].Rcode != dns.RcodeSuccess || len(w.msgs[0].Answer) == 0 {
			t.Fatalf("%v: %+v", src, w.msgs)
		}
	}

	// DoT and DoH are never gated.
	for _, proto := range []string{ProtoDoT, ProtoDoH} {
		w := e.serveVia(queryConn{proto: proto, source: lan}, question("enc.example", dns.TypeA))
		if len(w.msgs) != 1 || len(w.msgs[0].Answer) == 0 {
			t.Fatalf("%s: %+v", proto, w.msgs)
		}
	}

	// Fail open: nothing serving → plain DNS answers everyone.
	st.Store(&EncryptedState{})
	w := e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question("open.example", dns.TypeA))
	if w.msgs[0].Rcode != dns.RcodeSuccess || len(w.msgs[0].Answer) == 0 {
		t.Fatalf("fail open: %+v", w.msgs)
	}
	// Plain DNS on: nothing is gated.
	st.Store(&EncryptedState{DoT: true})
	e.update(func(a *settings.All) { a.DNS.PlainDNS = true })
	if w := e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question("on.example", dns.TypeA)); w.msgs[0].Rcode != dns.RcodeSuccess {
		t.Fatalf("plain on: %+v", w.msgs)
	}
}

// --- step 6: the server name ---

func TestEncryptedServerNameAnswered(t *testing.T) {
	e, _ := encEnv(t, nil)
	e.flt.setCheck("dns.lan", listBlock("everything"))
	e.flt.setCheck("phone.dns.lan", listBlock("everything"))
	lan := netip.MustParseAddr("192.168.1.50")
	for _, name := range []string{"dns.lan", "phone.dns.lan", "PHONE.Dns.Lan"} {
		w := e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question(name, dns.TypeA))
		if got := answerIPs(w.msgs[0].Answer); !slices.Equal(got, []string{testCacheIP.String()}) {
			t.Fatalf("%s: %v", name, got)
		}
		w = e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question(name, dns.TypeMX))
		if len(w.msgs[0].Answer) != 0 || len(w.msgs[0].Ns) != 1 || w.msgs[0].Rcode != dns.RcodeSuccess {
			t.Fatalf("%s MX: %+v", name, w.msgs[0])
		}
	}
	if n := len(e.up.callsFor("dns.lan")) + len(e.up.callsFor("phone.dns.lan")); n != 0 {
		t.Fatalf("forwarded %d times", n)
	}
	// dns.serverNameAddresses wins.
	e.update(func(a *settings.All) { a.DNS.ServerNameAddresses.IPv4 = []string{"192.168.1.99"} })
	w := e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question("tv.dns.lan", dns.TypeA))
	if got := answerIPs(w.msgs[0].Answer); !slices.Equal(got, []string{"192.168.1.99"}) {
		t.Fatalf("serverNameAddresses: %v", got)
	}
	// Other names below the server name take the normal path.
	for _, name := range []string{"a.b.dns.lan", "-x.dns.lan"} {
		e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question(name, dns.TypeA))
	}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "dns.lan"}, lan)
	if err != nil || !slices.ContainsFunc(res.Steps, func(s string) bool { return strings.Contains(s, "this server's own name (dns.encrypted.serverName)") }) {
		t.Fatalf("lookup trace %v %v", res.Steps, err)
	}
}

// TestEncryptedServerNameBridge: in a bridge network the configured cache
// addresses answer the server name.
func TestEncryptedServerNameBridge(t *testing.T) {
	e, _ := encEnv(t, func(a *settings.All) { a.DownloadCache.CacheIPv4 = []string{"192.168.1.77"} })
	e.srv.cacheIPs.Store(&cacheIPState{bridge: true, v4: []netip.Addr{netip.MustParseAddr("192.168.1.77")}})
	w := e.serveVia(queryConn{proto: ProtoUDP, source: netip.MustParseAddr("192.168.1.50")}, question("dns.lan", dns.TypeA))
	if got := answerIPs(w.msgs[0].Answer); !slices.Equal(got, []string{"192.168.1.77"}) {
		t.Fatalf("bridge: %v", got)
	}
}

// --- DDR ---

func svcbs(m *dns.Msg) []*dns.SVCB {
	var out []*dns.SVCB
	for _, rr := range m.Answer {
		if s, ok := rr.(*dns.SVCB); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestDDR(t *testing.T) {
	e, st := encEnv(t, nil)
	lan := netip.MustParseAddr("192.168.1.50")
	ask := func(qtype uint16) *dns.Msg {
		return e.serveVia(queryConn{proto: ProtoUDP, source: lan}, question("_dns.resolver.arpa", qtype)).msgs[0]
	}
	recs := svcbs(ask(dns.TypeSVCB))
	if len(recs) != 3 {
		t.Fatalf("records %v", recs)
	}
	want := []string{
		`_dns.resolver.arpa.	60	IN	SVCB	1 dns.lan. alpn="h2" port="4443" ipv4hint="192.168.1.10" ipv6hint="fd00::10" dohpath="/dns-query{?dns}"`,
		`_dns.resolver.arpa.	60	IN	SVCB	1 dns.lan. alpn="h2" port="8443" ipv4hint="192.168.1.10" ipv6hint="fd00::10" dohpath="/dns-query{?dns}"`,
		`_dns.resolver.arpa.	60	IN	SVCB	1 dns.lan. alpn="dot" port="853" ipv4hint="192.168.1.10" ipv6hint="fd00::10"`,
	}
	for i, r := range recs {
		if r.String() != want[i] {
			t.Fatalf("record %d:\n%s\nwant\n%s", i, r, want[i])
		}
	}
	// Other types and names below resolver.arpa: NODATA.
	for _, m := range []*dns.Msg{ask(dns.TypeA), ask(dns.TypeHTTPS)} {
		if len(m.Answer) != 0 || m.Rcode != dns.RcodeSuccess {
			t.Fatalf("other type: %+v", m)
		}
	}
	// Only DoT serving: only the DoT record.
	st.Store(&EncryptedState{DoT: true, DoTPorts: []uint16{853, 8853}, LeafIPs: []netip.Addr{testCacheIP}})
	if recs := svcbs(ask(dns.TypeSVCB)); len(recs) != 2 || !strings.Contains(recs[1].String(), `port="8853"`) {
		t.Fatalf("DoT only: %v", recs)
	}
	// The certificate has no hint address, nothing serves: NODATA.
	st.Store(&EncryptedState{DoT: true, DoTPorts: []uint16{853}, LeafIPs: []netip.Addr{netip.MustParseAddr("10.9.9.9")}})
	if m := ask(dns.TypeSVCB); len(m.Answer) != 0 || m.Rcode != dns.RcodeSuccess {
		t.Fatalf("no IP SAN: %+v", m)
	}
	st.Store(&EncryptedState{})
	if m := ask(dns.TypeSVCB); len(m.Answer) != 0 {
		t.Fatalf("nothing serving: %+v", m)
	}
	res, _ := e.srv.Lookup(context.Background(), LookupRequest{Name: "_dns.resolver.arpa", Type: "SVCB"}, lan)
	if !slices.Contains(res.Steps, "DDR: not announced (not-serving)") {
		t.Fatalf("trace %v", res.Steps)
	}

	ip := []netip.Addr{testCacheIP}
	for _, tc := range []struct {
		e    settings.EncryptedDNS
		st   EncryptedState
		want string
	}{
		{settings.EncryptedDNS{}, EncryptedState{DoT: true, LeafIPs: ip}, DDROff},
		{settings.EncryptedDNS{DoT: true}, EncryptedState{DoT: true, LeafIPs: ip}, DDRNoServerName},
		{settings.EncryptedDNS{DoT: true, ServerName: "x.lan"}, EncryptedState{DoH: true, LeafIPs: ip}, DDRNotServing},
		{settings.EncryptedDNS{DoH: true, ServerName: "x.lan"}, EncryptedState{DoH: true}, DDRNoIPAddress},
		{settings.EncryptedDNS{DoH: true, ServerName: "x.lan"}, EncryptedState{DoH: true, LeafIPs: ip}, ""},
	} {
		if got := DDRReason(tc.e, &tc.st, ip); got != tc.want {
			t.Fatalf("%+v: %q, want %q", tc, got, tc.want)
		}
	}
}

// --- ClientIDs ---

func TestClientIDIdentification(t *testing.T) {
	e, _ := encEnv(t, nil)
	const kids, adults = 5, 6
	tablet := &clients.Identity{IP: netip.MustParseAddr("192.168.1.20"), ClientID: 1, Name: "Tablet", MAC: "02:00:00:00:00:20", GroupIDs: []int64{kids}}
	e.cl.set(tablet)
	e.cl.byID = map[string]*clients.Identity{
		"dad":   {ClientID: 2, Name: "Dad", GroupIDs: []int64{adults}, DownloadCacheBypass: true},
		"quiet": {ClientID: 3, Name: "Quiet", GroupIDs: []int64{adults}, IgnoreLogs: true, IgnoreStats: true},
	}
	e.flt.setCheck("kids.example", listBlock("kids list"))
	e.flt.setScope("kids.example", kids)

	for _, proto := range []string{ProtoDoT, ProtoDoH} {
		// The MAC-identified tablet keeps Kids whatever ClientID it sends.
		for _, id := range []string{"unknown", "dad"} {
			w := e.serveVia(queryConn{proto: proto, source: tablet.IP, clientID: id}, question("kids.example", dns.TypeA))
			if len(w.msgs) != 1 || len(answerIPs(w.msgs[0].Answer)) != 1 || answerIPs(w.msgs[0].Answer)[0] != "0.0.0.0" {
				t.Fatalf("%s %s: not blocked for Kids: %+v", proto, id, w.msgs)
			}
		}
		// An unidentified source with a known ClientID becomes that client.
		stranger := netip.MustParseAddr("192.168.1.60")
		e.serveVia(queryConn{proto: proto, source: stranger, clientID: "dad"}, question("adult-"+proto+".example", dns.TypeA))
		if got := e.flt.lastChecked("adult-" + proto + ".example"); !slices.Equal(got, []int64{adults}) {
			t.Fatalf("%s: groups %v", proto, got)
		}
		ev := e.logs.waitEvent(t, "adult-"+proto+".example", 0)
		if ev.ClientIP != stranger.String() || ev.ClientName != "Dad" || ev.DNSClientID != "dad" || ev.Protocol != proto {
			t.Fatalf("%s: logged %+v", proto, ev)
		}
		if e.cl.seenIDs["dad"] != stranger {
			t.Fatalf("%s: ClientID not recorded: %v", proto, e.cl.seenIDs)
		}
	}
	// A ClientID of a client with ignoreLogs: not recorded, not logged.
	quiet := netip.MustParseAddr("192.168.1.61")
	e.serveVia(queryConn{proto: ProtoDoT, source: quiet, clientID: "quiet"}, question("quiet.example", dns.TypeA))
	if _, ok := e.cl.seenIDs["quiet"]; ok || e.cl.seen[quiet] != 0 {
		t.Fatal("ignoreLogs identity recorded")
	}

	// The traces of Lookup.
	for _, tc := range []struct {
		ip, id, want string
	}{
		{"192.168.1.20", "dad", "ClientID dad ignored: the source is client Tablet"},
		{"192.168.1.62", "dad", "ClientID dad: client Dad"},
		{"192.168.1.62", "nobody", "ClientID nobody: no client has it; identified by the source"},
	} {
		res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "x.example", ClientIP: tc.ip, DNSClientID: strings.ToUpper(tc.id)}, netip.Addr{})
		if err != nil || !slices.Contains(res.Steps, tc.want) {
			t.Fatalf("%+v: %v %v", tc, res.Steps, err)
		}
	}
	if _, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "x.example", ClientIP: "192.168.1.1", DNSClientID: "a_b"}, netip.Addr{}); err == nil ||
		!strings.Contains(err.Error(), "dnsClientId: must be a ClientID") {
		t.Fatalf("invalid ClientID: %v", err)
	}
}

// TestClientIDBlocked: a clientid: entry drops DoT and DoH queries that
// carry the ClientID; a MAC-blocked device stays blocked whatever
// ClientID it sends.
func TestClientIDBlocked(t *testing.T) {
	e, _ := encEnv(t, func(a *settings.All) {
		a.DNS.BlockedClients = []string{"clientid:banned", "02:00:00:00:00:30"}
	})
	blockedMAC := &clients.Identity{IP: netip.MustParseAddr("192.168.1.30"), MAC: "02:00:00:00:00:30", GroupIDs: []int64{1}}
	e.cl.set(blockedMAC)
	e.cl.byID = map[string]*clients.Identity{"good": {ClientID: 9, Name: "Good", GroupIDs: []int64{1}}}
	for _, proto := range []string{ProtoDoT, ProtoDoH} {
		before := e.srv.blockedClients.Load()
		w := e.serveVia(queryConn{proto: proto, source: netip.MustParseAddr("192.168.1.50"), clientID: "banned"}, question("b.example", dns.TypeA))
		if len(w.msgs) != 0 || !w.closed {
			t.Fatalf("%s: blocked ClientID answered", proto)
		}
		w = e.serveVia(queryConn{proto: proto, source: blockedMAC.IP, clientID: "good"}, question("b.example", dns.TypeA))
		if len(w.msgs) != 0 || !w.closed {
			t.Fatalf("%s: MAC-blocked device answered with a ClientID", proto)
		}
		if e.srv.blockedClients.Load() != before+2 {
			t.Fatalf("%s: blocked count", proto)
		}
	}
	if e.logs.count("b.example") != 0 {
		t.Fatal("blocked queries logged")
	}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "b.example", ClientIP: "192.168.1.50", DNSClientID: "banned"}, netip.Addr{})
	if err != nil || res.Status != StatusDropped || !slices.Contains(res.Steps, "blocked client: clientid:banned") {
		t.Fatalf("lookup %+v %v", res, err)
	}
}

// TestPadding: DoT and DoH replies to a query with a Padding option are
// padded to 468 bytes; never over UDP or TCP.
func TestPadding(t *testing.T) {
	e, _ := encEnv(t, nil)
	src := netip.MustParseAddr("192.168.1.50")
	for _, tc := range []struct {
		proto string
		pad   bool
	}{{ProtoDoT, true}, {ProtoDoH, true}, {ProtoUDP, false}, {ProtoTCP, false}} {
		m := question("pad.example", dns.TypeA)
		m.SetEdns0(1232, false)
		m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_PADDING{Padding: make([]byte, 20)})
		w := e.serveVia(queryConn{proto: tc.proto, source: src}, m)
		n := w.msgs[0].Len()
		padded := n%paddingBlock == 0
		if padded != tc.pad {
			t.Fatalf("%s: length %d", tc.proto, n)
		}
	}
	// Without the option: not padded.
	m := question("nopad.example", dns.TypeA)
	m.SetEdns0(1232, false)
	if w := e.serveVia(queryConn{proto: ProtoDoT, source: src}, m); hasPadding(w.msgs[0].IsEdns0()) {
		t.Fatal("padded without the option")
	}
}

func TestClientIDFromSNI(t *testing.T) {
	for sni, want := range map[string]string{
		"": "", "dns.lan": "", "phone.dns.lan": "phone", "PHONE.dns.lan.": "phone",
		"other.example": "", "-bad.dns.lan": "", "a.b.dns.lan": "", "192.168.1.10": "",
	} {
		if got := ClientIDFromSNI("dns.lan", sni); got != want {
			t.Fatalf("%q: %q, want %q", sni, got, want)
		}
	}
	if got := ClientIDFromSNI("", "phone.dns.lan"); got != "" {
		t.Fatal("ClientID without a server name")
	}
}

// --- DoT listeners ---

// testTLS returns a server configuration for dns.lan,
// *.dns.lan and 127.0.0.1 with ALPN dot, and a pool that trusts it.
func testTLS(t *testing.T, minVersion uint16) (*tls.Config, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "dns.lan"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true, DNSNames: []string{"dns.lan", "*.dns.lan"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Config{MinVersion: minVersion, NextProtos: []string{"dot"},
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}}}, pool
}

// serveDoT starts the server with a DoT listener on 127.0.0.1.
func (e *testEnv) serveDoT(conf *tls.Config) string {
	e.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		e.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(ctx, nil, nil, []net.Listener{tls.NewListener(ln, conf)}) }()
	e.t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

func writeFrame(t *testing.T, c net.Conn, m *dns.Msg) {
	t.Helper()
	b, _ := m.Pack()
	out := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(out, uint16(len(b)))
	copy(out[2:], b)
	if _, err := c.Write(out); err != nil {
		t.Fatal(err)
	}
}

func readFrame(c net.Conn) (*dns.Msg, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(c, b); err != nil {
		return nil, err
	}
	m := new(dns.Msg)
	return m, m.Unpack(b)
}

// TestDoTPipelinedInOrder: queries sent ahead on one connection are
// answered one after another, in order; the SNI carries the ClientID.
func TestDoTPipelinedInOrder(t *testing.T) {
	e, _ := encEnv(t, nil)
	conf, pool := testTLS(t, tls.VersionTLS12)
	addr := e.serveDoT(conf)
	c, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "kid.dns.lan", NextProtos: []string{"dot"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.ConnectionState().NegotiatedProtocol != "dot" {
		t.Fatalf("ALPN %q", c.ConnectionState().NegotiatedProtocol)
	}
	for i := range 5 {
		m := question("p"+string(rune('a'+i))+".example", dns.TypeA)
		m.Id = uint16(100 + i)
		writeFrame(t, c, m)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := range 5 {
		r, err := readFrame(c)
		if err != nil || r.Id != uint16(100+i) {
			t.Fatalf("reply %d: %v %v", i, r, err)
		}
	}
	if ev := e.logs.waitEvent(t, "pa.example", 0); ev.Protocol != ProtoDoT || ev.DNSClientID != "kid" {
		t.Fatalf("logged %+v", ev)
	}
	// A reply is counted right after it was written: the client can read
	// the fifth one before it is counted.
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		dot, _ := e.srv.EncryptedQueries()
		if dot == 5 {
			break
		}
		if dot > 5 || time.Now().After(deadline) {
			t.Fatalf("DoT queries %d", dot)
		}
	}

	// A client that offers ALPN without dot fails the handshake.
	if _, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "dns.lan", NextProtos: []string{"h2"}}); err == nil {
		t.Fatal("handshake with ALPN h2 succeeded")
	}
	// Any SNI (or none) completes the handshake.
	for _, sni := range []string{"", "other.example", "-bad.dns.lan"} {
		c, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: sni, InsecureSkipVerify: sni != "dns.lan"})
		if err != nil {
			t.Fatalf("SNI %q: %v", sni, err)
		}
		c.Close()
	}

	// DoT switched off: the next query closes the connection.
	e.update(func(a *settings.All) { a.DNS.Encrypted.DoT = false })
	writeFrame(t, c, question("off.example", dns.TypeA))
	if _, err := readFrame(c); err == nil {
		t.Fatal("answered while DoT is off")
	}
}

// TestDoTMinVersion: web.tlsMinVersion 1.3 refuses a TLS 1.2 client at
// once (the configuration of the listener).
func TestDoTMinVersion(t *testing.T) {
	e, _ := encEnv(t, nil)
	conf, pool := testTLS(t, tls.VersionTLS13)
	addr := e.serveDoT(conf)
	if _, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "dns.lan", MaxVersion: tls.VersionTLS12}); err == nil {
		t.Fatal("TLS 1.2 accepted")
	}
}

// pipeListener hands out the server ends of net.Pipe connections (a
// listener inside a synctest bubble).
type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// lanConn is a pipe end with the address of a LAN client.
type lanConn struct{ net.Conn }

func (lanConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(192, 168, 1, 50), Port: 40000} }

func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 853} }

// TestDoTTimeouts: a handshake that never finishes ends after 10 s, an
// idle connection after 30 s (fake time).
func TestDoTTimeouts(t *testing.T) {
	e, _ := encEnv(t, nil)
	conf, _ := testTLS(t, tls.VersionTLS12)
	synctest.Test(t, func(t *testing.T) {
		pl := &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
		srv := newDoTServer(tls.NewListener(pl, conf), &dnsHandler{s: e.srv, ctx: t.Context(), proto: ProtoDoT})
		go func() { _ = srv.ActivateAndServe() }()
		defer func() { _ = srv.Shutdown() }()
		dial := func() net.Conn {
			client, server := net.Pipe()
			pl.conns <- lanConn{server}
			return client
		}

		// No ClientHello: closed after 10 s.
		raw := dial()
		closed := make(chan time.Time, 1)
		go func() { _, _ = raw.Read(make([]byte, 1)); closed <- time.Now() }()
		start := time.Now()
		got := <-closed
		if d := got.Sub(start); d < dotTimeout || d > dotTimeout+time.Second {
			t.Fatalf("handshake ended after %v", d)
		}

		// Handshake and one query, then idle: closed after 30 s.
		c := tls.Client(dial(), &tls.Config{InsecureSkipVerify: true}) // the fake clock predates the certificate
		writeFrame(t, c, question("idle.example", dns.TypeA))
		if _, err := readFrame(c); err != nil {
			t.Fatal(err)
		}
		start = time.Now()
		if _, err := readFrame(c); err == nil {
			t.Fatal("idle connection answered")
		}
		if d := time.Since(start); d < dotIdle || d > dotIdle+time.Second {
			t.Fatalf("idle connection ended after %v", d)
		}
	})
}
