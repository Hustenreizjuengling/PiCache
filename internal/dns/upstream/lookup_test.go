package upstream

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// cdnAnswers serves cdn.example → CNAME edge.example → A + AAAA.
func cdnAnswers(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetReply(q)
	qn := q.Question[0]
	switch qn.Name {
	case "cdn.example.":
		m.Answer = append(m.Answer, &dns.CNAME{Hdr: dns.RR_Header{Name: "cdn.example.", Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 600}, Target: "Edge.Example."})
		if qn.Qtype == dns.TypeA {
			m.Answer = append(m.Answer,
				&dns.A{Hdr: dns.RR_Header{Name: "edge.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.IPv4(198, 51, 100, 1)},
				&dns.A{Hdr: dns.RR_Header{Name: "unrelated.example.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 120}, A: net.IPv4(203, 0, 113, 9)})
		} else {
			m.Answer = append(m.Answer, &dns.AAAA{Hdr: dns.RR_Header{Name: "edge.example.", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 90}, AAAA: net.ParseIP("2001:db8::1")})
		}
	default:
		m.Rcode = dns.RcodeNameError
		m.Ns = []dns.RR{soa("example.", 300, 300)}
	}
	return m, nil
}

func TestLookupIP(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) { d.CacheEnabled = false }))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: cdnAnswers}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		ctx := context.Background()

		v4, err := r.LookupIP(ctx, "CDN.example.", false)
		if err != nil || !slices.Equal(v4, []netip.Addr{netip.MustParseAddr("198.51.100.1")}) {
			t.Fatalf("v4 = %v, %v (CNAME chain must be followed, unrelated records ignored)", v4, err)
		}
		both, err := r.LookupIP(ctx, "cdn.example", true)
		want := []netip.Addr{netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("2001:db8::1")}
		if err != nil || !slices.Equal(both, want) {
			t.Fatalf("v4+v6 = %v, %v", both, err)
		}
		calls := f.calls()
		if _, err := r.LookupIP(ctx, "cdn.example", false); err != nil || f.calls() != calls {
			t.Fatalf("second lookup was not cached (calls %d → %d, cache disabled must not matter)", calls, f.calls())
		}
		time.Sleep(121 * time.Second) // beyond the smallest TTL of the chain
		if _, err := r.LookupIP(ctx, "cdn.example", false); err != nil || f.calls() != calls+1 {
			t.Fatalf("expired entry not refreshed: calls %d", f.calls())
		}

		_, err = r.LookupIP(ctx, "missing.example", false)
		var de *net.DNSError
		if !errors.As(err, &de) || !de.IsNotFound {
			t.Errorf("missing host: %v", err)
		}
		for _, bad := range []string{"", "bad_host!", "a..b", strings.Repeat("a", 64) + ".example"} {
			if _, err := r.LookupIP(ctx, bad, false); err == nil {
				t.Errorf("LookupIP(%q) accepted", bad)
			}
		}
		if got, err := r.LookupIP(ctx, "192.0.2.44", false); err != nil || got[0] != netip.MustParseAddr("192.0.2.44") {
			t.Errorf("IP literal: %v %v", got, err)
		}
		if _, err := r.LookupIP(ctx, "2001:db8::5", false); err == nil {
			t.Error("IPv6 literal returned although want6 is false")
		}
	})
}

func TestLookupIPUsesDefaultUpstreams(t *testing.T) {
	// LookupIP must use the default upstream set and the response cache
	// namespace of Resolve, never a ResolveVia set.
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		def := &fakeTransport{fn: cdnAnswers}
		via := &fakeTransport{fn: replyA("10.0.0.1", 60)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: def, "192.168.1.1": via})
		defer r.Close()
		ctx := context.Background()
		if _, _, err := r.ResolveVia(ctx, query("cdn.example.", dns.TypeA, 1, false), []string{"192.168.1.1"}); err != nil {
			t.Fatal(err)
		}
		got, err := r.LookupIP(ctx, "cdn.example", false)
		if err != nil || got[0] != netip.MustParseAddr("198.51.100.1") {
			t.Fatalf("LookupIP = %v %v", got, err)
		}
		if _, info, _ := r.Resolve(ctx, query("cdn.example.", dns.TypeA, 2, false)); !info.Cached {
			t.Error("LookupIP answer not shared with the response cache")
		}
	})
}

func TestIPCacheIsBounded(t *testing.T) {
	var c ipCache
	c.m = map[ipKey]ipEntry{}
	exp := time.Now().Add(time.Hour)
	for i := range ipCacheMax + 10 {
		c.put("h"+string(rune('a'+i%26))+strings.Repeat("x", i/26), false, []netip.Addr{netip.MustParseAddr("192.0.2.1")}, exp)
	}
	if len(c.m) > ipCacheMax {
		t.Fatalf("ipCache holds %d entries", len(c.m))
	}
	c.put("expired", false, nil, time.Now().Add(-time.Second))
	if _, ok := c.m[ipKey{"expired", false}]; ok {
		t.Error("expired entry stored")
	}
}

