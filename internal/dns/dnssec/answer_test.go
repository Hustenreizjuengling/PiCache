package dnssec

import (
	"context"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
)

func TestRootDNAME(t *testing.T) {
	// A DNAME owned by the root (never valid data, but any upstream or
	// anyone on the path of a plain upstream can send it) keeps the whole
	// name as the prefix of the substitution: the answer is bogus, never a
	// panic.
	u, _, _ := dnssectest.Example(t)
	v := validatorFor(u)
	dname := mustRR(". 300 IN DNAME x.")
	for _, tc := range []struct {
		name   string
		answer []dns.RR
	}{
		{"DNAME only", []dns.RR{dname}},
		{"signed DNAME", u.Root.WithSig([]dns.RR{dname})},
		{"with its CNAME", []dns.RR{dname, mustRR("www.example.com 300 IN CNAME www.example.com.x.")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := new(dns.Msg)
			m.SetQuestion("www.example.com.", dns.TypeA)
			m.Response = true
			m.Answer = dnssectest.CopyRRs(tc.answer)
			req := &Request{Route: "test", Name: "www.example.com.", Type: dns.TypeA, Msg: m, Lookup: lookupOf(u), Now: testNow,
				TimeChecks: true}
			if res := v.Validate(context.Background(), req); res.Status != Bogus {
				t.Fatalf("root DNAME: %+v", res)
			}
		})
	}
	d := &rrset{name: ".", typ: dns.TypeDNAME, rrs: []dns.RR{dname}}
	if got, ok := substitute(d, "www.example.com."); !ok || got != "www.example.com.x." {
		t.Fatalf("substitute below the root: %q %v", got, ok)
	}
	if _, ok := substitute(d, "."); ok {
		t.Fatal("the root substituted by its own DNAME")
	}
}

func TestAnswerScrubbed(t *testing.T) {
	// Records that are not part of the answer to the question are removed
	// before validation: they neither lower a secure verdict to insecure
	// nor ride along with it.
	u, com, ex := dnssectest.Example(t)
	other := u.AddChild(com, "other.com", true)
	other.A("evil.other.com", "203.0.113.66")
	other.A("target.other.com", "192.0.2.9")
	ex.Add(mustRR("alias.example.com 300 IN CNAME target.other.com"))
	noForged := func(t *testing.T, m *dns.Msg) {
		t.Helper()
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok && a.A.String() == "203.0.113.66" {
				t.Fatalf("forged record kept: %v", m.Answer)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		extra []dns.RR
	}{
		{"unsigned zone", []dns.RR{mustRR("www.unsigned.com 300 IN A 203.0.113.66")}},
		{"another signed zone", other.WithSig(other.Data["evil.other.com."][dns.TypeA])},
		{"another class", []dns.RR{mustRR("www.example.com 300 CH A 203.0.113.66")}},
		{"another name of the zone", ex.WithSig([]dns.RR{mustRR("mail.example.com 300 IN A 203.0.113.66")})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := validatorFor(u)
			req := request(u, "www.example.com", dns.TypeA)
			n := len(req.Msg.Answer)
			req.Msg.Answer = append(req.Msg.Answer, tc.extra...)
			if res := v.Validate(context.Background(), req); res.Status != Secure {
				t.Fatalf("status %+v", res)
			}
			if len(req.Msg.Answer) != n {
				t.Fatalf("answer %v", req.Msg.Answer)
			}
			noForged(t, req.Msg)
		})
	}
	// Every link of a CNAME chain across zones stays.
	v := validatorFor(u)
	req := request(u, "alias.example.com", dns.TypeA)
	n := len(req.Msg.Answer)
	req.Msg.Answer = append(req.Msg.Answer, other.WithSig(other.Data["evil.other.com."][dns.TypeA])...)
	if res := v.Validate(context.Background(), req); res.Status != Secure || n != 4 || len(req.Msg.Answer) != n {
		t.Fatalf("CNAME chain: %+v, %d of %d records: %v", res, len(req.Msg.Answer), n, req.Msg.Answer)
	}
	noForged(t, req.Msg)
	// A DNAME with its synthesised CNAME and the target's data stays.
	ex.Add(mustRR("old.example.com 300 IN DNAME example.com"))
	m := new(dns.Msg)
	m.SetQuestion("www.old.example.com.", dns.TypeA)
	m.Response = true
	m.Answer = append(m.Answer, ex.WithSig(ex.Data["old.example.com."][dns.TypeDNAME])...)
	m.Answer = append(m.Answer, mustRR("www.old.example.com 300 IN CNAME www.example.com."))
	m.Answer = append(m.Answer, ex.WithSig(ex.Data["www.example.com."][dns.TypeA])...)
	m.Answer = append(m.Answer, mustRR("www.unsigned.com 300 IN A 203.0.113.66"))
	req = &Request{Route: "test", Name: "www.old.example.com.", Type: dns.TypeA, Msg: m, Lookup: lookupOf(u), Now: testNow,
		TimeChecks: true}
	if res := v.Validate(context.Background(), req); res.Status != Secure || len(m.Answer) != 5 {
		t.Fatalf("DNAME: %+v %v", res, m.Answer)
	}
	// An insecure answer is scrubbed too.
	req = request(u, "www.unsigned.com", dns.TypeA)
	req.Msg.Answer = append(req.Msg.Answer, mustRR("www.example.com 300 IN A 203.0.113.66"))
	if res := v.Validate(context.Background(), req); res.Status != Insecure || len(req.Msg.Answer) != 1 {
		t.Fatalf("insecure: %+v %v", res, req.Msg.Answer)
	}
}

