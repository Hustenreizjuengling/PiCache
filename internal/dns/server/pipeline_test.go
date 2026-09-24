package dnsserver

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// pipelineEnv is a served environment with local records, a forwarder,
// filter decisions and LanCache services.
func pipelineEnv(t *testing.T) *testEnv {
	e := newEnv(t, func(a *settings.All) { a.LanCache.Enabled = true })
	e.addRecord("nas.example.org", "A", "192.168.1.5")
	e.addRecord("*.dev.example.org", "A", "192.168.1.6")
	e.addRecord("exact.dev.example.org", "A", "192.168.1.7")
	e.addRecord("alias.example.org", "CNAME", "nas.example.org")
	e.addRecord("ext.example.org", "CNAME", "cdn.example.net")
	e.addRecord("txt.example.org", "TXT", "hello world")
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.flt.check["ads.example.com"] = listBlock("TestList")
	e.flt.check["tracker.example.net"] = listBlock("Trackers")
	e.flt.rules["blocked.steamcontent.com"] = ruleBlock("blocked.steamcontent.com")
	e.svc["lancache.steamcontent.com"] = "steam"
	e.svc["blocked.steamcontent.com"] = "steam"
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		switch normalizeName(q.Name) {
		case "www.cnamed.example":
			return upAnswer(req, &dns.CNAME{Hdr: rrHeader(q.Name, dns.TypeCNAME, 300), Target: "tracker.example.net."}), upstream.Info{Upstream: "fake-upstream"}, nil
		case "signed.example":
			m := upAnswer(req)
			m.AuthenticatedData = true
			m.Answer = append(m.Answer, &dns.RRSIG{Hdr: rrHeader(q.Name, dns.TypeRRSIG, 300), TypeCovered: dns.TypeA,
				Algorithm: 13, SignerName: "example.", Signature: "AAAA"})
			return m, upstream.Info{Upstream: "fake-upstream"}, nil
		case "cached.example":
			return upAnswer(req), upstream.Info{Cached: true}, nil
		}
		return upAnswer(req), upstream.Info{Upstream: "fake-upstream"}, nil
	})
	return e.serve()
}

