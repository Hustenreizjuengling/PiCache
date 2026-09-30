package upstream

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec"
	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// validateMode puts the store into DNSSEC mode validate with the given
// upstreams (strict order).
func validateMode(ups ...string) func(d *settings.DNS) {
	return func(d *settings.DNS) {
		d.Upstreams = ups
		d.UpstreamMode = "strict"
		d.DNSSECMode = settings.DNSSECValidate
	}
}

// signedOptions are test options anchored at the universe's root.
func signedOptions(u *dnssectest.Universe) options {
	o := testOptions()
	o.anchors = u.Anchors()
	return o
}

// serveU answers from the universe.
func serveU(u *dnssectest.Universe) *fakeTransport { return &fakeTransport{fn: u.Reply} }

// stripping answers from the universe without RRSIG, NSEC and NSEC3
// records.
func stripping(u *dnssectest.Universe) *fakeTransport {
	return &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		m, err := u.Reply(ctx, q)
		if err != nil {
			return nil, err
		}
		m.Answer, m.Ns = withoutDNSSEC(m.Answer), withoutDNSSEC(m.Ns)
		return m, nil
	}}
}

// ignoringDO answers from the universe as if DO were never set.
func ignoringDO(u *dnssectest.Universe) *fakeTransport {
	return &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		c := q.Copy()
		c.IsEdns0().SetDo(false)
		m, err := u.Reply(ctx, c)
		if m != nil {
			m.Id = q.Id
		}
		return m, err
	}}
}

func withoutDNSSEC(rrs []dns.RR) []dns.RR {
	var out []dns.RR
	for _, rr := range rrs {
		switch rr.Header().Rrtype {
		case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3:
		default:
			out = append(out, rr)
		}
	}
	return out
}

func resolveA(t *testing.T, r *Resolver, name string, qtype uint16) (*dns.Msg, Info) {
	t.Helper()
	m, info, err := r.Resolve(context.Background(), query(name, qtype, 1, true), noECS)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return m, info
}

func wantVerdict(t *testing.T, info Info, status string) {
	t.Helper()
	if info.DNSSEC == nil || info.DNSSEC.Status != status {
		t.Fatalf("verdict %+v, want %s", info.DNSSEC, status)
	}
}

// waitProbe waits until the upstream's probe state is want.
func waitProbe(t *testing.T, r *Resolver, upstreamName, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, s := range append(r.Stats(), r.FallbackStats()...) {
			if s.Upstream == upstreamName && s.DNSSEC == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe state of %s never became %s: %+v", upstreamName, want, r.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestValidateVerdictsAndAD(t *testing.T) {
	u, _, ex := dnssectest.ExampleAt(t, time.Now())
	ex.A("bad.example.com", "192.0.2.66")
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		if q == "bad.example.com." {
			for _, rr := range m.Answer {
				if a, ok := rr.(*dns.A); ok {
					a.A = net.IPv4(203, 0, 113, 66).To4()
				}
			}
		}
	}
	st := newStore(t, validateMode(up1))
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
	defer r.Close()

	m, info := resolveA(t, r, "www.example.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Secure)
	if !m.AuthenticatedData || info.DNSSEC.Zone != "example.com" {
		t.Fatalf("secure answer: AD %v zone %q", m.AuthenticatedData, info.DNSSEC.Zone)
	}
	m, info = resolveA(t, r, "www.unsigned.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Insecure)
	if m.AuthenticatedData || info.DNSSEC.Reason != dnssec.ReasonNoDS {
		t.Fatalf("insecure answer: AD %v %+v", m.AuthenticatedData, info.DNSSEC)
	}
	m, info = resolveA(t, r, "bad.example.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Bogus)
	if m.AuthenticatedData || info.DNSSEC.EDE == nil || info.DNSSEC.EDE.Code != 6 ||
		info.DNSSEC.EDE.Text != "example.com: bad signature" {
		t.Fatalf("bogus answer: AD %v %+v %+v", m.AuthenticatedData, info.DNSSEC, info.DNSSEC.EDE)
	}
	// Cache hits keep the verdict and perform no signature verification.
	before := r.val.v.Verifications()
	m, info = resolveA(t, r, "www.example.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Secure)
	if !info.Cached || !m.AuthenticatedData || r.val.v.Verifications() != before {
		t.Fatalf("cache hit: cached %v AD %v verifications %d → %d", info.Cached, m.AuthenticatedData, before, r.val.v.Verifications())
	}
	// RRSIG, NSEC and NSEC3 queries are not validated.
	if _, info := resolveA(t, r, "www.example.com.", dns.TypeRRSIG); info.DNSSEC != nil {
		t.Fatalf("RRSIG query validated: %+v", info.DNSSEC)
	}
}

func TestUpstreamADOnRoutesThatAreNotValidated(t *testing.T) {
	adReply := func(_ context.Context, q *dns.Msg) (*dns.Msg, error) {
		m := answerA(q, "192.0.2.7", 60)
		m.AuthenticatedData = true
		return m, nil
	}
	for _, tc := range []struct {
		mode string
		ad   bool
	}{{settings.DNSSECValidate, false}, {settings.DNSSECPassthrough, true}, {settings.DNSSECOff, true}} {
		t.Run(tc.mode, func(t *testing.T) {
			st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{up1}; d.DNSSECMode = tc.mode })
			r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: {fn: adReply}, up2: {fn: adReply}})
			defer r.Close()
			m, info, err := r.ResolveVia(context.Background(), query("router.lan.", dns.TypeA, 1, true), []string{up2})
			if err != nil {
				t.Fatal(err)
			}
			if m.AuthenticatedData != tc.ad || info.DNSSEC != nil {
				t.Fatalf("AD %v, verdict %+v", m.AuthenticatedData, info.DNSSEC)
			}
		})
	}
}

