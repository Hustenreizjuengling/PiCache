package upstream

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

func TestDedupSharesOneExchangeWithPrivateCopies(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			<-release
			return answerA(q, "192.0.2.77", 60), nil
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()

		const n = 10
		names := []string{"dedup.example.", "DEDUP.example.", "Dedup.Example."}
		replies := make([]*dns.Msg, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				m, _, err := r.Resolve(context.Background(), query(names[i%len(names)], dns.TypeA, uint16(100+i), false))
				if err != nil {
					t.Error(err)
				}
				replies[i] = m
			})
		}
		synctest.Wait()
		if f.calls() != 1 {
			t.Fatalf("upstream calls = %d, want 1", f.calls())
		}
		close(release)
		wg.Wait()
		for i, m := range replies {
			if m.Id != uint16(100+i) || m.Question[0].Name != names[i%len(names)] {
				t.Errorf("reply %d: id %d question %q", i, m.Id, m.Question[0].Name)
			}
		}
		replies[0].Answer[0].Header().Ttl = 1
		if replies[1].Answer[0].Header().Ttl != 60 {
			t.Error("waiters share one message")
		}
	})
}

func TestWaiterCancellationDoesNotFailOthers(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			time.Sleep(200 * time.Millisecond)
			return answerA(q, "192.0.2.77", 60), nil
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		var wg sync.WaitGroup
		wg.Go(func() {
			if _, _, err := r.Resolve(ctx, query("slow.example.", dns.TypeA, 1, false)); !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("impatient caller: err = %v", err)
			}
		})
		if _, _, err := r.Resolve(context.Background(), query("slow.example.", dns.TypeA, 2, false)); err != nil {
			t.Fatalf("patient caller: %v", err)
		}
		wg.Wait()
	})
}

func twoUpstreams(mode string) func(d *settings.DNS) {
	return func(d *settings.DNS) {
		d.Upstreams = []string{up1, up2}
		d.UpstreamMode = mode
		d.CacheEnabled = false
		d.UpstreamTimeoutMs = 5000
	}
}

func failing(context.Context, *dns.Msg) (*dns.Msg, error) {
	return nil, errors.New("connection refused")
}

