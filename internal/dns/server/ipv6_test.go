package dnsserver

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Own addresses: deprecated and tentative addresses are never answered,
// temporary ones only when no stable address exists, ULA before global; a
// temporary primary address is replaced by a stable one of its /64.
func TestHostInfoAddressSelection(t *testing.T) {
	p := netip.MustParsePrefix
	a := func(iface, prefix string) netutil.HostAddr {
		return netutil.HostAddr{Iface: iface, Prefix: p(prefix), Up: true}
	}
	temp := func(ha netutil.HostAddr) netutil.HostAddr { ha.Temporary = true; return ha }
	depr := func(ha netutil.HostAddr) netutil.HostAddr { ha.Deprecated = true; return ha }
	tent := func(ha netutil.HostAddr) netutil.HostAddr { ha.Tentative = true; return ha }
	h := newHostInfo([]netutil.HostAddr{
		{Iface: "lo", Prefix: p("127.0.0.1/8"), Up: true, Loopback: true}, {Iface: "lo", Prefix: p("::1/128"), Up: true, Loopback: true},
		a("eth0", "192.168.1.10/24"),
		temp(a("eth0", "2001:db8::abcd/64")), // listed first by the kernel
		a("eth0", "2001:db8::10/64"), a("eth0", "fd00::10/64"),
		depr(a("eth0", "2001:db8:0:1::10/64")), tent(a("eth0", "fd00::99/64")), a("eth0", "fe80::10/64"),
		temp(a("wlan0", "2001:db8:2::abcd/64")),
		{Iface: "eth1", Prefix: p("10.0.0.5/24")}, // down
	}, netip.MustParseAddr("192.168.1.10"), netip.MustParseAddr("2001:db8::abcd"), nil)

	for _, tc := range []struct {
		client string
		v6     []string
	}{
		{"fd00::77", []string{"fd00::10"}},                     // the tentative ULA is not answered
		{"2001:db8::77", []string{"2001:db8::10"}},             // stable before temporary
		{"192.168.1.50", []string{"fd00::10", "2001:db8::10"}}, // ULA first, no deprecated address
		{"2001:db8:0:1::77", []string{"2001:db8::10"}},         // the deprecated prefix: primary
		{"2001:db8:2::5", []string{"2001:db8:2::abcd"}},        // only a temporary address exists
		{"2001:db8:ffff::1", []string{"2001:db8::10"}},         // routed: the stable primary
		{"::1", []string{"::1"}},
	} {
		if got := addrStrings(h.addrsFor(netip.MustParseAddr(tc.client), true)); !slices.Equal(got, tc.v6) {
			t.Errorf("client %s AAAA = %v, want %v", tc.client, got, tc.v6)
		}
	}
	if h.primary6.String() != "2001:db8::10" {
		t.Errorf("primary6 %s: want the stable address of the temporary one's /64", h.primary6)
	}
	for _, s := range []string{"2001:db8:0:1::10", "fd00::99", "2001:db8::abcd"} {
		if !h.isOwn(netip.MustParseAddr(s)) {
			t.Errorf("%s is still this machine's address (loop protection)", s)
		}
	}
	if h.isOwn(netip.MustParseAddr("10.0.0.5")) {
		t.Error("addresses of interfaces that are down are not used")
	}
}

