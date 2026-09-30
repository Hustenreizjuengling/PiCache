package dnssec

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
)

// coldChain is the chain of BenchmarkColdChain (docs/ARCHITECTURE.md 13,
// the default DNSSEC mode): root → tld → sld.tld signed with RSA-2048 KSKs
// and ECDSA P-256 ZSKs, tld with NSEC3; the answers are "www.sld.tld A"
// (secure) and "www.insecure.tld A" below an insecure delegation proven by
// NSEC3. The replies are prepared once, so the lookups cost nothing.
func coldChain(tb testing.TB) (*dnssectest.Universe, LookupFunc, *atomic.Int64, map[string]*dns.Msg) {
	u := dnssectest.NewRSAUniverse(tb)
	tld := u.AddChild(u.Root, "tld", true)
	tld.NSEC3 = true
	sld := u.AddChild(tld, "sld.tld", true)
	sld.A("www.sld.tld", "192.0.2.1")
	insecure := u.AddChild(tld, "insecure.tld", false)
	insecure.A("www.insecure.tld", "192.0.2.2")
	msgs := map[string]*dns.Msg{}
	for _, q := range []struct {
		name  string
		qtype uint16
	}{{".", dns.TypeDNSKEY}, {"tld.", dns.TypeDS}, {"tld.", dns.TypeDNSKEY}, {"sld.tld.", dns.TypeDS},
		{"sld.tld.", dns.TypeDNSKEY}, {"insecure.tld.", dns.TypeDS}, {"www.sld.tld.", dns.TypeA}, {"www.insecure.tld.", dns.TypeA}} {
		msgs[q.name+"/"+dns.TypeToString[q.qtype]] = u.Resolve(q.name, q.qtype)
	}
	var lookups atomic.Int64
	lookup := func(_ context.Context, name string, qtype uint16) (Reply, error) {
		lookups.Add(1)
		m, ok := msgs[canon(name)+"/"+dns.TypeToString[qtype]]
		if !ok {
			return Reply{}, fmt.Errorf("unexpected lookup %s %s", name, dns.TypeToString[qtype])
		}
		return Reply{Msg: m.Copy()}, nil
	}
	return u, lookup, &lookups, msgs
}

// coldValidation validates both answers of the cold chain with an empty
// validator.
func coldValidation(tb testing.TB, u *dnssectest.Universe, lookup LookupFunc, msgs map[string]*dns.Msg) {
	v := New(Config{Anchors: u.Anchors()})
	for _, q := range []struct {
		name, status string
	}{{"www.sld.tld.", Secure}, {"www.insecure.tld.", Insecure}} {
		res := v.Validate(context.Background(), &Request{Route: "bench", Name: q.name, Type: dns.TypeA,
			Msg: msgs[q.name+"/A"].Copy(), Lookup: lookup, Now: testNow, TimeChecks: true})
		if res.Status != q.status {
			tb.Fatalf("%s: %+v", q.name, res)
		}
	}
}

// BenchmarkColdChain measures the CPU of a cold validation (one secure A
// answer and one NSEC3 insecure-delegation proof, nothing cached, lookups
// without delay). The default DNSSEC mode of new installations depends on
// it staying at or below 20 ms per operation under linux/arm/v7 emulation
// (ARCHITECTURE 13).
func BenchmarkColdChain(b *testing.B) {
	u, lookup, _, msgs := coldChain(b)
	b.ReportAllocs()
	for b.Loop() {
		coldValidation(b, u, lookup, msgs)
	}
}

func TestColdChain(t *testing.T) {
	u, lookup, lookups, msgs := coldChain(t)
	coldValidation(t, u, lookup, msgs)
	// root DNSKEY, tld DS + DNSKEY, sld.tld DS + DNSKEY, insecure.tld DS.
	if n := lookups.Load(); n != 6 {
		t.Fatalf("%d chain lookups, want 6", n)
	}
}

// TestCachedTLDCostsTwoLookups: a cold signed name whose TLD key state is
// cached costs at most two extra exchanges (DS and DNSKEY of its zone).
func TestCachedTLDCostsTwoLookups(t *testing.T) {
	u, com, _ := dnssectest.Example(t)
	second := u.AddChild(com, "second.com", true)
	second.A("www.second.com", "192.0.2.20")
	v := validatorFor(u)
	check(t, u, v, "www.example.com", dns.TypeA, Secure, EDENone, "")
	before := u.Total()
	check(t, u, v, "www.second.com", dns.TypeA, Secure, EDENone, "")
	if n := u.Total() - before; n != 2 {
		t.Fatalf("%d extra lookups, want 2", n)
	}
	before = u.Total()
	check(t, u, v, "second.com", dns.TypeA, Secure, EDENone, "") // NODATA in a known zone
	if n := u.Total() - before; n != 0 {
		t.Fatalf("%d lookups for a known zone, want 0", n)
	}
}

// TestHeapTenThousandZones: validating 10 000 distinct signed zones grows
// the heap by at most 16 MiB (the key cache is bounded by 8 MiB).
func TestHeapTenThousandZones(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	u := dnssectest.NewUniverse(t)
	tld := u.AddChild(u.Root, "tld", true)
	const n = 10000
	names := make([]string, n)
	for i := range n {
		z := u.AddChild(tld, fmt.Sprintf("z%05d.tld", i), true)
		names[i] = "www." + z.Name
		z.A(names[i], "192.0.2.1")
	}
	// Every reply is prepared before the measurement; a lookup returns a
	// copy (garbage unless the validator keeps a part of it).
	replies := map[string]*dns.Msg{}
	msgs := make([]*dns.Msg, n)
	for i, name := range names {
		msgs[i] = u.Resolve(name, dns.TypeA)
		zone := parentName(canon(name))
		replies[zone+"/DS"] = u.Resolve(zone, dns.TypeDS)
		replies[zone+"/DNSKEY"] = u.Resolve(zone, dns.TypeDNSKEY)
	}
	for _, z := range []string{".", "tld."} {
		replies[z+"/DNSKEY"] = u.Resolve(z, dns.TypeDNSKEY)
	}
	replies["tld./DS"] = u.Resolve("tld.", dns.TypeDS)
	lookup := func(_ context.Context, name string, qtype uint16) (Reply, error) {
		m, ok := replies[canon(name)+"/"+dns.TypeToString[qtype]]
		if !ok {
			return Reply{}, fmt.Errorf("unexpected lookup %s %d", name, qtype)
		}
		return Reply{Msg: m.Copy()}, nil
	}
	v := validatorFor(u)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i, name := range names {
		res := v.Validate(context.Background(), &Request{Route: "heap", Name: name, Type: dns.TypeA, Msg: msgs[i],
			Lookup: lookup, Now: testNow, TimeChecks: true})
		if res.Status != Secure {
			t.Fatalf("%s: %+v", name, res)
		}
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	zones, bytes, _ := v.Stats()
	t.Logf("heap growth %.1f MiB, %d zone states kept, %.1f MiB counted by the key cache", float64(growth)/(1<<20), zones,
		float64(bytes)/(1<<20))
	if growth > 16<<20 {
		t.Fatalf("heap grew by %d bytes", growth)
	}
	runtime.KeepAlive(v)
	runtime.KeepAlive(msgs)
	runtime.KeepAlive(replies)
}
