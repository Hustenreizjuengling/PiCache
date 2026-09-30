package dnssec

import (
	"context"
	"time"

	"github.com/miekg/dns"
)

// Bounds of one validation context (docs/ARCHITECTURE.md 7.6).
const (
	maxSigsPerRRset    = 8  // RRSIGs considered per RRset (the rest are ignored)
	maxKeysPerSig      = 4  // DNSKEYs tried per RRSIG (same key tag and algorithm)
	maxDNSKEYs         = 32 // records in a DNSKEY RRset
	maxDSRecords       = 8  // records in a DS RRset
	maxVerifications   = 16 // signature verifications per context
	maxFailedVerifies  = 2  // of them failed
	maxNSEC3Hashes     = 256
	maxProofRecords    = 16 // NSEC/NSEC3 records in one proof
	maxChainHops       = 8  // CNAME/DNAME hops validated
	maxChainLookups    = 32 // chain lookups per context (every step that is not a key-cache hit)
	timeTolerance      = time.Hour
	maxNSEC3Iterations = 50 // RFC 9276 3.2
	maxNSEC3Salt       = 64 // bytes
)

// rrset is one RRset of a section with the RRSIGs that cover it.
type rrset struct {
	name string // canonical owner
	typ  uint16
	rrs  []dns.RR
	sigs []*dns.RRSIG // at most maxSigsPerRRset
}

// groupRRsets splits a section into RRsets (owner, type; class IN) and
// attaches the RRSIGs covering each (the first maxSigsPerRRset). OPT
// records, RRSIGs covering nothing present and records of other classes are
// skipped. The order is the order of first appearance.
func groupRRsets(section []dns.RR) []*rrset {
	type key struct {
		name string
		typ  uint16
	}
	var out []*rrset
	idx := map[key]*rrset{}
	for _, rr := range section {
		h := rr.Header()
		if h.Rrtype == dns.TypeOPT || h.Rrtype == dns.TypeRRSIG || h.Class != dns.ClassINET {
			continue
		}
		k := key{canon(h.Name), h.Rrtype}
		s := idx[k]
		if s == nil {
			s = &rrset{name: k.name, typ: k.typ}
			idx[k] = s
			out = append(out, s)
		}
		s.rrs = append(s.rrs, rr)
	}
	for _, rr := range section {
		sig, ok := rr.(*dns.RRSIG)
		if !ok || sig.Hdr.Class != dns.ClassINET {
			continue
		}
		if s := idx[key{canon(sig.Hdr.Name), sig.TypeCovered}]; s != nil && len(s.sigs) < maxSigsPerRRset {
			s.sigs = append(s.sigs, sig)
		}
	}
	return out
}

// find returns the RRset of name and type (nil if none).
func find(sets []*rrset, name string, typ uint16) *rrset {
	name = canon(name)
	for _, s := range sets {
		if s.typ == typ && s.name == name {
			return s
		}
	}
	return nil
}

// key is a DNSKEY with its precomputed key tag.
type key struct {
	k   *dns.DNSKEY
	tag uint16
}

func newKey(k *dns.DNSKEY) key { return key{k: k, tag: k.KeyTag()} }

// budget counts the work of one validation context.
type budget struct {
	lookups  int
	verifies int
	failed   int
	hashes   int
}

// sctx is the part of a validation context the checks need: the budget,
// the clock and the verification semaphore.
type sctx struct {
	ctx        context.Context
	v          *Validator
	b          *budget
	now        time.Time
	timeChecks bool
	// onMismatch re-probes the upstream that answered the root DNSKEY
	// lookup (Reply.AnchorMismatch).
	onMismatch func()
}

// sigTime converts an RRSIG time (seconds modulo 2^32, RFC 4034 3.1.5) to
// the absolute time closest to now (serial number arithmetic, RFC 1982).
func sigTime(t uint32, now time.Time) time.Time {
	n := now.Unix()
	diff := int64(int32(t - uint32(n)))
	return time.Unix(n+diff, 0)
}

// verified describes the RRSIG that authenticated an RRset.
type verified struct {
	sig     *dns.RRSIG
	expires time.Time // absolute expiration
	origTTL uint32
}

