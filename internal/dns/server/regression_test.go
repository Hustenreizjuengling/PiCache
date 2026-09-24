package dnsserver

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// A user regex deny must not beat a list allow entry for ordinary names
// (ARCHITECTURE 7.2: list allow is tier 8, user regex deny tier 10). The
// step-8 user-rule check applies only to LanCache override candidates.
func TestUserRulesBeforeOverridesOnlyForLanCacheNames(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.LanCache.Enabled = true }).serve()
	regexDeny := filter.Decision{Action: filter.ActionBlock, Source: "rule", Kind: "regex", RuleID: 4, Name: `^cdn\.`}
	listAllow := filter.Decision{Action: filter.ActionAllow, Source: "list", Kind: "subtree", ListID: 2, Name: "Allowlist"}

	// Ordinary name: the full-precedence decision (list allow) wins.
	e.flt.rules["cdn.example.com"] = regexDeny
	e.flt.check["cdn.example.com"] = listAllow
	r := e.query("udp", "cdn.example.com", dns.TypeA)
	if got := answerIPs(r.Answer); !slices.Equal(got, []string{"198.51.100.7"}) {
		t.Errorf("list allow must beat a user regex deny: answer %v", got)
	}
	if ev := e.logs.waitEvent(t, "cdn.example.com", 0); ev.Status != StatusForwarded {
		t.Errorf("status %q, want %q", ev.Status, StatusForwarded)
	}

	// LanCache name: user rules are checked before the override is answered.
	e.svc["cdn.steamcontent.com"] = "steam"
	e.flt.rules["cdn.steamcontent.com"] = regexDeny
	r = e.query("udp", "cdn.steamcontent.com", dns.TypeA)
	if got := answerIPs(r.Answer); !slices.Equal(got, []string{"0.0.0.0"}) {
		t.Errorf("a user rule must still block a LanCache name: answer %v", got)
	}
	if ev := e.logs.waitEvent(t, "cdn.steamcontent.com", 0); ev.Status != StatusBlockedRegex {
		t.Errorf("status %q, want %q", ev.Status, StatusBlockedRegex)
	}

	// Override inactive (client bypasses LanCache): the normal verdict applies.
	e.svc["dl.steamcontent.com"] = "steam"
	e.flt.rules["dl.steamcontent.com"] = regexDeny
	e.flt.check["dl.steamcontent.com"] = listAllow
	e.lcReady.Store(false)
	r = e.query("udp", "dl.steamcontent.com", dns.TypeA)
	if got := answerIPs(r.Answer); !slices.Equal(got, []string{"198.51.100.7"}) {
		t.Errorf("without an override the full precedence applies: answer %v", got)
	}
}

// A local domain below a special-use zone (home.internal, corp.local) still
// reaches the router resolver (ARCHITECTURE 7.1 step 6).
func TestLocalDomainBelowSpecialZoneUsesRouter(t *testing.T) {
	tests := []struct {
		localDomain string
		search      []string
		qname       string
		router      bool
	}{
		{"home.internal", nil, "nas.home.internal", true},
		{"home.internal", nil, "other.internal", false}, // plain special-use zone
		{"local", nil, "nas.local", true},               // configured local domain wins a tie
		{"lan", []string{"corp.local"}, "pc.corp.local", true},
		{"lan", nil, "printer.local", false},
		{"onion", nil, "abc.onion", false}, // never resolved, whatever is configured
		{"my.onion", nil, "abc.my.onion", false},
		{"lan", []string{"x.invalid"}, "a.x.invalid", false},
		{"lan", nil, "nas.lan", true},
	}
	for _, tc := range tests {
		t.Run(tc.qname, func(t *testing.T) {
			e := newEnv(t, func(a *settings.All) {
				a.DNS.LocalDomain = tc.localDomain
				a.DNS.RouterResolver = "192.168.1.1"
			})
			if tc.search != nil {
				base := e.srv.env.host
				e.srv.env.host = func() *hostInfo {
					h := base()
					h.search = tc.search
					return h
				}
				e.srv.host.Store(e.srv.env.host())
			}
			e.serve()
			r := e.query("udp", tc.qname, dns.TypeA)
			calls := e.up.callsFor(tc.qname)
			if tc.router {
				if len(calls) != 1 || !slices.Equal(calls[0].via, []string{"192.168.1.1:53"}) {
					t.Fatalf("%s must go to the router resolver, calls %v (rcode %s)", tc.qname, calls, dns.RcodeToString[r.Rcode])
				}
				return
			}
			if len(calls) != 0 || r.Rcode != dns.RcodeNameError {
				t.Fatalf("%s must be NXDOMAIN without any upstream, calls %v rcode %s", tc.qname, calls, dns.RcodeToString[r.Rcode])
			}
		})
	}
}