func TestPipeline(t *testing.T) {
	e := pipelineEnv(t)
	type check func(t *testing.T, r *dns.Msg)
	rcode := func(want int) check {
		return func(t *testing.T, r *dns.Msg) {
			t.Helper()
			if r.Rcode != want {
				t.Errorf("rcode %s, want %s", dns.RcodeToString[r.Rcode], dns.RcodeToString[want])
			}
		}
	}
	ips := func(want ...string) check {
		return func(t *testing.T, r *dns.Msg) {
			t.Helper()
			if got := answerIPs(r.Answer); !slices.Equal(got, want) {
				t.Errorf("answer %v, want %v (%v)", got, want, r.Answer)
			}
		}
	}
	nodataSOA := func(minttl uint32) check {
		return func(t *testing.T, r *dns.Msg) {
			t.Helper()
			if len(r.Answer) != 0 || len(r.Ns) != 1 {
				t.Fatalf("want NODATA with SOA, got answer %v ns %v", r.Answer, r.Ns)
			}
			soa, ok := r.Ns[0].(*dns.SOA)
			if !ok || soa.Ns != "picache.invalid." || soa.Mbox != "hostmaster.picache.invalid." || soa.Minttl != minttl {
				t.Errorf("unexpected SOA %v", r.Ns[0])
			}
		}
	}
	ttl := func(want uint32) check {
		return func(t *testing.T, r *dns.Msg) {
			t.Helper()
			for _, rr := range r.Answer {
				if rr.Header().Ttl != want {
					t.Errorf("TTL %d, want %d: %v", rr.Header().Ttl, want, rr)
				}
			}
		}
	}
	answerTypes := func(want ...uint16) check {
		return func(t *testing.T, r *dns.Msg) {
			t.Helper()
			var got []uint16
			for _, rr := range r.Answer {
				got = append(got, rr.Header().Rrtype)
			}
			if !slices.Equal(got, want) {
				t.Errorf("answer types %v, want %v (%v)", got, want, r.Answer)
			}
		}
	}
	noUpstream := func(name string) check {
		return func(t *testing.T, _ *dns.Msg) {
			t.Helper()
			if c := e.up.callsFor(name); len(c) != 0 {
				t.Errorf("%s must never be sent upstream, got %v", name, c)
			}
		}
	}
	upstreamVia := func(name string, via ...string) check {
		return func(t *testing.T, _ *dns.Msg) {
			t.Helper()
			c := e.up.callsFor(name)
			if len(c) == 0 || !slices.Equal(c[len(c)-1].via, via) {
				t.Errorf("%s: upstream calls %v, want via %v", name, c, via)
			}
		}
	}

	tests := []struct {
		name   string
		qname  string
		qtype  uint16
		status string
		checks []check
	}{
		{"forwarded", "www.example.com", dns.TypeA, StatusForwarded, []check{rcode(0), ips("198.51.100.7"), upstreamVia("www.example.com")}},
		{"cached", "cached.example", dns.TypeA, StatusCached, []check{ips("198.51.100.7")}},
		{"blocked A null mode", "ads.example.com", dns.TypeA, StatusBlockedList, []check{rcode(0), ips("0.0.0.0"), ttl(10), noUpstream("ads.example.com")}},
		{"blocked AAAA null mode", "ads.example.com", dns.TypeAAAA, StatusBlockedList, []check{ips("::")}},
		{"blocked MX null mode", "ads.example.com", dns.TypeMX, StatusBlockedList, []check{rcode(0), answerTypes()}},
		{"lancache A", "lancache.steamcontent.com", dns.TypeA, StatusLanCache, []check{ips("192.168.1.10"), ttl(60), noUpstream("lancache.steamcontent.com")}},
		{"lancache AAAA nodata", "lancache.steamcontent.com", dns.TypeAAAA, StatusLanCache, []check{rcode(0), nodataSOA(60)}},
		{"lancache HTTPS nodata", "lancache.steamcontent.com", dns.TypeHTTPS, StatusLanCache, []check{rcode(0), nodataSOA(60)}},
		{"user rule beats lancache", "blocked.steamcontent.com", dns.TypeA, StatusBlockedRule, []check{ips("0.0.0.0")}},
		{"local exact", "nas.example.org", dns.TypeA, StatusLocal, []check{ips("192.168.1.5"), ttl(300), noUpstream("nas.example.org")}},
		{"local wildcard", "a.b.dev.example.org", dns.TypeA, StatusLocal, []check{ips("192.168.1.6")}},
		{"exact beats wildcard", "exact.dev.example.org", dns.TypeA, StatusLocal, []check{ips("192.168.1.7")}},
		{"wildcard excludes apex", "dev.example.org", dns.TypeA, StatusForwarded, []check{ips("198.51.100.7")}},
		{"local nodata", "nas.example.org", dns.TypeAAAA, StatusLocal, []check{rcode(0), nodataSOA(300), noUpstream("nas.example.org")}},
		{"local CNAME to local", "alias.example.org", dns.TypeA, StatusLocal, []check{answerTypes(dns.TypeCNAME, dns.TypeA), ips("192.168.1.5")}},
		{"local CNAME qtype CNAME", "alias.example.org", dns.TypeCNAME, StatusLocal, []check{answerTypes(dns.TypeCNAME)}},
		{"local CNAME to external", "ext.example.org", dns.TypeA, StatusLocal, []check{answerTypes(dns.TypeCNAME, dns.TypeA), upstreamVia("cdn.example.net")}},
		{"local TXT", "txt.example.org", dns.TypeTXT, StatusLocal, []check{answerTypes(dns.TypeTXT)}},
		{"auto PTR", "5.1.168.192.in-addr.arpa", dns.TypePTR, StatusLocal, []check{answerTypes(dns.TypePTR)}},
		{"private PTR never forwarded", "9.1.168.192.in-addr.arpa", dns.TypePTR, StatusSpecial, []check{rcode(dns.RcodeNameError), noUpstream("9.1.168.192.in-addr.arpa")}},
		{"CGNAT PTR never forwarded", "1.0.64.100.in-addr.arpa", dns.TypePTR, StatusSpecial, []check{rcode(dns.RcodeNameError), noUpstream("1.0.64.100.in-addr.arpa")}},
		{"private SOA never forwarded", "168.192.in-addr.arpa", dns.TypeSOA, StatusSpecial, []check{rcode(dns.RcodeNameError), noUpstream("168.192.in-addr.arpa")}},
		{"own address PTR", "10.1.168.192.in-addr.arpa", dns.TypePTR, StatusSpecial, []check{answerTypes(dns.TypePTR)}},
		{"ANY refused", "www.example.com", dns.TypeANY, StatusRefused, []check{rcode(dns.RcodeNotImplemented)}},
		{"localhost", "foo.localhost", dns.TypeA, StatusSpecial, []check{ips("127.0.0.1")}},
		{"localhost AAAA", "localhost", dns.TypeAAAA, StatusSpecial, []check{ips("::1")}},
		{"server name", "picache.lan", dns.TypeA, StatusSpecial, []check{ips("192.168.1.10")}},
		{"server name AAAA", "picache", dns.TypeAAAA, StatusSpecial, []check{ips("fd00::10")}},
		{"resolver.arpa nodata", "_dns.resolver.arpa", dns.TypeSVCB, StatusSpecial, []check{rcode(0), answerTypes()}},
		{"local domain not forwarded", "printer.lan", dns.TypeA, StatusSpecial, []check{rcode(dns.RcodeNameError), noUpstream("printer.lan")}},
		{"onion not forwarded", "abc.onion", dns.TypeA, StatusSpecial, []check{rcode(dns.RcodeNameError), noUpstream("abc.onion")}},
		{"conditional forwarder", "host.corp.example", dns.TypeA, StatusForwarded, []check{upstreamVia("host.corp.example", "10.9.9.9")}},
		{"mozilla canary", "use-application-dns.net", dns.TypeA, StatusBlockedSpecial, []check{rcode(dns.RcodeNameError), noUpstream("use-application-dns.net")}},
		{"icloud private relay", "mask.icloud.com", dns.TypeHTTPS, StatusBlockedSpecial, []check{rcode(dns.RcodeNameError)}},
		{"cname inspection", "www.cnamed.example", dns.TypeA, StatusBlockedCNAME, []check{ips("0.0.0.0")}},
		{"DNSSEC records stripped without DO", "signed.example", dns.TypeA, StatusForwarded, []check{answerTypes(dns.TypeA), func(t *testing.T, r *dns.Msg) {
			if r.AuthenticatedData {
				t.Error("AD must not be set for a client without AD/DO")
			}
		}}},
	}
	for _, network := range []string{"udp", "tcp"} {
		for _, tc := range tests {
			t.Run(network+"/"+tc.name, func(t *testing.T) {
				before := e.logs.count(normalizeName(tc.qname))
				r := e.query(network, tc.qname, tc.qtype)
				for _, c := range tc.checks {
					c(t, r)
				}
				ev := e.logs.waitEvent(t, normalizeName(tc.qname), before)
				if ev.Status != tc.status {
					t.Errorf("status %q, want %q (reason %q)", ev.Status, tc.status, ev.Reason)
				}
				if ev.Protocol != network || ev.ClientIP != "127.0.0.1" {
					t.Errorf("log protocol/client = %s/%s", ev.Protocol, ev.ClientIP)
				}
			})
		}
	}
}

