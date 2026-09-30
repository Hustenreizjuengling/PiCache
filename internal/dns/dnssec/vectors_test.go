package dnssec

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// readZone parses a zone file of testdata (presentation format).
func readZone(t *testing.T, name string) []dns.RR {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zp := dns.NewZoneParser(f, "", name)
	var out []dns.RR
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		out = append(out, rr)
	}
	if err := zp.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestSignatureVectors verifies the RFC 5702, 6605 and 8080 examples
// through verifyRRset (RSA/SHA-512, ECDSA P-256/P-384, Ed25519) at a time
// inside their validity; the 512-bit RSA/SHA-256 key and Ed448 are
// unsupported by the policy, and the DS records match their keys.
func TestSignatureVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/rfc-signatures.zone")
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, block := range strings.Split(string(raw), "\n\n") {
		verified += verifyBlock(t, block)
	}
	if verified != 5 {
		t.Fatalf("%d vectors verified, want 5", verified)
	}
}

// verifyBlock verifies the RRSIGs of one example of rfc-signatures.zone and
// checks its DS records; it returns the verified RRSIGs.
func verifyBlock(t *testing.T, block string) int {
	zp := dns.NewZoneParser(strings.NewReader(block), "", "rfc-signatures.zone")
	var keys []*dns.DNSKEY
	var sigs []*dns.RRSIG
	var ds []*dns.DS
	data := map[string][]dns.RR{}
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		switch v := rr.(type) {
		case *dns.DNSKEY:
			keys = append(keys, v)
		case *dns.RRSIG:
			sigs = append(sigs, v)
		case *dns.DS:
			ds = append(ds, v)
		default:
			k := canon(v.Header().Name) + "/" + dns.TypeToString[v.Header().Rrtype]
			data[k] = append(data[k], v)
		}
	}
	verified := 0
	for _, sig := range sigs {
		var dk *dns.DNSKEY
		for _, k := range keys {
			if k.KeyTag() == sig.KeyTag && k.Algorithm == sig.Algorithm {
				dk = k
			}
		}
		if dk == nil {
			t.Fatalf("no key for %s", sig)
		}
		set := &rrset{name: canon(sig.Hdr.Name), typ: sig.TypeCovered, rrs: data[canon(sig.Hdr.Name)+"/"+dns.TypeToString[sig.TypeCovered]],
			sigs: []*dns.RRSIG{sig}}
		inception := sigTime(sig.Inception, time.Unix(int64(sig.Inception), 0))
		c := &sctx{ctx: context.Background(), v: New(Config{}), b: &budget{}, now: inception.Add(24 * time.Hour), timeChecks: true}
		supported := keySupported(dk)
		if want := !(sig.Algorithm == dns.RSASHA256); supported != want {
			t.Errorf("key %d (algorithm %d): supported %v", dk.KeyTag(), dk.Algorithm, supported)
		}
		if !supported {
			continue
		}
		if _, f := c.verifyRRset(set, sig.SignerName, []key{newKey(dk)}); f != nil {
			t.Errorf("algorithm %d, key %d: %s", sig.Algorithm, sig.KeyTag, f.text)
			continue
		}
		verified++
	}
	if err := zp.Err(); err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		matched := false
		for _, k := range keys {
			if k.Algorithm == d.Algorithm && matchesDS(newKey(k), []*dns.DS{d}) {
				matched = true
			}
		}
		if !matched {
			t.Errorf("DS %d matches no key", d.KeyTag)
		}
		if want := d.Algorithm != dns.ED448; algSupported(d.Algorithm) != want {
			t.Errorf("DS %d: algorithm %d supported %v", d.KeyTag, d.Algorithm, !want)
		}
	}
	return verified
}

