package upstream

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

// fakeDialer answers probe dials after the delay configured for the
// address and port (missing: refused) and records every dial.
type fakeDialer struct {
	mu     sync.Mutex
	delays map[string]time.Duration // "ip:port" → connect time
	dials  []string
}

func (d *fakeDialer) dial(ctx context.Context, _, address string) (net.Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, address)
	delay, ok := d.delays[address]
	d.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	select {
	case <-time.After(delay):
		c1, c2 := net.Pipe()
		_ = c2.Close()
		return c1, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (d *fakeDialer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.dials)
}

// multiA answers every A query with the given addresses.
func multiA(ips ...string) func(context.Context, *dns.Msg) (*dns.Msg, error) {
	return func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		m := reply(q, dns.RcodeSuccess, true)
		for _, ip := range ips {
			m.Answer = append(m.Answer, aRR(q.Question[0].Name, ip))
		}
		return m, nil
	}
}

func answerOrder(m *dns.Msg) []string {
	var out []string
	for _, rr := range m.Answer {
		if ip, ok := rrAddr(rr); ok {
			out = append(out, ip.String())
		}
	}
	return out
}

func fastestStore(t *testing.T) *settings.Store {
	return newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{up1, up2}
		d.UpstreamMode = "fastest_addr"
	})
}

// fastest_addr: the upstreams are asked in parallel, the address with the
// fastest TCP connect (443, else 80) moves to the front, the others keep
// their order, and the reordered answer is cached (hits are not probed).
func TestFastestAddressOrder(t *testing.T) {
	st := fastestStore(t)
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDialer{delays: map[string]time.Duration{
			"198.51.100.1:443": 80 * time.Millisecond,
			"198.51.100.2:80":  10 * time.Millisecond, // 443 refused, 80 fastest
			"198.51.100.3:443": 30 * time.Millisecond,
		}}
		opts := testOptions()
		opts.probeDial = d.dial
		opts.publicFilter = func(_ context.Context, addrs []netip.Addr) ([]netip.Addr, error) { return addrs, nil }
		u1 := &fakeTransport{fn: multiA("198.51.100.1", "198.51.100.2", "198.51.100.3")}
		u2 := &fakeTransport{fn: hanging}
		r := newTestResolver(t, st, opts, map[string]*fakeTransport{up1: u1, up2: u2})
		defer r.Close()
		m, _, err := r.Resolve(context.Background(), query("multi.example.", dns.TypeA, 1, false), noECS)
		if err != nil {
			t.Fatal(err)
		}
		if got := answerOrder(m); len(got) != 3 || got[0] != "198.51.100.2" || got[1] != "198.51.100.1" || got[2] != "198.51.100.3" {
			t.Fatalf("order %v", got)
		}
		if u2.calls() != 1 {
			t.Errorf("fastest_addr asks the default upstreams in parallel: second upstream calls %d", u2.calls())
		}
		dials := d.count()
		m, info, _ := r.Resolve(context.Background(), query("multi.example.", dns.TypeA, 2, false), noECS)
		if !info.Cached || answerOrder(m)[0] != "198.51.100.2" || d.count() != dials {
			t.Fatalf("hit: cached %v order %v, dials %d → %d", info.Cached, answerOrder(m), dials, d.count())
		}
	})
}

