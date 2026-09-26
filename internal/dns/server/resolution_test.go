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

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// answerWith builds an upstream reply to req with the given records.
func answerWith(req *dns.Msg, rrs ...dns.RR) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.RecursionAvailable = true
	m.Answer = rrs
	return m
}

func aRec(name, ip string) dns.RR {
	return &dns.A{Hdr: rrHeader(dns.Fqdn(name), dns.TypeA, 300), A: net.ParseIP(ip).To4()}
}

func aaaaRec(name, ip string) dns.RR {
	return &dns.AAAA{Hdr: rrHeader(dns.Fqdn(name), dns.TypeAAAA, 300), AAAA: net.ParseIP(ip)}
}

func cnameRec(name, target string) dns.RR {
	return &dns.CNAME{Hdr: rrHeader(dns.Fqdn(name), dns.TypeCNAME, 300), Target: dns.Fqdn(target)}
}

// edeOf returns the EDE option of a reply (nil if none).
func edeOf(m *dns.Msg) *dns.EDNS0_EDE {
	if opt := m.IsEdns0(); opt != nil {
		for _, o := range opt.Option {
			if e, ok := o.(*dns.EDNS0_EDE); ok {
				return e
			}
		}
	}
	return nil
}

const profileDoH = "https://dns.example/dns-query/profile-5ecret"

// Step 13a: an answer the default upstreams blocked themselves becomes the
// blocking reply, status blocked-upstream, reason "<host>: <kind>"; the EDE
// text sent to the client names the kind only (never the host, which can
// carry a profile ID, the DoH path or the upstream's own text); fresh and
// cached answers alike, while blocking is paused and for allowlisted
// names; never for forwarder answers.
func TestUpstreamBlocked(t *testing.T) {
	e := newEnv(t, nil)
	block := &upstream.BlockInfo{Kind: upstream.BlockNullIP, Host: "dns.example"}
	ede := &upstream.EDE{Code: 15, Text: "blocked by your profile"}
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		name := normalizeName(req.Question[0].Name)
		info := upstream.Info{Upstream: profileDoH, Block: block, EDE: ede}
		switch {
		case via != nil:
			return answerWith(req, aRec(name, "0.0.0.0")), upstream.Info{Upstream: "10.9.9.9", Block: block}, nil
		case strings.HasPrefix(name, "cached"):
			info = upstream.Info{Cached: true, Block: block, EDE: ede}
		case strings.HasPrefix(name, "stale"):
			info = upstream.Info{Cached: true, Stale: true, Block: block}
		case strings.HasPrefix(name, "clean"):
			return upAnswer(req), upstream.Info{Upstream: profileDoH, EDE: &upstream.EDE{Code: 3, Text: "stale answer"}}, nil
		case strings.HasPrefix(name, "profile"): // tls://abc123.dns.example: the ID is in the host
			info = upstream.Info{Upstream: "tls://abc123.dns.example", Block: &upstream.BlockInfo{Kind: upstream.BlockNXDomainNoRA, Host: "abc123.dns.example"}}
		}
		return answerWith(req, aRec(name, "0.0.0.0")), info, nil
	})
	e.flt.check["allowed.example"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Name: "@@allowed.example"}
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.serve()
	for _, tc := range []struct {
		name, upstream string
	}{
		{"malware.example", profileDoH},
		{"cached.example", ""},
		{"stale.example", ""},
		{"allowed.example", profileDoH},
	} {
		r := e.query("udp", tc.name, dns.TypeA, withEDNS(1232, false))
		if got := answerIPs(r.Answer); !slices.Equal(got, []string{"0.0.0.0"}) || r.Answer[0].Header().Ttl != 10 {
			t.Errorf("%s: answer %v (the blocking reply with filter.blockedTtl)", tc.name, r.Answer)
		}
		if x := edeOf(r); x == nil || x.InfoCode != 15 || x.ExtraText != "blocked by upstream (null-ip)" {
			t.Errorf("%s: EDE %v", tc.name, x)
		}
		ev := e.logs.waitEvent(t, tc.name, 0)
		if ev.Status != StatusBlockedUpstream || ev.Reason != "dns.example: null-ip" || ev.Upstream != tc.upstream {
			t.Errorf("%s: logged %+v", tc.name, ev)
		}
	}
	if ev := e.logs.waitEvent(t, "malware.example", 0); ev.UpstreamEDE == nil || ev.UpstreamEDE.Code != 15 || ev.UpstreamEDE.Text != "blocked by your profile" {
		t.Errorf("upstream EDE not logged: %+v", ev.UpstreamEDE)
	}
	// A host name with an account or profile ID is logged, never sent.
	r := e.query("udp", "profile.example", dns.TypeA, withEDNS(1232, false))
	if x := edeOf(r); x == nil || x.ExtraText != "blocked by upstream (nxdomain-no-ra)" || strings.Contains(x.ExtraText, "abc123") {
		t.Errorf("profile ID sent to the client: %v", x)
	}
	if ev := e.logs.waitEvent(t, "profile.example", 0); ev.Reason != "abc123.dns.example: nxdomain-no-ra" {
		t.Errorf("profile: logged %+v", ev)
	}
	// Paused blocking does not lift it.
	if _, err := e.srv.SetBlocking(context.Background(), false, time.Minute); err != nil {
		t.Fatal(err)
	}
	e.query("udp", "paused.example", dns.TypeA)
	if ev := e.logs.waitEvent(t, "paused.example", 0); ev.Status != StatusBlockedUpstream {
		t.Errorf("paused: %+v", ev)
	}
	// A forwarder answer is never an upstream block.
	e.query("udp", "host.corp.example", dns.TypeA)
	if ev := e.logs.waitEvent(t, "host.corp.example", 0); ev.Status != StatusForwarded {
		t.Errorf("forwarder answer: %+v", ev)
	}
	// Any EDE of the upstream is logged, blocked or not.
	e.query("udp", "clean.example", dns.TypeA)
	if ev := e.logs.waitEvent(t, "clean.example", 0); ev.Status != StatusForwarded || ev.UpstreamEDE == nil || ev.UpstreamEDE.Code != 3 {
		t.Errorf("clean answer: %+v", ev)
	}
	res, _ := e.srv.Lookup(context.Background(), LookupRequest{Name: "malware.example"}, netip.MustParseAddr("192.168.1.5"))
	if res.Status != StatusBlockedUpstream || !slices.Contains(res.Steps, "upstream block: dns.example: null-ip") {
		t.Errorf("lookup %+v", res)
	}
}