func TestLogEventFields(t *testing.T) {
	e := pipelineEnv(t)
	e.query("udp", "ads.example.com", dns.TypeA)
	ev := e.logs.waitEvent(t, "ads.example.com", 0)
	if ev.Reason != "TestList" || ev.ListID != 3 || ev.RCode != "NOERROR" || ev.QType != "A" || ev.Answer != "0.0.0.0" {
		t.Errorf("unexpected event %+v", ev)
	}
	e.query("udp", "lancache.steamcontent.com", dns.TypeA)
	if ev := e.logs.waitEvent(t, "lancache.steamcontent.com", 0); ev.Service != "steam" {
		t.Errorf("service %q, want steam", ev.Service)
	}
	e.query("udp", "www.example.com", dns.TypeA)
	if ev := e.logs.waitEvent(t, "www.example.com", 0); ev.Upstream != "fake-upstream" || ev.Answer != "198.51.100.7" {
		t.Errorf("unexpected event %+v", ev)
	}
}

func TestBlockedEDE(t *testing.T) {
	e := pipelineEnv(t)
	r := e.query("udp", "ads.example.com", dns.TypeA, withEDNS(1232, false))
	opt := r.IsEdns0()
	if opt == nil {
		t.Fatal("EDNS client must get an OPT record")
	}
	var ede *dns.EDNS0_EDE
	for _, o := range opt.Option {
		if v, ok := o.(*dns.EDNS0_EDE); ok {
			ede = v
		}
	}
	if ede == nil || ede.InfoCode != dns.ExtendedErrorCodeBlocked || !strings.Contains(ede.ExtraText, "TestList") {
		t.Fatalf("want EDE 15 with the list name, got %v", opt.Option)
	}
	if r := e.query("udp", "www.example.com", dns.TypeA, withEDNS(1232, false)); len(r.IsEdns0().Option) != 0 {
		t.Errorf("allowed replies carry no EDE: %v", r.IsEdns0().Option)
	}
	if r := e.query("udp", "ads.example.com", dns.TypeA); r.IsEdns0() != nil {
		t.Error("a client without EDNS must not get an OPT record")
	}
}