func TestDegradedUpstreams(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fake  func(u *dnssectest.Universe) *fakeTransport
		state string
	}{
		{"strips RRSIGs", stripping, dnssec.StateNoDNSSEC},
		{"ignores DO", ignoringDO, dnssec.StateNoDNSSEC},
		{"other root", func(*dnssectest.Universe) *fakeTransport {
			return serveU(dnssectest.NewForeignUniverseAt(t, time.Now()))
		},
			dnssec.StateAnchorMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _, _ := dnssectest.ExampleAt(t, time.Now())
			st := newStore(t, validateMode(up1))
			r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: tc.fake(u)})
			stop := start(r)
			defer stop()
			waitProbe(t, r, up1, tc.state)
			m, info := resolveA(t, r, "www.example.com.", dns.TypeA)
			wantVerdict(t, info, dnssec.Indeterminate)
			if m.AuthenticatedData || m.Rcode == dns.RcodeServerFailure {
				t.Fatalf("indeterminate answer: AD %v rcode %d", m.AuthenticatedData, m.Rcode)
			}
			if tc.state == dnssec.StateNoDNSSEC && info.DNSSEC.Reason != up1+" returns no DNSSEC data" {
				t.Fatalf("reason %q", info.DNSSEC.Reason)
			}
			h := r.DNSSECHealth()
			if (tc.state == dnssec.StateAnchorMismatch) != h.AnchorMismatch ||
				(tc.state == dnssec.StateNoDNSSEC) != (len(h.NoDNSSEC) == 1) {
				t.Fatalf("health %+v", h)
			}
		})
	}
}

func TestMixedSetBehavesPerReply(t *testing.T) {
	u, _, _ := dnssectest.ExampleAt(t, time.Now())
	st := newStore(t, validateMode(up1, up2))
	good, bad := serveU(u), stripping(u)
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: good, up2: bad})
	stop := start(r)
	defer stop()
	waitProbe(t, r, up1, dnssec.StateCapable)
	waitProbe(t, r, up2, dnssec.StateNoDNSSEC)
	updateDNS(t, st, func(d *settings.DNS) { d.CacheEnabled = false })
	// strict: up1 answers (secure); with up1 failing, up2 answers
	// (indeterminate).
	_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Secure)
	good.fn = rcodeReply(dns.RcodeServerFailure)
	_, info = resolveA(t, r, "www.example.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Indeterminate)
}

func TestRootMismatchDuringQuery(t *testing.T) {
	// A capable upstream answers the root DNSKEY lookup of a query with a
	// key set that matches no anchor: bogus (EDE 9) and a re-probe.
	u, _, _ := dnssectest.ExampleAt(t, time.Now())
	other := dnssectest.NewForeignUniverseAt(t, time.Now())
	var swap atomic.Bool
	f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		if swap.Load() && q.Question[0].Name == "." && q.Question[0].Qtype == dns.TypeDNSKEY {
			return other.Reply(ctx, q)
		}
		return u.Reply(ctx, q)
	}}
	st := newStore(t, validateMode(up1))
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: f})
	stop := start(r)
	defer stop()
	waitProbe(t, r, up1, dnssec.StateCapable)
	swap.Store(true)
	r.val.v.Flush()
	_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Bogus)
	if info.DNSSEC.EDE == nil || info.DNSSEC.EDE.Code != 9 {
		t.Fatalf("EDE %+v", info.DNSSEC.EDE)
	}
	waitProbe(t, r, up1, dnssec.StateAnchorMismatch) // the re-probe
}

