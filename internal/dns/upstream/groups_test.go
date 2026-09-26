package upstream

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/settings"
)

const cloudflareFamily = "https://family.cloudflare-dns.com/dns-query"

// GroupFor: a group with a preset wins over one with its own upstreams;
// among equals the lowest id; groups naming the same list share one set.
func TestGroupSelection(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{"192.0.2.53"} })
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{})
	defer r.Close()
	if _, ok := r.GroupFor([]int64{1, 2}); ok {
		t.Fatal("no group sets yet")
	}
	r.SetGroupUpstreams([]GroupUpstreams{
		{ID: 2, Name: "Two", Upstreams: []string{"192.0.2.1"}},
		{ID: 3, Name: "Three", Preset: "cloudflare-family"},
		{ID: 4, Name: "Four", Upstreams: []string{"192.0.2.1"}},
		{ID: 5, Name: "Five", Preset: "cloudflare-family"},
	})
	cases := []struct {
		groups []int64
		want   int64
		ok     bool
	}{
		{[]int64{1, 2, 3}, 3, true},
		{[]int64{2, 4}, 2, true},
		{[]int64{4, 5}, 5, true},
		{[]int64{1}, 0, false},
		{nil, 0, false},
	}
	for _, c := range cases {
		if got, ok := r.GroupFor(c.groups); got != c.want || ok != c.ok {
			t.Errorf("GroupFor(%v) = %d %v, want %d %v", c.groups, got, ok, c.want, c.ok)
		}
	}
	if r.GroupName(4) != "Four" || r.GroupName(9) != "" {
		t.Errorf("names %q %q", r.GroupName(4), r.GroupName(9))
	}
	stats := r.GroupStats()
	if len(stats) != 2 || !slices.Equal(stats[0].GroupIDs, []int64{2, 4}) || !slices.Equal(stats[1].GroupIDs, []int64{3, 5}) ||
		stats[1].Preset != "cloudflare-family" || len(stats[0].Upstreams) != 1 || !slices.Equal(stats[1].Names, []string{"Three", "Five"}) {
		t.Fatalf("stats %+v", stats)
	}
	if n := testing.AllocsPerRun(100, func() { r.GroupFor([]int64{1, 2, 3}) }); n != 0 {
		t.Errorf("GroupFor allocates %.1f times", n)
	}
}

// A group set has its own cache namespace and no fallbacks: an outage is an
// error, never the default set or the fallbacks; blocked answers are
// classified like the default set's.
func TestGroupSetResolve(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{"192.0.2.53"}
		d.FallbackUpstreams = []string{"192.0.2.99"}
	})
	def := &fakeTransport{fn: replyA("198.51.100.1", 300)}
	fb := &fakeTransport{fn: replyA("198.51.100.99", 300)}
	grp := &fakeTransport{fn: replyA("203.0.113.1", 300)}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{"192.0.2.53": def, "192.0.2.99": fb, "192.0.2.7": grp})
	stop := start(r)
	defer stop()
	r.SetGroupUpstreams([]GroupUpstreams{{ID: 2, Name: "Kids", Upstreams: []string{"192.0.2.7"}}})
	ctx := context.Background()
	m, _, err := r.Resolve(ctx, query("a.example.", dns.TypeA, 1, false), noECS)
	if err != nil {
		t.Fatal(err)
	}
	if ip, _ := firstA(t, m); ip != "198.51.100.1" {
		t.Fatalf("default %s", ip)
	}
	m, info, err := r.ResolveGroup(ctx, query("a.example.", dns.TypeA, 2, false), 2)
	if err != nil || info.Cached || info.Upstream != "192.0.2.7" {
		t.Fatalf("group %+v %v", info, err)
	}
	if ip, _ := firstA(t, m); ip != "203.0.113.1" {
		t.Fatalf("group answer %s: the caches are not separate", ip)
	}
	if _, info, _ = r.ResolveGroup(ctx, query("a.example.", dns.TypeA, 3, false), 2); !info.Cached {
		t.Fatal("the group answer is not cached")
	}
	for _, q := range grp.queries {
		if opt := q.IsEdns0(); opt != nil {
			for _, o := range opt.Option {
				if _, ok := o.(*dns.EDNS0_SUBNET); ok {
					t.Fatal("a client subnet went to a group set")
				}
			}
		}
	}
	grp.fn = func(context.Context, *dns.Msg) (*dns.Msg, error) { return nil, errors.New("down") }
	if _, _, err := r.ResolveGroup(ctx, query("b.example.", dns.TypeA, 4, false), 2); err == nil {
		t.Fatal("an outage of the group upstreams must fail")
	}
	if fb.calls() != 0 {
		t.Fatal("a group set asked the fallbacks")
	}
	grp.fn = func(_ context.Context, q *dns.Msg) (*dns.Msg, error) { return answerA(q, "0.0.0.0", 60), nil }
	if _, info, err := r.ResolveGroup(ctx, query("c.example.", dns.TypeA, 5, false), 2); err != nil || info.Block == nil {
		t.Fatalf("classification %+v %v", info, err)
	}
	if _, _, err := r.ResolveGroup(ctx, query("c.example.", dns.TypeA, 6, false), 9); err == nil {
		t.Fatal("an unknown group must fail")
	}
}