func TestNonexistentTLD(t *testing.T) {
	// A name below a top-level domain that does not exist (nas.lan): the
	// root's NXDOMAIN proof covers the name and the wildcard "*." at the
	// closest encloser, the root itself.
	for _, nsec3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "NSEC", true: "NSEC3"}[nsec3], func(t *testing.T) {
			u, _, _ := dnssectest.Example(t)
			u.AddChild(u.Root, "org", true)
			u.Root.NSEC3 = nsec3
			v := validatorFor(u)
			check(t, u, v, "nas.lan", dns.TypeA, Secure, EDENone, "")
			check(t, u, v, "_ldap._tcp.corp", dns.TypeSRV, Secure, EDENone, "")
			// The walk's "lan DS" gets the same proof: an unsigned answer
			// below it lacks its RRSIG (EDE 10) while the proof holds.
			m := new(dns.Msg)
			m.SetQuestion("x.nas.lan.", dns.TypeA)
			m.Response = true
			m.Answer = []dns.RR{mustRR("x.nas.lan 300 IN A 192.0.2.1")}
			req := &Request{Route: "test", Name: "x.nas.lan.", Type: dns.TypeA, Msg: m, Lookup: lookupOf(u), Now: testNow, TimeChecks: true}
			if res := v.Validate(context.Background(), req); res.Status != Bogus || res.EDE != EDERRSIGsMissing {
				t.Fatalf("unsigned answer below a missing TLD: %+v", res)
			}
		})
	}
}

func TestNegativeAfterWildcardCNAME(t *testing.T) {
	// A wildcard CNAME into another zone whose target has no AAAA: the
	// no-closer-name proof of the wildcard (example.com) comes first in the
	// Authority section, the denial of the target's zone after it. The
	// denial is judged in the target's zone, signed or not.
	for _, signed := range []bool{true, false} {
		t.Run(map[bool]string{true: "signed target", false: "unsigned target"}[signed], func(t *testing.T) {
			u, _, ex := dnssectest.Example(t)
			net := u.AddChild(u.Root, "net", true)
			cdn := u.AddChild(net, "cdn.net", signed)
			cdn.A("shops.cdn.net", "192.0.2.50")
			ex.Add(mustRR("*.shop.example.com 300 IN CNAME shops.cdn.net"))
			first := u.Resolve("a.shop.example.com.", dns.TypeCNAME) // the expansion with its proof
			last := u.Resolve("shops.cdn.net.", dns.TypeAAAA)        // NODATA
			m := new(dns.Msg)
			m.SetQuestion("a.shop.example.com.", dns.TypeAAAA)
			m.Response = true
			m.Answer = first.Answer
			m.Ns = append(first.Ns, last.Ns...)
			want := Secure
			if !signed {
				want = Insecure
			}
			v := validatorFor(u)
			req := &Request{Route: "test", Name: "a.shop.example.com.", Type: dns.TypeAAAA, Msg: m, Lookup: lookupOf(u), Now: testNow,
				TimeChecks: true}
			if res := v.Validate(context.Background(), req); res.Status != want {
				t.Fatalf("got %+v, want %s", res, want)
			}
		})
	}
}

func TestLiteralWildcardName(t *testing.T) {
	// The wildcard name itself asked literally: its RRSIG's labels leave
	// out the "*" (RFC 4034 3.1.3), yet it is no expansion and needs no
	// no-closer-name proof.
	u, _, ex := dnssectest.Example(t)
	ex.Add(mustRR("*.wild.example.com 300 IN A 192.0.2.7"))
	v := validatorFor(u)
	check(t, u, v, "*.wild.example.com", dns.TypeA, Secure, EDENone, "")
	check(t, u, v, "a.wild.example.com", dns.TypeA, Secure, EDENone, "")
	// An expansion without its proof still fails.
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		if q == "b.wild.example.com." {
			m.Ns = nil
		}
	}
	check(t, u, v, "b.wild.example.com", dns.TypeA, Bogus, EDENSECMissing, FailNSEC)
}
