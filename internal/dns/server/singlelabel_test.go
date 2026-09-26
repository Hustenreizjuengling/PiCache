package dnsserver

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// singleLabelEnv: local domain lan, router 192.168.1.1, a forwarder for
// fwd.lan, and upstream answers by name: gone.* NXDOMAIN, empty.* NODATA,
// fail.* SERVFAIL.
func singleLabelEnv(t *testing.T, mutate func(*settings.All)) *testEnv {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.RouterResolver = "192.168.1.1"
		if mutate != nil {
			mutate(a)
		}
	})
	e.srv.d.Leases = &fakeLeases{}
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: "office.lan", Upstreams: []string{"10.9.9.9"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.up.setAnswer(func(req *dns.Msg, via []string) (*dns.Msg, upstream.Info, error) {
		name := normalizeName(req.Question[0].Name)
		switch {
		case strings.HasPrefix(name, "gone"):
			m := answerWith(req)
			m.Rcode = dns.RcodeNameError
			return m, upstream.Info{Upstream: "x"}, nil
		case strings.HasPrefix(name, "empty"):
			return answerWith(req), upstream.Info{Upstream: "x"}, nil
		case strings.HasPrefix(name, "fail"):
			m := answerWith(req)
			m.Rcode = dns.RcodeServerFailure
			return m, upstream.Info{Upstream: "x"}, nil
		}
		return upAnswer(req), upstream.Info{Upstream: "x"}, nil
	})
	return e.serve()
}