// Step 13b: default-set answers with an address in dns.bogusNxdomain
// become NXDOMAIN (status special, reason bogus-nxdomain), also while
// blocking is paused; forwarder answers are left alone; an upstream block
// wins, and a private bogus address is bogus, not rebinding.
func TestBogusNXDomain(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.BogusNXDomain = []string{"198.51.100.0/24", "10.0.0.1"}
		a.Filter.BlockedTTL = 42
	})
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		name := normalizeName(req.Question[0].Name)
		switch name {
		case "private.example":
			return answerWith(req, aRec(name, "10.0.0.1")), upstream.Info{Upstream: "u"}, nil
		case "blocked.example":
			return answerWith(req, aRec(name, "198.51.100.9")), upstream.Info{Upstream: "u", Block: &upstream.BlockInfo{Kind: "ede", Host: "u"}}, nil
		}
		return upAnswer(req), upstream.Info{Upstream: "u"}, nil // 198.51.100.7
	})
	e.serve()
	r := e.query("udp", "typo.example", dns.TypeA)
	if r.Rcode != dns.RcodeNameError || len(r.Ns) != 1 || r.Ns[0].(*dns.SOA).Minttl != 42 || r.Ns[0].Header().Ttl != 42 {
		t.Fatalf("bogus answer: %v", r)
	}
	if ev := e.logs.waitEvent(t, "typo.example", 0); ev.Status != StatusSpecial || ev.Reason != ReasonBogusNXDomain {
		t.Errorf("logged %+v", ev)
	}
	e.query("udp", "private.example", dns.TypeA)
	if ev := e.logs.waitEvent(t, "private.example", 0); ev.Status != StatusSpecial || ev.Reason != ReasonBogusNXDomain {
		t.Errorf("a private bogus address: %+v, want bogus-nxdomain (not rebind)", ev)
	}
	e.query("udp", "blocked.example", dns.TypeA)
	if ev := e.logs.waitEvent(t, "blocked.example", 0); ev.Status != StatusBlockedUpstream {
		t.Errorf("upstream block and bogus: %+v", ev)
	}
	if r := e.query("udp", "host.corp.example", dns.TypeA); r.Rcode != dns.RcodeSuccess {
		t.Errorf("a forwarder answer was turned into NXDOMAIN")
	}
	if _, err := e.srv.SetBlocking(context.Background(), false, 0); err != nil {
		t.Fatal(err)
	}
	if r := e.query("udp", "typo2.example", dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Error("bogus NXDOMAIN must apply while blocking is disabled")
	}
}