// TestRootAnchors checks the recorded root DNSKEY RRset (testdata, the
// date in the file) against the built-in anchors at a time inside its
// validity: it holds KSK-2017 (20326) and KSK-2024 (38696), both match
// their digests, the RRSIG by 20326 verifies, and the recorded SOA passes
// the probe check. With the REVOKE flag set, 20326 no longer matches.
func TestRootAnchors(t *testing.T) {
	rrs := readZone(t, "root-2026-09-29.zone")
	dnskey, soa := new(dns.Msg), new(dns.Msg)
	dnskey.SetQuestion(".", dns.TypeDNSKEY)
	soa.SetQuestion(".", dns.TypeSOA)
	for _, rr := range rrs {
		t := rr.Header().Rrtype
		if s, ok := rr.(*dns.RRSIG); ok {
			t = s.TypeCovered
		}
		if t == dns.TypeSOA {
			soa.Answer = append(soa.Answer, rr)
		} else {
			dnskey.Answer = append(dnskey.Answer, rr)
		}
	}
	anchors := RootAnchors()
	found := map[uint16]bool{}
	for _, rr := range dnskey.Answer {
		k, ok := rr.(*dns.DNSKEY)
		if !ok || k.Flags&dns.SEP == 0 {
			continue
		}
		if !anchored(k, anchors) {
			t.Errorf("key %d does not match an anchor", k.KeyTag())
		}
		found[k.KeyTag()] = true
		if k.KeyTag() == 20326 {
			revoked := *k
			revoked.Flags |= dns.REVOKE
			if anchored(&revoked, anchors) {
				t.Error("KSK-2017 with the REVOKE flag still matches")
			}
		}
	}
	if !found[20326] || !found[38696] {
		t.Fatalf("root KSKs found: %v", found)
	}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	v := New(Config{})
	check := v.CheckRoot(context.Background(), dnskey, soa, now)
	if check.State != StateCapable || !check.TimeOK || check.NewRootKey {
		t.Fatalf("probe check: %+v", check)
	}
	// A year later the RRSIG is outside its validity period.
	if check := v.CheckRoot(context.Background(), dnskey, soa, now.AddDate(1, 0, 0)); check.State != StateCapable || check.TimeOK {
		t.Fatalf("probe check a year later: %+v", check)
	}
	// Validated as a chain step: the root's keys.
	set := find(groupRRsets(dnskey.Answer), ".", dns.TypeDNSKEY)
	c := &sctx{ctx: context.Background(), v: v, b: &budget{}, now: now, timeChecks: true}
	if r := c.rootKeys(set); r.kind != stepKeys || len(r.keys) != 4 {
		t.Fatalf("root keys: %+v", r)
	}
	// A key set without the anchored keys: anchor mismatch.
	var zsks []dns.RR
	for _, rr := range dnskey.Answer {
		if k, ok := rr.(*dns.DNSKEY); !ok || k.Flags&dns.SEP == 0 {
			zsks = append(zsks, rr)
		}
	}
	m := new(dns.Msg)
	m.Answer = zsks
	if check := v.CheckRoot(context.Background(), m, soa, now); check.State != StateAnchorMismatch {
		t.Fatalf("without the KSKs: %+v", check)
	}
	if check := v.CheckRoot(context.Background(), new(dns.Msg), soa, now); check.State != StateNoDNSSEC {
		t.Fatalf("empty answer: %+v", check)
	}
}

// nsec3Vectors returns the NSEC3 records of RFC 5155 Appendix A and the
// hashes it lists.
func nsec3Vectors(t *testing.T) ([]*dns.NSEC3, map[string]string) {
	t.Helper()
	var recs []*dns.NSEC3
	for _, rr := range readZone(t, "rfc5155.zone") {
		recs = append(recs, rr.(*dns.NSEC3))
	}
	hashes := map[string]string{}
	f, err := os.Open("testdata/rfc5155.zone")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if name, hash, ok := strings.Cut(strings.TrimPrefix(sc.Text(), "; H "), " = "); ok && strings.HasPrefix(sc.Text(), "; H ") {
			hashes[name] = hash
		}
	}
	return recs, hashes
}

func TestNSEC3Hashes(t *testing.T) {
	_, hashes := nsec3Vectors(t)
	if len(hashes) != 16 {
		t.Fatalf("%d hashes", len(hashes))
	}
	d := &denial{c: &sctx{b: &budget{}}, zone: "example.", hashes: map[hashKey]string{}}
	r := &dns.NSEC3{Hash: dns.SHA1, Iterations: 12, Salt: "aabbccdd"}
	for name, want := range hashes {
		got, f := d.hash(name+".", r)
		if f != nil || !strings.EqualFold(got, want) {
			t.Errorf("H(%s) = %s, want %s", name, got, want)
		}
	}
}

