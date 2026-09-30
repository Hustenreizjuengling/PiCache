package dnssec

import (
	"context"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
)

func TestSecureAnswers(t *testing.T) {
	u, com, ex := dnssectest.Example(t)
	other := u.AddChild(com, "other.com", true)
	other.A("target.other.com", "192.0.2.9")
	ex.Add(mustRR("alias.example.com 300 IN CNAME target.other.com"))
	ex.Add(mustRR("*.wild.example.com 300 IN A 192.0.2.7"))
	ex.Add(mustRR("deep.ent.example.com 300 IN A 192.0.2.8"))
	v := validatorFor(u)

	res := check(t, u, v, "www.example.com", dns.TypeA, Secure, EDENone, "")
	if res.Zone != "example.com" || res.Expires.IsZero() {
		t.Fatalf("secure A: zone %q expires %v", res.Zone, res.Expires)
	}
	// A CNAME chain across two zones.
	if r := check(t, u, v, "alias.example.com", dns.TypeA, Secure, EDENone, ""); r.Zone != "other.com" {
		t.Fatalf("CNAME chain: zone %q", r.Zone)
	}
	// NXDOMAIN and NODATA by NSEC (example.com) and NSEC3 (com).
	check(t, u, v, "nothere.example.com", dns.TypeA, Secure, EDENone, "")
	check(t, u, v, "www.example.com", dns.TypeTXT, Secure, EDENone, "")
	check(t, u, v, "nothere.com", dns.TypeA, Secure, EDENone, "")
	check(t, u, v, "com", dns.TypeTXT, Secure, EDENone, "")
	// A wildcard expansion with its proof, and the wildcard NODATA proof.
	check(t, u, v, "a.wild.example.com", dns.TypeA, Secure, EDENone, "")
	check(t, u, v, "a.wild.example.com", dns.TypeTXT, Secure, EDENone, "")
	// An empty non-terminal.
	check(t, u, v, "ent.example.com", dns.TypeA, Secure, EDENone, "")
	// DS and DNSKEY answers validated from the answer itself.
	check(t, u, v, "example.com", dns.TypeDS, Secure, EDENone, "")
	check(t, u, v, "example.com", dns.TypeDNSKEY, Secure, EDENone, "")
	check(t, u, v, ".", dns.TypeDNSKEY, Secure, EDENone, "")
	check(t, u, v, "unsigned.com", dns.TypeDS, Secure, EDENone, "") // NODATA with the NS bit
}

func TestSecureAnswerTrimsSections(t *testing.T) {
	u, _, _ := dnssectest.Example(t)
	v := validatorFor(u)
	req := request(u, "www.example.com", dns.TypeA)
	req.Msg.Ns = append(req.Msg.Ns, mustRR("example.com 300 IN NS ns.evil.test"))
	req.Msg.Extra = append(req.Msg.Extra, mustRR("ns.evil.test 300 IN A 203.0.113.1"))
	req.Msg.SetEdns0(1232, true)
	if res := v.Validate(context.Background(), req); res.Status != Secure {
		t.Fatalf("status %s", res.Status)
	}
	if len(req.Msg.Ns) != 0 || len(req.Msg.Extra) != 1 || req.Msg.Extra[0].Header().Rrtype != dns.TypeOPT {
		t.Fatalf("unvalidated sections kept: ns %v extra %v", req.Msg.Ns, req.Msg.Extra)
	}
	// A negative answer keeps its validated SOA and proof.
	req = request(u, "nothere.example.com", dns.TypeA)
	n := len(req.Msg.Ns)
	if res := v.Validate(context.Background(), req); res.Status != Secure || len(req.Msg.Ns) != n {
		t.Fatalf("negative answer: %s, authority %d → %d", res.Status, n, len(req.Msg.Ns))
	}
}

func TestInsecureAnswers(t *testing.T) {
	u, com, ex := dnssectest.Example(t)
	// An insecure delegation proven by NSEC (in example.com) and by NSEC3
	// (in com); an opt-out span in the opt-out zone org.
	sub := u.AddChild(ex, "sub.example.com", false)
	sub.A("www.sub.example.com", "192.0.2.4")
	org := u.AddChild(u.Root, "org", true)
	org.NSEC3, org.OptOut = true, true
	oo := u.AddChild(org, "optout.org", false)
	oo.A("www.optout.org", "192.0.2.5")
	_ = com
	v := validatorFor(u)
	check(t, u, v, "www.unsigned.com", dns.TypeA, Insecure, EDENone, ReasonNoDS)
	check(t, u, v, "www.sub.example.com", dns.TypeA, Insecure, EDENone, ReasonNoDS)
	check(t, u, v, "www.optout.org", dns.TypeA, Insecure, EDENone, ReasonOptOut)
	// The state of the cut is cached: no further DS lookup.
	before := u.Count("unsigned.com", dns.TypeDS)
	check(t, u, v, "www.unsigned.com", dns.TypeAAAA, Insecure, EDENone, ReasonNoDS)
	if after := u.Count("unsigned.com", dns.TypeDS); after != before {
		t.Fatalf("DS lookups %d → %d: the insecure state was not cached", before, after)
	}
}

func TestInsecureByPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		dsAlg  uint8
		digest uint8
		reason string
		ede    int
	}{
		{"algorithm 5", dns.RSASHA1, dns.SHA256, ReasonUnsupportedAlgorithm, EDEUnsupportedAlgorithm},
		{"algorithm 7", dns.RSASHA1NSEC3SHA1, dns.SHA256, ReasonUnsupportedAlgorithm, EDEUnsupportedAlgorithm},
		{"algorithm 16", dns.ED448, dns.SHA256, ReasonUnsupportedAlgorithm, EDEUnsupportedAlgorithm},
		{"SHA-1 only", 0, dns.SHA1, ReasonUnsupportedDigest, EDEUnsupportedDigest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, com, _ := dnssectest.Example(t)
			z := u.Zones["example.com."]
			z.DSAlg, z.DSDigest = tc.dsAlg, tc.digest
			com.SetDS(z)
			v := validatorFor(u)
			check(t, u, v, "www.example.com", dns.TypeA, Insecure, tc.ede, tc.reason)
		})
	}
}

// rsaPublic builds an RSA public key in DNSKEY form with a modulus of bits
// bits (not a real key: the validator classifies it before verifying).
func rsaPublic(bits int) string {
	mod := make([]byte, bits/8)
	mod[0] = 0xC1
	for i := 1; i < len(mod); i++ {
		mod[i] = byte(i)
	}
	return base64.StdEncoding.EncodeToString(append([]byte{3, 1, 0, 1}, mod...))
}

func TestInsecureByKeySize(t *testing.T) {
	for _, bits := range []int{512, 8192} {
		t.Run(strconv.Itoa(bits), func(t *testing.T) {
			u, com, ex := dnssectest.Example(t)
			big := &dns.DNSKEY{Hdr: dnssectest.Hdr("example.com", dns.TypeDNSKEY, 3600), Flags: 257, Protocol: 3, Algorithm: dns.RSASHA256,
				PublicKey: rsaPublic(bits)}
			ex.Data[ex.Name][dns.TypeDNSKEY] = []dns.RR{big, ex.ZSK.Key}
			delete(com.Data[ex.Name], dns.TypeDS)
			ds := big.ToDS(dns.SHA256)
			ds.Hdr.Ttl = 3600
			com.Add(ds)
			v := validatorFor(u)
			check(t, u, v, "www.example.com", dns.TypeA, Insecure, EDEUnsupportedAlgorithm, ReasonUnsupportedKey)
		})
	}
}

func TestDSDigests(t *testing.T) {
	// A wrong SHA-1 DS next to a correct SHA-256 DS is ignored.
	u, com, ex := dnssectest.Example(t)
	wrong := ex.KSK.Key.ToDS(dns.SHA1)
	wrong.Digest = strings.Repeat("0", 40)
	com.Add(wrong)
	v := validatorFor(u)
	check(t, u, v, "www.example.com", dns.TypeA, Secure, EDENone, "")

	// A wrong SHA-256 DS alone is bogus (no DNSKEY matches the DS).
	u, com, ex = dnssectest.Example(t)
	delete(com.Data[ex.Name], dns.TypeDS)
	bad := ex.KSK.Key.ToDS(dns.SHA256)
	bad.Hdr.Ttl = 3600
	bad.Digest = strings.Repeat("A", 64)
	com.Add(bad)
	v = validatorFor(u)
	check(t, u, v, "www.example.com", dns.TypeA, Bogus, EDEDNSKEYMissing, FailNoDNSKEYMatch)
}