// Only addresses that pass the filter are probed (public, not this
// machine), results are cached per address, a single address is never
// probed, and nothing answering leaves the order alone.
func TestFastestAddressProbeRules(t *testing.T) {
	st := fastestStore(t)
	synctest.Test(t, func(t *testing.T) {
		d := &fakeDialer{delays: map[string]time.Duration{"10.0.0.1:443": time.Millisecond, "11.0.0.9:443": 50 * time.Millisecond}}
		opts := testOptions()
		opts.probeDial = d.dial
		opts.publicFilter = safeFilter
		u := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			switch q.Question[0].Name {
			case "private.example.":
				return multiA("11.0.0.8", "10.0.0.1", "11.0.0.9")(nil, q)
			case "single.example.":
				return multiA("11.0.0.9")(nil, q)
			case "dead.example.":
				return multiA("11.0.0.20", "11.0.0.21")(nil, q)
			}
			return multiA("11.0.0.9", "11.0.0.8")(nil, q)
		}}
		r := newTestResolver(t, st, opts, map[string]*fakeTransport{up1: u, up2: {fn: hanging}})
		defer r.Close()
		m, _, _ := r.Resolve(context.Background(), query("private.example.", dns.TypeA, 1, false), noECS)
		for _, a := range d.dials {
			if a == "10.0.0.1:443" || a == "10.0.0.1:80" {
				t.Fatal("a private address was probed")
			}
		}
		if got := answerOrder(m); got[0] != "11.0.0.9" || got[1] != "11.0.0.8" || got[2] != "10.0.0.1" {
			t.Errorf("order %v", got)
		}
		before := d.count()
		m, _, _ = r.Resolve(context.Background(), query("other.example.", dns.TypeA, 1, false), noECS)
		if d.count() != before || answerOrder(m)[0] != "11.0.0.9" {
			t.Errorf("cached probe results must be reused: dials %d → %d, order %v", before, d.count(), answerOrder(m))
		}
		before = d.count()
		r.Resolve(context.Background(), query("single.example.", dns.TypeA, 1, false), noECS)
		if d.count() != before {
			t.Error("a single address was probed")
		}
		m, _, _ = r.Resolve(context.Background(), query("dead.example.", dns.TypeA, 1, false), noECS)
		if got := answerOrder(m); got[0] != "11.0.0.20" {
			t.Errorf("nothing answered: order changed to %v", got)
		}
	})
}

// The global bounds fail open: an answer whose probes cannot all get a
// slot or a token is ordered by cached results only, without waiting.
func TestFastestAddressBounds(t *testing.T) {
	p := newProber(func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("unused") },
		func(_ context.Context, a []netip.Addr) ([]netip.Addr, error) { return a, nil })
	now := time.Now()
	if !p.acquire(probeMaxDials, now) {
		t.Fatal("the first 32 slots")
	}
	if p.acquire(1, now) {
		t.Fatal("more than 32 probe dials at a time")
	}
	p.slots.Release(probeMaxDials)
	for _, n := range []int{32, 32, 4} { // the rest of the burst of 100 tokens
		if !p.acquire(n, now) {
			t.Fatalf("burst: %d more", n)
		}
		p.slots.Release(int64(n))
	}
	if p.acquire(1, now) {
		t.Fatal("more than 100 new targets within the burst")
	}
	if p.slots.TryAcquire(probeMaxDials) {
		p.slots.Release(probeMaxDials) // a refused token must not keep slots
	} else {
		t.Fatal("slots leaked after a refused token")
	}
	// Exhausted: an answer is ordered by the cached results only.
	p.store(probeResult{ip: netip.MustParseAddr("198.51.100.2"), ok: true, rtt: time.Millisecond, expires: now.Add(time.Minute)})
	m := reply(newQuery("x.example.", dns.TypeA, dns.ClassINET, false), dns.RcodeSuccess, true,
		aRR("x.example.", "198.51.100.1"), aRR("x.example.", "198.51.100.2"), aRR("x.example.", "198.51.100.3"))
	start := time.Now()
	p.reorder(context.Background(), m, dns.TypeA)
	if got := answerOrder(m); got[0] != "198.51.100.2" || got[1] != "198.51.100.1" || time.Since(start) > 100*time.Millisecond {
		t.Errorf("order %v after %v", got, time.Since(start))
	}
}

// ResolveVia sets use load_balance while fastest_addr is set and are never
// reordered.
func TestFastestAddressDefaultSetOnly(t *testing.T) {
	st := fastestStore(t)
	d := &fakeDialer{delays: map[string]time.Duration{"198.51.100.2:443": time.Millisecond}}
	opts := testOptions()
	opts.probeDial = d.dial
	opts.publicFilter = func(_ context.Context, a []netip.Addr) ([]netip.Addr, error) { return a, nil }
	f1 := &fakeTransport{fn: multiA("198.51.100.1", "198.51.100.2")}
	f2 := &fakeTransport{fn: multiA("198.51.100.1", "198.51.100.2")}
	r := newTestResolver(t, st, opts, map[string]*fakeTransport{"192.0.2.53": f1, "192.0.2.54": f2})
	defer r.Close()
	m, _, err := r.ResolveVia(context.Background(), query("corp.example.", dns.TypeA, 1, false), []string{"192.0.2.53", "192.0.2.54"})
	if err != nil {
		t.Fatal(err)
	}
	if d.count() != 0 || answerOrder(m)[0] != "198.51.100.1" {
		t.Errorf("a ResolveVia answer was probed (%d dials) or reordered: %v", d.count(), answerOrder(m))
	}
	if f1.calls()+f2.calls() != 1 {
		t.Errorf("ResolveVia asked %d upstreams, want 1 (load_balance)", f1.calls()+f2.calls())
	}
}

