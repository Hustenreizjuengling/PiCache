package dnssec

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
)

// Every bound of docs/ARCHITECTURE.md 7.6 once.

// signedA returns the A RRset of www.example.com with its RRSIG and the
// zone's keys.
func signedA(t *testing.T) (*rrset, []key, *dnssectest.Zone) {
	t.Helper()
	_, _, ex := dnssectest.Example(t)
	rrs := ex.Data["www.example.com."][dns.TypeA]
	sig := ex.Sign(rrs)
	var keys []key
	for _, rr := range ex.Data[ex.Name][dns.TypeDNSKEY] {
		keys = append(keys, newKey(rr.(*dns.DNSKEY)))
	}
	return &rrset{name: "www.example.com.", typ: dns.TypeA, rrs: dnssectest.CopyRRs(rrs), sigs: []*dns.RRSIG{sig}}, keys, ex
}

func testSctx(b *budget) *sctx {
	return &sctx{ctx: context.Background(), v: New(Config{}), b: b, now: testNow, timeChecks: true}
}

func TestBoundSignaturesPerRRset(t *testing.T) {
	set, keys, _ := signedA(t)
	good := set.sigs[0]
	// Eight RRSIGs naming a key the zone does not have (no verification),
	// then the valid one: only the first 8 count.
	var sigs []*dns.RRSIG
	for i := range 8 {
		s := *good
		s.KeyTag = good.KeyTag + uint16(i) + 1
		sigs = append(sigs, &s)
	}
	m := new(dns.Msg)
	for _, s := range append(sigs, good) {
		m.Answer = append(m.Answer, s)
	}
	m.Answer = append(m.Answer, set.rrs...)
	grouped := groupRRsets(m.Answer)
	if len(grouped) != 1 || len(grouped[0].sigs) != maxSigsPerRRset {
		t.Fatalf("grouped %d sets, %d sigs", len(grouped), len(grouped[0].sigs))
	}
	if _, f := testSctx(&budget{}).verifyRRset(grouped[0], "example.com.", keys); f == nil || f.text != FailBadSignature {
		t.Fatalf("9th RRSIG considered: %+v", f)
	}
	// The valid one as the 8th: secure.
	m.Answer = nil
	for _, s := range sigs[:7] {
		m.Answer = append(m.Answer, s)
	}
	m.Answer = append(append(m.Answer, good), set.rrs...)
	if _, f := testSctx(&budget{}).verifyRRset(groupRRsets(m.Answer)[0], "example.com.", keys); f != nil {
		t.Fatalf("8th RRSIG: %+v", f)
	}
}

func TestBoundKeysPerSignature(t *testing.T) {
	set, keys, ex := signedA(t)
	zsk := ex.ZSK.Key
	tag := zsk.KeyTag()
	// Four wrong keys that claim the ZSK's tag, then the ZSK: the fifth
	// key of a tag is ignored. (The failures do not end the context here.)
	var many []key
	for range 4 {
		many = append(many, key{k: keys[0].k, tag: tag})
	}
	c := testSctx(&budget{failed: -100})
	if _, f := c.verifyRRset(set, "example.com.", append(many, key{k: zsk, tag: tag})); f == nil || c.b.verifies != 4 {
		t.Fatalf("5th key tried: %+v, %d verifications", f, c.b.verifies)
	}
	c = testSctx(&budget{failed: -100})
	if _, f := c.verifyRRset(set, "example.com.", append(many[:3], key{k: zsk, tag: tag})); f != nil {
		t.Fatalf("4th key: %+v", f)
	}
}

func TestBoundVerifications(t *testing.T) {
	set, keys, _ := signedA(t)
	if _, f := testSctx(&budget{verifies: 16}).verifyRRset(set, "example.com.", keys); f == nil || f.text != FailLimit || !f.lookup {
		t.Fatalf("17th verification: %+v", f)
	}
	if _, f := testSctx(&budget{verifies: 15}).verifyRRset(set, "example.com.", keys); f != nil {
		t.Fatalf("16th verification: %+v", f)
	}
	bad := *set.sigs[0]
	bad.Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
	set.sigs = []*dns.RRSIG{&bad}
	if _, f := testSctx(&budget{failed: 2}).verifyRRset(set, "example.com.", keys); f == nil || f.text != FailLimit {
		t.Fatalf("3rd failed verification: %+v", f)
	}
	if _, f := testSctx(&budget{failed: 1}).verifyRRset(set, "example.com.", keys); f == nil || f.text != FailBadSignature {
		t.Fatalf("2nd failed verification: %+v", f)
	}
}