// routerEnv is a server with the router resolver on "auto" and a router
// whose addresses are 192.168.1.1 (optional), fe80::1 (the IPv6 gateway),
// fd00::1, fd00::2 and 2001:db8::1, all with one MAC.
func routerEnv(t *testing.T, gw4 bool, routerULA bool) *testEnv {
	t.Helper()
	e := newEnv(t, func(a *settings.All) {
		a.DNS.RouterResolver = "auto"
		a.DNS.RateLimitQPS, a.DNS.RateLimitBurst = 1, 1
	})
	e.up.probe = true
	e.srv.env.gateway = func() (netip.Addr, error) {
		if gw4 {
			return netip.MustParseAddr("192.168.1.1"), nil
		}
		return netip.Addr{}, errors.New("no IPv4 default route")
	}
	e.srv.env.gateway6 = func() (netip.Addr, error) { return netip.MustParseAddr("fe80::1%eth0"), nil }
	nbs := []clients.Neighbour{
		{IP: netip.MustParseAddr("fe80::1"), MAC: "3c:00:00:00:00:01"},
		{IP: netip.MustParseAddr("192.168.1.50"), MAC: "aa:00:00:00:00:50"},
	}
	if gw4 {
		nbs = append(nbs, clients.Neighbour{IP: netip.MustParseAddr("192.168.1.1"), MAC: "3c:00:00:00:00:01"})
	}
	if routerULA {
		for _, s := range []string{"2001:db8::1", "fd00::2", "fd00::1"} {
			nbs = append(nbs, clients.Neighbour{IP: netip.MustParseAddr(s), MAC: "3c:00:00:00:00:01"})
		}
	}
	e.srv.env.neighbours = func(context.Context) ([]clients.Neighbour, error) { return nbs, nil }
	e.srv.detectRouter(context.Background())
	return e
}

// Without an IPv4 default gateway the router is reached over IPv6: at a
// ULA/global address from the neighbour table, else at the link-local
// gateway with its zone.
func TestRouterOverIPv6(t *testing.T) {
	e := routerEnv(t, false, true)
	if st := e.srv.Router(); st.Mode != "auto" || st.Address != "fd00::1" || !st.Answers {
		t.Fatalf("router %+v, want the router's smallest ULA", st)
	}
	w := udpFrom("fd00::99")
	e.handle(w, "printer.lan", dns.TypeA)
	if c := e.up.callsFor("printer.lan"); len(c) != 1 || !slices.Equal(c[0].via, []string{"[fd00::1]:53"}) {
		t.Fatalf("local domain via %v", c)
	}
	want := []string{"2001:db8::1", "fd00::1", "fd00::2", "fe80::1"}
	if got := addrStrings(e.srv.routerAddrs()); !slices.Equal(got, want) {
		t.Errorf("router addresses %v, want %v", got, want)
	}

	e = routerEnv(t, false, false)
	if st := e.srv.Router(); st.Address != "fe80::1%eth0" {
		t.Fatalf("router %+v, want the link-local gateway with its zone", st)
	}
	e.handle(udpFrom("fd00::99"), "printer.lan", dns.TypeA)
	if c := e.up.callsFor("printer.lan"); len(c) != 1 || !slices.Equal(c[0].via, []string{"[fe80::1%eth0]:53"}) {
		t.Fatalf("local domain via %v", c)
	}
	if _, err := settings.ParseUpstream("[fe80::1%eth0]:53"); err != nil {
		t.Errorf("the router upstream must parse: %v", err)
	}

	// With an IPv4 gateway the router is used over IPv4; its IPv6
	// addresses still belong to it.
	e = routerEnv(t, true, true)
	if st := e.srv.Router(); st.Address != "192.168.1.1" {
		t.Fatalf("router %+v", st)
	}
	if got := addrStrings(e.srv.routerAddrs()); !slices.Equal(got, []string{"192.168.1.1", "2001:db8::1", "fd00::1", "fd00::2", "fe80::1"}) {
		t.Errorf("router addresses %v", got)
	}
}

// The loop guard and the rate-limit exemption cover every address of the
// router, not only the one PiCache forwards to.
func TestRouterLoopGuardAndExemptionForAllAddresses(t *testing.T) {
	e := routerEnv(t, true, true)
	for _, from := range []string{"192.168.1.1", "fd00::2", "fe80::1"} {
		w := udpFrom(from)
		e.handle(w, "nas.lan", dns.TypeA)
		if len(w.msgs) != 1 || w.msgs[0].Rcode != dns.RcodeServerFailure {
			t.Errorf("query from the router's %s: %v, want SERVFAIL (loop guard)", from, w.msgs)
		}
	}
	if c := e.up.callsFor("nas.lan"); len(c) != 0 {
		t.Errorf("the loop guard must not forward: %v", c)
	}
	for _, from := range []string{"fd00::2", "fe80::1", "192.168.1.1"} {
		for range 5 {
			w := udpFrom(from)
			e.handle(w, "example.com", dns.TypeA)
			if len(w.msgs) != 1 {
				t.Fatalf("the router's %s was rate limited", from)
			}
		}
	}
	w1, w2 := udpFrom("fd00::99"), udpFrom("fd00::99")
	e.handle(w1, "a.example", dns.TypeA)
	e.handle(w2, "b.example", dns.TypeA)
	if len(w1.msgs) != 1 || len(w2.msgs) != 0 {
		t.Errorf("another device must be limited: %d / %d replies", len(w1.msgs), len(w2.msgs))
	}
}