// Plain upstreams given by name are resolved through the bootstrap
// servers; only public addresses that are not this machine are dialled,
// and a name with no such address fails the attempt.
func TestNamedPlainUpstream(t *testing.T) {
	var target netip.AddrPort
	srv := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) { _ = w.WriteMsg(answerA(q, "198.51.100.77", 60)) })
	target = srv
	boot := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		switch q.Question[0].Name {
		case "dns.example.com.":
			if q.Question[0].Qtype == dns.TypeA {
				m.Answer = append(m.Answer, aRR("dns.example.com.", target.Addr().String()))
			}
		case "private.example.com.":
			if q.Question[0].Qtype == dns.TypeA {
				m.Answer = append(m.Answer, aRR("private.example.com.", "10.0.0.53"))
			}
		}
		_ = w.WriteMsg(m)
	})
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{"udp://dns.example.com:" + itoaPort(target.Port()), "tcp://private.example.com:" + itoaPort(target.Port())}
		d.UpstreamMode = "strict"
		d.Bootstrap = []string{boot.Addr().String()}
		d.CacheEnabled = false
	})
	opts := testOptions()
	opts.plainPort = int(boot.Port())
	// Loopback stands in for a public address here; everything else keeps
	// the real filter's rules.
	opts.publicFilter = func(ctx context.Context, addrs []netip.Addr) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range addrs {
			if a.IsLoopback() {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return nil, errors.New("no public address")
		}
		return out, nil
	}
	r := newTestResolver(t, st, opts, nil)
	defer r.Close()
	m, info, err := r.Resolve(context.Background(), query("named.example.", dns.TypeA, 1, false), noECS)
	if err != nil {
		t.Fatal(err)
	}
	if ip, _ := firstA(t, m); ip != "198.51.100.77" || info.Upstream != "udp://dns.example.com:"+itoaPort(target.Port()) {
		t.Fatalf("answer %s via %q", ip, info.Upstream)
	}
	res := r.Test(context.Background(), "tcp://private.example.com:"+itoaPort(target.Port()))
	if res.OK || res.Error == "" {
		t.Fatalf("a name resolving to a private address must fail: %+v", res)
	}
	if res.Error != errNoPublicAddr.Error() {
		t.Errorf("error %q", res.Error)
	}
	// Named plain upstreams are not part of the clock-guard set.
	if g := r.buildGuardSet(st.Get().DNS, r.def.Load().boot, nil); g == nil || len(g.ups) != 1 || g.ups[0].name != boot.String() {
		t.Errorf("guard set %+v, want only the bootstrap server", g)
	}
}

func itoaPort(p uint16) string { return strconv.Itoa(int(p)) }

// dns.bootstrapPreferIpv6 dials the resolved addresses IPv6 first.
func TestBootstrapPreferIPv6(t *testing.T) {
	boot := startDNS(t, func(w dns.ResponseWriter, q *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(q)
		if q.Question[0].Qtype == dns.TypeA {
			m.Answer = append(m.Answer, aRR(q.Question[0].Name, "198.51.100.1"))
		} else {
			m.Answer = append(m.Answer, aaaaRR(q.Question[0].Name, "2001:db8::1"))
		}
		_ = w.WriteMsg(m)
	})
	for _, prefer6 := range []bool{false, true} {
		b := newBootstrap([]string{boot.Addr().String()}, int(boot.Port()), prefer6)
		addrs, err := b.lookup(context.Background(), "dns.example.com")
		if err != nil || len(addrs) != 2 {
			t.Fatalf("lookup: %v %v", addrs, err)
		}
		if first6 := addrs[0].Is6(); first6 != prefer6 {
			t.Errorf("prefer6=%v: order %v", prefer6, addrs)
		}
	}
	// A settings change rebuilds the sets with the new order.
	st := newStore(t, func(d *settings.DNS) { d.Bootstrap = []string{boot.Addr().String()} })
	r := newTestResolver(t, st, testOptions(), nil)
	defer r.Close()
	if r.def.Load().boot.prefer6 {
		t.Fatal("IPv4 first by default")
	}
	updateDNS(t, st, func(d *settings.DNS) { d.BootstrapPreferIPv6 = true })
	if !r.def.Load().boot.prefer6 {
		t.Error("the setting was not applied")
	}
}