// httpsWithHints builds an HTTPS record with IPv4 and IPv6 hints.
func httpsWithHints(name string, v4 []string, v6 []string) dns.RR {
	rr := &dns.HTTPS{SVCB: dns.SVCB{Hdr: rrHeader(dns.Fqdn(name), dns.TypeHTTPS, 300), Priority: 1, Target: "."}}
	var h4, h6 []net.IP
	for _, s := range v4 {
		h4 = append(h4, net.ParseIP(s).To4())
	}
	for _, s := range v6 {
		h6 = append(h6, net.ParseIP(s))
	}
	rr.Value = append(rr.Value, &dns.SVCBAlpn{Alpn: []string{"h2"}}, &dns.SVCBIPv4Hint{Hint: h4}, &dns.SVCBIPv6Hint{Hint: h6})
	return rr
}

// Step 14c: answers of the default set that point at rebind targets are
// blocked (status blocked-rebind, reason "rebind: <address>"), also while
// blocking is paused; hints and additional records are cleaned otherwise;
// the exemptions, forwarder answers and the collisions with 13a and DNS64.
func TestRebindProtection(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.Web.AllowedHosts = []string{"picache.example.net", "192.168.1.10"}
		a.DNS.RebindAllow = []string{"plex.direct", "nas.example.org"}
	})
	ctx := context.Background()
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "corp.example", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "public.corp.example", Upstreams: []string{"default"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.flt.rules["allow-rule.example"] = filter.Decision{Action: filter.ActionAllow, Source: "rule", Kind: "exact", Name: "@@allow-rule.example"}
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		q := req.Question[0]
		name := normalizeName(q.Name)
		info := upstream.Info{Upstream: "u"}
		switch {
		case name == "null.example":
			return answerWith(req, aRec(name, "0.0.0.0")), upstream.Info{Upstream: "u", Block: &upstream.BlockInfo{Kind: "null-ip", Host: "u"}}, nil
		case name == "chain.example":
			return answerWith(req, cnameRec(name, "inner.example"), aRec("inner.example", "10.1.2.3")), info, nil
		case name == "v6.example":
			return answerWith(req, aaaaRec(name, "::ffff:192.168.1.1")), info, nil
		case name == "hints.example":
			m := answerWith(req, httpsWithHints(name, []string{"10.0.0.1", "8.8.8.8"}, []string{"fd00::1"}))
			m.Extra = []dns.RR{aRec(name, "192.168.1.1"), aRec(name, "8.8.4.4")}
			m.AuthenticatedData = true
			return m, info, nil
		case name == "dns64.example" && q.Qtype == dns.TypeAAAA:
			return answerWith(req), info, nil
		case name == "dns64.example":
			return answerWith(req, aRec(name, "10.0.0.1")), info, nil
		case q.Qtype == dns.TypeA:
			return answerWith(req, aRec(name, "192.168.1.50")), info, nil
		}
		return upAnswer(req), info, nil
	})
	e.serve()
	status := func(name string, qtype uint16) (string, string) {
		t.Helper()
		before := e.logs.count(name)
		e.query("udp", name, qtype)
		ev := e.logs.waitEvent(t, name, before)
		return ev.Status, ev.Reason
	}
	for _, tc := range []struct {
		name   string
		qtype  uint16
		status string
		reason string
	}{
		{"evil.example", dns.TypeA, StatusBlockedRebind, "rebind: 192.168.1.50"},
		{"chain.example", dns.TypeA, StatusBlockedRebind, "rebind: 10.1.2.3"},
		{"v6.example", dns.TypeAAAA, StatusBlockedRebind, "rebind: 192.168.1.1"},
		{"x.plex.direct", dns.TypeA, StatusForwarded, ""},       // dns.rebindAllow subtree
		{"nas.example.org", dns.TypeA, StatusForwarded, ""},     // dns.rebindAllow
		{"sub.nas.example.org", dns.TypeA, StatusForwarded, ""}, // subtree
		{"picache.example.net", dns.TypeA, StatusForwarded, ""}, // web.allowedHosts (exact)
		{"x.picache.example.net", dns.TypeA, StatusBlockedRebind, "rebind: 192.168.1.50"},
		{"allow-rule.example", dns.TypeA, StatusForwarded, ""},                              // a user allow rule
		{"host.corp.example", dns.TypeA, StatusForwarded, ""},                               // explicit forwarder
		{"www.public.corp.example", dns.TypeA, StatusBlockedRebind, "rebind: 192.168.1.50"}, // default forwarder
		{"null.example", dns.TypeA, StatusBlockedUpstream, "u: null-ip"},                    // 13a first
	} {
		if st, reason := status(tc.name, tc.qtype); st != tc.status || reason != tc.reason {
			t.Errorf("%s: %s %q, want %s %q", tc.name, st, reason, tc.status, tc.reason)
		}
	}
	// Hints and additional records are cleaned, AD cleared; the status stays.
	r := e.query("udp", "hints.example", dns.TypeHTTPS, withEDNS(1232, true))
	h := r.Answer[0].(*dns.HTTPS)
	var v4 []string
	for _, kv := range h.Value {
		switch v := kv.(type) {
		case *dns.SVCBIPv4Hint:
			for _, ip := range v.Hint {
				v4 = append(v4, ip.String())
			}
		case *dns.SVCBIPv6Hint:
			t.Errorf("an ipv6hint left empty must be removed: %v", v)
		}
	}
	if !slices.Equal(v4, []string{"8.8.8.8"}) || r.AuthenticatedData || len(r.Extra) != 2 { // the kept A and our OPT
		t.Errorf("hints %v, AD %v, extra %v", v4, r.AuthenticatedData, r.Extra)
	}
	if ev := e.logs.waitEvent(t, "hints.example", 0); ev.Status != StatusForwarded {
		t.Errorf("hints: %+v", ev)
	}
	// DNS64: the synthesised AAAA carries the private IPv4 address.
	e.update(func(a *settings.All) { a.DNS.DNS64.Enabled = true })
	if st, reason := status("dns64.example", dns.TypeAAAA); st != StatusBlockedRebind || reason != "rebind: 64:ff9b::a00:1" {
		t.Errorf("DNS64: %s %q", st, reason)
	}
	// Applied while blocking is paused; off with dns.rebindProtection.
	if _, err := e.srv.SetBlocking(ctx, false, time.Minute); err != nil {
		t.Fatal(err)
	}
	if st, _ := status("paused.example", dns.TypeA); st != StatusBlockedRebind {
		t.Errorf("paused: %s", st)
	}
	e.update(func(a *settings.All) { a.DNS.RebindProtection = false })
	if st, _ := status("off.example", dns.TypeA); st != StatusForwarded {
		t.Errorf("protection off: %s", st)
	}
	e.update(func(a *settings.All) { a.DNS.RebindProtection = true })
	res, _ := e.srv.Lookup(ctx, LookupRequest{Name: "x.plex.direct"}, netip.MustParseAddr("192.168.1.5"))
	if !slices.Contains(res.Steps, "rebind protection: exempt (dns.rebindAllow plex.direct)") {
		t.Errorf("lookup steps %v", res.Steps)
	}
	res, _ = e.srv.Lookup(ctx, LookupRequest{Name: "evil2.example"}, netip.MustParseAddr("192.168.1.5"))
	if res.Status != StatusBlockedRebind || !slices.Contains(res.Steps, "rebind protection: 192.168.1.50 blocked") {
		t.Errorf("lookup %+v", res)
	}
}

// The local zones are exempt from the rebind check (their names are
// answered at step 6 anyway, but a later settings change must not block
// them).
func TestRebindExemptLocalZones(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.host.Store(&hostInfo{search: []string{"corp.search"}})
	for name, want := range map[string]string{
		"nas.lan":            "local zone lan",
		"router.home.arpa":   "local zone home.arpa",
		"x.corp.search":      "local zone corp.search",
		"sub.x.plex.direct":  "dns.rebindAllow plex.direct",
		"public.example.com": "",
	} {
		m := new(dns.Msg)
		m.SetQuestion(dns.Fqdn(name), dns.TypeA)
		qc := newQuery(context.Background(), m, netip.MustParseAddr("192.168.1.5"), "udp", e.set.Get())
		qc.id = e.srv.identify(qc.source)
		why, ok := e.srv.rebindExempt(qc)
		if why != want || ok != (want != "") {
			t.Errorf("%s: %q %v, want %q", name, why, ok, want)
		}
	}
}