func TestTimeChecks(t *testing.T) {
	t.Run("clock guard", func(t *testing.T) {
		u, _, _ := dnssectest.ExampleAt(t, time.Now())
		st := newStore(t, validateMode(up1))
		o := signedOptions(u)
		o.buildDate = time.Now().Add(24 * time.Hour)
		r := newTestResolver(t, st, o, map[string]*fakeTransport{up1: serveU(u)})
		defer r.Close()
		_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
		wantVerdict(t, info, dnssec.Indeterminate)
		if active, reason := r.TimeChecks(); active || reason != TimeReasonClockGuard {
			t.Fatalf("time checks %v %q", active, reason)
		}
	})
	for _, tc := range []struct {
		name             string
		synced, readable bool
		active           bool
	}{{"unsynchronised", false, true, false}, {"unreadable", false, false, true}, {"synchronised", true, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			u, _, _ := dnssectest.ExampleAt(t, time.Now())
			st := newStore(t, validateMode(up1))
			r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
			defer r.Close()
			r.SetClockReader(func() (bool, bool) { return tc.synced, tc.readable })
			if active, reason := r.TimeChecks(); active != tc.active || (!active && reason != TimeReasonUnsynced) {
				t.Fatalf("time checks %v %q", active, reason)
			}
			_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
			want := dnssec.Secure
			if !tc.active {
				want = dnssec.Indeterminate
			}
			wantVerdict(t, info, want)
		})
	}
	t.Run("root signatures outside their period and resuming", func(t *testing.T) {
		st := newStore(t, validateMode(up1))
		synctest.Test(t, func(t *testing.T) {
			// The root signs with dates a year back; the other zones are
			// valid now.
			u, _, _ := dnssectest.ExampleAt(t, time.Now())
			u.Root.SigInc, u.Root.SigExp = time.Now().Add(-400*24*time.Hour), time.Now().Add(-370*24*time.Hour)
			u.Root.ClearSigs()
			f := serveU(u)
			r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: f})
			stop := start(r)
			defer stop()
			time.Sleep(time.Second)
			synctest.Wait()
			if active, reason := r.TimeChecks(); active || reason != TimeReasonRootSignatures {
				t.Fatalf("time checks %v %q", active, reason)
			}
			_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
			wantVerdict(t, info, dnssec.Indeterminate)
			// The root is signed correctly again: the next probe (every 30
			// minutes) clears the reason, and 60 s later the checks resume
			// and the caches are emptied.
			u.Root.SigInc, u.Root.SigExp = time.Now().Add(-time.Hour), time.Now().Add(30*24*time.Hour)
			u.Root.ClearSigs()
			time.Sleep(30*time.Minute + time.Second)
			synctest.Wait()
			if active, _ := r.TimeChecks(); active {
				t.Fatal("time checks resumed at once")
			}
			time.Sleep(61 * time.Second)
			synctest.Wait()
			if active, _ := r.TimeChecks(); !active {
				t.Fatal("time checks did not resume")
			}
			if n := r.cache.len(); n != 0 {
				t.Fatalf("%d cache entries after resuming", n)
			}
			_, info = resolveA(t, r, "www.example.com.", dns.TypeA)
			wantVerdict(t, info, dnssec.Secure)
		})
	})
}

func TestBogusCaching(t *testing.T) {
	st := newStore(t, validateMode(up1))
	synctest.Test(t, func(t *testing.T) {
		u, _, ex := dnssectest.ExampleAt(t, time.Now())
		ex.A("slow.example.com", "192.0.2.5")
		sub := u.AddChild(ex, "sub.example.com", true)
		sub.A("www.sub.example.com", "192.0.2.6")
		var dnskeyTimeout atomic.Bool
		f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
			if dnskeyTimeout.Load() && q.Question[0].Name == "sub.example.com." && q.Question[0].Qtype == dns.TypeDNSKEY {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return u.Reply(ctx, q)
		}}
		r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: f})
		defer r.Close()
		// A DNSKEY lookup that times out inside a signed zone: bogus (EDE
		// 9), cached 5 s, never served stale.
		dnskeyTimeout.Store(true)
		_, info := resolveA(t, r, "www.sub.example.com.", dns.TypeA)
		wantVerdict(t, info, dnssec.Bogus)
		if info.DNSSEC.EDE == nil || info.DNSSEC.EDE.Code != 9 || !strings.HasPrefix(info.DNSSEC.Reason, "DNSKEY lookup failed") {
			t.Fatalf("verdict %+v %+v", info.DNSSEC, info.DNSSEC.EDE)
		}
		dnskeyTimeout.Store(false)
		time.Sleep(4 * time.Second)
		_, info = resolveA(t, r, "www.sub.example.com.", dns.TypeA)
		if !info.Cached || info.DNSSEC.Status != dnssec.Bogus {
			t.Fatalf("within 5 s: cached %v %+v", info.Cached, info.DNSSEC)
		}
		time.Sleep(2 * time.Second)
		_, info = resolveA(t, r, "www.sub.example.com.", dns.TypeA)
		if info.Cached || info.Stale || info.DNSSEC.Status != dnssec.Secure {
			t.Fatalf("after 5 s: cached %v stale %v %+v", info.Cached, info.Stale, info.DNSSEC)
		}
		// A cryptographic bogus: cached min(TTL, 30 s).
		u.Tamper = func(q string, qt uint16, m *dns.Msg) {
			if q == "slow.example.com." && qt == dns.TypeA {
				for _, rr := range m.Answer {
					if a, ok := rr.(*dns.A); ok {
						a.A = net.IPv4(203, 0, 113, 66).To4()
					}
				}
			}
		}
		_, info = resolveA(t, r, "slow.example.com.", dns.TypeA)
		wantVerdict(t, info, dnssec.Bogus)
		u.Tamper = nil
		time.Sleep(29 * time.Second)
		if _, info = resolveA(t, r, "slow.example.com.", dns.TypeA); !info.Cached || info.DNSSEC.Status != dnssec.Bogus {
			t.Fatalf("within 30 s: %+v", info)
		}
		time.Sleep(2 * time.Second)
		if _, info = resolveA(t, r, "slow.example.com.", dns.TypeA); info.Cached || info.DNSSEC.Status != dnssec.Secure {
			t.Fatalf("after 30 s: %+v", info)
		}
	})
}

func TestSecureLifetimeCappedByRRSIG(t *testing.T) {
	st := newStore(t, validateMode(up1))
	updateDNS(t, st, func(d *settings.DNS) { d.CacheMinTTL = 3600 })
	synctest.Test(t, func(t *testing.T) {
		u, _, ex := dnssectest.ExampleAt(t, time.Now())
		ex.SigExp = time.Now().Add(10 * time.Minute)
		ex.ClearSigs()
		r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
		defer r.Close()
		m, info := resolveA(t, r, "www.example.com.", dns.TypeA)
		wantVerdict(t, info, dnssec.Secure)
		for _, rr := range m.Answer {
			if rr.Header().Ttl > 600 {
				t.Fatalf("TTL %d above the RRSIG expiration", rr.Header().Ttl)
			}
		}
		time.Sleep(11 * time.Minute)
		if _, info = resolveA(t, r, "www.example.com.", dns.TypeA); info.Cached && !info.Stale {
			t.Fatal("served fresh after the RRSIG expired")
		}
	})
}

