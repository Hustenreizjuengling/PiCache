package dnsserver

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestRecordValidation(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.addRecord("web.example", "A", "192.168.1.20")
	e.addRecord("chain1.example", "CNAME", "chain2.example")

	tests := []struct {
		name string
		in   RecordInput
		kind apperr.Kind
	}{
		{"bad name", RecordInput{Name: "bad name", Type: "A", Value: "1.2.3.4"}, apperr.KindInvalid},
		{"inner wildcard", RecordInput{Name: "a.*.example", Type: "A", Value: "1.2.3.4"}, apperr.KindInvalid},
		{"unicode name", RecordInput{Name: "bücher.example", Type: "A", Value: "1.2.3.4"}, apperr.KindInvalid},
		{"reserved localhost", RecordInput{Name: "x.localhost", Type: "A", Value: "1.2.3.4"}, apperr.KindInvalid},
		{"bad type", RecordInput{Name: "x.example", Type: "MX", Value: "mail.example"}, apperr.KindInvalid},
		{"A with IPv6", RecordInput{Name: "x.example", Type: "A", Value: "fd00::1"}, apperr.KindInvalid},
		{"AAAA with IPv4", RecordInput{Name: "x.example", Type: "AAAA", Value: "1.2.3.4"}, apperr.KindInvalid},
		{"TTL too high", RecordInput{Name: "x.example", Type: "A", Value: "1.2.3.4", TTL: 90000}, apperr.KindInvalid},
		{"TXT control chars", RecordInput{Name: "x.example", Type: "TXT", Value: "a\x00b"}, apperr.KindInvalid},
		{"CNAME self", RecordInput{Name: "self.example", Type: "CNAME", Value: "self.example."}, apperr.KindInvalid},
		{"wildcard CNAME into itself", RecordInput{Name: "*.w.example", Type: "CNAME", Value: "a.w.example"}, apperr.KindInvalid},
		{"CNAME shares name", RecordInput{Name: "web.example", Type: "CNAME", Value: "other.example"}, apperr.KindConflict},
		{"record next to CNAME", RecordInput{Name: "chain1.example", Type: "A", Value: "1.2.3.4"}, apperr.KindConflict},
		{"CNAME loop", RecordInput{Name: "chain2.example", Type: "CNAME", Value: "chain1.example"}, apperr.KindConflict},
		{"duplicate", RecordInput{Name: "WEB.example.", Type: "a", Value: "192.168.1.20"}, apperr.KindConflict},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.srv.CreateRecord(ctx, tc.in)
			if got := apperr.KindOf(err); got != tc.kind {
				t.Fatalf("kind %v, want %v (err %v)", got, tc.kind, err)
			}
		})
	}

	r, err := e.srv.CreateRecord(ctx, RecordInput{Name: " Mixed.Example. ", Type: "aaaa", Value: "FD00::0001", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "mixed.example" || r.Type != "AAAA" || r.Value != "fd00::1" || r.TTL != 300 {
		t.Errorf("not normalised: %+v", r)
	}
	upd, err := e.srv.UpdateRecord(ctx, r.ID, RecordInput{Name: "mixed.example", Type: "AAAA", Value: "fd00::2", TTL: 60})
	if err != nil || upd.Value != "fd00::2" || upd.Enabled || upd.TTL != 60 {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := e.srv.UpdateRecord(ctx, 9999, RecordInput{Name: "a.example", Type: "A", Value: "1.2.3.4"}); apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("update missing: %v", err)
	}
	if err := e.srv.DeleteRecord(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.DeleteRecord(ctx, r.ID); apperr.KindOf(err) != apperr.KindNotFound {
		t.Errorf("delete twice: %v", err)
	}
	list, err := e.srv.Records(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("records %v %v", list, err)
	}
}

func TestDisabledRecordNotServed(t *testing.T) {
	e := newEnv(t, nil)
	r := e.addRecord("off.example", "A", "192.168.1.30")
	if _, err := e.srv.UpdateRecord(context.Background(), r.ID, RecordInput{Name: r.Name, Type: r.Type, Value: r.Value}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.srv.zone.Load().lookup("off.example"); ok {
		t.Error("disabled records must not be served")
	}
	if _, ok := e.srv.zone.Load().lookup(reverseName(netip.MustParseAddr("192.168.1.30"))); ok {
		t.Error("disabled records must not produce auto-PTR")
	}
}

func TestForwarderMatch(t *testing.T) {
	tbl := newFwdTable([]Forwarder{
		{Domain: "example.lan", Upstreams: []string{"10.0.0.1"}, Enabled: true},
		{Domain: "*.sub.example.lan", Upstreams: []string{"10.0.0.2"}, Enabled: true},
		{Domain: "deep.sub.example.lan", Upstreams: []string{"10.0.0.3"}, Enabled: true},
		{Domain: "off.example.lan", Upstreams: []string{"10.0.0.4"}, Enabled: false},
		{Domain: "doh.example", Upstreams: []string{"https://dns.example/dns-query"}, Enabled: true},
	})
	tests := []struct{ name, want string }{
		{"example.lan", "example.lan"},
		{"host.example.lan", "example.lan"},
		{"sub.example.lan", "example.lan"}, // wildcard excludes its apex
		{"a.sub.example.lan", "*.sub.example.lan"},
		{"deep.sub.example.lan", "deep.sub.example.lan"},
		{"x.deep.sub.example.lan", "deep.sub.example.lan"},
		{"x.off.example.lan", "example.lan"},
		{"other.lan", ""},
		{"notexample.lan", ""},
	}
	for _, tc := range tests {
		got := ""
		if f := tbl.match(tc.name, false); f != nil {
			got = f.domain
		}
		if got != tc.want {
			t.Errorf("match(%s) = %q, want %q", tc.name, got, tc.want)
		}
	}
	if want := []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")}; !sameAddrs(tbl.ips, want) {
		t.Errorf("target IPs %v, want %v", tbl.ips, want)
	}
}

func sameAddrs(a, b []netip.Addr) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.SortFunc(a, netip.Addr.Compare)
	slices.SortFunc(b, netip.Addr.Compare)
	return slices.Equal(a, b)
}

func TestForwarderCRUD(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	f, err := e.srv.CreateForwarder(ctx, ForwarderInput{Domain: "Fritz.Box.", Upstreams: []string{" 192.168.178.1 "}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if f.Domain != "fritz.box" || !slices.Equal(f.Upstreams, []string{"192.168.178.1"}) {
		t.Errorf("not normalised: %+v", f)
	}
	for _, in := range []ForwarderInput{
		{Domain: "fritz.box", Upstreams: []string{"192.168.178.2"}},
		{Domain: "bad domain", Upstreams: []string{"1.1.1.1"}},
		{Domain: "x.example", Upstreams: nil},
		{Domain: "x.example", Upstreams: []string{"udp://dns.lan"}}, // a plain target by a local name
		{Domain: "localhost", Upstreams: []string{"1.1.1.1"}},
	} {
		if _, err := e.srv.CreateForwarder(ctx, in); err == nil {
			t.Errorf("CreateForwarder(%+v) must fail", in)
		}
	}
	if _, err := e.srv.UpdateForwarder(ctx, f.ID, ForwarderInput{Domain: "*.fritz.box", Upstreams: []string{"192.168.178.1:5353"}, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.fwd.Load().match("fritz.box", false); got != nil {
		t.Errorf("*.fritz.box must not match the apex, got %+v", got)
	}
	// Forwarder targets are exempt from the rate limit.
	if ok, _ := e.srv.limiter.Allow(netip.MustParseAddr("192.168.178.1")); !ok {
		t.Error("forwarder target must be exempt")
	}
	if err := e.srv.DeleteForwarder(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.srv.Forwarders(ctx); len(list) != 0 {
		t.Errorf("forwarders %v", list)
	}
}

func TestReverseNames(t *testing.T) {
	for _, s := range []string{"192.168.1.5", "10.0.0.1", "fd00::1", "2001:db8::abcd:1"} {
		ip := netip.MustParseAddr(s)
		name := reverseName(ip)
		want, _ := dns.ReverseAddr(s)
		if name+"." != want {
			t.Errorf("reverseName(%s) = %s, want %s", s, name, want)
		}
		back, ok := parseReverse(name)
		if !ok || back != ip {
			t.Errorf("parseReverse(%s) = %v %v", name, back, ok)
		}
	}
	for _, bad := range []string{"1.168.192.in-addr.arpa", "01.1.168.192.in-addr.arpa", "256.1.168.192.in-addr.arpa", "x.ip6.arpa"} {
		if _, ok := parseReverse(bad); ok {
			t.Errorf("parseReverse(%s) must fail", bad)
		}
	}
}

func TestPrivateReverseZone(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"5.1.168.192.in-addr.arpa", true},
		{"1.0.0.10.in-addr.arpa", true},
		{"1.0.16.172.in-addr.arpa", true},
		{"1.0.31.172.in-addr.arpa", true},
		{"1.0.32.172.in-addr.arpa", false},
		{"1.1.64.100.in-addr.arpa", true},
		{"1.1.127.100.in-addr.arpa", true},
		{"1.1.128.100.in-addr.arpa", false},
		{"1.0.254.169.in-addr.arpa", true},
		{"1.0.0.127.in-addr.arpa", true},
		{"8.8.8.8.in-addr.arpa", false},
		{reverseName(netip.MustParseAddr("fd12::1")), true},
		{reverseName(netip.MustParseAddr("fc00::1")), true},
		{reverseName(netip.MustParseAddr("fe80::1")), true},
		{reverseName(netip.MustParseAddr("febf::1")), true},
		{reverseName(netip.MustParseAddr("fec0::1")), false},
		{reverseName(netip.MustParseAddr("2001:db8::1")), true},
		{reverseName(netip.MustParseAddr("2a00::1")), false},
		{reverseName(netip.MustParseAddr("::1")), true},
		{"example.com", false},
	}
	for _, tc := range tests {
		if _, got := privateReverseZone(tc.name); got != tc.want {
			t.Errorf("privateReverseZone(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestComputeCacheIPs(t *testing.T) {
	ok := func(s string) func() (netip.Addr, error) {
		return func() (netip.Addr, error) { return netip.MustParseAddr(s), nil }
	}
	lan := func(docker0 bool, ips ...string) func() ([]interfaceIPv4, bool) {
		return func() ([]interfaceIPv4, bool) {
			var out []interfaceIPv4
			for _, s := range ips {
				out = append(out, interfaceIPv4{iface: "eth0", ip: netip.MustParseAddr(s)})
			}
			return out, docker0
		}
	}
	tests := []struct {
		name      string
		l         settings.DownloadCache
		env       hostEnv
		v4        []string
		auto      bool
		hasReason bool
	}{
		{"configured", settings.DownloadCache{CacheIPv4: []string{"10.1.1.1"}}, hostEnv{primary: ok("192.168.1.2"), ifaces: lan(false)}, []string{"10.1.1.1"}, false, false},
		{"primary private", settings.DownloadCache{}, hostEnv{primary: ok("192.168.1.2"), ifaces: lan(false)}, []string{"192.168.1.2"}, true, false},
		{"primary public falls back", settings.DownloadCache{}, hostEnv{primary: ok("203.0.113.5"), ifaces: lan(false, "10.0.0.5")}, []string{"10.0.0.5"}, true, true},
		{"primary CGNAT falls back", settings.DownloadCache{}, hostEnv{primary: ok("100.64.1.2"), ifaces: lan(false, "10.0.0.5")}, []string{"10.0.0.5"}, true, true},
		{"configured invalid", settings.DownloadCache{CacheIPv4: []string{"8.8.8.8"}}, hostEnv{primary: ok("192.168.1.2"), ifaces: lan(false)}, nil, false, true},
		{"no private address", settings.DownloadCache{}, hostEnv{primary: ok("203.0.113.5"), ifaces: lan(false)}, nil, true, true},
		{"docker bridge", settings.DownloadCache{}, hostEnv{container: "docker", primary: ok("172.17.0.2"), ifaces: lan(false, "172.17.0.2")}, nil, true, true},
		{"docker host network", settings.DownloadCache{}, hostEnv{container: "docker", primary: ok("172.20.0.2"), ifaces: lan(true, "172.20.0.2")}, []string{"172.20.0.2"}, true, false},
		{"podman bridge", settings.DownloadCache{}, hostEnv{container: "podman", primary: ok("172.16.5.2"), ifaces: lan(false)}, nil, true, true},
		{"lxc uses primary", settings.DownloadCache{}, hostEnv{container: "lxc", primary: ok("172.16.5.2"), ifaces: lan(false)}, []string{"172.16.5.2"}, true, false},
		{"no route", settings.DownloadCache{}, hostEnv{primary: func() (netip.Addr, error) { return netip.Addr{}, errors.New("no route") }, ifaces: lan(false, "192.168.9.9")}, []string{"192.168.9.9"}, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := computeCacheIPs(&tc.l, tc.env)
			if got := addrStrings(st.v4); !slices.Equal(got, append([]string{}, tc.v4...)) {
				t.Errorf("v4 %v, want %v", got, tc.v4)
			}
			if st.auto != tc.auto || (st.warning != "") != tc.hasReason {
				t.Errorf("auto %v warning %q", st.auto, st.warning)
			}
		})
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, func(a *settings.All) {
		a.DNS.RateLimitQPS, a.DNS.RateLimitBurst = 1, 1
		a.DNS.RateLimitExempt = []string{"192.168.2.0/24"}
	})
	w1 := udpFrom("192.168.1.50")
	e.handle(w1, "a.example", dns.TypeA)
	w2 := udpFrom("192.168.1.50")
	e.handle(w2, "b.example", dns.TypeA)
	if len(w1.msgs) != 1 || len(w2.msgs) != 0 {
		t.Fatalf("first query answered, second dropped: got %d / %d replies", len(w1.msgs), len(w2.msgs))
	}
	wt := tcpFrom("192.168.1.50")
	e.handle(wt, "c.example", dns.TypeA)
	if len(wt.msgs) != 1 || wt.msgs[0].Rcode != dns.RcodeRefused {
		t.Fatalf("TCP over the limit must get REFUSED: %v", wt.msgs)
	}
	st := e.srv.Stats()
	if st.RateLimited != 2 || len(st.TopRateLimited) != 1 || st.TopRateLimited[0].Client != "192.168.1.50/32" {
		t.Errorf("stats %+v", st)
	}
	if e.logs.count("b.example") != 0 {
		t.Error("rate-limited queries are not logged")
	}
	for range 5 { // exempt networks and loopback are never limited
		for _, ip := range []string{"192.168.2.7", "127.0.0.1"} {
			w := udpFrom(ip)
			e.handle(w, "x.example", dns.TypeA)
			if len(w.msgs) != 1 {
				t.Fatalf("exempt client %s was limited", ip)
			}
		}
	}
	// Settings apply live: raising the limit reconfigures the limiter in
	// place; buckets and drop statistics are kept.
	rl := e.srv.limiter
	e.update(func(a *settings.All) { a.DNS.RateLimitQPS, a.DNS.RateLimitBurst = 1000, 1000 })
	w3 := udpFrom("192.168.1.50")
	e.handle(w3, "d.example", dns.TypeA)
	if len(w3.msgs) != 1 {
		t.Error("new limit must apply immediately")
	}
	if e.srv.limiter != rl {
		t.Error("the limiter must be reconfigured, not replaced")
	}
	if st := e.srv.Stats(); len(st.TopRateLimited) != 1 || st.TopRateLimited[0].Client != "192.168.1.50/32" || st.TopRateLimited[0].Dropped != 2 {
		t.Errorf("drop statistics must survive a settings change: %+v", st.TopRateLimited)
	}
}

func TestACLDrop(t *testing.T) {
	e := newEnv(t, nil)
	w := udpFrom("203.0.113.7")
	e.handle(w, "a.example", dns.TypeA)
	wt := tcpFrom("203.0.113.7")
	e.handle(wt, "a.example", dns.TypeA)
	if len(w.msgs) != 0 || len(wt.msgs) != 0 || !wt.closed {
		t.Fatalf("public clients get nothing: udp %v tcp %v closed %v", w.msgs, wt.msgs, wt.closed)
	}
	if e.srv.Stats().Refused != 2 || len(e.up.callsFor("a.example")) != 0 {
		t.Errorf("stats %+v", e.srv.Stats())
	}
	e.update(func(a *settings.All) { a.DNS.AllowedNetworks = []string{"203.0.113.0/24"} })
	w = udpFrom("203.0.113.7")
	e.handle(w, "a.example", dns.TypeA)
	if len(w.msgs) != 1 {
		t.Error("allowed networks apply live")
	}
}

// packetSource is a dns.PacketConnReader returning canned packets.
type packetSource struct {
	dns.PacketConnReader
	pkts  [][]byte
	addrs []net.Addr
}

func (p *packetSource) ReadPacketConn(net.PacketConn, time.Duration) ([]byte, net.Addr, error) {
	if len(p.pkts) == 0 {
		return nil, nil, errors.New("eof")
	}
	b, a := p.pkts[0], p.addrs[0]
	p.pkts, p.addrs = p.pkts[1:], p.addrs[1:]
	return b, a, nil
}

func TestACLReaderDropsBeforeParsing(t *testing.T) {
	e := newEnv(t, nil)
	src := &packetSource{
		pkts:  [][]byte{[]byte("garbage-from-outside"), []byte("query-from-lan")},
		addrs: []net.Addr{&net.UDPAddr{IP: net.ParseIP("198.51.100.1"), Port: 5353}, &net.UDPAddr{IP: net.ParseIP("192.168.1.9"), Port: 5353}},
	}
	r := e.srv.decorateReader(src).(dns.PacketConnReader)
	b, addr, err := r.ReadPacketConn(nil, time.Second)
	if err != nil || string(b) != "query-from-lan" || !strings.HasPrefix(addr.String(), "192.168.1.9") {
		t.Fatalf("got %q from %v (%v)", b, addr, err)
	}
	if e.srv.Stats().Refused != 1 {
		t.Errorf("dropped packets are counted: %+v", e.srv.Stats())
	}
}

func TestLookup(t *testing.T) {
	e := newEnv(t, func(a *settings.All) { a.DownloadCache.Enabled = true })
	e.flt.check["ads.example.com"] = listBlock("TestList")
	e.flt.matches = []filter.Match{{Action: "block", Source: "list", Name: "TestList", Applies: true, Decisive: true}}
	ctx := context.Background()
	caller := netip.MustParseAddr("192.168.1.44")

	res, err := e.srv.Lookup(ctx, LookupRequest{Name: "Ads.Example.com."}, caller)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusBlockedList || res.Type != "A" || res.RCode != "NOERROR" || len(res.Answers) != 1 ||
		len(res.Matches) != 1 || len(res.Steps) < 2 || !slices.Equal(res.GroupIDs, []int64{1}) {
		t.Fatalf("unexpected result %+v", res)
	}
	if !strings.Contains(res.Steps[0], "192.168.1.44") {
		t.Errorf("default client must be the caller: %v", res.Steps)
	}
	if e.logs.count("ads.example.com") != 0 {
		t.Error("lookups are never logged")
	}
	res, err = e.srv.Lookup(ctx, LookupRequest{Name: "www.example.com", Type: "aaaa", ClientIP: "10.1.2.3"}, caller)
	if err != nil || res.Status != StatusForwarded || res.Upstream != "fake-upstream" || !strings.Contains(res.Steps[0], "10.1.2.3") {
		t.Fatalf("unexpected result %+v %v", res, err)
	}
	for _, bad := range []LookupRequest{{Name: "bad name"}, {Name: "x.example", Type: "NOPE"}, {Name: "x.example", ClientIP: "nope"}} {
		if _, err := e.srv.Lookup(ctx, bad, caller); apperr.KindOf(err) != apperr.KindInvalid {
			t.Errorf("Lookup(%+v) = %v, want invalid", bad, err)
		}
	}
	e.cl.mu.Lock()
	seen := len(e.cl.seen)
	e.cl.mu.Unlock()
	if seen != 0 {
		t.Error("lookups do not mark clients as seen")
	}
}

func TestSummarize(t *testing.T) {
	var rrs []dns.RR
	rrs = append(rrs, &dns.CNAME{Hdr: rrHeader("a.example.", dns.TypeCNAME, 60), Target: "b.example."})
	for i := range 40 {
		rrs = append(rrs, &dns.A{Hdr: rrHeader("b.example.", dns.TypeA, 60), A: net.IPv4(198, 51, 100, byte(i))})
	}
	rrs = append(rrs, &dns.MX{Hdr: rrHeader("b.example.", dns.TypeMX, 60), Preference: 10, Mx: "mail.example."})
	got := summarize(rrs)
	if !strings.HasPrefix(got, "CNAME b.example, 198.51.100.0, 198.51.100.1") || len(got) > maxSummaryLen || !strings.HasSuffix(got, "...") {
		t.Errorf("summary %q (%d bytes)", got, len(got))
	}
	if got := summarize(rrs[40:]); got != "198.51.100.39, MX 10 mail.example." {
		t.Errorf("summary %q", got)
	}
}

func TestQPS(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &Server{}
		s.sampleQPS(time.Now())
		for range 7 {
			time.Sleep(10 * time.Second)
			s.queries.Add(100)
			s.sampleQPS(time.Now())
		}
		// The window keeps the last 7 samples (60 s): 600 queries / 60 s.
		if got := s.qps(); got != 10 {
			t.Errorf("qps = %v, want 10", got)
		}
	})
}

func TestSplitTXT(t *testing.T) {
	long := strings.Repeat("a", 254) + "é" + strings.Repeat("b", 300)
	parts := splitTXT(long)
	if strings.Join(parts, "") != long {
		t.Fatal("parts must join to the original")
	}
	for _, p := range parts {
		if len(p) > 255 || !utf8Valid(p) {
			t.Errorf("bad part %q", p)
		}
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

func TestServeFailsOnBrokenListener(t *testing.T) {
	e := newEnv(t, nil)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	done := make(chan error, 1)
	go func() { done <- e.srv.Serve(context.Background(), nil, []net.Listener{ln}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Serve must report a failed listener")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return")
	}
}
