package dnsserver

import (
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// replyRule is a user block rule decision with its own reply.
func replyRule(name, reply, v4, v6 string) filter.Decision {
	return filter.Decision{Action: filter.ActionBlock, Source: "rule", Kind: "exact", RuleID: 11, Name: name,
		Reply: reply, ReplyIPv4: v4, ReplyIPv6: v6}
}

// answerOf runs a query as the client 192.168.1.50 through the handler and
// returns the reply.
func (e *testEnv) answerOf(name string, qtype uint16) *dns.Msg {
	e.t.Helper()
	w := udpFrom("192.168.1.50")
	e.handle(w, name, qtype)
	if len(w.msgs) != 1 {
		e.t.Fatalf("%s: %d replies", name, len(w.msgs))
	}
	return w.msgs[0]
}

func isNodataSOA(m *dns.Msg) bool {
	return m.Rcode == dns.RcodeSuccess && len(m.Answer) == 0 && len(m.Ns) == 1 && m.Ns[0].Header().Rrtype == dns.TypeSOA
}

// A user rule's reply replaces the blocking mode in the answers it
// decides (steps 11, 8, the 7c guard and 14); lists keep the global mode.
func TestRuleReplies(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.Filter.BlockingMode = "nxdomain"
		a.DownloadCache.Enabled = true
	})
	e.flt.check["null.example"] = replyRule("null.example", "null", "", "")
	e.flt.check["nodata.example"] = replyRule("nodata.example", "nodata", "", "")
	e.flt.check["refused.example"] = replyRule("refused.example", "refused", "", "")
	e.flt.check["custom.example"] = replyRule("custom.example", "custom_ip", "192.0.2.9", "")
	e.flt.check["self.example"] = replyRule("self.example", "custom_ip", "self", "self")
	e.flt.check["global.example"] = ruleBlock("global.example")
	e.flt.check["list.example"] = listBlock("Ads")
	cases := []struct {
		name  string
		qtype uint16
		check func(*dns.Msg) bool
	}{
		{"null.example", dns.TypeA, func(m *dns.Msg) bool { return slices.Equal(answerIPs(m.Answer), []string{"0.0.0.0"}) }},
		{"null.example", dns.TypeAAAA, func(m *dns.Msg) bool { return slices.Equal(answerIPs(m.Answer), []string{"::"}) }},
		{"null.example", dns.TypeMX, isNodataSOA},
		{"nodata.example", dns.TypeA, isNodataSOA},
		{"refused.example", dns.TypeA, func(m *dns.Msg) bool { return m.Rcode == dns.RcodeRefused }},
		{"custom.example", dns.TypeA, func(m *dns.Msg) bool { return slices.Equal(answerIPs(m.Answer), []string{"192.0.2.9"}) }},
		{"custom.example", dns.TypeAAAA, isNodataSOA},
		{"custom.example", dns.TypeTXT, isNodataSOA},
		{"self.example", dns.TypeA, func(m *dns.Msg) bool { return slices.Equal(answerIPs(m.Answer), []string{testCacheIP.String()}) }},
		{"self.example", dns.TypeAAAA, func(m *dns.Msg) bool { return slices.Equal(answerIPs(m.Answer), []string{testServerV6.String()}) }},
		{"global.example", dns.TypeA, func(m *dns.Msg) bool { return m.Rcode == dns.RcodeNameError }},
		{"list.example", dns.TypeA, func(m *dns.Msg) bool { return m.Rcode == dns.RcodeNameError }},
	}
	for _, c := range cases {
		m := e.answerOf(c.name, c.qtype)
		if !c.check(m) {
			t.Errorf("%s %s: %v", c.name, dns.TypeToString[c.qtype], m)
		}
		if ttl := firstTTL(m); ttl != 10 && ttl != 0 {
			t.Errorf("%s: TTL %d", c.name, ttl)
		}
	}
	if ev := e.logs.waitEvent(t, "custom.example", 0); ev.Status != StatusBlockedRule || ev.RuleID != 11 || ev.Purpose != PurposeRule {
		t.Errorf("a custom_ip rule is blocked traffic: %+v", ev)
	}
	// Step 8: a user rule blocks a download service name with its reply.
	e.svc["dl.steamcontent.com"] = "steam"
	e.flt.rules["dl.steamcontent.com"] = replyRule("dl.steamcontent.com", "custom_ip", "192.0.2.8", "")
	if m := e.answerOf("dl.steamcontent.com", dns.TypeA); !slices.Equal(answerIPs(m.Answer), []string{"192.0.2.8"}) {
		t.Errorf("step 8: %v", m)
	}
	// Step 14: blocked-cname takes the decisive rule's reply.
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		if normalizeName(q.Name) == "front.example" {
			return upAnswer(req, &dns.CNAME{Hdr: rrHeader(q.Name, dns.TypeCNAME, 300), Target: "nodata.example."}), upstream.Info{Upstream: "u"}, nil
		}
		return upAnswer(req), upstream.Info{Upstream: "u"}, nil
	})
	if m := e.answerOf("front.example", dns.TypeA); !isNodataSOA(m) {
		t.Errorf("blocked-cname: %v", m)
	}
	// The 7c guard: a blocked name takes the normal path and the rule's reply.
	e.srv.d.Parental = &fakeParental{safe: map[string]parental.SafeSearchRewrite{
		"www.google.com": {Target: "forcesafesearch.google.com", Group: "Kids"}}}
	e.flt.check["www.google.com"] = replyRule("www.google.com", "refused", "", "")
	if m := e.answerOf("www.google.com", dns.TypeA); m.Rcode != dns.RcodeRefused {
		t.Errorf("7c guard: %v", m)
	}
	// "self" without an address of the family: NODATA + SOA.
	e.srv.host.Store(&hostInfo{ifaces: []hostIface{{name: "eth0", prefixes: []netip.Prefix{netip.PrefixFrom(testCacheIP, 24)}}},
		own: []netip.Addr{testCacheIP}, primary4: testCacheIP})
	if m := e.answerOf("self.example", dns.TypeAAAA); !isNodataSOA(m) {
		t.Errorf("self without IPv6: %v", m)
	}
	// The global mode custom_ip accepts self too; serverNameAddresses win.
	e.update(func(a *settings.All) {
		a.Filter.BlockingMode, a.Filter.BlockingIPv4 = "custom_ip", "self"
		a.DNS.ServerNameAddresses.IPv4 = []string{"192.168.1.2"}
	})
	if m := e.answerOf("list.example", dns.TypeA); !slices.Equal(answerIPs(m.Answer), []string{"192.168.1.2"}) {
		t.Errorf("global self: %v", m)
	}
}