func TestStaleHitIsIndeterminate(t *testing.T) {
	st := newStore(t, validateMode(up1))
	synctest.Test(t, func(t *testing.T) {
		u, _, _ := dnssectest.ExampleAt(t, time.Now())
		f := serveU(u)
		r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: f})
		defer r.Close()
		resolveA(t, r, "www.example.com.", dns.TypeA)
		time.Sleep(301 * time.Second) // the A TTL is 300
		f.fn = func(context.Context, *dns.Msg) (*dns.Msg, error) { return nil, errTimeout }
		m, info := resolveA(t, r, "www.example.com.", dns.TypeA)
		if !info.Stale || m.AuthenticatedData {
			t.Fatalf("stale %v AD %v", info.Stale, m.AuthenticatedData)
		}
		wantVerdict(t, info, dnssec.Indeterminate)
		if info.DNSSEC.Reason != "stale answer" {
			t.Fatalf("reason %q", info.DNSSEC.Reason)
		}
	})
}

func TestModeSwitchEmptiesCachesAndKey(t *testing.T) {
	u, _, _ := dnssectest.ExampleAt(t, time.Now())
	st := newStore(t, validateMode(up1))
	release := make(chan struct{})
	var slow atomic.Bool
	f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		if slow.Load() && q.Question[0].Name == "www.example.com." {
			<-release
		}
		return u.Reply(ctx, q)
	}}
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: f})
	defer r.Close()
	resolveA(t, r, "example.com.", dns.TypeA)
	if zones, _, _ := r.val.v.Stats(); zones == 0 || r.cache.len() == 0 {
		t.Fatal("nothing cached")
	}
	// A fetch that started in validate mode and finishes after the switch
	// is never served in passthrough (the cache key holds val).
	slow.Store(true)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = r.Resolve(context.Background(), query("www.example.com.", dns.TypeA, 1, true), noECS)
	}()
	time.Sleep(50 * time.Millisecond)
	updateDNS(t, st, func(d *settings.DNS) { d.DNSSECMode = settings.DNSSECPassthrough })
	if zones, _, _ := r.val.v.Stats(); zones != 0 || r.cache.len() != 0 {
		t.Fatalf("after the switch: %d zones, %d entries", zones, r.cache.len())
	}
	close(release)
	<-done
	slow.Store(false)
	_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
	if info.Cached || info.DNSSEC != nil {
		t.Fatalf("passthrough answer: cached %v verdict %+v", info.Cached, info.DNSSEC)
	}
}

func TestLookupIPVerdicts(t *testing.T) {
	u, _, ex := dnssectest.ExampleAt(t, time.Now())
	ex.A("bad.example.com", "192.0.2.66")
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		if q == "bad.example.com." {
			for _, rr := range m.Answer {
				if a, ok := rr.(*dns.A); ok {
					a.A = net.IPv4(203, 0, 113, 66).To4()
				}
			}
		}
	}
	st := newStore(t, validateMode(up1))
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
	defer r.Close()
	for _, name := range []string{"www.example.com", "www.unsigned.com"} {
		if addrs, err := r.LookupIP(context.Background(), name, false); err != nil || len(addrs) != 1 {
			t.Fatalf("%s: %v %v", name, addrs, err)
		}
	}
	_, err := r.LookupIP(context.Background(), "bad.example.com", false)
	var de *net.DNSError
	if !errors.As(err, &de) || de.Err != "DNSSEC validation failed: example.com: bad signature" {
		t.Fatalf("bogus: %v", err)
	}
	if _, ok := r.ips.get("bad.example.com", false, time.Now()); ok {
		t.Fatal("bogus result cached")
	}
}

func TestValidationWithoutResponseCache(t *testing.T) {
	u, _, _ := dnssectest.ExampleAt(t, time.Now())
	st := newStore(t, validateMode(up1))
	updateDNS(t, st, func(d *settings.DNS) { d.CacheEnabled = false })
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
	defer r.Close()
	for range 2 {
		_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
		wantVerdict(t, info, dnssec.Secure)
	}
	// The key states come from the key cache: the second answer needed no
	// chain lookup.
	if n := u.Count("example.com.", dns.TypeDNSKEY); n != 1 {
		t.Fatalf("%d DNSKEY lookups", n)
	}
}