func TestBoundKeyAndDSRecords(t *testing.T) {
	u, com, ex := dnssectest.Example(t)
	for i := range maxDNSKEYs - 1 { // 2 + 31 = 33 keys
		k := *ex.ZSK.Key
		k.PublicKey = rsaPublic(1024 + 8*i)
		k.Algorithm = dns.RSASHA256
		ex.Add(&k)
	}
	v := validatorFor(u)
	check(t, u, v, "www.example.com", dns.TypeA, Bogus, EDEBogus, FailLimit)

	u, com, ex = dnssectest.Example(t)
	for i := range maxDSRecords { // 1 + 8 = 9 DS records
		ds := ex.KSK.Key.ToDS(dns.SHA256)
		ds.Hdr.Ttl = 3600
		ds.KeyTag += uint16(i + 1)
		com.Add(ds)
	}
	v = validatorFor(u)
	check(t, u, v, "www.example.com", dns.TypeA, Bogus, EDEBogus, FailLimit)
}

func TestBoundNSEC3Parameters(t *testing.T) {
	for _, tc := range []struct {
		name       string
		iterations uint16
		salt       string
	}{{"51 iterations", 51, ""}, {"65-byte salt", 0, strings.Repeat("ab", 65)}, {"1000 iterations", 1000, "aa"}} {
		t.Run(tc.name, func(t *testing.T) {
			u, com, _ := dnssectest.Example(t)
			com.Iterations, com.Salt = tc.iterations, tc.salt
			v := validatorFor(u)
			// A NXDOMAIN in com and the delegation proof of unsigned.com: the
			// proof's zone is insecure (EDE 27), no hash is computed.
			for _, name := range []string{"nothere.com", "www.unsigned.com"} {
				req := request(u, name, dns.TypeA)
				c := &vctx{sctx: sctx{ctx: context.Background(), v: v, b: &budget{}, now: testNow, timeChecks: true}, req: req,
					steps: map[stepKey]stepResult{}, zones: map[string]*zoneState{}}
				start := time.Now()
				res := c.answer()
				if res.Status != Insecure || res.EDE != EDEUnsupportedNSEC3Params || res.Reason != ReasonNSEC3Iterations {
					t.Fatalf("%s: %+v", name, res)
				}
				if c.b.hashes != 0 {
					t.Fatalf("%s: %d hashes computed", name, c.b.hashes)
				}
				if d := time.Since(start); d > 100*time.Millisecond {
					t.Fatalf("%s took %v", name, d)
				}
			}
		})
	}
}

func TestBoundNSEC3Hashes(t *testing.T) {
	u, _, _ := dnssectest.Example(t)
	v := validatorFor(u)
	req := request(u, "nothere.com", dns.TypeA)
	c := &vctx{sctx: sctx{ctx: context.Background(), v: v, b: &budget{hashes: maxNSEC3Hashes}, now: testNow, timeChecks: true},
		req: req, steps: map[stepKey]stepResult{}, zones: map[string]*zoneState{}}
	if res := c.answer(); res.Status != Bogus || res.Reason != FailLimit || res.EDE != EDEBogus {
		t.Fatalf("257th hash: %+v", res)
	}
}

func TestBoundProofRecords(t *testing.T) {
	u, com, _ := dnssectest.Example(t)
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		if q != "nothere.com." {
			return
		}
		for i := range maxProofRecords {
			r := &dns.NSEC3{Hdr: dnssectest.Hdr(fmt.Sprintf("%032d.com", i), dns.TypeNSEC3, 300), Hash: dns.SHA1,
				NextDomain: fmt.Sprintf("%032d", i+1), HashLength: 20}
			m.Ns = append(m.Ns, com.WithSig([]dns.RR{r})...)
		}
	}
	v := validatorFor(u)
	check(t, u, v, "nothere.com", dns.TypeA, Bogus, EDEBogus, FailLimit)
}