// verifyRRset authenticates set with a key of zone: one valid RRSIG of any
// supported algorithm is enough (RFC 6840 5.11). An RRSIG counts only when
// its signer is the zone, the owner is at or below the zone, its labels are
// at most the owner's, a usable key has its key tag and algorithm and,
// while time checks are active, its validity period (±1 h) contains now.
// While time checks are suspended a signature that fails only on its dates
// counts as valid. On success the TTLs of the RRset are reduced to the
// RRSIG's original TTL and (time checks active) the time until its
// expiration (RFC 4035 5.3.3).
func (c *sctx) verifyRRset(set *rrset, zone string, keys []key) (verified, *failure) {
	if len(set.sigs) == 0 {
		return verified{}, fail(zone, FailNoRRSIG, EDERRSIGsMissing)
	}
	var expired, notYet, bad, wrongSigner bool
	var signer string
	for _, sig := range set.sigs {
		if !equalName(sig.SignerName, zone) || !below(zone, set.name) {
			wrongSigner, signer = true, sig.SignerName
			continue
		}
		if int(sig.Labels) > dns.CountLabel(set.name) || !algSupported(sig.Algorithm) {
			continue
		}
		inception, expiration := sigTime(sig.Inception, c.now), sigTime(sig.Expiration, c.now)
		if c.timeChecks {
			if c.now.After(expiration.Add(timeTolerance)) {
				expired = true
				continue
			}
			if c.now.Before(inception.Add(-timeTolerance)) {
				notYet = true
				continue
			}
		}
		tried := 0
		for _, k := range keys {
			if k.tag != sig.KeyTag || k.k.Algorithm != sig.Algorithm {
				continue
			}
			if tried == maxKeysPerSig {
				break
			}
			tried++
			ok, f := c.verifyOne(sig, k.k, set.rrs, zone)
			if f != nil {
				return verified{}, f
			}
			if ok {
				c.reduceTTLs(set, sig, expiration)
				return verified{sig: sig, expires: expiration, origTTL: sig.OrigTtl}, nil
			}
			bad = true
		}
		if tried == 0 {
			bad = true // no key of the zone has its key tag and algorithm
		}
	}
	switch {
	case expired:
		return verified{}, fail(zone, FailSignatureExpired, EDESignatureExpired)
	case notYet:
		return verified{}, fail(zone, FailSignatureNotValid, EDESignatureNotYetValid)
	case bad:
		return verified{}, fail(zone, FailBadSignature, EDEBogus)
	case wrongSigner:
		return verified{}, signerFail(signer)
	}
	// Only RRSIGs of unsupported algorithms or with too many labels.
	return verified{}, fail(zone, FailBadSignature, EDEBogus)
}

// verifyOne runs one signature verification within the budget (16 per
// context, 2 of them failed) under the global semaphore.
func (c *sctx) verifyOne(sig *dns.RRSIG, k *dns.DNSKEY, rrs []dns.RR, zone string) (bool, *failure) {
	if c.b.verifies >= maxVerifications {
		return false, limitFail(zone)
	}
	c.b.verifies++
	c.v.verifies.Add(1)
	select {
	case c.v.sem <- struct{}{}:
	case <-c.ctx.Done():
		return false, limitFail(zone)
	}
	err := sig.Verify(k, sameCase(rrs))
	<-c.v.sem
	if err == nil {
		return true, nil
	}
	c.b.failed++
	if c.b.failed > maxFailedVerifies {
		return false, limitFail(zone)
	}
	return false, nil
}

// sameCase returns rrs with owner names of one spelling: miekg's IsRRset
// compares them byte for byte, while an RRset's owners may differ in the
// case of ASCII letters (the signature covers the lower-cased form). The
// records are copied only when their spellings differ.
func sameCase(rrs []dns.RR) []dns.RR {
	if len(rrs) < 2 {
		return rrs
	}
	for _, rr := range rrs[1:] {
		if rr.Header().Name != rrs[0].Header().Name {
			out := make([]dns.RR, len(rrs))
			for i, r := range rrs {
				out[i] = dns.Copy(r)
				out[i].Header().Name = canon(r.Header().Name)
			}
			return out
		}
	}
	return rrs
}

// reduceTTLs caps the TTLs of an authenticated RRset (RFC 4035 5.3.3).
func (c *sctx) reduceTTLs(set *rrset, sig *dns.RRSIG, expiration time.Time) {
	limit := sig.OrigTtl
	if c.timeChecks {
		left := expiration.Sub(c.now)
		switch {
		case left <= 0:
			limit = 0
		case left < time.Duration(limit)*time.Second:
			limit = uint32(left / time.Second)
		}
	}
	for _, rr := range set.rrs {
		if h := rr.Header(); h.Ttl > limit {
			h.Ttl = limit
		}
	}
}

// minTTL returns the smallest TTL of an RRset.
func (s *rrset) minTTL() uint32 {
	ttl := ^uint32(0)
	for _, rr := range s.rrs {
		ttl = min(ttl, rr.Header().Ttl)
	}
	return ttl
}