func TestBogusAnswers(t *testing.T) {
	type tamperFn func(u *dnssectest.Universe, com, ex *dnssectest.Zone) (string, uint16)
	for _, tc := range []struct {
		name   string
		setup  tamperFn
		ede    int
		reason string
	}{
		{"bad signature", func(u *dnssectest.Universe, _, _ *dnssectest.Zone) (string, uint16) {
			u.Tamper = func(q string, qt uint16, m *dns.Msg) {
				if qt == dns.TypeA {
					for _, rr := range m.Answer {
						if a, ok := rr.(*dns.A); ok {
							a.A = []byte{198, 51, 100, 66}
						}
					}
				}
			}
			return "www.example.com", dns.TypeA
		}, EDEBogus, FailBadSignature},
		{"expired", func(u *dnssectest.Universe, _, ex *dnssectest.Zone) (string, uint16) {
			ex.SigInc, ex.SigExp = testNow.Add(-10*24*time.Hour), testNow.Add(-2*time.Hour)
			return "www.example.com", dns.TypeA
		}, EDESignatureExpired, FailSignatureExpired},
		{"not yet valid", func(u *dnssectest.Universe, _, ex *dnssectest.Zone) (string, uint16) {
			ex.SigInc, ex.SigExp = testNow.Add(2*time.Hour), testNow.Add(10*24*time.Hour)
			return "www.example.com", dns.TypeA
		}, EDESignatureNotYetValid, FailSignatureNotValid},
		{"missing DNSKEY", func(u *dnssectest.Universe, _, ex *dnssectest.Zone) (string, uint16) {
			ex.Data[ex.Name][dns.TypeDNSKEY] = []dns.RR{ex.ZSK.Key}
			return "www.example.com", dns.TypeA
		}, EDEDNSKEYMissing, FailNoDNSKEYMatch},
		{"missing RRSIG", func(u *dnssectest.Universe, _, _ *dnssectest.Zone) (string, uint16) {
			u.Tamper = stripSigs(dns.TypeA)
			return "www.example.com", dns.TypeA
		}, EDERRSIGsMissing, FailNoRRSIG},
		{"no zone key bit", func(u *dnssectest.Universe, com, ex *dnssectest.Zone) (string, uint16) {
			ksk := *ex.KSK.Key
			ksk.Flags = dns.SEP // zone-key bit clear
			ex.Data[ex.Name][dns.TypeDNSKEY] = []dns.RR{&ksk, ex.ZSK.Key}
			delete(com.Data[ex.Name], dns.TypeDS)
			ds := ksk.ToDS(dns.SHA256)
			ds.Hdr.Ttl = 3600
			com.Add(ds)
			return "www.example.com", dns.TypeA
		}, EDENoZoneKeyBit, FailNoZoneKey},
		{"missing NSEC proof", func(u *dnssectest.Universe, _, _ *dnssectest.Zone) (string, uint16) {
			u.Tamper = func(q string, qt uint16, m *dns.Msg) {
				if q == "nothere.example.com." {
					m.Ns = keepTypes(m.Ns, dns.TypeSOA)
				}
			}
			return "nothere.example.com", dns.TypeA
		}, EDENSECMissing, FailNSEC},
		{"wildcard without no-closer proof", func(u *dnssectest.Universe, _, ex *dnssectest.Zone) (string, uint16) {
			ex.Add(mustRR("*.wild.example.com 300 IN A 192.0.2.7"))
			u.Tamper = func(q string, qt uint16, m *dns.Msg) {
				if q == "a.wild.example.com." {
					m.Ns = nil
				}
			}
			return "a.wild.example.com", dns.TypeA
		}, EDENSECMissing, FailNSEC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, com, ex := dnssectest.Example(t)
			name, qt := tc.setup(u, com, ex)
			v := validatorFor(u)
			check(t, u, v, name, qt, Bogus, tc.ede, tc.reason)
		})
	}
}

// stripSigs removes the RRSIGs of the Answer section covering typ.
func stripSigs(typ uint16) func(string, uint16, *dns.Msg) {
	return func(_ string, qt uint16, m *dns.Msg) {
		if qt != typ {
			return
		}
		var out []dns.RR
		for _, rr := range m.Answer {
			if s, ok := rr.(*dns.RRSIG); ok && s.TypeCovered == typ {
				continue
			}
			out = append(out, rr)
		}
		m.Answer = out
	}
}

func keepTypes(rrs []dns.RR, types ...uint16) []dns.RR {
	var out []dns.RR
	for _, rr := range rrs {
		t := rr.Header().Rrtype
		if s, ok := rr.(*dns.RRSIG); ok {
			t = s.TypeCovered
		}
		for _, k := range types {
			if t == k {
				out = append(out, rr)
			}
		}
	}
	return out
}

func TestSignerSuffixPitfall(t *testing.T) {
	// www.devil.com signed by evil.com: miekg's Verify only checks a string
	// suffix; the validator checks label boundaries itself.
	u := dnssectest.NewUniverse(t)
	com := u.AddChild(u.Root, "com", true)
	evil := u.AddChild(com, "evil.com", true)
	devil := u.AddChild(com, "devil.com", true)
	devil.A("www.devil.com", "192.0.2.1")
	rrs := []dns.RR{mustRR("www.devil.com 300 IN A 203.0.113.66")}
	sig := evil.Sign(rrs)
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		if q == "www.devil.com." && qt == dns.TypeA {
			m.Answer = append(dnssectest.CopyRRs(rrs), sig)
		}
	}
	v := validatorFor(u)
	res := check(t, u, v, "www.devil.com", dns.TypeA, Bogus, EDEBogus, "signer evil.com is not the zone")
	if res.Zone != "evil.com" {
		t.Fatalf("zone %q", res.Zone)
	}
}