func TestUpstreamBlocksAreNotValidated(t *testing.T) {
	u, _, ex := dnssectest.ExampleAt(t, time.Now())
	ex.A("zero.example.com", "0.0.0.0")
	ex.A("chain.example.com", "192.0.2.9")
	const quad9 = "9.9.9.9"
	f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		qq := q.Question[0]
		switch {
		case qq.Name == "gone.example.com.": // Quad9's block: NXDOMAIN without RA, unsigned
			m := new(dns.Msg)
			m.SetRcode(q, dns.RcodeNameError)
			m.RecursionAvailable = false
			return m, nil
		case qq.Name == "sub.chain.example.com." && qq.Qtype == dns.TypeDS: // a chain reply with EDE 15
			m := new(dns.Msg)
			m.SetRcode(q, dns.RcodeNameError)
			m.RecursionAvailable = true
			m.SetEdns0(1232, true)
			m.IsEdns0().Option = append(m.IsEdns0().Option, &dns.EDNS0_EDE{InfoCode: dns.ExtendedErrorCodeBlocked, ExtraText: "blocked"})
			return m, nil
		}
		return u.Reply(ctx, q)
	}}
	st := newStore(t, validateMode(quad9))
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{quad9: f})
	defer r.Close()
	for _, name := range []string{"gone.example.com.", "zero.example.com."} {
		_, info := resolveA(t, r, name, dns.TypeA)
		if info.Block == nil || info.DNSSEC != nil {
			t.Fatalf("%s: block %+v verdict %+v", name, info.Block, info.DNSSEC)
		}
	}
	// An unsigned answer below example.com needs "sub.chain.example.com
	// DS": its blocked reply makes the query blocked-upstream.
	f2 := f.fn
	f.fn = func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		if q.Question[0].Name == "x.sub.chain.example.com." {
			m := answerA(q, "192.0.2.10", 60)
			return m, nil
		}
		return f2(ctx, q)
	}
	_, info := resolveA(t, r, "x.sub.chain.example.com.", dns.TypeA)
	if info.Block == nil || info.Block.Kind != BlockEDE || info.DNSSEC != nil {
		t.Fatalf("chain block: %+v %+v", info.Block, info.DNSSEC)
	}
}

func TestChainRoutes(t *testing.T) {
	// The default upstream serves a universe without the zones below; the
	// group set and the forwarder serve theirs. Chain lookups stay on the
	// route that answered.
	u, com, _ := dnssectest.ExampleAt(t, time.Now())
	priv := u.AddChild(com, "corp.com", false) // insecure in the forwarder's view
	priv.A("www.corp.com", "192.0.2.40")
	def := serveU(u)
	grp := serveU(u)
	fwd := serveU(u)
	const g1, f1 = "192.0.2.21", "192.0.2.31"
	st := newStore(t, validateMode(up1))
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: def, g1: grp, f1: fwd})
	defer r.Close()
	r.SetGroupUpstreams([]GroupUpstreams{{ID: 5, Name: "Kids", Upstreams: []string{g1}}})
	r.SetValidatingForwarders([][]string{{f1}})

	m, info, err := r.ResolveGroup(context.Background(), query("www.example.com.", dns.TypeA, 1, true), 5)
	if err != nil || !m.AuthenticatedData {
		t.Fatalf("group: %v AD %v", err, m != nil && m.AuthenticatedData)
	}
	wantVerdict(t, info, dnssec.Secure)
	if def.calls() != 0 {
		t.Fatalf("the default set saw %d queries of the group's validation", def.calls())
	}
	m, info, err = r.ResolveValidating(context.Background(), query("www.corp.com.", dns.TypeA, 2, true), []string{f1})
	if err != nil {
		t.Fatal(err)
	}
	wantVerdict(t, info, dnssec.Insecure)
	if def.calls() != 0 {
		t.Fatalf("the default set saw %d queries of the forwarder's validation", def.calls())
	}
	// The insecure state learned through the forwarder does not change
	// the default route: its key cache has nothing for corp.com.
	if st := r.val.v; st == nil {
		t.Fatal("no validator")
	}
	before := u.Count("corp.com.", dns.TypeDS)
	_, info = resolveA(t, r, "www.corp.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Insecure)
	if u.Count("corp.com.", dns.TypeDS) != before+1 {
		t.Fatal("the default route reused the forwarder's state")
	}
	if stats := r.ForwarderStats([]string{f1}); len(stats) != 1 || stats[0].DNSSEC != dnssec.StateUnknown {
		t.Fatalf("forwarder stats %+v", stats)
	}
}

// With the default upstream dead, the fallbacks answer the fetch; its chain
// lookups go to them directly instead of waiting for the dead upstream
// again, which would use up the validation's 4 s (found by the end-to-end
// test: every name of a zone not in the key cache was bogus).
func TestChainLookupsOfFallbackAnswers(t *testing.T) {
	st := newStore(t, func(d *settings.DNS) {
		validateMode(up1)(d)
		d.FallbackUpstreams = []string{fb1}
		d.CacheEnabled = false
	})
	synctest.Test(t, func(t *testing.T) {
		u, _, _ := dnssectest.ExampleAt(t, time.Now())
		dead := &fakeTransport{fn: hanging}
		fb := serveU(u)
		opts := signedOptions(u)
		opts.attempt = defaultAttemptTimeout
		r := newTestResolver(t, st, opts, map[string]*fakeTransport{up1: dead, fb1: fb})
		defer r.Close()
		m, info := resolveA(t, r, "www.example.com.", dns.TypeA)
		wantVerdict(t, info, dnssec.Secure)
		if !info.Fallback || !m.AuthenticatedData {
			t.Fatalf("fallback %v AD %v", info.Fallback, m.AuthenticatedData)
		}
		if n := dead.calls(); n != 1 {
			t.Fatalf("the dead upstream was asked %d times, want once (the fetch only)", n)
		}
		if u.Count("example.com.", dns.TypeDNSKEY) == 0 {
			t.Fatal("no chain lookup reached the fallback")
		}
	})
}