func TestDNSSECWithDO(t *testing.T) {
	e := pipelineEnv(t)
	r := e.query("udp", "signed.example", dns.TypeA, withEDNS(1232, true))
	if !slices.ContainsFunc(r.Answer, func(rr dns.RR) bool { return rr.Header().Rrtype == dns.TypeRRSIG }) {
		t.Errorf("RRSIG must be kept for DO clients: %v", r.Answer)
	}
	if !r.AuthenticatedData || !r.IsEdns0().Do() {
		t.Error("AD and DO must be echoed to a DO client")
	}
	if c := e.up.callsFor("signed.example"); !c[len(c)-1].do {
		t.Error("DO must be requested upstream for a DO client")
	}
}

func TestBlockingModesApplyLive(t *testing.T) {
	e := pipelineEnv(t)
	tests := []struct {
		mode    string
		qtype   uint16
		rcode   int
		ips     []string
		withSOA bool
	}{
		{"nxdomain", dns.TypeA, dns.RcodeNameError, nil, true},
		{"nodata", dns.TypeA, dns.RcodeSuccess, nil, true},
		{"refused", dns.TypeA, dns.RcodeRefused, nil, false},
		{"custom_ip", dns.TypeA, dns.RcodeSuccess, []string{"192.168.1.99"}, false},
		{"custom_ip", dns.TypeAAAA, dns.RcodeSuccess, nil, false},
		{"null", dns.TypeTXT, dns.RcodeSuccess, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.mode+"/"+dns.TypeToString[tc.qtype], func(t *testing.T) {
			e.update(func(a *settings.All) {
				a.Filter.BlockingMode = tc.mode
				a.Filter.BlockingIPv4 = "192.168.1.99"
				a.Filter.BlockedTTL = 30
			})
			r := e.query("udp", "ads.example.com", tc.qtype)
			if r.Rcode != tc.rcode {
				t.Errorf("rcode %s, want %s", dns.RcodeToString[r.Rcode], dns.RcodeToString[tc.rcode])
			}
			if got := answerIPs(r.Answer); !slices.Equal(got, tc.ips) {
				t.Errorf("answer %v, want %v", got, tc.ips)
			}
			if hasSOA := len(r.Ns) == 1; hasSOA != tc.withSOA {
				t.Errorf("SOA present = %v, want %v", hasSOA, tc.withSOA)
			} else if hasSOA && r.Ns[0].(*dns.SOA).Minttl != 30 {
				t.Errorf("SOA minimum %d, want blocked TTL 30", r.Ns[0].(*dns.SOA).Minttl)
			}
		})
	}
}