func TestDNAME(t *testing.T) {
	u, _, ex := dnssectest.Example(t)
	ex.Add(mustRR("old.example.com 300 IN DNAME example.com"))
	v := validatorFor(u)
	answer := func(target string) *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion("www.old.example.com.", dns.TypeA)
		m.Response = true
		m.Answer = append(m.Answer, ex.WithSig(ex.Data["old.example.com."][dns.TypeDNAME])...)
		m.Answer = append(m.Answer, mustRR("www.old.example.com 300 IN CNAME "+target))
		m.Answer = append(m.Answer, ex.WithSig(ex.Data["www.example.com."][dns.TypeA])...)
		return m
	}
	req := &Request{Route: "test", Name: "www.old.example.com.", Type: dns.TypeA, Msg: answer("www.example.com."), Lookup: lookupOf(u),
		Now: testNow, TimeChecks: true}
	if res := v.Validate(context.Background(), req); res.Status != Secure {
		t.Fatalf("DNAME with the right CNAME: %+v", res)
	}
	req.Msg = answer("evil.example.net.")
	if res := v.Validate(context.Background(), req); res.Status != Bogus || res.EDE != EDEBogus {
		t.Fatalf("DNAME with a wrong CNAME: %+v", res)
	}
}

func TestChainLoop(t *testing.T) {
	// A DS answer for example.com signed by example.com itself: validating
	// it needs "example.com DS", the query being validated.
	u, _, ex := dnssectest.Example(t)
	ds := ex.KSK.Key.ToDS(dns.SHA256)
	ds.Hdr.Ttl = 3600
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		if q == "example.com." && qt == dns.TypeDS {
			m.Answer = []dns.RR{ds, ex.Sign([]dns.RR{ds})}
		}
	}
	v := validatorFor(u)
	req := request(u, "example.com", dns.TypeDS)
	u.Tamper = nil
	res := v.Validate(context.Background(), req)
	if res.Status != Bogus || res.Reason != FailChainLoop || res.EDE != EDEBogus {
		t.Fatalf("chain loop: %+v", res)
	}
}

func TestWalkCNAMEAndNotCut(t *testing.T) {
	// An unsigned answer below a CNAME met by the walk is bogus (the name
	// is no zone cut), like unsigned data inside a signed zone.
	u, _, ex := dnssectest.Example(t)
	ex.Add(mustRR("cn.example.com 300 IN CNAME www.example.com"))
	v := validatorFor(u)
	m := new(dns.Msg)
	m.SetQuestion("x.cn.example.com.", dns.TypeA)
	m.Answer = []dns.RR{mustRR("x.cn.example.com 300 IN A 203.0.113.9")}
	req := &Request{Route: "test", Name: "x.cn.example.com.", Type: dns.TypeA, Msg: m, Lookup: lookupOf(u), Now: testNow, TimeChecks: true}
	if res := v.Validate(context.Background(), req); res.Status != Bogus || res.EDE != EDERRSIGsMissing {
		t.Fatalf("unsigned data below a CNAME: %+v", res)
	}
}

func TestTimeChecks(t *testing.T) {
	u, _, ex := dnssectest.Example(t)
	ex.SigInc, ex.SigExp = testNow.Add(-10*24*time.Hour), testNow.Add(-2*time.Hour)
	v := validatorFor(u)
	// Suspended: the dates do not count, the verdict is indeterminate.
	req := request(u, "www.example.com", dns.TypeA)
	req.TimeChecks = false
	if res := v.Validate(context.Background(), req); res.Status != Indeterminate || res.Reason != ReasonTimeSuspended {
		t.Fatalf("suspended: %+v", res)
	}
	// Forged data is still bogus while suspended.
	u.Tamper = func(q string, qt uint16, m *dns.Msg) {
		for _, rr := range m.Answer {
			if a, ok := rr.(*dns.A); ok {
				a.A = []byte{203, 0, 113, 66}
			}
		}
	}
	req = request(u, "www.example.com", dns.TypeA)
	req.TimeChecks = false
	if res := v.Validate(context.Background(), req); res.Status != Bogus {
		t.Fatalf("forged while suspended: %+v", res)
	}
	u.Tamper = nil
	// Within the 1 h tolerance: valid.
	v = validatorFor(u)
	ex.SigInc, ex.SigExp = testNow.Add(-10*24*time.Hour), testNow.Add(-30*time.Minute)
	ex.ClearSigs()
	check(t, u, v, "www.example.com", dns.TypeA, Secure, EDENone, "")
}