// A blocked download service name never gets this server's address: the
// download cache and the SNI relay would serve it anyway (3.6).
func TestSelfReplyNotForDownloadServices(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.Filter.BlockingMode, a.Filter.BlockingIPv4, a.Filter.BlockingIPv6 = "custom_ip", "self", "self"
		a.DownloadCache.Enabled = true
	})
	e.svc["cdn.steamcontent.com"] = "steam"
	e.svc["dl.delivery.mp.microsoft.com"] = "wsus"
	e.svc["img.steamcontent.com"] = "steam"
	e.svc["other.steamcontent.com"] = "steam"
	// Step 8 with the global mode, step 8 with a rule's self reply, a list
	// decision (step 11 while the download cache is off for the name).
	e.flt.rules["cdn.steamcontent.com"] = ruleBlock("cdn.steamcontent.com")
	e.flt.rules["dl.delivery.mp.microsoft.com"] = replyRule("dl.delivery.mp.microsoft.com", "custom_ip", "self", "self")
	// An explicit address of this server is the same as self.
	e.flt.rules["img.steamcontent.com"] = replyRule("img.steamcontent.com", "custom_ip", testCacheIP.String(), "")
	// Any other address is answered as configured.
	e.flt.rules["other.steamcontent.com"] = replyRule("other.steamcontent.com", "custom_ip", "192.0.2.7", "")
	for _, name := range []string{"cdn.steamcontent.com", "dl.delivery.mp.microsoft.com", "img.steamcontent.com"} {
		for _, qt := range []uint16{dns.TypeA, dns.TypeAAAA} {
			if m := e.answerOf(name, qt); !isNodataSOA(m) {
				t.Errorf("%s %s: %v", name, dns.TypeToString[qt], m)
			}
		}
	}
	if m := e.answerOf("other.steamcontent.com", dns.TypeA); !slices.Equal(answerIPs(m.Answer), []string{"192.0.2.7"}) {
		t.Errorf("other address: %v", m)
	}
	// A list block of a download service name (a client that bypasses the
	// download cache) and a parental protection-list block.
	e.flt.check["bypass.steamcontent.com"] = listBlock("Games")
	e.svc["bypass.steamcontent.com"] = "steam"
	e.flt.protect["x.steamcontent.com"] = listBlock("Protect")
	e.svc["x.steamcontent.com"] = "steam"
	e.update(func(a *settings.All) { a.DownloadCache.Enabled = false })
	for _, name := range []string{"bypass.steamcontent.com", "x.steamcontent.com"} {
		if m := e.answerOf(name, dns.TypeA); !isNodataSOA(m) {
			t.Errorf("%s: %v", name, m)
		}
	}
	// Every other blocked name still gets this server's address.
	e.flt.check["ads.example"] = listBlock("Ads")
	if m := e.answerOf("ads.example", dns.TypeA); !slices.Equal(answerIPs(m.Answer), []string{testCacheIP.String()}) {
		t.Errorf("self for other names: %v", m)
	}
}

