package upstream

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const up1, up2 = "192.0.2.1", "192.0.2.2"

func oneUpstream(extra func(d *settings.DNS)) func(d *settings.DNS) {
	return func(d *settings.DNS) {
		d.Upstreams = []string{up1}
		d.UpstreamMode = "strict"
		if extra != nil {
			extra(d)
		}
	}
}

func TestCacheHitDecrementsTTLAndKeepsQuestionCase(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: replyA("198.51.100.7", 300)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		ctx := context.Background()

		m, info, err := r.Resolve(ctx, query("Example.COM.", dns.TypeA, 1, false))
		if err != nil {
			t.Fatal(err)
		}
		if info.Cached || info.Upstream != up1 {
			t.Fatalf("first answer info = %+v", info)
		}
		if got := f.queries[0].Question[0].Name; got != "example.com." {
			t.Errorf("upstream qname = %q, want lower case", got)
		}

		time.Sleep(100 * time.Second)
		req := query("EXAMPLE.com.", dns.TypeA, 4242, false)
		m, info, err = r.Resolve(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Cached || info.Stale || info.Upstream != "" {
			t.Fatalf("second answer info = %+v", info)
		}
		if f.calls() != 1 {
			t.Fatalf("upstream calls = %d, want 1", f.calls())
		}
		if m.Id != 4242 || m.Question[0] != req.Question[0] {
			t.Errorf("reply id/question = %d %v, want %d %v", m.Id, m.Question[0], req.Id, req.Question[0])
		}
		if ip, ttl := firstA(t, m); ip != "198.51.100.7" || ttl != 200 {
			t.Errorf("answer = %s ttl %d, want 198.51.100.7 ttl 200", ip, ttl)
		}

		time.Sleep(201 * time.Second) // expired: served stale (serve-stale is on by default)
		_, info, _ = r.Resolve(ctx, query("example.com.", dns.TypeA, 5, false))
		if !info.Stale {
			t.Errorf("expected a stale answer after expiry, got %+v", info)
		}
		st := r.CacheStats()
		if st.Hits != 2 || st.StaleHits != 1 || st.Misses != 1 || st.Entries != 1 {
			t.Errorf("cache stats = %+v", st)
		}
	})
}

func TestNegativeCaching(t *testing.T) {
	tests := []struct {
		name      string
		rcode     int
		soa       *dns.SOA
		cached    bool
		wantTTL   uint32 // cache lifetime and SOA TTL of the answer
		configure func(d *settings.DNS)
	}{
		{name: "nxdomain uses soa minimum", rcode: dns.RcodeNameError, soa: soa("example.", 7200, 600), cached: true, wantTTL: 600},
		{name: "nodata uses soa ttl when smaller", rcode: dns.RcodeSuccess, soa: soa("example.", 120, 600), cached: true, wantTTL: 120},
		{name: "capped at one hour", rcode: dns.RcodeNameError, soa: soa("example.", 86400, 86400), cached: true, wantTTL: 3600},
		{name: "max ttl clamp applies", rcode: dns.RcodeNameError, soa: soa("example.", 900, 900), cached: true, wantTTL: 300,
			configure: func(d *settings.DNS) { d.CacheMaxTTL = 300 }},
		{name: "no soa is not cached", rcode: dns.RcodeNameError, cached: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, oneUpstream(func(d *settings.DNS) {
				d.ServeStale = false
				if tc.configure != nil {
					tc.configure(d)
				}
			}))
			synctest.Test(t, func(t *testing.T) {
				f := &fakeTransport{fn: func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
					m := new(dns.Msg)
					m.SetRcode(q, tc.rcode)
					if tc.soa != nil {
						m.Ns = []dns.RR{dns.Copy(tc.soa)}
					}
					return m, nil
				}}
				r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
				defer r.Close()
				ctx := context.Background()
				m, _, err := r.Resolve(ctx, query("missing.example.", dns.TypeA, 1, false))
				if err != nil {
					t.Fatal(err)
				}
				if m.Rcode != tc.rcode {
					t.Fatalf("rcode = %d", m.Rcode)
				}
				if tc.soa != nil && m.Ns[0].Header().Ttl != tc.wantTTL {
					t.Errorf("fresh SOA ttl = %d, want %d", m.Ns[0].Header().Ttl, tc.wantTTL)
				}
				if !tc.cached {
					_, info, _ := r.Resolve(ctx, query("missing.example.", dns.TypeA, 2, false))
					if info.Cached || f.calls() != 2 {
						t.Fatalf("answer without SOA must not be cached (calls %d)", f.calls())
					}
					return
				}
				time.Sleep(time.Duration(tc.wantTTL-1) * time.Second)
				m, info, _ := r.Resolve(ctx, query("missing.example.", dns.TypeA, 2, false))
				if !info.Cached || m.Ns[0].Header().Ttl != 1 {
					t.Fatalf("before expiry: info %+v soa ttl %d", info, m.Ns[0].Header().Ttl)
				}
				time.Sleep(time.Second)
				if _, info, _ = r.Resolve(ctx, query("missing.example.", dns.TypeA, 3, false)); info.Cached {
					t.Fatal("negative answer served after its TTL")
				}
			})
		})
	}
}