// Local CNAMEs pointing at names PiCache answers itself are resolved
// locally instead of NXDOMAIN.
func TestLocalCNAMEToSpecialUseTarget(t *testing.T) {
	e := newEnv(t, nil).serve()
	e.addRecord("dns.example", "CNAME", "picache.lan")
	e.addRecord("lh.example", "CNAME", "localhost")
	e.addRecord("ra.example", "CNAME", "_dns.resolver.arpa")
	tests := []struct {
		name  string
		qtype uint16
		ips   []string
	}{
		{"dns.example", dns.TypeA, []string{"192.168.1.10"}},
		{"dns.example", dns.TypeAAAA, []string{"fd00::10"}},
		{"lh.example", dns.TypeA, []string{"127.0.0.1"}},
		{"lh.example", dns.TypeAAAA, []string{"::1"}},
		{"ra.example", dns.TypeA, nil},
		{"lh.example", dns.TypeTXT, nil},
	}
	for _, tc := range tests {
		r := e.query("udp", tc.name, tc.qtype)
		if r.Rcode != dns.RcodeSuccess {
			t.Errorf("%s %s: rcode %s, want NOERROR", tc.name, dns.TypeToString[tc.qtype], dns.RcodeToString[r.Rcode])
			continue
		}
		if len(r.Answer) == 0 || r.Answer[0].Header().Rrtype != dns.TypeCNAME {
			t.Errorf("%s: the CNAME must come first: %v", tc.name, r.Answer)
		}
		if got := answerIPs(r.Answer); !slices.Equal(got, tc.ips) {
			t.Errorf("%s %s: answer %v, want %v", tc.name, dns.TypeToString[tc.qtype], got, tc.ips)
		}
		if tc.ips == nil && len(r.Ns) != 1 {
			t.Errorf("%s %s: NODATA needs the synthetic SOA, got %v", tc.name, dns.TypeToString[tc.qtype], r.Ns)
		}
	}
	for _, n := range []string{"picache.lan", "localhost", "_dns.resolver.arpa"} {
		if c := e.up.callsFor(n); len(c) != 0 {
			t.Errorf("%s must never be sent upstream: %v", n, c)
		}
	}
}