func TestModes(t *testing.T) {
	tests := []struct {
		name      string
		mode      string
		fn1, fn2  func(context.Context, *dns.Msg) (*dns.Msg, error)
		queries   int
		wantFrom  string
		check     func(t *testing.T, f1, f2 *fakeTransport, r *Resolver)
		wantError bool
	}{
		{
			name: "strict always asks the first upstream first", mode: "strict", queries: 5,
			fn1: failing, fn2: replyA("192.0.2.2", 60), wantFrom: up2,
			check: func(t *testing.T, f1, f2 *fakeTransport, r *Resolver) {
				if f1.calls() != 5 || f2.calls() != 5 {
					t.Errorf("calls = %d/%d, want 5/5", f1.calls(), f2.calls())
				}
				st := r.Stats()
				if st[0].Errors != 5 || st[0].Healthy || st[0].LastError == "" || !st[1].Healthy {
					t.Errorf("stats = %+v", st)
				}
			},
		},
		{
			name: "strict falls over on SERVFAIL", mode: "strict", queries: 1,
			fn1: rcodeReply(dns.RcodeServerFailure), fn2: replyA("192.0.2.2", 60), wantFrom: up2,
		},
		{
			name: "strict returns SERVFAIL when nothing better answers", mode: "strict", queries: 1,
			fn1: rcodeReply(dns.RcodeServerFailure), fn2: failing, wantFrom: up1,
		},
		{
			name: "load_balance avoids a failing upstream", mode: "load_balance", queries: 50,
			fn1: failing, fn2: replyA("192.0.2.2", 60), wantFrom: up2,
			check: func(t *testing.T, f1, f2 *fakeTransport, r *Resolver) {
				if f2.calls() != 50 || f1.calls() > 10 {
					t.Errorf("calls = %d/%d: the failing upstream was not penalised", f1.calls(), f2.calls())
				}
			},
		},
		{
			name: "parallel takes the fastest answer", mode: "parallel", queries: 1,
			fn1: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
				select {
				case <-time.After(2 * time.Second):
					return answerA(q, "192.0.2.1", 60), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
			fn2: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
				time.Sleep(100 * time.Millisecond)
				return answerA(q, "192.0.2.2", 60), nil
			},
			wantFrom: up2,
			check: func(t *testing.T, f1, f2 *fakeTransport, r *Resolver) {
				synctest.Wait()
				if f1.calls() != 1 || f2.calls() != 1 {
					t.Errorf("calls = %d/%d, want 1/1", f1.calls(), f2.calls())
				}
				if st := r.Stats(); st[0].Errors != 0 || st[0].Queries != 0 {
					t.Errorf("cancelled loser was counted: %+v", st[0])
				}
			},
		},
		{
			name: "all upstreams failing is an error", mode: "load_balance", queries: 1,
			fn1: failing, fn2: failing, wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, twoUpstreams(tc.mode))
			synctest.Test(t, func(t *testing.T) {
				f1, f2 := &fakeTransport{fn: tc.fn1}, &fakeTransport{fn: tc.fn2}
				r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f1, up2: f2})
				defer r.Close()
				for i := range tc.queries {
					_, info, err := r.Resolve(context.Background(), query("mode.example.", dns.TypeA, uint16(i), false))
					if tc.wantError {
						if err == nil || !strings.Contains(err.Error(), "all upstreams failed") {
							t.Fatalf("err = %v", err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if info.Upstream != tc.wantFrom {
						t.Fatalf("query %d answered by %q, want %q", i, info.Upstream, tc.wantFrom)
					}
				}
				if tc.check != nil {
					tc.check(t, f1, f2, r)
				}
			})
		})
	}
}

func TestTimeouts(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{up1, up2}
		d.UpstreamMode = "strict"
		d.UpstreamTimeoutMs = 1500
	})
	synctest.Test(t, func(t *testing.T) {
		hang := func(ctx context.Context, _ *dns.Msg) (*dns.Msg, error) {
			<-ctx.Done()
			return nil, ctxErr(ctx, ctx.Err())
		}
		f1, f2 := &fakeTransport{fn: hang}, &fakeTransport{fn: hang}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f1, up2: f2})
		defer r.Close()
		begin := time.Now()
		_, _, err := r.Resolve(context.Background(), query("hang.example.", dns.TypeA, 1, false))
		if err == nil || !errors.Is(err, errTimeout) {
			t.Fatalf("err = %v", err)
		}
		// First attempt: the full per-attempt timeout (1 s); second: the
		// rest of the 1.5 s total.
		if d := time.Since(begin); d != 1500*time.Millisecond {
			t.Errorf("took %v, want 1.5s", d)
		}
		if f1.calls() != 1 || f2.calls() != 1 {
			t.Errorf("calls = %d/%d", f1.calls(), f2.calls())
		}
	})
}

func TestResolveViaUsesSeparateNamespace(t *testing.T) {
	const router = "192.168.1.1"
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		def := &fakeTransport{fn: replyA("198.51.100.1", 300)}
		via := &fakeTransport{fn: replyA("192.168.1.50", 300)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: def, router: via})
		defer r.Close()
		ctx := context.Background()
		for range 2 {
			m, _, err := r.ResolveVia(ctx, query("nas.fritz.box.", dns.TypeA, 1, false), []string{router})
			if err != nil {
				t.Fatal(err)
			}
			if ip, _ := firstA(t, m); ip != "192.168.1.50" {
				t.Fatalf("via answer = %s", ip)
			}
		}
		m, _, err := r.Resolve(ctx, query("nas.fritz.box.", dns.TypeA, 1, false))
		if err != nil {
			t.Fatal(err)
		}
		if ip, _ := firstA(t, m); ip != "198.51.100.1" {
			t.Fatalf("default answer = %s: namespaces are shared", ip)
		}
		if via.calls() != 1 || def.calls() != 1 {
			t.Errorf("calls via=%d default=%d, want 1/1", via.calls(), def.calls())
		}
		if _, _, err := r.ResolveVia(ctx, query("x.", dns.TypeA, 1, false), []string{"not an upstream"}); err == nil {
			t.Error("invalid via upstream accepted")
		}
	})
}