func ptrServer(t *testing.T, answers map[string]string) netip.AddrPort {
	return startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		if name, ok := answers[q.Question[0].Name]; ok {
			m.Answer = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60}, Ptr: name}}
		} else {
			m.Rcode = dns.RcodeNameError
		}
		_ = w.WriteMsg(m)
	})
}

func TestLookupPTR(t *testing.T) {
	srv := ptrServer(t, map[string]string{
		"10.1.168.192.in-addr.arpa.": "NAS-1.Lan.",
		"11.1.168.192.in-addr.arpa.": `evil\032name.lan.`,
	})
	st := newStore(t, nil)
	r := newTestResolver(t, st, testOptions(), nil)
	defer r.Close()
	ctx := context.Background()
	servers := []string{"https://dns.example/dns-query", srv.String()} // non-plain entries are ignored
	tests := []struct {
		ip   string
		want string
	}{
		{"192.168.1.10", "nas-1.lan"},
		{"192.168.1.11", ""}, // not a plain host name
		{"192.168.1.12", ""}, // NXDOMAIN
	}
	for _, tc := range tests {
		got, err := r.LookupPTR(ctx, netip.MustParseAddr(tc.ip), servers)
		if err != nil || got != tc.want {
			t.Errorf("LookupPTR(%s) = %q, %v; want %q", tc.ip, got, err, tc.want)
		}
	}
	if _, err := r.LookupPTR(ctx, netip.MustParseAddr("192.168.1.10"), []string{"tls://dns.example"}); !errors.Is(err, errNoPlainPTR) {
		t.Errorf("no plain servers: %v", err)
	}
}

func TestCleanPTRName(t *testing.T) {
	for in, want := range map[string]string{
		"Host.LAN.":                         "host.lan",
		"my_printer.fritz.box.":             "my_printer.fritz.box",
		`a\032b.lan.`:                       "",
		"a..b.":                             "",
		".":                                 "",
		strings.Repeat("a", 64) + ".lan.":   "",
		strings.Repeat("abc.", 70) + "lan.": "",
	} {
		if got := cleanPTRName(in); got != want {
			t.Errorf("cleanPTRName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProbe(t *testing.T) {
	tests := []struct {
		name  string
		rcode int
		want  bool
	}{
		{"nxdomain answers", dns.RcodeNameError, true},
		{"noerror answers", dns.RcodeSuccess, true},
		{"refused does not", dns.RcodeRefused, false},
		{"servfail does not", dns.RcodeServerFailure, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
				m := new(dns.Msg)
				m.SetRcode(q, tc.rcode)
				_ = w.WriteMsg(m)
			})
			opts := testOptions()
			opts.plainPort = int(srv.Port())
			r := newTestResolver(t, newStore(t, nil), opts, nil)
			defer r.Close()
			if got := r.Probe(context.Background(), srv.Addr()); got != tc.want {
				t.Errorf("Probe = %v, want %v", got, tc.want)
			}
		})
	}
	r := newTestResolver(t, newStore(t, nil), testOptions(), nil)
	defer r.Close()
	if r.Probe(context.Background(), netip.Addr{}) {
		t.Error("invalid address probed as answering")
	}
}

func TestTest(t *testing.T) {
	var queries atomic.Int32
	srv := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
		queries.Add(1)
		m := answerA(q, "192.0.2.123", 60)
		if q.Question[0].Name != testName {
			m.Rcode = dns.RcodeRefused
		}
		_ = w.WriteMsg(m)
	})
	r := newTestResolver(t, newStore(t, nil), testOptions(), nil)
	defer r.Close()
	ctx := context.Background()

	res := r.Test(ctx, srv.String())
	if !res.OK || res.Answer != "192.0.2.123" || res.Error != "" || res.Upstream != srv.String() {
		t.Fatalf("result = %+v", res)
	}
	if res2 := r.Test(ctx, srv.String()); !res2.OK || queries.Load() != 2 {
		t.Errorf("Test must bypass the cache: %+v, queries %d", res2, queries.Load())
	}
	if len(r.Stats()) != 2 || r.Stats()[0].Queries != 0 {
		t.Errorf("Test must not touch the statistics: %+v", r.Stats())
	}
	if res := r.Test(ctx, "ftp://example.com"); res.OK || !strings.HasPrefix(res.Error, "invalid upstream") {
		t.Errorf("invalid upstream: %+v", res)
	}
}

func TestSummarizeAnswer(t *testing.T) {
	q := newQuery(testName, dns.TypeA, dns.ClassINET, false)
	m := new(dns.Msg)
	m.SetReply(q)
	if got := summarizeAnswer(m); got != "NOERROR (no address)" {
		t.Errorf("empty: %q", got)
	}
	for i := range 6 {
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: testName, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.IPv4(192, 0, 2, byte(i))})
	}
	if got := summarizeAnswer(m); got != "192.0.2.0, 192.0.2.1, 192.0.2.2, 192.0.2.3, …" {
		t.Errorf("summary = %q", got)
	}
}