// Server-name answers use the addresses on the client's subnet, else the
// primary address; never unrelated (virtual) interfaces, and loopback only
// for loopback clients.
func TestServerNameAddrsFor(t *testing.T) {
	p := netip.MustParsePrefix
	h := &hostInfo{
		ifaces: []hostIface{
			{name: "lo", prefixes: []netip.Prefix{p("127.0.0.1/8"), p("::1/128")}},
			{name: "eth0", prefixes: []netip.Prefix{p("192.168.1.248/24"), p("fd00::248/64"), p("2001:db8:1::248/64"), p("fe80::1/64")}},
			{name: "vEthernet (Default Switch)", prefixes: []netip.Prefix{p("172.29.0.1/20"), p("fe80::2/64")}},
			{name: "docker0", prefixes: []netip.Prefix{p("172.17.0.1/16")}},
		},
		primary4: netip.MustParseAddr("192.168.1.248"),
		primary6: netip.MustParseAddr("2001:db8:1::248"),
	}
	tests := []struct {
		client string
		v4, v6 []string
	}{
		{"192.168.1.50", []string{"192.168.1.248"}, []string{"fd00::248", "2001:db8:1::248"}},
		{"10.8.0.9", []string{"192.168.1.248"}, []string{"2001:db8:1::248"}}, // routed client: primary only
		{"172.29.5.5", []string{"172.29.0.1"}, []string{"2001:db8:1::248"}},    // on the virtual switch
		{"172.17.0.5", []string{"172.17.0.1"}, []string{"2001:db8:1::248"}},
		{"127.0.0.1", []string{"127.0.0.1"}, []string{"::1"}},
		{"::1", []string{"127.0.0.1"}, []string{"::1"}},
		{"fd00::77", []string{"192.168.1.248"}, []string{"fd00::248"}},
		{"fe80::99", []string{"192.168.1.248"}, []string{"2001:db8:1::248"}}, // link-local: ambiguous, primary
	}
	for _, tc := range tests {
		c := netip.MustParseAddr(tc.client)
		if got := addrStrings(h.addrsFor(c, false)); !slices.Equal(got, tc.v4) {
			t.Errorf("client %s A = %v, want %v", tc.client, got, tc.v4)
		}
		if got := addrStrings(h.addrsFor(c, true)); !slices.Equal(got, tc.v6) {
			t.Errorf("client %s AAAA = %v, want %v", tc.client, got, tc.v6)
		}
	}
	// No primary address: nothing (NODATA) rather than an unrelated address.
	h.primary4 = netip.Addr{}
	if got := h.addrsFor(netip.MustParseAddr("10.8.0.9"), false); len(got) != 0 {
		t.Errorf("without a primary address: %v", got)
	}
}

// The cache-IP status reports the would-be addresses and detection warnings
// while LanCache is disabled.
func TestCacheIPsWhileDisabled(t *testing.T) {
	e := newEnv(t, nil) // LanCache is disabled by default
	e.srv.env.primary = func() (netip.Addr, error) { return netip.MustParseAddr("100.64.1.2"), nil }
	e.srv.env.ifaces = func() ([]interfaceIPv4, bool) {
		return []interfaceIPv4{{iface: "eth0", ip: netip.MustParseAddr("192.168.1.5")}}, false
	}
	e.srv.updateCacheIPs(e.set.Get())
	st := e.srv.CacheIPs()
	if st.Ready || st.Reason != "LanCache is disabled" {
		t.Errorf("ready %v reason %q", st.Ready, st.Reason)
	}
	if !slices.Equal(st.IPv4, []string{"192.168.1.5"}) || !st.Auto {
		t.Errorf("would-be addresses %v auto %v", st.IPv4, st.Auto)
	}
	if !strings.Contains(st.Warning, "100.64.1.2") || !strings.Contains(st.Warning, "carrier-grade NAT") {
		t.Errorf("warning %q must name the CGNAT primary address", st.Warning)
	}

	// Container bridge: no address, a warning, and not ready once enabled.
	e.srv.env.container = "docker"
	e.srv.env.primary = func() (netip.Addr, error) { return netip.MustParseAddr("172.18.0.2"), nil }
	e.srv.env.ifaces = func() ([]interfaceIPv4, bool) { return nil, false }
	e.srv.updateCacheIPs(e.set.Get())
	st = e.srv.CacheIPs()
	if len(st.IPv4) != 0 || !strings.Contains(st.Warning, "container bridge") || st.Reason != "LanCache is disabled" {
		t.Errorf("bridge while disabled: %+v", st)
	}
	e.update(func(a *settings.All) { a.LanCache.Enabled = true })
	st = e.srv.CacheIPs()
	if st.Ready || st.Reason != st.Warning || st.Warning == "" {
		t.Errorf("bridge while enabled: %+v", st)
	}

	// Enabled and ready: the warning is also kept in reason (compatibility).
	e.srv.env.container = ""
	e.srv.env.primary = func() (netip.Addr, error) { return netip.MustParseAddr("93.184.215.14"), nil }
	e.srv.env.ifaces = func() ([]interfaceIPv4, bool) {
		return []interfaceIPv4{{iface: "eth0", ip: netip.MustParseAddr("192.168.1.5")}}, false
	}
	e.srv.updateCacheIPs(e.set.Get())
	st = e.srv.CacheIPs()
	if !st.Ready || !strings.Contains(st.Warning, "public address") || st.Reason != st.Warning {
		t.Errorf("ready with warning: %+v", st)
	}
}