func TestPause(t *testing.T) {
	e := pipelineEnv(t)
	ctx := context.Background()
	st, err := e.srv.SetBlocking(ctx, false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if st.Enabled || st.Permanent || st.PausedUntil == nil || time.Until(*st.PausedUntil) <= 0 {
		t.Fatalf("unexpected paused status %+v", st)
	}
	if r := e.query("udp", "ads.example.com", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"198.51.100.7"}) {
		t.Errorf("paused: blocked name must be forwarded, got %v", r.Answer)
	}
	if r := e.query("udp", "use-application-dns.net", dns.TypeA); r.Rcode != dns.RcodeSuccess {
		t.Errorf("paused: special domains are not blocked, got %s", dns.RcodeToString[r.Rcode])
	}
	if r := e.query("udp", "blocked.steamcontent.com", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"192.168.1.10"}) {
		t.Errorf("paused: user rules do not apply, LanCache answers: %v", r.Answer)
	}
	// An elapsed pause is cleared from the settings.
	e.srv.expirePause(ctx, time.Now().Add(2*time.Minute))
	if f := e.set.Get().Filter; f.PausedUntil != nil || !f.Enabled {
		t.Fatalf("elapsed pause not cleared: %+v", f)
	}
	if r := e.query("udp", "ads.example.com", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"0.0.0.0"}) {
		t.Errorf("after the pause blocking applies again, got %v", r.Answer)
	}

	st, err = e.srv.SetBlocking(ctx, false, 0)
	if err != nil || st.Enabled || !st.Permanent || st.PausedUntil != nil {
		t.Fatalf("permanent disable: %+v %v", st, err)
	}
	if r := e.query("udp", "ads.example.com", dns.TypeA); answerIPs(r.Answer)[0] != "198.51.100.7" {
		t.Errorf("disabled: blocked name must be forwarded, got %v", r.Answer)
	}
	if st, _ := e.srv.SetBlocking(ctx, true, 0); !st.Enabled || st.Permanent {
		t.Fatalf("re-enable: %+v", st)
	}
	if _, err := e.srv.SetBlocking(ctx, false, -time.Second); err == nil {
		t.Error("negative pause must be rejected")
	}
}

func TestLanCacheGating(t *testing.T) {
	e := pipelineEnv(t)
	name := "lancache.steamcontent.com"

	e.lcReady.Store(false)
	if r := e.query("udp", name, dns.TypeA); answerIPs(r.Answer)[0] != "198.51.100.7" {
		t.Errorf("not ready: must be forwarded, got %v", r.Answer)
	}
	if st := e.srv.CacheIPs(); st.Ready || st.Reason != "cache listener not bound" {
		t.Errorf("status %+v", st)
	}
	e.lcReady.Store(true)

	client := netip.MustParseAddr("127.0.0.1")
	e.cl.set(&clients.Identity{IP: client, ClientID: 4, Name: "console", GroupIDs: []int64{1}, LanCacheBypass: true})
	if r := e.query("udp", name, dns.TypeA); answerIPs(r.Answer)[0] != "198.51.100.7" {
		t.Errorf("bypass client: must be forwarded, got %v", r.Answer)
	}
	e.cl.set(&clients.Identity{IP: client, GroupIDs: []int64{1}})

	e.update(func(a *settings.All) {
		a.LanCache.CacheIPv4 = []string{"10.0.0.2", "10.0.0.3"}
		a.LanCache.CacheIPv6 = []string{"fd00::2"}
	})
	first := answerIPs(e.query("udp", name, dns.TypeA).Answer)
	second := answerIPs(e.query("udp", name, dns.TypeA).Answer)
	if len(first) != 2 || first[0] == second[0] {
		t.Errorf("configured IPs must be rotated: %v then %v", first, second)
	}
	if got := answerIPs(e.query("udp", name, dns.TypeAAAA).Answer); !slices.Equal(got, []string{"fd00::2"}) {
		t.Errorf("AAAA with configured ULA: %v", got)
	}
	if st := e.srv.CacheIPs(); !st.Ready || st.Auto || len(st.IPv4) != 2 {
		t.Errorf("status %+v", st)
	}

	e.update(func(a *settings.All) { a.LanCache.Enabled = false })
	if r := e.query("udp", name, dns.TypeA); answerIPs(r.Answer)[0] != "198.51.100.7" {
		t.Errorf("disabled: must be forwarded, got %v", r.Answer)
	}
}