func TestServfailCachedFiveSeconds(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: rcodeReply(dns.RcodeServerFailure)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		ctx := context.Background()
		for i, wantCalls := range []int{1, 1, 2} {
			m, _, err := r.Resolve(ctx, query("broken.example.", dns.TypeA, 1, false))
			if err != nil || m.Rcode != dns.RcodeServerFailure {
				t.Fatalf("query %d: %v %v", i, m, err)
			}
			if f.calls() != wantCalls {
				t.Fatalf("query %d: calls = %d, want %d", i, f.calls(), wantCalls)
			}
			time.Sleep(3 * time.Second)
		}
	})
}

func TestTTLClamp(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) {
		d.CacheMinTTL = 60
		d.CacheMaxTTL = 600
	}))
	synctest.Test(t, func(t *testing.T) {
		ttls := map[string]uint32{"short.example.": 5, "long.example.": 86400}
		f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
			return answerA(q, "192.0.2.53", ttls[q.Question[0].Name]), nil
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		for name, want := range map[string]uint32{"short.example.": 60, "long.example.": 600} {
			m, _, err := r.Resolve(context.Background(), query(name, dns.TypeA, 1, false))
			if err != nil {
				t.Fatal(err)
			}
			if _, ttl := firstA(t, m); ttl != want {
				t.Errorf("%s: ttl = %d, want %d", name, ttl, want)
			}
		}
		time.Sleep(59 * time.Second)
		if _, info, _ := r.Resolve(context.Background(), query("short.example.", dns.TypeA, 1, false)); !info.Cached {
			t.Error("short TTL was not raised to the minimum")
		}
	})
}

func TestCacheKeyIncludesDOAndQtype(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: replyA("192.0.2.9", 300)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		ctx := context.Background()
		reqs := []*dns.Msg{
			query("a.example.", dns.TypeA, 1, false),
			query("a.example.", dns.TypeA, 2, true),
			query("a.example.", dns.TypeAAAA, 3, false),
			query("A.EXAMPLE.", dns.TypeA, 4, false), // same key as the first
		}
		for _, q := range reqs {
			if _, _, err := r.Resolve(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		if f.calls() != 3 {
			t.Fatalf("upstream calls = %d, want 3", f.calls())
		}
		if !f.queries[1].IsEdns0().Do() || f.queries[0].IsEdns0().Do() {
			t.Error("DO bit not forwarded per client")
		}
	})
}

func TestLRUEvictionAndFlush(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) { d.CacheSize = 2 }))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: replyA("192.0.2.9", 300)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		ctx := context.Background()
		for _, n := range []string{"a.example.", "b.example.", "a.example.", "c.example."} {
			if _, _, err := r.Resolve(ctx, query(n, dns.TypeA, 1, false)); err != nil {
				t.Fatal(err)
			}
		}
		// b was least recently used and must be gone; a must still be cached.
		if _, info, _ := r.Resolve(ctx, query("a.example.", dns.TypeA, 1, false)); !info.Cached {
			t.Error("a.example evicted")
		}
		if _, info, _ := r.Resolve(ctx, query("b.example.", dns.TypeA, 1, false)); info.Cached {
			t.Error("b.example not evicted")
		}
		if got := r.CacheStats(); got.Entries != 2 || got.Capacity != 2 {
			t.Errorf("stats = %+v", got)
		}
		r.FlushCache()
		if got := r.CacheStats().Entries; got != 0 {
			t.Errorf("entries after flush = %d", got)
		}
	})
}

func TestCacheDisabled(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) { d.CacheEnabled = false }))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: replyA("192.0.2.9", 300)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		for range 2 {
			if _, info, err := r.Resolve(context.Background(), query("a.example.", dns.TypeA, 1, false)); err != nil || info.Cached {
				t.Fatal(info, err)
			}
		}
		if f.calls() != 2 || r.CacheStats().Capacity != 0 {
			t.Errorf("calls %d stats %+v", f.calls(), r.CacheStats())
		}
	})
}