func firstTTL(m *dns.Msg) uint32 {
	for _, rr := range append(slices.Clone(m.Answer), m.Ns...) {
		return rr.Header().Ttl
	}
	return 0
}

// Every filter check passes the query type: a rule or entry of another
// type does not apply (steps 7a, 8, 10, 11, 14, 14b and the 14c
// exemption).
func TestQueryTypeAtCallSites(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DownloadCache.Enabled = true })
	allow := filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "subtree", RuleID: 5, Name: "x"}
	// 11
	e.flt.check["typed.example"], e.flt.types["typed.example"] = listBlock("L"), dns.TypeAAAA
	// 7a: an allow rule for A lifts nothing for AAAA.
	e.flt.protect["adult.example"] = listBlock("Adult")
	e.flt.rules["adult.example"], e.flt.ruleTypes["adult.example"] = allow, dns.TypeA
	// 8
	e.svc["cdn.steamcontent.com"] = "steam"
	e.flt.rules["cdn.steamcontent.com"], e.flt.types["cdn.steamcontent.com"] = ruleBlock("cdn.steamcontent.com"), dns.TypeAAAA
	// 10
	e.flt.check["use-application-dns.net"], e.flt.types["use-application-dns.net"] = allow, dns.TypeA
	// 14c: the rebind exemption of an allow rule for A.
	e.flt.rules["rebind.example"], e.flt.ruleTypes["rebind.example"] = allow, dns.TypeA
	// 14
	e.flt.check["target.example"], e.flt.types["target.example"] = listBlock("T"), dns.TypeAAAA
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		switch normalizeName(q.Name) {
		case "rebind.example":
			m := new(dns.Msg)
			m.SetReply(req)
			if q.Qtype == dns.TypeA {
				m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, 60), A: net.ParseIP("192.168.9.9").To4()}}
			} else {
				m.Answer = []dns.RR{&dns.AAAA{Hdr: rrHeader(q.Name, dns.TypeAAAA, 60), AAAA: net.ParseIP("fd00::99")}}
			}
			return m, upstream.Info{Upstream: "u"}, nil
		case "cname.example":
			return upAnswer(req, &dns.CNAME{Hdr: rrHeader(q.Name, dns.TypeCNAME, 300), Target: "target.example."}), upstream.Info{Upstream: "u"}, nil
		}
		return upAnswer(req), upstream.Info{Upstream: "u"}, nil
	})
	cases := []struct {
		name   string
		qtype  uint16
		status string
	}{
		{"typed.example", dns.TypeA, StatusForwarded},
		{"typed.example", dns.TypeAAAA, StatusBlockedList},
		{"adult.example", dns.TypeA, StatusForwarded},
		{"adult.example", dns.TypeAAAA, StatusBlockedList},
		{"cdn.steamcontent.com", dns.TypeA, StatusOverride},
		{"cdn.steamcontent.com", dns.TypeAAAA, StatusBlockedRule},
		{"use-application-dns.net", dns.TypeA, StatusForwarded},
		{"use-application-dns.net", dns.TypeAAAA, StatusBlockedSpecial},
		{"rebind.example", dns.TypeA, StatusForwarded},
		{"rebind.example", dns.TypeAAAA, StatusBlockedRebind},
		{"cname.example", dns.TypeA, StatusForwarded},
		{"cname.example", dns.TypeAAAA, StatusBlockedCNAME},
	}
	for _, c := range cases {
		res := e.lookupAs(c.name, dns.TypeToString[c.qtype], "192.168.1.50")
		if res.Status != c.status {
			t.Errorf("%s %s: status %s, want %s (%v)", c.name, dns.TypeToString[c.qtype], res.Status, c.status, res.Steps)
		}
	}
	// 14b: DNS64 checks the final name of the A answer with the type AAAA.
	e.update(func(a *settings.All) { a.DNS.DNS64.Enabled = true })
	e.up.setAnswer(dns64Upstream)
	e.lookupAs("v4only.example", "AAAA", "192.168.1.50")
	if got := e.flt.checkedTypes["v4only.example"]; len(got) == 0 || slices.ContainsFunc(got, func(q uint16) bool { return q != dns.TypeAAAA }) {
		t.Errorf("DNS64 checks with %v", got)
	}
}