func TestChaosAndValidation(t *testing.T) {
	e := newEnv(t, nil).serve()
	for _, network := range []string{"udp", "tcp"} {
		m := new(dns.Msg)
		m.SetQuestion("version.bind.", dns.TypeTXT)
		m.Question[0].Qclass = dns.ClassCHAOS
		if r := e.exchange(network, m); r.Rcode != dns.RcodeRefused {
			t.Errorf("%s CHAOS: rcode %s", network, dns.RcodeToString[r.Rcode])
		}
		m = new(dns.Msg)
		m.SetQuestion("id.server.", dns.TypeTXT)
		if r := e.exchange(network, m); r.Rcode != dns.RcodeRefused {
			t.Errorf("%s id.server: rcode %s", network, dns.RcodeToString[r.Rcode])
		}
		m = new(dns.Msg)
		m.SetQuestion("example.com.", dns.TypeAXFR)
		if r := e.exchange(network, m); r.Rcode != dns.RcodeRefused {
			t.Errorf("%s AXFR: rcode %s", network, dns.RcodeToString[r.Rcode])
		}
		m = new(dns.Msg)
		m.SetQuestion("example.com.", dns.TypeA)
		m.Opcode = dns.OpcodeNotify
		if r := e.exchange(network, m); r.Rcode != dns.RcodeNotImplemented {
			t.Errorf("%s NOTIFY: rcode %s", network, dns.RcodeToString[r.Rcode])
		}
		m = new(dns.Msg)
		m.SetQuestion("example.com.", dns.TypeA)
		m.SetEdns0(1232, false)
		m.IsEdns0().SetVersion(1)
		if r := e.exchange(network, m); r.Rcode != dns.RcodeBadVers {
			t.Errorf("%s EDNS version 1: rcode %s", network, dns.RcodeToString[r.Rcode])
		}
	}
	if c := e.up.callsFor("version.bind"); len(c) != 0 {
		t.Error("CHAOS queries must not be forwarded")
	}
	if e.srv.Stats().Refused < 10 {
		t.Errorf("refusals not counted: %+v", e.srv.Stats())
	}
}

func TestTruncation(t *testing.T) {
	e := newEnv(t, nil).serve()
	e.up.setAnswer(func(req *dns.Msg, _ []string) (*dns.Msg, upstream.Info, error) {
		m := new(dns.Msg)
		m.SetReply(req)
		for i := range 100 {
			m.Answer = append(m.Answer, &dns.A{Hdr: rrHeader(req.Question[0].Name, dns.TypeA, 60), A: net.IPv4(198, 51, 100, byte(i))})
		}
		return m, upstream.Info{Upstream: "fake"}, nil
	})
	tests := []struct {
		name    string
		network string
		opts    []queryOpt
		maxLen  int
		tc      bool
	}{
		{"udp without EDNS", "udp", nil, 512, true},
		{"udp EDNS 4096 capped", "udp", []queryOpt{withEDNS(4096, false)}, 1232, true},
		{"udp EDNS 800", "udp", []queryOpt{withEDNS(800, false)}, 800, true},
		{"tcp", "tcp", nil, dns.MaxMsgSize, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := new(dns.Msg)
			m.SetQuestion("many.example.", dns.TypeA)
			for _, o := range tc.opts {
				o(m)
			}
			r := e.exchange(tc.network, m)
			if r.Truncated != tc.tc {
				t.Errorf("TC = %v, want %v", r.Truncated, tc.tc)
			}
			r.Compress = true // Len of the wire form
			if r.Len() > tc.maxLen {
				t.Errorf("reply %d bytes > %d", r.Len(), tc.maxLen)
			}
			if !tc.tc && len(r.Answer) != 100 {
				t.Errorf("TCP must carry all 100 answers, got %d", len(r.Answer))
			}
		})
	}
}

func TestPrivatePTRUsesLocalResolvers(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.LocalPTRUpstreams = []string{"192.168.1.1"} }).serve()
	name := "20.1.168.192.in-addr.arpa"
	e.query("udp", name, dns.TypePTR)
	calls := e.up.callsFor(name)
	if len(calls) != 1 || !slices.Equal(calls[0].via, []string{"192.168.1.1"}) {
		t.Fatalf("private PTR must go to localPtrUpstreams only, calls %v", calls)
	}
	// A conditional forwarder for the reverse zone is more specific.
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: "1.168.192.in-addr.arpa", Upstreams: []string{"192.168.1.2"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.query("udp", name, dns.TypePTR)
	calls = e.up.callsFor(name)
	if !slices.Equal(calls[len(calls)-1].via, []string{"192.168.1.2"}) {
		t.Fatalf("forwarder must win over localPtrUpstreams, calls %v", calls)
	}
	// Public reverse names are forwarded normally.
	e.query("udp", "7.100.51.198.in-addr.arpa", dns.TypePTR) // documentation range: private
	e.query("udp", "8.8.8.8.in-addr.arpa", dns.TypePTR)
	if c := e.up.callsFor("8.8.8.8.in-addr.arpa"); len(c) != 1 || c[0].via != nil {
		t.Errorf("public PTR goes to the default upstreams: %v", c)
	}
	if c := e.up.callsFor("7.100.51.198.in-addr.arpa"); len(c) != 1 || c[0].via == nil {
		t.Errorf("documentation reverse zone is locally served: %v", c)
	}
}