// dns.disableAAAA: forwarded AAAA queries get NODATA with the SOA (status
// special, reason aaaa-disabled); local AAAA records and this server's
// names are answered; ipv6hint is removed from HTTPS answers.
func TestDisableAAAA(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.DisableAAAA = true }).serve()
	e.addRecord("nas.lan", "AAAA", "fd00::5")
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		m := upAnswer(req)
		if req.Question[0].Qtype == dns.TypeHTTPS {
			h := &dns.HTTPS{SVCB: dns.SVCB{Hdr: rrHeader(req.Question[0].Name, dns.TypeHTTPS, 300), Priority: 1, Target: "."}}
			h.Value = []dns.SVCBKeyValue{
				&dns.SVCBAlpn{Alpn: []string{"h2"}},
				&dns.SVCBIPv4Hint{Hint: []net.IP{net.ParseIP("198.51.100.7").To4()}},
				&dns.SVCBIPv6Hint{Hint: []net.IP{net.ParseIP("2001:db8::7")}},
			}
			m.Answer = []dns.RR{h}
			m.AuthenticatedData = true
		}
		return m, upstream.Info{Upstream: "fake-upstream"}, nil
	})

	r := e.query("udp", "www.example.com", dns.TypeAAAA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 || r.Ns[0].Header().Rrtype != dns.TypeSOA {
		t.Fatalf("AAAA: %v", r)
	}
	if c := e.up.callsFor("www.example.com"); len(c) != 0 {
		t.Errorf("AAAA must not be forwarded: %v", c)
	}
	if ev := e.logs.waitEvent(t, "www.example.com", 0); ev.Status != StatusSpecial || ev.Reason != "aaaa-disabled" {
		t.Errorf("log %s / %s", ev.Status, ev.Reason)
	}
	if r := e.query("udp", "www.example.com", dns.TypeA); len(answerIPs(r.Answer)) != 1 {
		t.Errorf("A still answered: %v", r)
	}
	if r := e.query("udp", "nas.lan", dns.TypeAAAA); !slices.Equal(answerIPs(r.Answer), []string{"fd00::5"}) {
		t.Errorf("local AAAA: %v", r.Answer)
	}
	if r := e.query("udp", "picache.lan", dns.TypeAAAA); len(answerIPs(r.Answer)) == 0 {
		t.Errorf("server name AAAA: %v", r)
	}
	r = e.query("udp", "svc.example.com", dns.TypeHTTPS, withEDNS(1232, true))
	if len(r.Answer) != 1 {
		t.Fatalf("HTTPS: %v", r)
	}
	s := r.Answer[0].String()
	if strings.Contains(s, "ipv6hint") || !strings.Contains(s, "ipv4hint") || !strings.Contains(s, "alpn") || r.AuthenticatedData {
		t.Errorf("HTTPS answer %s (AD %v): ipv6hint must be removed, the rest kept, AD cleared", s, r.AuthenticatedData)
	}
}