func TestBoundChainHops(t *testing.T) {
	u, _, ex := dnssectest.Example(t)
	for i := range maxChainHops + 1 {
		ex.Add(mustRR(fmt.Sprintf("c%d.example.com 300 IN CNAME c%d.example.com", i, i+1)))
	}
	ex.A(fmt.Sprintf("c%d.example.com", maxChainHops+1), "192.0.2.9")
	v := validatorFor(u)
	check(t, u, v, "c0.example.com", dns.TypeA, Bogus, EDEBogus, FailLimit)
	check(t, u, v, "c1.example.com", dns.TypeA, Secure, EDENone, "") // 8 hops
}

func TestBoundChainLookups(t *testing.T) {
	// An unsigned answer 35 labels below example.com: the walk asks DS for
	// every empty non-terminal on the way and stops at 32 lookups.
	u, _, ex := dnssectest.Example(t)
	labels := make([]string, 35)
	for i := range labels {
		labels[i] = fmt.Sprintf("l%d", i)
	}
	deep := strings.Join(labels, ".") + ".example.com"
	ex.A(deep, "192.0.2.9")
	v := validatorFor(u)
	check(t, u, v, "www.example.com", dns.TypeA, Secure, EDENone, "") // the zone states
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(deep), dns.TypeA)
	m.Answer = []dns.RR{mustRR(deep + " 300 IN A 203.0.113.1")}
	res := v.Validate(context.Background(), &Request{Route: "test", Name: dns.Fqdn(deep), Type: dns.TypeA, Msg: m,
		Lookup: lookupOf(u), Now: testNow, TimeChecks: true})
	if res.Status != Bogus || res.Reason != FailLimit || !res.Lookup {
		t.Fatalf("33rd lookup: %+v", res)
	}
}

func TestBoundChainFlights(t *testing.T) {
	u, _, _ := dnssectest.Example(t)
	v := validatorFor(u)
	for i := range maxChainFlights {
		v.flights[stepKey{route: "other", name: fmt.Sprintf("n%d.", i)}] = &stepCall{done: make(chan struct{})}
	}
	res := v.Validate(context.Background(), request(u, "www.example.com", dns.TypeA))
	if res.Status != Bogus || !res.Lookup || !res.Local || !strings.Contains(res.Reason, "too many chain lookups") {
		t.Fatalf("1025th flight: %+v", res)
	}
	if _, _, failures := v.Stats(); failures != 0 {
		t.Fatalf("a local refusal in the failure cache: %d", failures)
	}
}

func TestChainStepPanic(t *testing.T) {
	// A chain step that panics (the goroutine of Config.Spawn recovers it)
	// ends as a lookup failure for its waiters, and its flight is gone.
	u, _, _ := dnssectest.Example(t)
	v := New(Config{Anchors: u.Anchors(), Spawn: func(fn func()) bool {
		go func() {
			defer func() { _ = recover() }()
			fn()
		}()
		return true
	}})
	req := request(u, "www.example.com", dns.TypeA)
	req.Lookup = func(context.Context, string, uint16) (Reply, error) { panic("boom") }
	res := v.Validate(context.Background(), req)
	if res.Status != Bogus || !res.Lookup || res.Reason != "DNSKEY lookup failed (internal error)" || v.Flights() != 0 {
		t.Fatalf("%+v, %d flights", res, v.Flights())
	}
}

func TestRateLimitedLookupIsNotCached(t *testing.T) {
	// A chain lookup refused by a local rate limit fails the answer as a
	// local lookup failure that the failure cache does not keep.
	u, _, _ := dnssectest.Example(t)
	v := validatorFor(u)
	var limited atomic.Bool
	limited.Store(true)
	lookup := func(ctx context.Context, name string, qtype uint16) (Reply, error) {
		if limited.Load() {
			return Reply{}, fmt.Errorf("upstream: %w", ErrRateLimited)
		}
		return lookupOf(u)(ctx, name, qtype)
	}
	req := request(u, "www.example.com", dns.TypeA)
	req.Lookup = lookup
	res := v.Validate(context.Background(), req)
	if res.Status != Bogus || !res.Lookup || !res.Local || res.Reason != "DNSKEY lookup failed (chain lookup rate limit)" {
		t.Fatalf("limited: %+v", res)
	}
	if _, _, failures := v.Stats(); failures != 0 {
		t.Fatalf("%d failures kept", failures)
	}
	limited.Store(false)
	req = request(u, "www.example.com", dns.TypeA)
	req.Lookup = lookup
	if res := v.Validate(context.Background(), req); res.Status != Secure {
		t.Fatalf("after the limit: %+v", res)
	}
}