func TestRouterResolverAndLoopGuard(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.RouterResolver = "192.168.1.1" }).serve()
	e.query("udp", "printer.lan", dns.TypeA)
	if c := e.up.callsFor("printer.lan"); len(c) != 1 || !slices.Equal(c[0].via, []string{"192.168.1.1:53"}) {
		t.Fatalf("local domain must go to the router resolver: %v", c)
	}
	if st := e.srv.Router(); st.Mode != "manual" || st.Address != "192.168.1.1" || st.Domain != "lan" {
		t.Errorf("router status %+v", st)
	}
	// Queries from the router itself for names PiCache would send back to it fail.
	e.update(func(a *settings.All) { a.DNS.RouterResolver = "127.0.0.1" })
	r := e.query("udp", "nas.lan", dns.TypeA)
	if r.Rcode != dns.RcodeServerFailure {
		t.Errorf("loop guard: rcode %s", dns.RcodeToString[r.Rcode])
	}
	if c := e.up.callsFor("nas.lan"); len(c) != 0 {
		t.Errorf("loop guard must not forward: %v", c)
	}
	e.update(func(a *settings.All) { a.DNS.RouterResolver = "" })
	if st := e.srv.Router(); st.Mode != "off" || st.Address != "" {
		t.Errorf("router status %+v", st)
	}
}

func TestIgnoreLogsAndSeen(t *testing.T) {
	e := newEnv(t, nil).serve()
	client := netip.MustParseAddr("127.0.0.1")
	e.cl.set(&clients.Identity{IP: client, GroupIDs: []int64{1}, IgnoreLogs: true})
	e.query("udp", "quiet.example", dns.TypeA)
	e.cl.set(&clients.Identity{IP: client, GroupIDs: []int64{1}})
	e.query("udp", "loud.example", dns.TypeA)
	e.logs.waitEvent(t, "loud.example", 0)
	if n := e.logs.count("quiet.example"); n != 0 {
		t.Errorf("ignoreLogs client was logged %d times", n)
	}
	e.cl.mu.Lock()
	seen := e.cl.seen[client]
	e.cl.mu.Unlock()
	if seen != 1 {
		t.Errorf("Seen called %d times, want 1 (not for the ignoreLogs client)", seen)
	}
	// With anonymised client addresses activity is recorded in memory only.
	e.update(func(a *settings.All) { a.Logs.AnonymizeClientIPs = true })
	e.query("udp", "anon.example", dns.TypeA)
	e.logs.waitEvent(t, "anon.example", 0)
	e.cl.mu.Lock()
	seen, transient := e.cl.seen[client], e.cl.transient[client]
	e.cl.mu.Unlock()
	if seen != 1 || transient != 1 {
		t.Errorf("anonymised: Seen %d, SeenTransient %d; want 1, 1", seen, transient)
	}
}

func TestLocalCNAMELoopServfail(t *testing.T) {
	e := newEnv(t, nil).serve()
	e.addRecord("*.loop.example", "CNAME", "loop.example")
	e.addRecord("loop.example", "CNAME", "x.loop.example")
	r := e.query("udp", "a.loop.example", dns.TypeA)
	if r.Rcode != dns.RcodeServerFailure {
		t.Fatalf("CNAME loop: rcode %s, answer %v", dns.RcodeToString[r.Rcode], r.Answer)
	}
	if ev := e.logs.waitEvent(t, "a.loop.example", 0); ev.Status != StatusError {
		t.Errorf("status %q", ev.Status)
	}
}

func TestUpstreamFailure(t *testing.T) {
	e := newEnv(t, nil).serve()
	e.up.setAnswer(func(*dns.Msg, []string) (*dns.Msg, upstream.Info, error) {
		return nil, upstream.Info{Upstream: "broken"}, context.DeadlineExceeded
	})
	r := e.query("tcp", "down.example", dns.TypeA)
	if r.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode %s", dns.RcodeToString[r.Rcode])
	}
	if ev := e.logs.waitEvent(t, "down.example", 0); ev.Status != StatusError || ev.Upstream != "broken" {
		t.Errorf("event %+v", ev)
	}
}