func TestConcurrentKeyQueriesDrainFlights(t *testing.T) {
	u, _, _ := dnssectest.ExampleAt(t, time.Now())
	u.Delay = 20 * time.Millisecond
	st := newStore(t, validateMode(up1))
	updateDNS(t, st, func(d *settings.DNS) { d.CacheEnabled = false })
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
	defer r.Close()
	var wg sync.WaitGroup
	for i := range 20 {
		qt := dns.TypeDNSKEY
		if i%2 == 1 {
			qt = dns.TypeDS
		}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, info, err := r.Resolve(ctx, query("example.com.", qt, uint16(i), true), noECS)
			if err != nil || info.DNSSEC == nil || info.DNSSEC.Status != dnssec.Secure {
				t.Errorf("%s: %v %+v", dns.TypeToString[qt], err, info.DNSSEC)
			}
		})
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.flight.mu.Lock()
		client := len(r.flight.m)
		r.flight.mu.Unlock()
		if client == 0 && r.val.v.Flights() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("flights left: %d client, %d chain", client, r.val.v.Flights())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChainRateLimit(t *testing.T) {
	u, _, _ := dnssectest.ExampleAt(t, time.Now())
	st := newStore(t, validateMode(up1))
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
	defer r.Close()
	r.val.limiter.SetBurst(0)
	r.val.limiter.SetLimit(0)
	_, info := resolveA(t, r, "www.example.com.", dns.TypeA)
	wantVerdict(t, info, dnssec.Bogus)
	if !strings.Contains(info.DNSSEC.Reason, "lookup failed") {
		t.Fatalf("reason %q", info.DNSSEC.Reason)
	}
}

func TestProbeSchedule(t *testing.T) {
	st := newStore(t, validateMode(up1))
	synctest.Test(t, func(t *testing.T) {
		u, _, _ := dnssectest.ExampleAt(t, time.Now())
		var fail atomic.Bool
		fail.Store(true)
		f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
			if fail.Load() {
				return nil, errTimeout
			}
			return u.Reply(ctx, q)
		}}
		r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: f})
		stop := start(r)
		defer stop()
		probes := func() int { return u.Count(".", dns.TypeDNSKEY) }
		synctest.Wait()
		// At the start: probed (the transport fails, the state stays
		// unknown).
		if n := f.calls(); n != 1 {
			t.Fatalf("%d probe queries at the start", n)
		}
		// Every 30 s while unknown.
		time.Sleep(31 * time.Second)
		synctest.Wait()
		if n := f.calls(); n != 2 {
			t.Fatalf("%d probe queries after 30 s", n)
		}
		fail.Store(false)
		time.Sleep(31 * time.Second)
		synctest.Wait()
		if s := r.Stats()[0]; s.DNSSEC != dnssec.StateCapable || s.DNSSECCheckedAt.IsZero() {
			t.Fatalf("state %+v", s)
		}
		// Capable: the next probe after 30 minutes, not before.
		n := probes()
		time.Sleep(20 * time.Minute)
		synctest.Wait()
		if probes() != n {
			t.Fatal("probed before 30 minutes")
		}
		time.Sleep(11 * time.Minute)
		synctest.Wait()
		if probes() != n+1 {
			t.Fatalf("probes %d → %d after 30 minutes", n, probes())
		}
		// A rebuild probes again.
		updateDNS(t, st, func(d *settings.DNS) { d.UpstreamTimeoutMs = 4000; d.Bootstrap = []string{"9.9.9.9"} })
		synctest.Wait()
		if probes() != n+2 {
			t.Fatalf("no probe after the rebuild: %d", probes())
		}
		// Probes are not counted in the upstream statistics.
		if s := r.Stats()[0]; s.Queries != 0 {
			t.Fatalf("probes counted: %d", s.Queries)
		}
	})
}

func TestDNSSECTest(t *testing.T) {
	// The test names served by a universe: example.com secure, google.com
	// insecure, dnssec-failed.org without a matching DNSKEY,
	// sigfail.ippacket.stream a bad signature behind a CNAME.
	u := dnssectest.NewUniverseAt(t, time.Now())
	com := u.AddChild(u.Root, "com", true)
	ex := u.AddChild(com, "example.com", true)
	ex.A("example.com", "192.0.2.1")
	g := u.AddChild(com, "google.com", false)
	g.A("google.com", "192.0.2.2")
	org := u.AddChild(u.Root, "org", true)
	failed := u.AddChild(org, "dnssec-failed.org", true)
	failed.A("dnssec-failed.org", "192.0.2.3")
	failed.Data[failed.Name][dns.TypeDNSKEY] = []dns.RR{failed.ZSK.Key}
	stream := u.AddChild(u.Root, "stream", true)
	ip := u.AddChild(stream, "ippacket.stream", true)
	ip.Add(dnssectest.MustRR("sigfail.ippacket.stream 300 IN CNAME sigfail.rsa2048-sha256.ippacket.stream"))
	ip.A("sigfail.rsa2048-sha256.ippacket.stream", "192.0.2.4")
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok && strings.HasPrefix(a.Hdr.Name, "sigfail.rsa2048") {
				a.A = net.IPv4(203, 0, 113, 1).To4()
			}
		}
		// A validating upstream refuses the bogus names with CD=0.
	}
	refusing := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		n := q.Question[0].Name
		if !q.CheckingDisabled && q.Question[0].Qtype == dns.TypeA && (n == "dnssec-failed.org." || n == "sigfail.ippacket.stream.") {
			m := new(dns.Msg)
			m.SetRcode(q, dns.RcodeServerFailure)
			return m, nil
		}
		return u.Reply(ctx, q)
	}}
	st := newStore(t, validateMode(up1))
	updateDNS(t, st, func(d *settings.DNS) { d.DNSSECMode = settings.DNSSECPassthrough })
	r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: refusing})
	defer r.Close()
	res, err := r.TestDNSSEC(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != settings.DNSSECPassthrough || res.TimeChecks != "active" || len(res.Upstreams) != 1 ||
		res.Upstreams[0].DNSSEC != dnssec.StateCapable || len(res.Checks) != 4 {
		t.Fatalf("result %+v", res)
	}
	for _, c := range res.Checks {
		if c.Verdict != TestPass {
			t.Errorf("%s: %+v", c.Name, c)
		}
		if (c.Expect == dnssec.Bogus) != c.UpstreamRefused {
			t.Errorf("%s: upstreamRefused %v", c.Name, c.UpstreamRefused)
		}
	}
	if c := res.Checks[2]; c.EDE == nil || c.EDE.Code != 9 {
		t.Errorf("dnssec-failed.org: %+v", c.EDE)
	}
	if c := res.Checks[3]; c.EDE == nil || c.EDE.Code != 6 {
		t.Errorf("sigfail: %+v", c.EDE)
	}
	// The response cache was neither read nor written.
	if r.cache.len() != 0 {
		t.Fatalf("%d cache entries", r.cache.len())
	}
	if _, err := r.TestDNSSEC(context.Background()); !errors.Is(err, ErrDNSSECTestTooSoon) {
		t.Fatalf("second test: %v", err)
	}
	r.val.testing.Store(true)
	if _, err := r.TestDNSSEC(context.Background()); !errors.Is(err, ErrDNSSECTestRunning) {
		t.Fatalf("concurrent test: %v", err)
	}
}