// ipBlock is a decision of a list of answer addresses.
func ipBlock(name string, list int64) filter.Decision {
	return filter.Decision{Action: filter.ActionBlock, Source: "list", Kind: "ip", ListID: list, Name: name, Category: filter.CategorySecurity}
}

// Step 14d: the addresses of an answer of the default set (answer and
// additional sections, HTTPS hints; mapped and embedded IPv4 judged as
// IPv4; DNS64 records) are checked; the first blocked one makes the whole
// answer the blocking reply (blocked-ip); this machine's addresses,
// forwarder answers, allowed names and a later 14c block are not.
func TestResponseIPCheck(t *testing.T) {
	e := newEnv(t, nil)
	bad := netip.MustParseAddr("198.51.100.66")
	e.flt.ips[bad] = ipBlock("Bad addresses", 5)
	e.flt.ips[netip.MustParseAddr("2001:db8::66")] = filter.Decision{Action: filter.ActionBlock, Source: "ip-rule", Kind: "ip",
		RuleID: 3, Name: "2001:db8::66"}
	e.flt.ips[testCacheIP] = ipBlock("Bad addresses", 5)
	e.flt.check["allowed.example"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "exact", Name: "allowed.example"}
	if _, err := e.srv.CreateForwarder(t.Context(), ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	a := func(name, ip string) dns.RR {
		return &dns.A{Hdr: rrHeader(name, dns.TypeA, 60), A: net.ParseIP(ip).To4()}
	}
	aaaa := func(name, ip string) dns.RR {
		return &dns.AAAA{Hdr: rrHeader(name, dns.TypeAAAA, 60), AAAA: net.ParseIP(ip)}
	}
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		m := new(dns.Msg)
		m.SetReply(req)
		switch name := normalizeName(q.Name); name {
		case "evil.example", "allowed.example", "x.corp.example":
			m.Answer = []dns.RR{a(q.Name, "203.0.113.1"), a(q.Name, "198.51.100.66")}
		case "evil6.example":
			m.Answer = []dns.RR{aaaa(q.Name, "2001:db8::66")}
		case "mapped.example":
			m.Answer = []dns.RR{aaaa(q.Name, "::ffff:198.51.100.66")}
		case "nat64.example":
			m.Answer = []dns.RR{aaaa(q.Name, "64:ff9b::c633:6442")}
		case "hint.example":
			m.Answer = []dns.RR{&dns.HTTPS{SVCB: dns.SVCB{Hdr: rrHeader(q.Name, dns.TypeHTTPS, 60), Priority: 1, Target: ".",
				Value: []dns.SVCBKeyValue{&dns.SVCBIPv4Hint{Hint: []net.IP{net.ParseIP("198.51.100.66").To4()}}}}}}
		case "extra.example":
			m.Answer = []dns.RR{a(q.Name, "203.0.113.1")}
			m.Extra = []dns.RR{a("ns.extra.example.", "198.51.100.66")}
		case "own.example":
			m.Answer = []dns.RR{a(q.Name, testCacheIP.String())}
		case "both.example":
			m.Answer = []dns.RR{a(q.Name, "10.1.1.1"), a(q.Name, "198.51.100.66")}
		case "many.example":
			for i := range 300 {
				m.Answer = append(m.Answer, a(q.Name, netip.AddrFrom4([4]byte{203, 0, byte(113 + i/250), byte(i % 250)}).String()))
			}
			m.Answer = append(m.Answer, a(q.Name, "198.51.100.66"))
		default:
			return upAnswer(req), upstream.Info{Upstream: "u"}, nil
		}
		return m, upstream.Info{Upstream: "u"}, nil
	})
	cases := []struct {
		name, typ, status string
		list, rule        int64
	}{
		{"evil.example", "A", StatusBlockedIP, 5, 0},
		{"evil6.example", "AAAA", StatusBlockedIP, 0, 3},
		{"mapped.example", "AAAA", StatusBlockedIP, 5, 0},
		{"nat64.example", "AAAA", StatusBlockedIP, 5, 0},
		{"hint.example", "HTTPS", StatusBlockedIP, 5, 0},
		{"extra.example", "A", StatusBlockedIP, 5, 0},
		{"allowed.example", "A", StatusForwarded, 0, 0},
		{"x.corp.example", "A", StatusForwarded, 0, 0},
		{"both.example", "A", StatusBlockedRebind, 0, 0},
		{"many.example", "A", StatusForwarded, 0, 0},
	}
	for _, c := range cases {
		res := e.lookupAs(c.name, c.typ, "192.168.1.50")
		if res.Status != c.status {
			t.Errorf("%s: status %s, want %s (%v)", c.name, res.Status, c.status, res.Steps)
		}
	}
	res := e.lookupAs("evil.example", "A", "192.168.1.50")
	if res.Reason != "Bad addresses: 198.51.100.66" || !slices.Equal(res.Answers, []string{"evil.example.\t10\tIN\tA\t0.0.0.0"}) ||
		!slices.ContainsFunc(res.Steps, func(s string) bool { return s == `response IP 198.51.100.66 blocked by list "Bad addresses"` }) {
		t.Errorf("evil.example %+v", res)
	}
	if res := e.lookupAs("evil6.example", "AAAA", "192.168.1.50"); !slices.ContainsFunc(res.Steps, func(s string) bool {
		return s == `response IP 2001:db8::66 blocked by IP rule "2001:db8::66"`
	}) {
		t.Errorf("IP rule trace %v", res.Steps)
	}
	// Logged: status, reason, list, purpose, the upstream's answer; EDE 15
	// carries the list name only.
	m := e.answerOf("evil.example", dns.TypeA)
	ev := e.logs.waitEvent(t, "evil.example", 0)
	if ev.Status != StatusBlockedIP || ev.ListID != 5 || ev.Purpose != filter.CategorySecurity || ev.UpstreamAnswer == "" ||
		ev.Reason != "Bad addresses: 198.51.100.66" {
		t.Errorf("event %+v", ev)
	}
	e.answerOf("evil6.example", dns.TypeAAAA)
	if ev := e.logs.waitEvent(t, "evil6.example", 0); ev.RuleID != 3 || ev.Purpose != PurposeRule {
		t.Errorf("IP rule event %+v", ev)
	}
	req := new(dns.Msg)
	req.SetQuestion("evil.example.", dns.TypeA)
	req.SetEdns0(1232, false)
	w := udpFrom("192.168.1.50")
	(&dnsHandler{s: e.srv, ctx: t.Context()}).ServeDNS(w, req)
	if ede := edeOf(w.msgs[0]); ede == nil || ede.ExtraText != "Bad addresses" {
		t.Errorf("EDE %q (%v)", ede, m)
	}
	// This machine's address is never blocked (rebind protection off here).
	e.update(func(a *settings.All) { a.DNS.RebindProtection = false })
	if res := e.lookupAs("own.example", "A", "192.168.1.50"); res.Status != StatusForwarded {
		t.Errorf("own address %+v", res)
	}
	// DNS64: the synthesised records embed a blocked IPv4 address.
	e.update(func(a *settings.All) { a.DNS.DNS64.Enabled = true })
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		m := new(dns.Msg)
		m.SetReply(req)
		if q.Qtype == dns.TypeA {
			m.Answer = []dns.RR{a(q.Name, "198.51.100.66")}
		}
		return m, upstream.Info{Upstream: "u"}, nil
	})
	if res := e.lookupAs("v4.example", "AAAA", "192.168.1.50"); res.Status != StatusBlockedIP {
		t.Errorf("DNS64 %+v", res)
	}
	// Lists judge an embedding address only by its IPv4 address: a list
	// entry around the DNS64 prefix blocks no synthesised answer (the IP
	// guard cannot know a configured prefix); an IP rule may name the IPv6
	// form.
	e.update(func(a *settings.All) { a.DNS.DNS64.Prefix = "2001:db8:64::/96" })
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		m := new(dns.Msg)
		m.SetReply(req)
		if q.Qtype == dns.TypeA {
			m.Answer = []dns.RR{a(q.Name, "203.0.113.9")}
		}
		return m, upstream.Info{Upstream: "u"}, nil
	})
	synth := netip.MustParseAddr("2001:db8:64::cb00:7109")
	e.flt.ips[synth] = ipBlock("Wide", 6)
	if res := e.lookupAs("list64.example", "AAAA", "192.168.1.50"); res.Status != StatusForwarded ||
		!slices.Equal(res.Answers, []string{"list64.example.\t60\tIN\tAAAA\t2001:db8:64::cb00:7109"}) {
		t.Errorf("a list entry around the DNS64 prefix: %+v", res)
	}
	e.flt.ips[synth] = filter.Decision{Action: filter.ActionBlock, Source: "ip-rule", Kind: "ip", RuleID: 4, Name: synth.String()}
	if res := e.lookupAs("rule64.example", "AAAA", "192.168.1.50"); res.Status != StatusBlockedIP {
		t.Errorf("an IP rule for the synthesised address: %+v", res)
	}
	// Blocking off: nothing is checked.
	e.update(func(a *settings.All) { a.Filter.Enabled = false })
	if res := e.lookupAs("v4.example", "A", "192.168.1.50"); res.Status != StatusForwarded {
		t.Errorf("blocking off %+v", res)
	}
}