// TestNSEC3Proofs runs the proofs of the responses of RFC 5155 Appendix B
// with their NSEC3 records (signatures are not part of this test: the
// example zone is signed with algorithm 7, which PiCache does not
// support). Every span of the example zone carries the opt-out flag, so a
// next closer name covered by it gives an insecure result (§1.3).
func TestNSEC3Proofs(t *testing.T) {
	recs, _ := nsec3Vectors(t)
	byOwner := map[string]*dns.NSEC3{}
	for _, r := range recs {
		byOwner[strings.SplitN(r.Hdr.Name, ".", 2)[0]] = r
	}
	proof := func(owners ...string) *denial {
		d := &denial{c: &sctx{b: &budget{}}, zone: "example.", hashes: map[hashKey]string{}}
		for _, o := range owners {
			d.nsec3 = append(d.nsec3, byOwner[o])
		}
		return d
	}
	for _, tc := range []struct {
		name string
		run  func() (proofResult, *failure)
		want proofResult
	}{
		{"B.1 name error (next closer name in an opt-out span)", func() (proofResult, *failure) {
			return proof("0p9mhaveqvm6t7vbl5lop2u3t2rp3tom", "b4um86eghhds6nea196smvmlo4ors995",
				"35mthgpgcu1qg68fab165klnsnk3dpvl").nxdomain("a.c.x.w.example.")
		}, proofOptOut},
		{"B.1 without the wildcard record", func() (proofResult, *failure) {
			return proof("0p9mhaveqvm6t7vbl5lop2u3t2rp3tom", "b4um86eghhds6nea196smvmlo4ors995").nxdomain("a.c.x.w.example.")
		}, proofOptOut},
		{"B.2 no data", func() (proofResult, *failure) {
			return proof("2t7b4g4vsa5smi47k61mv5bv1a22bojr").nodata("ns1.example.", dns.TypeMX)
		}, proofOK},
		{"B.2 the type exists", func() (proofResult, *failure) {
			return proof("2t7b4g4vsa5smi47k61mv5bv1a22bojr").nodata("ns1.example.", dns.TypeA)
		}, proofFailed},
		{"B.2.1 empty non-terminal", func() (proofResult, *failure) {
			return proof("ji6neoaepv8b5o6k4ev33abha8ht9fgc").nodata("y.w.example.", dns.TypeA)
		}, proofOK},
		{"B.3 opt-out delegation", func() (proofResult, *failure) {
			return proof("35mthgpgcu1qg68fab165klnsnk3dpvl", "0p9mhaveqvm6t7vbl5lop2u3t2rp3tom").delegation("c.example.")
		}, proofOptOut},
		{"B.3 the secure delegation a.example has DS", func() (proofResult, *failure) {
			return proof("35mthgpgcu1qg68fab165klnsnk3dpvl").delegation("a.example.")
		}, proofFailed},
		{"B.4 wildcard expansion (next closer name in an opt-out span)", func() (proofResult, *failure) {
			return proof("q04jkcevqvmu85r014c7dkba38o0ji5r").wildcard("a.z.w.example.", 2)
		}, proofOptOut},
		{"B.5 wildcard no data", func() (proofResult, *failure) {
			return proof("k8udemvp1j2f7eg6jebps17vp3n8i58h", "q04jkcevqvmu85r014c7dkba38o0ji5r",
				"r53bq7cc2uvmubfu5ocmm6pers9tk9en").nodata("a.z.w.example.", dns.TypeAAAA)
		}, proofOK},
		{"B.5 the wildcard has the type", func() (proofResult, *failure) {
			return proof("k8udemvp1j2f7eg6jebps17vp3n8i58h", "q04jkcevqvmu85r014c7dkba38o0ji5r",
				"r53bq7cc2uvmubfu5ocmm6pers9tk9en").nodata("a.z.w.example.", dns.TypeMX)
		}, proofFailed},
		{"B.6 DS from the child's apex", func() (proofResult, *failure) {
			return proof("0p9mhaveqvm6t7vbl5lop2u3t2rp3tom").nodata("example.", dns.TypeDS)
		}, proofFailed},
	} {
		got, f := tc.run()
		if f != nil || got != tc.want {
			t.Errorf("%s: %v %v, want %v", tc.name, got, f, tc.want)
		}
	}
}
