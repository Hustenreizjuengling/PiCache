package upstream

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// sentECS returns the client subnet option of q (invalid if none).
func sentECS(q *dns.Msg) netip.Prefix {
	p, _ := ecsOf(q)
	return p
}

// echoECS answers with an A record and echoes the query's client subnet
// with the given scope (like an ECS-aware resolver).
func echoECS(q *dns.Msg) *dns.Msg {
	m := answerA(q, "198.51.100.7", 300)
	if p, ok := ecsOf(q); ok {
		m.SetEdns0(1232, false)
		e := &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: uint8(p.Bits()), SourceScope: 24, Address: net.IP(p.Addr().AsSlice())}
		if p.Addr().Is6() {
			e.Family = 2
		}
		m.IsEdns0().Option = append(m.IsEdns0().Option, e)
	}
	return m
}

// The subnet is sent with SCOPE 0 and the address masked; it is part of
// the cache key and of the in-flight key, so two subnets never share an
// exchange or an answer.
func TestECSKeys(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			<-release
			return echoECS(q), nil
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		a := netip.MustParsePrefix("203.0.113.77/24") // not masked by the caller
		b := netip.MustParsePrefix("198.51.100.0/24")
		var wg sync.WaitGroup
		for i, p := range []netip.Prefix{a, a, b, {}} {
			wg.Go(func() {
				if _, _, err := r.Resolve(context.Background(), query("ecs.example.", dns.TypeA, uint16(i), false), p); err != nil {
					t.Error(err)
				}
			})
		}
		synctest.Wait()
		if f.calls() != 3 {
			t.Fatalf("upstream calls = %d, want 3 (one per subnet and one without)", f.calls())
		}
		close(release)
		wg.Wait()
		seen := map[netip.Prefix]bool{}
		for _, q := range f.queries {
			seen[sentECS(q)] = true
			if opt := q.IsEdns0(); opt != nil {
				for _, o := range opt.Option {
					if e, ok := o.(*dns.EDNS0_SUBNET); ok && e.SourceScope != 0 {
						t.Errorf("scope %d sent", e.SourceScope)
					}
				}
			}
		}
		if !seen[netip.MustParsePrefix("203.0.113.0/24")] || !seen[b] || !seen[netip.Prefix{}] {
			t.Fatalf("subnets sent: %v", seen)
		}
		for _, p := range []netip.Prefix{a, b, {}} {
			if _, info, _ := r.Resolve(context.Background(), query("ecs.example.", dns.TypeA, 9, false), p); !info.Cached {
				t.Errorf("subnet %v: not cached per subnet", p)
			}
		}
		if f.calls() != 3 {
			t.Errorf("calls %d after the cache hits", f.calls())
		}
	})
}

// A reply whose subnet differs from the one sent in family, source prefix
// length or address is discarded like a malformed reply (UDP keeps
// waiting, other transports fail the attempt); a subnet in a reply to a
// query without one is ignored.
func TestECSReplyMismatch(t *testing.T) {
	sent := netip.MustParsePrefix("203.0.113.0/24")
	q := newQuery("x.example.", dns.TypeA, dns.ClassINET, false)
	addECS(q, sent)
	plain := newQuery("x.example.", dns.TypeA, dns.ClassINET, false)
	mk := func(family uint16, bits uint8, addr string) *dns.Msg {
		m := answerA(q, "192.0.2.1", 60)
		m.SetEdns0(1232, false)
		m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: family,
			SourceNetmask: bits, SourceScope: bits, Address: net.ParseIP(addr)})
		return m
	}
	for _, tc := range []struct {
		name string
		q, m *dns.Msg
		ok   bool
	}{
		{"same", q, mk(1, 24, "203.0.113.0"), true},
		{"no option in the reply", q, answerA(q, "192.0.2.1", 60), true},
		{"other address", q, mk(1, 24, "198.51.100.0"), false},
		{"other length", q, mk(1, 16, "203.0.0.0"), false},
		{"other family", q, mk(2, 24, "2001:db8::"), false},
		{"reply to a query without a subnet", plain, mk(1, 24, "198.51.100.0"), true},
	} {
		if err := checkReply(tc.q, tc.m); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
	// Over UDP the mismatching reply is skipped and the right one taken.
	srv := startDNS(t, func(w dns.ResponseWriter, req *dns.Msg) {
		bad := mk(1, 24, "198.51.100.0")
		bad.Id = req.Id
		_ = w.WriteMsg(bad)
		_ = w.WriteMsg(echoECS(req))
	})
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{srv.String()}; d.CacheEnabled = false })
	r := newTestResolver(t, st, testOptions(), nil)
	defer r.Close()
	m, _, err := r.Resolve(context.Background(), query("x.example.", dns.TypeA, 1, false), sent)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := ecsOf(m); p != sent {
		t.Errorf("reply subnet %v, want %v", p, sent)
	}
}

// No subnet is ever sent through ResolveVia or LookupIP.
func TestECSScope(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) { d.CacheEnabled = false }))
	f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) { return echoECS(q), nil }}
	router := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) { return echoECS(q), nil }}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f, "192.168.178.1": router})
	defer r.Close()
	if _, err := r.LookupIP(context.Background(), "cdn.example", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ResolveVia(context.Background(), query("nas.fritz.box.", dns.TypeA, 1, false), []string{"192.168.178.1"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range append(f.queries, router.queries...) {
		if sentECS(q).IsValid() {
			t.Fatalf("a subnet was sent: %v", q)
		}
	}
}