// The upstreams of a group answer its clients instead of the default set
// (step 13, default forwarders); a failure is SERVFAIL with the group's
// name, never the default set; forwarders with explicit targets keep
// precedence.
func TestGroupUpstreamsPipeline(t *testing.T) {
	e := newEnv(t, nil)
	e.up.groups = map[int64]string{2: "Kids"}
	e.cl.set(&clients.Identity{IP: netip.MustParseAddr("192.168.1.60"), ClientID: 4, GroupIDs: []int64{1, 2}})
	if _, err := e.srv.CreateForwarder(t.Context(), ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.CreateForwarder(t.Context(), ForwarderInput{Domain: "public.example", Upstreams: []string{DefaultTarget}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	res := e.lookupAs("www.example.net", "A", "192.168.1.60")
	if res.Status != StatusForwarded || !slices.Equal(e.up.last().via, []string{"group:2"}) ||
		!slices.ContainsFunc(res.Steps, func(s string) bool { return strings.HasPrefix(s, "answered via group upstreams of Kids (") }) {
		t.Fatalf("group set %+v %v", res, e.up.last())
	}
	e.lookupAs("www.public.example", "A", "192.168.1.60")
	if !slices.Equal(e.up.last().via, []string{"group:2"}) {
		t.Fatalf("default forwarder %+v", e.up.last())
	}
	e.lookupAs("x.corp.example", "A", "192.168.1.60")
	if !slices.Equal(e.up.last().via, []string{"10.9.9.9"}) {
		t.Fatalf("explicit forwarder %+v", e.up.last())
	}
	e.lookupAs("www.example.net", "A", "192.168.1.50")
	if last := e.up.last(); len(last.via) != 0 {
		t.Fatalf("a client without the group %+v", last)
	}
	e.up.groupFail = errors.New("all upstreams failed")
	res = e.lookupAs("fail.example.net", "A", "192.168.1.60")
	if res.Status != StatusError || res.RCode != "SERVFAIL" || !strings.HasPrefix(res.Reason, "group upstreams of Kids failed: all upstreams failed") {
		t.Fatalf("failure %+v", res)
	}
	for _, c := range e.up.callsFor("fail.example.net") {
		if len(c.via) == 0 {
			t.Fatal("the default set answered for a group")
		}
	}
	// A group set's answers are default answers for the later steps:
	// upstream blocks are classified, the rebind check and DNS64 apply;
	// paused blocking does not change the set.
	e.up.groupFail = nil
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		m := new(dns.Msg)
		m.SetReply(req)
		if !slices.Equal(via, []string{"group:2"}) {
			return upAnswer(req), upstream.Info{Upstream: "default"}, nil
		}
		info := upstream.Info{Upstream: "https://family.example/dns-query"}
		switch normalizeName(q.Name) {
		case "adult.example":
			m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, 60), A: net.IPv4zero.To4()}}
			info.Block = &upstream.BlockInfo{Kind: upstream.BlockNullIP, Host: "family.example"}
		case "rebind.example":
			m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, 60), A: net.ParseIP("192.168.9.9").To4()}}
		case "v4only.example":
			if q.Qtype == dns.TypeA {
				m.Answer = []dns.RR{&dns.A{Hdr: rrHeader(q.Name, dns.TypeA, 60), A: net.ParseIP("203.0.113.5").To4()}}
			}
		}
		return m, info, nil
	})
	if res := e.lookupAs("adult.example", "A", "192.168.1.60"); res.Status != StatusBlockedUpstream || res.Reason != "family.example: null-ip" {
		t.Errorf("upstream block %+v", res)
	}
	if res := e.lookupAs("rebind.example", "A", "192.168.1.60"); res.Status != StatusBlockedRebind {
		t.Errorf("rebind %+v", res)
	}
	e.update(func(a *settings.All) { a.DNS.DNS64.Enabled = true })
	if res := e.lookupAs("v4only.example", "AAAA", "192.168.1.60"); !slices.Equal(values(res), []string{"64:ff9b::cb00:7105"}) {
		t.Errorf("DNS64 %+v", res)
	}
	for _, c := range e.up.callsFor("v4only.example") {
		if !slices.Equal(c.via, []string{"group:2"}) {
			t.Errorf("DNS64 asked %v", c.via)
		}
	}
	if _, err := e.srv.SetBlocking(t.Context(), false, time.Minute); err != nil {
		t.Fatal(err)
	}
	e.lookupAs("paused.example", "A", "192.168.1.60")
	if !slices.Equal(e.up.last().via, []string{"group:2"}) {
		t.Errorf("paused blocking %+v", e.up.last())
	}
}