// While the clock guard is active a group set asks only the plain
// IP-literal entries of its own list (a preset's plain addresses); none is
// an error; the bootstrap servers are never asked.
func TestGroupSetClockGuard(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{"https://dns.example/dns-query"}
		d.Bootstrap = []string{"192.0.2.250"}
	})
	boot := &fakeTransport{fn: replyA("192.0.2.251", 300)}
	doh := &fakeTransport{fn: replyA("198.51.100.1", 300)}
	plain := &fakeTransport{fn: replyA("198.51.100.3", 300)}
	own := &fakeTransport{fn: replyA("198.51.100.4", 300)}
	opts := testOptions()
	opts.buildDate = time.Now().Add(24 * time.Hour)
	r := newTestResolver(t, st, opts, map[string]*fakeTransport{
		"192.0.2.250": boot, cloudflareFamily: doh, "1.1.1.3": plain, "1.0.0.3": plain,
		"2606:4700:4700::1113": plain, "2606:4700:4700::1003": plain, "192.0.2.8": own,
	})
	stop := start(r)
	defer stop()
	r.SetGroupUpstreams([]GroupUpstreams{
		{ID: 2, Name: "Family", Preset: "cloudflare-family"},
		{ID: 3, Name: "DoH only", Upstreams: []string{"https://filter.example/dns-query"}},
		{ID: 4, Name: "Mixed", Upstreams: []string{"https://filter.example/dns-query", "192.0.2.8"}},
	})
	ctx := context.Background()
	m, info, err := r.ResolveGroup(ctx, query("a.example.", dns.TypeA, 1, false), 2)
	if err != nil || doh.calls() != 0 || plain.calls() != 1 {
		t.Fatalf("preset under the clock guard: %+v %v (doh %d plain %d)", info, err, doh.calls(), plain.calls())
	}
	if ip, _ := firstA(t, m); ip != "198.51.100.3" {
		t.Fatalf("answer %s", ip)
	}
	if _, _, err := r.ResolveGroup(ctx, query("a.example.", dns.TypeA, 2, false), 3); err == nil ||
		!strings.Contains(err.Error(), "no plain upstream given by IP address") {
		t.Fatalf("no plain entry: %v", err)
	}
	if _, _, err := r.ResolveGroup(ctx, query("a.example.", dns.TypeA, 3, false), 4); err != nil || own.calls() != 1 {
		t.Fatalf("own plain entry: %v", err)
	}
	if boot.calls() != 0 {
		t.Fatal("the bootstrap servers answered a group query")
	}
	for _, s := range r.GroupStats() {
		if !s.ClockGuard {
			t.Fatalf("stats %+v", s)
		}
		if slices.Equal(s.GroupIDs, []int64{3}) && s.Error == "" {
			t.Fatalf("a set that cannot answer under the clock guard reports no error: %+v", s)
		}
	}
}

// A set that cannot be built (a name without bootstrap servers, an unknown
// preset) fails closed; a bootstrap change rebuilds the group sets.
func TestGroupSetBuildFailure(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		d.Upstreams = []string{"192.0.2.53"}
		d.Bootstrap = nil
	})
	named := &fakeTransport{fn: replyA("198.51.100.5", 300)}
	r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{"tls://dns.example": named})
	stop := start(r)
	defer stop()
	r.SetGroupUpstreams([]GroupUpstreams{
		{ID: 2, Name: "Named", Upstreams: []string{"tls://dns.example"}},
		{ID: 3, Name: "Gone", Preset: "no-such-preset"},
	})
	ctx := context.Background()
	for _, g := range []int64{2, 3} {
		if _, _, err := r.ResolveGroup(ctx, query("a.example.", dns.TypeA, 1, false), g); err == nil {
			t.Fatalf("group %d: a set that cannot be built answered", g)
		}
	}
	stats := r.GroupStats()
	if len(stats) != 2 || !strings.Contains(stats[0].Error, "needs dns.bootstrap servers") || !strings.Contains(stats[1].Error, "unknown preset") {
		t.Fatalf("stats %+v", stats)
	}
	updateDNS(t, st, func(d *settings.DNS) { d.Bootstrap = []string{"192.0.2.250"} })
	if _, _, err := r.ResolveGroup(ctx, query("a.example.", dns.TypeA, 2, false), 2); err != nil || named.calls() != 1 {
		t.Fatalf("after the bootstrap change: %v", err)
	}
	r.SetGroupUpstreams(nil)
	if _, ok := r.GroupFor([]int64{2}); ok || len(r.GroupStats()) != 0 {
		t.Fatal("the group sets were not removed")
	}
}