func TestUpstreamQueryIsBuiltFresh(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) { d.DNSSEC = true }))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: replyA("192.0.2.1", 60)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		req := query("Fresh.Example.", dns.TypeA, 7, false)
		req.CheckingDisabled = true
		req.SetEdns0(4096, false)
		req.IsEdns0().Option = append(req.IsEdns0().Option,
			&dns.EDNS0_SUBNET{Code: dns.EDNS0SUBNET, Family: 1, SourceNetmask: 24, Address: []byte{192, 168, 1, 0}},
			&dns.EDNS0_COOKIE{Code: dns.EDNS0COOKIE, Cookie: "0123456789abcdef"})
		before := req.Copy()
		if _, _, err := r.Resolve(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if req.String() != before.String() {
			t.Error("Resolve modified the request")
		}
		q := f.queries[0]
		opt := q.IsEdns0()
		switch {
		case !q.RecursionDesired, !q.AuthenticatedData, q.CheckingDisabled:
			t.Errorf("flags: %v", q.MsgHdr)
		case opt == nil || opt.UDPSize() != ednsSize || !opt.Do() || len(opt.Option) != 0:
			t.Errorf("OPT = %v", opt)
		case q.Question[0].Name != "fresh.example.":
			t.Errorf("qname = %q", q.Question[0].Name)
		}
	})
}

func TestSettingsChangeRebuildsUpstreams(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) { d.CacheEnabled = false }))
	f1 := &fakeTransport{fn: replyA("192.0.2.1", 60)}
	f2 := &fakeTransport{fn: replyA("192.0.2.2", 60)}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f1, up2: f2})
	defer r.Close()
	ctx := context.Background()
	if _, info, _ := r.Resolve(ctx, query("a.example.", dns.TypeA, 1, false)); info.Upstream != up1 {
		t.Fatalf("answered by %q", info.Upstream)
	}
	updateDNS(t, st, func(d *settings.DNS) { d.Upstreams = []string{up2, up1} })
	if _, info, _ := r.Resolve(ctx, query("a.example.", dns.TypeA, 1, false)); info.Upstream != up2 {
		t.Fatalf("after change answered by %q", info.Upstream)
	}
	stats := r.Stats()
	if len(stats) != 2 || stats[0].Upstream != up2 || stats[1].Queries != 1 {
		t.Errorf("stats = %+v (the kept upstream must keep its counters)", stats)
	}
}

func TestResolveRejectsBadRequestsAndClosedResolver(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: {fn: replyA("192.0.2.1", 60)}})
	if _, _, err := r.Resolve(context.Background(), new(dns.Msg)); !errors.Is(err, errBadRequest) {
		t.Errorf("empty question: %v", err)
	}
	_ = r.Close()
	if _, _, err := r.Resolve(context.Background(), query("a.example.", dns.TypeA, 1, false)); !errors.Is(err, errClosed) {
		t.Errorf("after close: %v", err)
	}
}