// On the real machine, loopback addresses are answered only to loopback
// clients and a routed client gets at most the primary addresses.
func TestLoadHostInfoAnswers(t *testing.T) {
	h := loadHostInfo()
	for _, c := range []string{"198.51.100.77", "192.0.2.77", "2001:db8:ffff::1"} {
		for _, v6 := range []bool{false, true} {
			got := h.addrsFor(netip.MustParseAddr(c), v6)
			for _, ip := range got {
				if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
					t.Errorf("client %s: answered %s", c, ip)
				}
			}
			if len(got) > 1 {
				t.Errorf("client %s (v6 %v): an unrelated client must get only the primary address, got %v", c, v6, got)
			}
		}
	}
	t.Logf("primary %v / %v; for 127.0.0.1: %v %v; own %v", h.primary4, h.primary6,
		h.addrsFor(netip.MustParseAddr("127.0.0.1"), false), h.addrsFor(netip.MustParseAddr("127.0.0.1"), true), h.own)
}

// In a container bridge network every client arrives from the bridge
// gateway, which shares the container's subnet. Server names must then be
// answered with the configured cache address (the host's LAN address), never
// with the unreachable bridge address.
func TestServerNameInBridge(t *testing.T) {
	e := newEnv(t, nil)
	bridgeIP := netip.MustParseAddr("172.18.0.2")
	e.srv.env.container = "docker"
	e.srv.env.primary = func() (netip.Addr, error) { return bridgeIP, nil }
	e.srv.env.ifaces = func() ([]interfaceIPv4, bool) { return nil, false }
	e.srv.env.host = func() *hostInfo {
		return &hostInfo{
			ifaces:   []hostIface{{name: "eth0", prefixes: []netip.Prefix{netip.PrefixFrom(bridgeIP, 16)}}},
			own:      []netip.Addr{bridgeIP},
			primary4: bridgeIP,
		}
	}
	e.srv.host.Store(e.srv.env.host())
	e.srv.updateCacheIPs(e.set.Get())

	answer := func(client string) []string {
		t.Helper()
		w := udpFrom(client)
		e.handle(w, "picache.lan", dns.TypeA)
		if len(w.msgs) != 1 || w.msgs[0].Rcode != dns.RcodeSuccess {
			t.Fatalf("client %s: %v", client, w.msgs)
		}
		var out []string
		for _, rr := range w.msgs[0].Answer {
			if a, ok := rr.(*dns.A); ok {
				out = append(out, a.A.String())
			}
		}
		return out
	}
	if got := answer("172.18.0.1"); len(got) != 0 {
		t.Errorf("without a cache IP the bridge address must not be answered: %v", got)
	}
	e.update(func(a *settings.All) { a.LanCache.CacheIPv4 = []string{"192.168.1.248"} })
	if got := answer("172.18.0.1"); !slices.Equal(got, []string{"192.168.1.248"}) {
		t.Errorf("bridge client got %v, want the configured host address", got)
	}
	if got := answer("127.0.0.1"); !slices.Equal(got, []string{"172.18.0.2"}) {
		t.Errorf("loopback client got %v, want the container's own address", got)
	}
}