// dns64Upstream answers like a network where v4only.example and its
// aliases have A records only.
func dns64Upstream(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
	q := req.Question[0]
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = true
	m.AuthenticatedData = true
	soa := &dns.SOA{Hdr: rrHeader("example.", dns.TypeSOA, 900), Ns: "ns.example.", Mbox: "h.example.", Minttl: 300}
	a := func(owner, ip string, ttl uint32) dns.RR {
		return &dns.A{Hdr: rrHeader(owner, dns.TypeA, ttl), A: net.ParseIP(ip).To4()}
	}
	cname := func(owner, target string) dns.RR {
		return &dns.CNAME{Hdr: rrHeader(owner, dns.TypeCNAME, 600), Target: target}
	}
	name := strings.ToLower(q.Name)
	switch {
	case q.Qtype == dns.TypePTR && name == "8.8.8.8.in-addr.arpa.":
		m.Answer = []dns.RR{&dns.PTR{Hdr: rrHeader(q.Name, dns.TypePTR, 300), Ptr: "dns.google."}}
	case q.Qtype == dns.TypeA:
		switch name {
		case "v4only.example.":
			m.Answer = []dns.RR{a(q.Name, "192.0.2.10", 3600), a(q.Name, "192.0.2.11", 60)}
		case "loop.example.":
			m.Answer = []dns.RR{a(q.Name, "127.0.0.1", 60), a(q.Name, "0.0.0.0", 60)}
		case "mixed.example.":
			m.Answer = []dns.RR{a(q.Name, "169.254.1.1", 60), a(q.Name, "198.51.100.20", 60)}
		case "dual.example.":
			m.Answer = []dns.RR{a(q.Name, "198.51.100.30", 60)}
		case "hop.example.":
			m.Answer = []dns.RR{cname(q.Name, "ads.tracker.example."), a("ads.tracker.example.", "198.51.100.40", 60)}
		case "nx.example.":
			m.Rcode = dns.RcodeNameError
		}
	case q.Qtype == dns.TypeAAAA:
		switch name {
		case "alias.example.":
			m.Answer = []dns.RR{cname(q.Name, "v4only.example.")}
		case "blockedalias.example.":
			m.Answer = []dns.RR{cname(q.Name, "ads.tracker.example.")}
		case "dual.example.":
			m.Answer = []dns.RR{&dns.AAAA{Hdr: rrHeader(q.Name, dns.TypeAAAA, 60), AAAA: net.ParseIP("2001:db8::30")}}
		case "nx.example.":
			m.Rcode = dns.RcodeNameError
		}
	}
	if len(m.Answer) == 0 || (q.Qtype == dns.TypeAAAA && name == "alias.example.") {
		m.Ns = []dns.RR{soa}
	}
	return m, upstream.Info{Upstream: "fake-upstream"}, nil
}