// Duplicate records (RFC 2181 5; Docker's embedded DNS repeats every A and
// AAAA record) are removed before the answer is cached and returned, keeping
// the first copy with the lowest TTL.
func TestDuplicateRecordsAreRemoved(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
			m := new(dns.Msg)
			m.SetReply(q)
			for _, s := range []string{
				"www.example.com. 300 IN CNAME example.com.",
				"example.com. 300 IN A 192.0.2.1",
				"example.com. 300 IN A 192.0.2.2",
				"www.example.com. 300 IN CNAME example.com.",
				"EXAMPLE.com. 120 IN A 192.0.2.1",
				"example.com. 300 IN A 192.0.2.2",
			} {
				m.Answer = append(m.Answer, mustRR(s))
			}
			return m, nil
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()

		want := []string{
			"www.example.com.\t300\tIN\tCNAME\texample.com.",
			"example.com.\t120\tIN\tA\t192.0.2.1",
			"example.com.\t300\tIN\tA\t192.0.2.2",
		}
		m, _, err := r.Resolve(context.Background(), query("www.example.com.", dns.TypeA, 1, false))
		if err != nil {
			t.Fatal(err)
		}
		if got := rrStrings(m.Answer); !slices.Equal(got, want) {
			t.Fatalf("answer %q, want %q", got, want)
		}
		m, info, err := r.Resolve(context.Background(), query("www.example.com.", dns.TypeA, 2, false))
		if err != nil || !info.Cached {
			t.Fatalf("second answer: %v %+v", err, info)
		}
		if got := rrStrings(m.Answer); !slices.Equal(got, want) {
			t.Errorf("cached answer %q, want %q", got, want)
		}
	})
}

func TestDedupSection(t *testing.T) {
	rrs := func(ss ...string) []dns.RR {
		var out []dns.RR
		for _, s := range ss {
			out = append(out, mustRR(s))
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		in, out []dns.RR
	}{
		{"single", rrs("a.example. 60 IN A 192.0.2.1"), rrs("a.example. 60 IN A 192.0.2.1")},
		{"owner case", rrs("a.example. 60 IN A 192.0.2.1", "A.EXAMPLE. 30 IN A 192.0.2.1"), rrs("a.example. 30 IN A 192.0.2.1")},
		{"interleaved", rrs("a.example. 60 IN AAAA 2001:db8::1", "a.example. 60 IN AAAA 2001:db8::2", "a.example. 60 IN AAAA 2001:db8::1", "a.example. 60 IN AAAA 2001:db8::2"),
			rrs("a.example. 60 IN AAAA 2001:db8::1", "a.example. 60 IN AAAA 2001:db8::2")},
		{"distinct owners", rrs("a.example. 60 IN A 192.0.2.1", "b.example. 60 IN A 192.0.2.1"), rrs("a.example. 60 IN A 192.0.2.1", "b.example. 60 IN A 192.0.2.1")},
		{"distinct types", rrs("a.example. 60 IN A 192.0.2.1", "a.example. 60 IN AAAA ::ffff:192.0.2.1"), rrs("a.example. 60 IN A 192.0.2.1", "a.example. 60 IN AAAA ::ffff:192.0.2.1")},
		{"TXT is case-sensitive", rrs(`a.example. 60 IN TXT "x"`, `a.example. 60 IN TXT "X"`, `a.example. 60 IN TXT "x"`), rrs(`a.example. 60 IN TXT "x"`, `a.example. 60 IN TXT "X"`)},
	} {
		if got, want := rrStrings(dedupSection(tc.in)), rrStrings(tc.out); !slices.Equal(got, want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, want)
		}
	}
	big := make([]dns.RR, maxDedupRRs+1)
	for i := range big {
		big[i] = mustRR("a.example. 60 IN A 192.0.2.1")
	}
	if got := dedupSection(big); len(got) != len(big) {
		t.Errorf("sections above the bound must be passed on unchanged, got %d records", len(got))
	}
}

func rrStrings(rrs []dns.RR) []string {
	out := make([]string, len(rrs))
	for i, rr := range rrs {
		out[i] = rr.String()
	}
	return out
}

func mustRR(s string) dns.RR {
	rr, err := dns.NewRR(s)
	if err != nil {
		panic(err)
	}
	return rr
}