// dns.serverNameAddresses answer this server's names for every
// non-loopback client and their PTR queries; loopback clients keep
// loopback.
func TestServerNameAddresses(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.ServerNameAddresses.IPv4 = []string{"192.168.1.2"}
		a.DNS.ServerNameAddresses.IPv6 = []string{"fd00::2", "2001:db8::2"}
	})
	if got := values(e.lookupAs("picache.lan", "A", "192.168.1.50")); !slices.Equal(got, []string{"192.168.1.2"}) {
		t.Errorf("A %v", got)
	}
	if got := values(e.lookupAs("picache", "AAAA", "10.8.0.5")); !slices.Equal(got, []string{"fd00::2", "2001:db8::2"}) {
		t.Errorf("AAAA %v", got)
	}
	if got := values(e.lookupAs("picache.lan", "A", "127.0.0.1")); !slices.Equal(got, []string{}) && !slices.Equal(got, []string{"192.168.1.10"}) {
		t.Errorf("loopback %v", got)
	}
	if got := values(e.lookupAs("2.1.168.192.in-addr.arpa", "PTR", "192.168.1.50")); !slices.Equal(got, []string{"picache.lan."}) {
		t.Errorf("PTR %v", got)
	}
	e.update(func(a *settings.All) { a.DNS.ServerNameAddresses.IPv4 = nil })
	if got := values(e.lookupAs("picache.lan", "A", "192.168.1.50")); !slices.Equal(got, []string{testCacheIP.String()}) {
		t.Errorf("automatic %v", got)
	}
}