// With dns.domainNeeded (default on) bare names are answered as
// <name>.<localDomain>: local records, DHCP lease names, forwarders with
// explicit targets, the router, then (unqualified), else NXDOMAIN; other
// query types and the root keep the normal path.
func TestSingleLabelNames(t *testing.T) {
	e := singleLabelEnv(t, nil)
	e.addRecord("nas.lan", "A", "192.168.1.20")
	// Validating resolvers behind PiCache: other types go upstream.
	for _, q := range []struct {
		name  string
		qtype uint16
	}{{".", dns.TypeNS}, {".", dns.TypeDNSKEY}, {"com", dns.TypeDS}, {"com", dns.TypeDNSKEY}, {"de", dns.TypeNS}, {"nas", dns.TypeMX}} {
		r := e.query("udp", q.name, q.qtype)
		c := e.up.callsFor(normalizeName(q.name))
		if r.Rcode != dns.RcodeSuccess || len(c) == 0 || c[len(c)-1].via != nil || c[len(c)-1].qtype != q.qtype {
			t.Errorf("%s %s: rcode %d, calls %v (want the default upstreams)", q.name, dns.TypeToString[q.qtype], r.Rcode, c)
		}
	}
	// 1. A local record, answered with the client's owner name.
	r := e.query("udp", "NaS", dns.TypeA)
	if !slices.Equal(answerIPs(r.Answer), []string{"192.168.1.20"}) || r.Answer[0].Header().Name != "NaS." || r.Question[0].Name != "NaS." {
		t.Fatalf("nas: %v", r)
	}
	if ev := e.logs.waitEvent(t, "nas", 1); ev.Status != StatusLocal || ev.Reason != ReasonSingleLabel {
		t.Errorf("nas logged %+v", ev)
	}
	// 1. A DHCP lease name.
	if r := e.query("udp", "laptop", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"192.168.1.100"}) || r.Answer[0].Header().Name != "laptop." {
		t.Errorf("lease: %v", r)
	}
	// 2. A forwarder with explicit targets for the full name.
	e.update(func(a *settings.All) { a.DNS.LocalDomain = "office.lan" })
	r = e.query("udp", "printer", dns.TypeA)
	if c := e.up.callsFor("printer.office.lan"); len(c) != 1 || !slices.Equal(c[0].via, []string{"10.9.9.9"}) || r.Answer[0].Header().Name != "printer." {
		t.Errorf("forwarder: calls %v, answer %v", c, r.Answer)
	}
	if ev := e.logs.waitEvent(t, "printer", 0); ev.Status != StatusForwarded || ev.Reason != ReasonSingleLabel {
		t.Errorf("forwarder logged %+v", ev)
	}
	// 2 → 3: NXDOMAIN from the forwarder, then the router; NODATA is an answer.
	e.query("udp", "gone1", dns.TypeA)
	if c := e.up.callsFor("gone1.office.lan"); len(c) != 2 || !slices.Equal(c[1].via, []string{"192.168.1.1:53"}) {
		t.Errorf("NXDOMAIN must go on to the router: %v", c)
	}
	r = e.query("udp", "empty1", dns.TypeAAAA)
	if c := e.up.callsFor("empty1.office.lan"); len(c) != 1 || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Errorf("NODATA is an answer: calls %v, %v", c, r)
	}
	// 3. The router (a name outside every forwarder's domain).
	e.update(func(a *settings.All) { a.DNS.LocalDomain = "lan" })
	e.query("udp", "tv", dns.TypeA)
	if c := e.up.callsFor("tv.lan"); len(c) != 1 || !slices.Equal(c[0].via, []string{"192.168.1.1:53"}) {
		t.Errorf("router: %v", c)
	}
	// 5. Nothing answers: NXDOMAIN, never the default upstreams.
	r = e.query("udp", "gone2", dns.TypeA)
	if r.Rcode != dns.RcodeNameError || len(e.up.callsFor("gone2")) != 0 {
		t.Errorf("gone2: %v, calls %v", r, e.up.callsFor("gone2"))
	}
	if ev := e.logs.waitEvent(t, "gone2", 0); ev.Status != StatusSpecial || ev.Reason != ReasonSingleLabel {
		t.Errorf("gone2 logged %+v", ev)
	}
	for _, qtype := range []uint16{dns.TypeAAAA, dns.TypeHTTPS, dns.TypeSVCB} {
		e.query("udp", "fail1", qtype)
	}
	if c := e.up.callsFor("fail1"); len(c) != 0 {
		t.Errorf("a failure must not reach the default upstreams: %v", c)
	}
	// 4. The (unqualified) forwarder when nothing local answered.
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: Unqualified, Upstreams: []string{"10.8.8.8"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.query("udp", "gone3", dns.TypeA)
	if c := e.up.callsFor("gone3"); len(c) != 1 || !slices.Equal(c[0].via, []string{"10.8.8.8"}) {
		t.Errorf("(unqualified): %v", c)
	}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "nas"}, netip.MustParseAddr("192.168.1.5"))
	if err != nil || !slices.Contains(res.Steps, "single-label: answered as nas.lan by local record") {
		t.Errorf("lookup %+v %v", res, err)
	}
	res, _ = e.srv.Lookup(context.Background(), LookupRequest{Name: "gone4"}, netip.MustParseAddr("192.168.1.5"))
	if !slices.Contains(res.Steps, "single-label: (unqualified) forwarder") {
		t.Errorf("lookup steps %v", res.Steps)
	}
}

// wpad and isatap come from local records only: never from DHCP lease
// names, forwarders, the router or (unqualified).
func TestSingleLabelWPAD(t *testing.T) {
	e := singleLabelEnv(t, nil)
	e.srv.d.Leases = &wpadLeases{}
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: Unqualified, Upstreams: []string{"10.8.8.8"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"wpad", "isatap"} {
		r := e.query("udp", name, dns.TypeA)
		if r.Rcode != dns.RcodeNameError {
			t.Errorf("%s: %v", name, r)
		}
		if c := append(e.up.callsFor(name), e.up.callsFor(name+".lan")...); len(c) != 0 {
			t.Errorf("%s was sent upstream: %v", name, c)
		}
	}
	res, _ := e.srv.Lookup(context.Background(), LookupRequest{Name: "wpad"}, netip.MustParseAddr("192.168.1.5"))
	if !slices.Contains(res.Steps, "single-label: wpad only from local records") {
		t.Errorf("lookup steps %v", res.Steps)
	}
	e.addRecord("wpad.lan", "A", "192.168.1.3")
	if r := e.query("udp", "wpad", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"192.168.1.3"}) {
		t.Errorf("wpad local record: %v", r)
	}
}