// DNS64: AAAA synthesised from the A records of the final name (also at the
// end of a CNAME chain), excluded ranges left out, existing AAAA untouched,
// blocked names never synthesised, never AD, reason dns64.
func TestDNS64Synthesis(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.DNS64.Enabled = true }).serve()
	e.up.setAnswer(dns64Upstream)
	e.flt.check["ads.tracker.example"] = listBlock("Ads")

	aaaa := func(name string) (*dns.Msg, []string) {
		t.Helper()
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeAAAA)
		m.AuthenticatedData = true
		r := e.exchange("udp", m)
		return r, answerIPs(r.Answer)
	}

	r, ips := aaaa("v4only.example")
	if !slices.Equal(ips, []string{"64:ff9b::c000:20a", "64:ff9b::c000:20b"}) || r.AuthenticatedData || len(r.Ns) != 0 {
		t.Fatalf("synthesis: %v (AD %v, Ns %v)", ips, r.AuthenticatedData, r.Ns)
	}
	if ttl := r.Answer[0].Header().Ttl; ttl != 300 {
		t.Errorf("TTL %d, want the SOA minimum 300 (A TTL 3600)", ttl)
	}
	if ttl := r.Answer[1].Header().Ttl; ttl != 60 {
		t.Errorf("TTL %d, want the A TTL 60", ttl)
	}
	if ev := e.logs.waitEvent(t, "v4only.example", 0); ev.Status != StatusForwarded || ev.Reason != "dns64" {
		t.Errorf("log %s / %s", ev.Status, ev.Reason)
	}

	r, ips = aaaa("alias.example")
	if !slices.Equal(ips, []string{"64:ff9b::c000:20a", "64:ff9b::c000:20b"}) || r.Answer[0].Header().Rrtype != dns.TypeCNAME ||
		!strings.EqualFold(r.Answer[1].Header().Name, "v4only.example.") {
		t.Fatalf("CNAME chain: %v", r.Answer)
	}
	if _, ips = aaaa("loop.example"); len(ips) != 0 {
		t.Errorf("excluded ranges synthesised: %v", ips)
	}
	if _, ips = aaaa("mixed.example"); !slices.Equal(ips, []string{"64:ff9b::c633:6414"}) {
		t.Errorf("mixed: %v, want only the usable address", ips)
	}
	if _, ips = aaaa("dual.example"); !slices.Equal(ips, []string{"2001:db8::30"}) {
		t.Errorf("an existing AAAA must be answered as it is: %v", ips)
	}
	if r, ips = aaaa("nx.example"); r.Rcode != dns.RcodeNameError || len(ips) != 0 {
		t.Errorf("NXDOMAIN: %v", r)
	}
	// A CNAME target in the A answer that is blocked for the client.
	if _, ips = aaaa("hop.example"); len(ips) != 0 {
		t.Errorf("hop.example leads to a blocked name: %v", ips)
	}
	if c := e.up.callsFor("www.example.com"); len(c) != 0 {
		t.Errorf("unexpected calls %v", c)
	}
	// A blocked final name is not synthesised (CNAME inspection off, so
	// the AAAA answer itself is not replaced by the blocking reply).
	e.update(func(a *settings.All) { a.Filter.CNAMEInspection = false })
	if _, ips = aaaa("blockedalias.example"); len(ips) != 0 {
		t.Errorf("blocked final name synthesised: %v", ips)
	}
	if c := e.up.callsFor("ads.tracker.example"); len(c) != 0 {
		t.Errorf("the A records of a blocked name must not be looked up: %v", c)
	}
	// Off: the NODATA stays.
	e.update(func(a *settings.All) { a.DNS.DNS64.Enabled = false })
	if _, ips = aaaa("v4only.example"); len(ips) != 0 {
		t.Errorf("DNS64 off: %v", ips)
	}
}

// PTR queries for addresses inside the DNS64 prefix are answered with the
// PTR of the embedded IPv4 address, renamed back to the ip6.arpa name.
func TestDNS64PTR(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.DNS64 = settings.DNS64{Enabled: true, Prefix: "2001:db8:64::/96"} }).serve()
	e.up.setAnswer(dns64Upstream)
	name, _ := dns.ReverseAddr("2001:db8:64::808:808")
	r := e.query("udp", name, dns.TypePTR)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Fatalf("PTR: %v", r)
	}
	if p, ok := r.Answer[0].(*dns.PTR); !ok || p.Ptr != "dns.google." || !strings.EqualFold(p.Hdr.Name, name) ||
		!strings.EqualFold(r.Question[0].Name, name) {
		t.Fatalf("PTR answer %v for %s", r.Answer[0], name)
	}
	if c := e.up.callsFor("8.8.8.8.in-addr.arpa"); len(c) != 1 {
		t.Errorf("the embedded IPv4 address must be asked: %v", c)
	}
	if ev := e.logs.waitEvent(t, normalizeName(name), 0); ev.Reason != "dns64" {
		t.Errorf("log reason %q", ev.Reason)
	}
	// A private embedded address stays local (RFC 6303): NXDOMAIN without
	// a router resolver, with the SOA renamed to the question.
	name, _ = dns.ReverseAddr("2001:db8:64::a00:1")
	r = e.query("udp", name, dns.TypePTR)
	if r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 || !strings.EqualFold(r.Ns[0].Header().Name, name) {
		t.Fatalf("private embedded address: %v", r)
	}
	if c := e.up.callsFor("1.0.0.10.in-addr.arpa"); len(c) != 0 {
		t.Errorf("private reverse names never reach the upstreams: %v", c)
	}
	name, _ = dns.ReverseAddr("2a00:1450:64::808:808") // outside the prefix: the normal path
	e.query("udp", name, dns.TypePTR)
	if c := e.up.callsFor(normalizeName(name)); len(c) != 1 {
		t.Errorf("outside the prefix: %v", c)
	}
}