func TestServeStaleRefreshesOncePerKey(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		n := 0
		f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
			n++
			if n == 1 {
				return answerA(q, "192.0.2.10", 10), nil
			}
			<-release
			return answerA(q, "192.0.2.20", 10), nil
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		stop := start(r)
		defer stop()
		synctest.Wait()
		ctx := context.Background()
		if _, _, err := r.Resolve(ctx, query("stale.example.", dns.TypeA, 1, false)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(15 * time.Second)
		for i := range 5 {
			m, info, err := r.Resolve(ctx, query("stale.example.", dns.TypeA, uint16(i), false))
			if err != nil {
				t.Fatal(err)
			}
			ip, ttl := firstA(t, m)
			if !info.Stale || !info.Cached || ip != "192.0.2.10" || ttl != staleTTL {
				t.Fatalf("stale answer %d: %+v %s ttl %d", i, info, ip, ttl)
			}
		}
		synctest.Wait()
		if f.calls() != 2 {
			t.Fatalf("upstream calls = %d, want exactly one refresh", f.calls())
		}
		close(release)
		synctest.Wait()
		m, info, _ := r.Resolve(ctx, query("stale.example.", dns.TypeA, 9, false))
		if ip, ttl := firstA(t, m); info.Stale || ip != "192.0.2.20" || ttl != 10 {
			t.Fatalf("after refresh: %+v %s ttl %d", info, ip, ttl)
		}
	})
}

func TestStaleEntrySurvivesFailedRefresh(t *testing.T) {
	st := newStore(t, oneUpstream(nil))
	synctest.Test(t, func(t *testing.T) {
		fail := false
		f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
			if !fail {
				return answerA(q, "192.0.2.10", 10), nil
			}
			m := new(dns.Msg)
			m.SetRcode(q, dns.RcodeServerFailure)
			return m, nil
		}}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		stop := start(r)
		defer stop()
		synctest.Wait()
		ctx := context.Background()
		if _, _, err := r.Resolve(ctx, query("stale.example.", dns.TypeA, 1, false)); err != nil {
			t.Fatal(err)
		}
		fail = true
		time.Sleep(20 * time.Second)
		for range 2 {
			_, info, err := r.Resolve(ctx, query("stale.example.", dns.TypeA, 2, false))
			if err != nil || !info.Stale {
				t.Fatalf("want stale answer, got %+v %v", info, err)
			}
			synctest.Wait()
		}
		if f.calls() != 2 {
			t.Fatalf("calls = %d: refresh must back off after a failure", f.calls())
		}
		time.Sleep(refreshBackoff)
		if _, info, _ := r.Resolve(ctx, query("stale.example.", dns.TypeA, 3, false)); !info.Stale {
			t.Fatal("SERVFAIL replaced the stale entry")
		}
		synctest.Wait()
		if f.calls() != 3 {
			t.Fatalf("calls = %d, want a new refresh after the backoff", f.calls())
		}
	})
}

func TestStaleWindowEnds(t *testing.T) {
	st := newStore(t, oneUpstream(func(d *settings.DNS) { d.ServeStaleMaxAgeSec = 60 }))
	synctest.Test(t, func(t *testing.T) {
		f := &fakeTransport{fn: replyA("192.0.2.10", 10)}
		r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: f})
		defer r.Close()
		ctx := context.Background()
		_, _, _ = r.Resolve(ctx, query("old.example.", dns.TypeA, 1, false))
		time.Sleep(10*time.Second + time.Minute)
		if _, info, _ := r.Resolve(ctx, query("old.example.", dns.TypeA, 1, false)); info.Cached {
			t.Fatal("entry served beyond the serve-stale window")
		}
	})
}

func TestClamp(t *testing.T) {
	tests := []struct {
		ttl, min, max, want uint32
	}{
		{ttl: 100, want: 100},
		{ttl: 5, min: 60, max: 600, want: 60},
		{ttl: 86400, min: 60, max: 600, want: 600},
		{ttl: 1 << 31, want: hardMaxTTL},
		{ttl: 30 * 86400, min: 10 * 86400, want: hardMaxTTL},
	}
	for _, tc := range tests {
		if got := clamp(tc.ttl, cachePolicy{minTTL: tc.min, maxTTL: tc.max}); got != tc.want {
			t.Errorf("clamp(%d, min %d, max %d) = %d, want %d", tc.ttl, tc.min, tc.max, got, tc.want)
		}
	}
}