// replaying answers the root DNSKEY and SOA queries with the root's records
// signed a year ago (genuine, but expired now: a replay by someone on the
// path of a plain upstream) and everything else from u.
func replaying(u *dnssectest.Universe) *fakeTransport {
	root := u.Root
	inc, exp := root.SigInc, root.SigExp
	root.SigInc, root.SigExp = time.Now().Add(-400*24*time.Hour), time.Now().Add(-370*24*time.Hour)
	root.ClearSigs()
	old := map[uint16]*dns.Msg{dns.TypeDNSKEY: u.Resolve(".", dns.TypeDNSKEY), dns.TypeSOA: u.Resolve(".", dns.TypeSOA)}
	root.SigInc, root.SigExp = inc, exp
	root.ClearSigs()
	return &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
		qq := q.Question[0]
		if m := old[qq.Qtype]; m != nil && qq.Name == "." {
			rep := m.Copy()
			rep.Id, rep.Question = q.Id, []dns.Question{qq}
			rep.SetEdns0(1232, true)
			return rep, nil
		}
		return u.Reply(ctx, q)
	}}
}

func TestRootSignaturesNeedAgreement(t *testing.T) {
	// Old root signatures replayed to the probes of one path suspend the
	// time checks of no route. (Alone, with the clock state unknown, they
	// still do: TestTimeChecks.)
	const boot = "192.0.2.53"
	for _, tc := range []struct {
		name     string
		mutate   func(d *settings.DNS)
		clock    func() (bool, bool)
		unprobed bool // the replaying path is never probed
	}{
		// The clock-guard set (plain DNS to the bootstrap servers) is no
		// route while the guard is inactive: it is not probed at all.
		{name: "clock-guard set while the guard is inactive", mutate: func(d *settings.DNS) { d.Bootstrap = []string{boot} }, unprobed: true},
		// Another upstream shows that the clock agrees with the root zone.
		{name: "an upstream disagrees", mutate: func(d *settings.DNS) { d.Upstreams = []string{up1, up2} }},
		// A synchronised host clock is trusted over the probes.
		{name: "synchronised host clock", mutate: func(d *settings.DNS) { d.Upstreams = []string{up2} },
			clock: func() (bool, bool) { return true, true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newStore(t, validateMode(up1))
			updateDNS(t, st, tc.mutate)
			synctest.Test(t, func(t *testing.T) {
				u, _, _ := dnssectest.ExampleAt(t, time.Now())
				replay := replaying(u)
				// A bootstrap server is named with its port in the guard set.
				r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u), up2: replay, boot + ":53": replay})
				if tc.clock != nil {
					r.SetClockReader(tc.clock)
				}
				stop := start(r)
				defer stop()
				time.Sleep(time.Second)
				synctest.Wait()
				if active, reason := r.TimeChecks(); !active {
					t.Fatalf("time checks suspended: %q", reason)
				}
				if tc.unprobed && replay.calls() != 0 {
					t.Fatalf("the clock-guard set was probed %d times", replay.calls())
				}
			})
		})
	}
}

