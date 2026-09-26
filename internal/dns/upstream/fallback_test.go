package upstream

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const fb1, fb2 = "192.0.2.201", "192.0.2.202"

// hanging never answers (a dead upstream: timeout).
func hanging(ctx context.Context, _ *dns.Msg) (*dns.Msg, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The fallbacks are asked only when no default upstream replied at all;
// a reply of any rcode, or one carrying a blocking EDE, never leads to them.
func TestFallbackTrigger(t *testing.T) {
	for _, tc := range []struct {
		name     string
		primary  func(context.Context, *dns.Msg) (*dns.Msg, error)
		fallback bool
		rcode    int
	}{
		{"transport error", failing, true, dns.RcodeSuccess},
		{"timeout", hanging, true, dns.RcodeSuccess},
		{"SERVFAIL", rcodeReply(dns.RcodeServerFailure), false, dns.RcodeServerFailure},
		{"REFUSED", rcodeReply(dns.RcodeRefused), false, dns.RcodeRefused},
		{"NXDOMAIN", rcodeReply(dns.RcodeNameError), false, dns.RcodeNameError},
		{"EDE 15", func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			return withEDE(reply(q, dns.RcodeNameError, true), &dns.EDNS0_EDE{InfoCode: 15}), nil
		}, false, dns.RcodeNameError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, func(d *settings.DNS) {
				d.Upstreams = []string{up1, up2}
				d.FallbackUpstreams = []string{fb1}
				d.CacheEnabled = false
			})
			synctest.Test(t, func(t *testing.T) {
				p1, p2 := &fakeTransport{fn: tc.primary}, &fakeTransport{fn: tc.primary}
				f := &fakeTransport{fn: replyA("198.51.100.99", 60)}
				r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: p1, up2: p2, fb1: f})
				defer r.Close()
				m, info, err := r.Resolve(context.Background(), query("fb.example.", dns.TypeA, 1, false), noECS)
				if err != nil {
					t.Fatal(err)
				}
				if got := f.calls() > 0; got != tc.fallback || info.Fallback != tc.fallback || m.Rcode != tc.rcode {
					t.Fatalf("fallback asked %v, info %+v, rcode %d; want %v %d", got, info, m.Rcode, tc.fallback, tc.rcode)
				}
				if tc.fallback {
					if info.Upstream != fb1 || r.LastFallback().IsZero() {
						t.Errorf("upstream %q, last fallback %v", info.Upstream, r.LastFallback())
					}
					if fs := r.FallbackStats(); len(fs) != 1 || fs[0].Upstream != fb1 || fs[0].Queries != 1 {
						t.Errorf("fallback stats %+v", fs)
					}
				} else if !r.LastFallback().IsZero() {
					t.Error("LastFallback set without a fallback answer")
				}
			})
		})
	}
}

// Four dead upstreams with a 10 s upstream timeout still get a fallback
// answer before the 10 s a client query may take.
func TestFallbackBudget(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{up1, up2, "192.0.2.3", "192.0.2.4"}
		d.FallbackUpstreams = []string{fb1, fb2}
		d.UpstreamTimeoutMs = 10000
		d.UpstreamMode = "load_balance"
	})
	synctest.Test(t, func(t *testing.T) {
		fakes := map[string]*fakeTransport{}
		for _, u := range []string{up1, up2, "192.0.2.3", "192.0.2.4"} {
			fakes[u] = &fakeTransport{fn: hanging}
		}
		fakes[fb1] = &fakeTransport{fn: hanging} // one fallback is dead too
		fakes[fb2] = &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			time.Sleep(50 * time.Millisecond)
			return answerA(q, "198.51.100.98", 60), nil
		}}
		opts := testOptions()
		opts.attempt = 3 * time.Second
		r := newTestResolver(t, st, opts, fakes)
		defer r.Close()
		start := time.Now()
		m, info, err := r.Resolve(context.Background(), query("budget.example.", dns.TypeA, 1, false), noECS)
		if err != nil {
			t.Fatal(err)
		}
		if el := time.Since(start); el >= 10*time.Second || el < 7*time.Second {
			t.Fatalf("answered after %v, want between 7 and 10 s", el)
		}
		if ip, _ := firstA(t, m); ip != "198.51.100.98" || !info.Fallback || info.Upstream != fb2 {
			t.Fatalf("answer %s via %q (fallback %v)", ip, info.Upstream, info.Fallback)
		}
		// Cached under the default set's key: the next query is a hit that
		// still says it came from a fallback.
		_, info, _ = r.Resolve(context.Background(), query("budget.example.", dns.TypeA, 2, false), noECS)
		if !info.Cached || !info.Fallback || info.Upstream != "" {
			t.Fatalf("cache hit %+v", info)
		}
	})
}

// When the fallbacks fail too, nothing is cached and the error reaches the
// caller.
func TestFallbackFailsToo(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{up1}
		d.FallbackUpstreams = []string{fb1}
	})
	f := &fakeTransport{fn: failing}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: {fn: failing}, fb1: f})
	defer r.Close()
	_, _, err := r.Resolve(context.Background(), query("dead.example.", dns.TypeA, 1, false), noECS)
	if err == nil || f.calls() != 1 {
		t.Fatalf("err %v, fallback calls %d", err, f.calls())
	}
	if r.CacheStats().Entries != 0 || !r.LastFallback().IsZero() {
		t.Errorf("entries %d, last fallback %v", r.CacheStats().Entries, r.LastFallback())
	}
}

// ResolveVia sets never use the fallbacks; neither does the clock guard.
func TestFallbackScope(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{"https://dns.example/dns-query"}
		d.FallbackUpstreams = []string{fb1}
		d.CacheEnabled = false
	})
	f := &fakeTransport{fn: replyA("198.51.100.99", 60)}
	router := &fakeTransport{fn: failing}
	doh := &fakeTransport{fn: failing}
	opts := testOptions()
	r := newTestResolver(t, st, opts, map[string]*fakeTransport{fb1: f, "192.168.178.1": router, "https://dns.example/dns-query": doh})
	defer r.Close()
	if _, _, err := r.ResolveVia(context.Background(), query("nas.fritz.box.", dns.TypeA, 1, false), []string{"192.168.178.1"}); err == nil {
		t.Fatal("the router failing must fail the query")
	}
	if f.calls() != 0 {
		t.Fatal("a ResolveVia set used the fallbacks")
	}
	// Clock guard: plain DNS to the bootstrap servers, no fallbacks.
	boot := &fakeTransport{fn: failing}
	opts.buildDate = time.Now().Add(24 * time.Hour)
	g := newTestResolver(t, st, opts, map[string]*fakeTransport{fb1: f, "9.9.9.9:53": boot, "149.112.112.112:53": boot,
		"1.1.1.1:53": boot, "1.0.0.1:53": boot, "[2620:fe::fe]:53": boot, "[2606:4700:4700::1111]:53": boot})
	defer g.Close()
	if _, _, err := g.Resolve(context.Background(), query("guard.example.", dns.TypeA, 1, false), noECS); err == nil || !g.ClockGuard() {
		t.Fatalf("clock guard: err %v guard %v", err, g.ClockGuard())
	}
	if f.calls() != 0 || boot.calls() == 0 {
		t.Fatalf("fallback calls %d, guard calls %d", f.calls(), boot.calls())
	}
	if len(g.FallbackStats()) != 1 {
		t.Errorf("fallback stats while the clock guard is active: %+v", g.FallbackStats())
	}
}