// collidingKeys returns n RSA-2048-sized public keys (not real keys) with
// the flags 256 and the key tag tag: a KeyTrap zone.
func collidingKeys(zone string, n int, tag uint16) []dns.RR {
	var out []dns.RR
	for i := 0; len(out) < n; i++ {
		mod := make([]byte, 256)
		mod[0] = 0xC0
		mod[1] = byte(i)
		mod[2] = byte(i >> 8)
		k := &dns.DNSKEY{Hdr: dnssectest.Hdr(zone, dns.TypeDNSKEY, 3600), Flags: 256, Protocol: 3, Algorithm: dns.RSASHA256}
		// The key tag is a 16-bit sum of the RDATA: the last two bytes
		// of the modulus correct it.
		k.PublicKey = base64.StdEncoding.EncodeToString(append([]byte{3, 1, 0, 1}, mod...))
		diff := int(tag) - int(k.KeyTag())
		for diff < 0 {
			diff += 0xffff
		}
		mod[254], mod[255] = byte(diff>>8), byte(diff)
		k.PublicKey = base64.StdEncoding.EncodeToString(append([]byte{3, 1, 0, 1}, mod...))
		if k.KeyTag() == tag {
			out = append(out, k)
		}
	}
	return out
}

func TestKeyTrap(t *testing.T) {
	// The zone holds 30 keys with one key tag, and the answer carries 8
	// RRSIGs claiming that tag: the context stops at the third failed
	// verification.
	u, _, ex := dnssectest.Example(t)
	const tag = 4242
	for _, rr := range collidingKeys(ex.Name, 30, tag) {
		ex.Add(rr)
	}
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		if q != "www.example.com." || qt != dns.TypeA {
			return
		}
		sig := m.Answer[len(m.Answer)-1].(*dns.RRSIG)
		for i := range maxSigsPerRRset {
			s := *sig
			s.KeyTag, s.Algorithm = tag, dns.RSASHA256
			s.Signature = base64.StdEncoding.EncodeToString(append(make([]byte, 255), byte(i)))
			m.Answer = append(m.Answer, &s)
		}
		m.Answer = append(m.Answer[:1], m.Answer[2:]...) // without the valid RRSIG
	}
	v := validatorFor(u)
	start := time.Now()
	res := v.Validate(context.Background(), request(u, "www.example.com", dns.TypeA))
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("KeyTrap zone took %v", d)
	}
	if res.Status != Bogus || res.Reason != FailLimit {
		t.Fatalf("KeyTrap zone: %+v", res)
	}
}