func TestRootSignaturesJudgedNow(t *testing.T) {
	st := newStore(t, validateMode(up1))
	synctest.Test(t, func(t *testing.T) {
		// A clock two hours behind the root zone at the start (no RTC, no
		// time synchronisation yet): the probe finds the root signatures
		// not yet valid and the time checks are suspended.
		u, _, _ := dnssectest.ExampleAt(t, time.Now())
		u.Root.SigInc, u.Root.SigExp = time.Now().Add(2*time.Hour), time.Now().Add(30*24*time.Hour)
		u.Root.ClearSigs()
		o := signedOptions(u)
		o.probeEvery = 24 * time.Hour
		r := newTestResolver(t, st, o, map[string]*fakeTransport{up1: serveU(u)})
		stop := start(r)
		defer stop()
		time.Sleep(time.Second)
		synctest.Wait()
		if active, reason := r.TimeChecks(); active || reason != TimeReasonRootSignatures {
			t.Fatalf("time checks %v %q", active, reason)
		}
		// Once the clock is inside the period (1 h tolerance) the finding
		// no longer holds, without another probe; 60 s later the checks
		// resume.
		probes := u.Count(".", dns.TypeDNSKEY)
		time.Sleep(time.Hour + 10*time.Second)
		synctest.Wait()
		if active, _ := r.TimeChecks(); active {
			t.Fatal("resumed at once")
		}
		time.Sleep(61 * time.Second)
		synctest.Wait()
		if active, reason := r.TimeChecks(); !active {
			t.Fatalf("still suspended: %q", reason)
		}
		if u.Count(".", dns.TypeDNSKEY) != probes {
			t.Fatal("probed again")
		}
	})
}

func TestRootSignaturesOfSilentUpstreamExpire(t *testing.T) {
	st := newStore(t, validateMode(up1))
	synctest.Test(t, func(t *testing.T) {
		// The only upstream reports root signatures outside their period,
		// then stops answering: its probes are retried every 30 s, and
		// after three without a reply its finding no longer counts.
		u, _, _ := dnssectest.ExampleAt(t, time.Now())
		replay := replaying(u)
		var silent atomic.Bool
		f := &fakeTransport{fn: func(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
			if silent.Load() {
				return nil, errTimeout
			}
			return replay.fn(ctx, q)
		}}
		r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: f})
		stop := start(r)
		defer stop()
		time.Sleep(time.Second)
		synctest.Wait()
		if active, reason := r.TimeChecks(); active || reason != TimeReasonRootSignatures {
			t.Fatalf("time checks %v %q", active, reason)
		}
		silent.Store(true)
		// The next probe (after 30 minutes) fails: one failure keeps the
		// finding (a clock that is really wrong must stay covered).
		time.Sleep(30*time.Minute + 10*time.Second)
		synctest.Wait()
		if active, _ := r.TimeChecks(); active {
			t.Fatal("resumed after one failed probe")
		}
		// Two retries 30 s apart fail too; 60 s later the checks resume.
		time.Sleep(2*30*time.Second + 61*time.Second)
		synctest.Wait()
		if active, reason := r.TimeChecks(); !active {
			t.Fatalf("still suspended: %q", reason)
		}
	})
}

func TestChainShares(t *testing.T) {
	st := newStore(t, validateMode(up1))
	synctest.Test(t, func(t *testing.T) {
		u, _, _ := dnssectest.ExampleAt(t, time.Now())
		r := newTestResolver(t, st, signedOptions(u), map[string]*fakeTransport{up1: serveU(u)})
		defer r.Close()
		// A share of 5 chain exchanges per client, and a global rate that
		// makes lookups wait for their turn.
		r.val.shares = netutil.NewRateLimiterMax(1, 5, 16)
		r.val.limiter.SetLimit(10)
		r.val.limiter.SetBurst(1)
		attacker := WithClient(context.Background(), netip.MustParseAddr("192.168.1.66"))
		victim := WithClient(context.Background(), netip.MustParseAddr("192.168.1.10"))
		resolve := func(ctx context.Context, name string) Info {
			t.Helper()
			_, info, err := r.Resolve(ctx, query(name, dns.TypeA, 1, true), noECS)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			return info
		}
		// The first validation needs 5 chain lookups (root, com and
		// example.com keys): each waits for the global rate, none fails.
		wantVerdict(t, resolve(attacker, "www.example.com."), dnssec.Secure)
		// The client's share is used up: its next lookup is refused, a
		// local failure that no cache keeps.
		info := resolve(attacker, "www.unsigned.com.")
		wantVerdict(t, info, dnssec.Bogus)
		if !strings.Contains(info.DNSSEC.Reason, "chain lookup rate limit") {
			t.Fatalf("reason %q", info.DNSSEC.Reason)
		}
		if _, _, failures := r.val.v.Stats(); failures != 0 {
			t.Fatalf("%d chain failures cached", failures)
		}
		// Another client validates the same name with its own share.
		info = resolve(victim, "www.unsigned.com.")
		wantVerdict(t, info, dnssec.Insecure)
		if info.Cached {
			t.Fatal("the refused answer was cached")
		}
	})
}

func TestBackgroundPanicIsContained(t *testing.T) {
	// A panic while fetching (here in a transport) ends the fetch with an
	// error for its waiters instead of the process.
	for _, mode := range []string{"strict", "parallel"} {
		t.Run(mode, func(t *testing.T) {
			st := newStore(t, func(d *settings.DNS) { d.Upstreams = []string{up1, up2}; d.UpstreamMode = mode })
			boom := func(context.Context, *dns.Msg) (*dns.Msg, error) { panic("boom") }
			r := newTestResolver(t, st, testOptions(), map[string]*fakeTransport{up1: {fn: boom}, up2: {fn: boom}})
			defer r.Close()
			if _, _, err := r.Resolve(context.Background(), query("x.example.", dns.TypeA, 1, false), noECS); err == nil {
				t.Fatal("no error")
			}
			r.flight.mu.Lock()
			n := len(r.flight.m)
			r.flight.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d flights left", n)
			}
		})
	}
}
