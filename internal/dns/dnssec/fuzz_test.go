package dnssec

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
)

// The validator reads untrusted messages (answers and chain replies of
// upstreams): the fuzz tests check that no input panics or runs beyond the
// budgets of a validation context.

// fuzzSeeds packs answers of the example universe.
func fuzzSeeds(f *testing.F) *dnssectest.Universe {
	u, _, ex := dnssectest.Example(f)
	ex.Add(mustRR("*.wild.example.com 300 IN A 192.0.2.7"))
	for _, q := range []struct {
		name  string
		qtype uint16
	}{{"www.example.com.", dns.TypeA}, {"nothere.example.com.", dns.TypeA}, {"nothere.com.", dns.TypeA},
		{"www.unsigned.com.", dns.TypeA}, {"a.wild.example.com.", dns.TypeA}, {"example.com.", dns.TypeDNSKEY},
		{"example.com.", dns.TypeDS}, {".", dns.TypeDNSKEY}, {"www.example.com.", dns.TypeTXT}} {
		wire, err := u.Resolve(q.name, q.qtype).Pack()
		if err != nil {
			f.Fatal(err)
		}
		chain, err := u.Resolve("unsigned.com.", dns.TypeDS).Pack()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(wire, chain)
	}
	// DNAMEs owned by the root, alone and with a synthesised CNAME.
	for _, answer := range [][]dns.RR{
		{mustRR(". 300 IN DNAME x.")},
		{mustRR(". 300 IN DNAME x."), mustRR("www.example.com 300 IN CNAME www.example.com.x.")},
		{mustRR(". 300 IN DNAME .")},
	} {
		m := new(dns.Msg)
		m.SetQuestion("www.example.com.", dns.TypeA)
		m.Response = true
		m.Answer = answer
		wire, err := m.Pack()
		if err != nil {
			f.Fatal(err)
		}
		f.Add(wire, []byte{})
	}
	return u
}

// FuzzValidate validates an arbitrary answer (its own question) whose
// chain lookups get the second message (its question set to the lookup's)
// or, when that does not unpack, the answers of the example universe.
func FuzzValidate(f *testing.F) {
	u := fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, answer, chain []byte) {
		m := new(dns.Msg)
		if err := m.Unpack(answer); err != nil || len(m.Question) != 1 {
			return
		}
		reply := new(dns.Msg)
		replyOK := reply.Unpack(chain) == nil
		lookup := func(ctx context.Context, name string, qtype uint16) (Reply, error) {
			if replyOK {
				c := reply.Copy()
				c.Question = []dns.Question{{Name: name, Qtype: qtype, Qclass: dns.ClassINET}}
				return Reply{Msg: c}, nil
			}
			r, err := u.Serve(ctx, name, qtype)
			return Reply{Msg: r}, err
		}
		v := New(Config{Anchors: u.Anchors()})
		q := m.Question[0]
		start := time.Now()
		for _, checks := range []bool{true, false} {
			res := v.Validate(context.Background(), &Request{Route: "fuzz", Name: q.Name, Type: q.Qtype, Msg: m.Copy(), Lookup: lookup,
				Now: dnssectest.Now, TimeChecks: checks})
			switch res.Status {
			case "", Secure, Insecure, Bogus, Indeterminate:
			default:
				t.Fatalf("status %q", res.Status)
			}
			if res.Status == Bogus && res.Reason == "" {
				t.Fatal("bogus without a reason")
			}
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("validation took %v", d)
		}
	})
}

// FuzzCheckRoot judges arbitrary probe answers.
func FuzzCheckRoot(f *testing.F) {
	u := fuzzSeeds(f)
	k, _ := u.Resolve(".", dns.TypeDNSKEY).Pack()
	s, _ := u.Resolve(".", dns.TypeSOA).Pack()
	f.Add(k, s)
	f.Fuzz(func(t *testing.T, dnskey, soa []byte) {
		km, sm := new(dns.Msg), new(dns.Msg)
		if km.Unpack(dnskey) != nil || sm.Unpack(soa) != nil {
			return
		}
		v := New(Config{Anchors: u.Anchors()})
		switch r := v.CheckRoot(context.Background(), km, sm, dnssectest.Now); r.State {
		case StateCapable, StateNoDNSSEC, StateAnchorMismatch:
		default:
			t.Fatalf("state %q", r.State)
		}
	})
}

// FuzzNames checks the name helpers with arbitrary names.
func FuzzNames(f *testing.F) {
	for _, s := range [][2]string{{"www.example.com.", "example.com."}, {"a.", "b."}, {".", "."}, {`\000.example.`, "*.example."},
		{"x.y.z", "Y.Z."}} {
		f.Add(s[0], s[1], uint8(1))
	}
	f.Fuzz(func(t *testing.T, a, b string, n uint8) {
		a, b = dns.Fqdn(a), dns.Fqdn(b) // names of messages are fully qualified
		if !dns.IsFqdn(a) || !dns.IsFqdn(b) {
			return // an escaped final dot
		}
		if _, ok := dns.IsDomainName(a); !ok {
			return
		}
		if _, ok := dns.IsDomainName(b); !ok {
			return
		}
		ab, ba := compareNames(a, b), compareNames(b, a)
		if ab != -ba {
			t.Fatalf("compare(%q, %q) = %d, reverse %d", a, b, ab, ba)
		}
		if compareNames(a, a) != 0 {
			t.Fatalf("compare(%q, itself) != 0", a)
		}
		ce := commonAncestor(a, b)
		if !below(ce, a) || !below(ce, b) {
			t.Fatalf("common ancestor %q of %q and %q", ce, a, b)
		}
		if below(ce, a) {
			for _, l := range ancestorsBelow(ce, a) {
				if !below(ce, l) {
					t.Fatalf("%q not below %q", l, ce)
				}
			}
		}
		_ = lastLabels(a, int(n))
		_ = parentName(a)
		var buf [2]byte
		binary.BigEndian.PutUint16(buf[:], uint16(n))
		_ = display(a + string(buf[:1]))
	})
}