// wpadLeases claims wpad.lan (a device calling itself wpad).
type wpadLeases struct{ fakeLeases }

func (w *wpadLeases) LeaseAddr(name string) (netip.Addr, uint32, bool) {
	if name == "wpad.lan" || name == "isatap.lan" {
		return netip.MustParseAddr("192.168.1.66"), 60, true
	}
	return w.fakeLeases.LeaseAddr(name)
}

// Filtering and parental controls still apply to bare names (step 11a runs
// after them); with an empty local domain only (unqualified) or NXDOMAIN
// remain; with dns.domainNeeded off bare names go to (unqualified) or the
// default upstreams.
func TestSingleLabelOrderAndOff(t *testing.T) {
	e := singleLabelEnv(t, nil)
	e.flt.check["ads"] = filter.Decision{Action: filter.ActionBlock, Source: "list", Kind: "exact", Name: "L"}
	if r := e.query("udp", "ads", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"0.0.0.0"}) {
		t.Errorf("a blocked bare name: %v", r)
	}
	if ev := e.logs.waitEvent(t, "ads", 0); ev.Status != StatusBlockedList {
		t.Errorf("logged %+v", ev)
	}
	e.update(func(a *settings.All) { a.DNS.LocalDomain = "" })
	if r := e.query("udp", "tv", dns.TypeA); r.Rcode != dns.RcodeNameError || len(e.up.callsFor("tv")) != 0 {
		t.Errorf("empty local domain: %v", r)
	}
	e.update(func(a *settings.All) { a.DNS.LocalDomain = "lan"; a.DNS.DomainNeeded = false })
	e.query("udp", "tv2", dns.TypeA)
	if c := e.up.callsFor("tv2"); len(c) != 1 || c[0].via != nil {
		t.Errorf("off: the default upstreams, got %v", c)
	}
	if _, err := e.srv.CreateForwarder(context.Background(), ForwarderInput{Domain: Unqualified, Upstreams: []string{"10.8.8.8"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	e.query("udp", "tv3", dns.TypeAAAA)
	e.query("udp", "tv3", dns.TypeMX)
	if c := e.up.callsFor("tv3"); len(c) != 2 || !slices.Equal(c[0].via, []string{"10.8.8.8"}) || c[1].via != nil {
		t.Errorf("off with (unqualified): AAAA via %v, MX via %v", c[0].via, c[1].via)
	}
	// The root never matches (unqualified).
	e.query("udp", ".", dns.TypeA)
	if c := e.up.callsFor(""); len(c) != 1 || c[0].via != nil {
		t.Errorf("root: %v", c)
	}
}

// dns.privateReverseNetworks: their reverse zones are served like the
// built-in private zones (router, NXDOMAIN) and never reach the default
// upstreams.
func TestPrivateReverseNetworks(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DNS.PrivateReverseNetworks = []string{"8.8.8.0/24", "2a00:1450::/32"} })
	e.serve()
	for _, name := range []string{"1.8.8.8.in-addr.arpa", "8.8.8.in-addr.arpa",
		"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.5.4.1.0.0.a.2.ip6.arpa"} {
		r := e.query("udp", name, dns.TypePTR)
		if r.Rcode != dns.RcodeNameError || len(e.up.callsFor(name)) != 0 {
			t.Errorf("%s: %v, calls %v", name, r, e.up.callsFor(name))
		}
	}
	e.query("udp", "1.4.4.8.in-addr.arpa", dns.TypePTR)
	if c := e.up.callsFor("1.4.4.8.in-addr.arpa"); len(c) != 1 || c[0].via != nil {
		t.Errorf("another public PTR goes to the default upstreams: %v", c)
	}
	e.update(func(a *settings.All) { a.DNS.RouterResolver = "192.168.1.1" })
	e.query("udp", "2.8.8.8.in-addr.arpa", dns.TypePTR)
	if c := e.up.callsFor("2.8.8.8.in-addr.arpa"); len(c) != 1 || !slices.Equal(c[0].via, []string{"192.168.1.1:53"}) {
		t.Errorf("router: %v", c)
	}
}