func TestKeyCache(t *testing.T) {
	now := testNow
	big := func(zone string) *zoneState {
		var keys []key
		for i := range 29 { // about 15.7 KiB of RSA-4096 keys
			k := &dns.DNSKEY{Hdr: dnssectest.Hdr(zone, dns.TypeDNSKEY, 3600), Flags: 256, Protocol: 3, Algorithm: dns.RSASHA256,
				PublicKey: rsaPublic(4096 - i)}
			keys = append(keys, newKey(k))
		}
		return &zoneState{zone: zone, secure: true, keys: keys}
	}
	var c keyCache
	for i := range 1000 {
		zone := fmt.Sprintf("z%d.test.", i)
		c.put(cacheKey{"r", zone}, big(zone), time.Hour, now, false)
	}
	entries, bytes := c.stats()
	if bytes > maxKeyBytes || entries >= 1000 || entries < 500 {
		t.Fatalf("%d entries, %d bytes", entries, bytes)
	}
	if c.get(cacheKey{"r", "z999.test."}, now) == nil || c.get(cacheKey{"r", "z0.test."}, now) != nil {
		t.Fatal("not LRU")
	}
	// An RRset above 16 KiB is not kept.
	st := big("huge.test.")
	st.keys = append(st.keys, big("huge.test.").keys...)
	c.put(cacheKey{"r", "huge.test."}, st, time.Hour, now, false)
	if c.get(cacheKey{"r", "huge.test."}, now) != nil {
		t.Fatal("an RRset above 16 KiB was kept")
	}
	// The entry bound.
	var small keyCache
	for i := range maxKeyEntries + 10 {
		zone := fmt.Sprintf("s%d.test.", i)
		small.put(cacheKey{"r", zone}, &zoneState{zone: zone, reason: ReasonNoDS}, time.Hour, now, false)
	}
	if n, _ := small.stats(); n != maxKeyEntries {
		t.Fatalf("%d entries", n)
	}
	// Expiry and suspended states.
	small.put(cacheKey{"r", "sus.test."}, &zoneState{zone: "sus.test."}, time.Hour, now, true)
	small.dropSuspended()
	if small.get(cacheKey{"r", "sus.test."}, now) != nil || small.get(cacheKey{"r", "s20.test."}, now.Add(2*time.Hour)) != nil {
		t.Fatal("suspended or expired state kept")
	}
}

func TestLifetime(t *testing.T) {
	now := testNow
	for _, tc := range []struct {
		ttl     uint32
		expires time.Time
		maxTTL  uint32
		checks  bool
		want    time.Duration
	}{
		{86400 * 2, time.Time{}, 0, true, 24 * time.Hour},            // at most 24 h
		{3600, now.Add(10 * time.Minute), 0, true, 10 * time.Minute}, // the RRSIG expiration
		{3600, now.Add(10 * time.Minute), 0, false, time.Hour},       // suspended: the dates do not count
		{3600, time.Time{}, 600, true, 10 * time.Minute},             // dns.cacheMaxTtl
		{60, now.Add(time.Hour), 0, true, time.Minute},               // the TTL (dns.cacheMinTtl never raises it)
		{3600, now.Add(-time.Minute), 0, true, -time.Minute},         // expired: not kept
		{0, time.Time{}, 0, true, 0},
	} {
		if got := lifetime(tc.ttl, tc.expires, tc.maxTTL, now, tc.checks); got != tc.want {
			t.Errorf("lifetime(%d, %v, %d, %v) = %v, want %v", tc.ttl, tc.expires, tc.maxTTL, tc.checks, got, tc.want)
		}
	}
}

func TestFailureCache(t *testing.T) {
	u, _, _ := dnssectest.Example(t)
	var fail bool
	lookup := func(ctx context.Context, name string, qtype uint16) (Reply, error) {
		if fail && qtype == dns.TypeDNSKEY && name == "example.com." {
			return Reply{}, context.DeadlineExceeded
		}
		m, err := u.Serve(ctx, name, qtype)
		return Reply{Msg: m}, err
	}
	v := validatorFor(u)
	req := func(now time.Time) *Request {
		return &Request{Route: "test", Name: "www.example.com.", Type: dns.TypeA, Msg: u.Resolve("www.example.com.", dns.TypeA),
			Lookup: lookup, Now: now, TimeChecks: true}
	}
	fail = true
	if res := v.Validate(context.Background(), req(testNow)); res.Status != Bogus || res.EDE != EDEDNSKEYMissing ||
		res.Reason != "DNSKEY lookup failed (timeout)" || !res.Lookup {
		t.Fatalf("DNSKEY timeout: %+v", res)
	}
	if _, _, failures := v.Stats(); failures != 1 {
		t.Fatalf("%d failures kept", failures)
	}
	fail = false
	n := u.Count("example.com.", dns.TypeDNSKEY)
	if res := v.Validate(context.Background(), req(testNow.Add(4*time.Second))); res.Status != Bogus {
		t.Fatalf("within 5 s: %+v", res)
	}
	if u.Count("example.com.", dns.TypeDNSKEY) != n {
		t.Fatal("the failure cache did not answer")
	}
	if res := v.Validate(context.Background(), req(testNow.Add(6*time.Second))); res.Status != Secure {
		t.Fatalf("after 5 s: %+v", res)
	}
}
