package dnssec

import (
	"errors"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// walkKind is the outcome of a walk.
type walkKind int

const (
	walkSecure        walkKind = iota // st: the keys of the deepest secure zone reached (the target when it is a zone cut)
	walkInsecure                      // st: an insecure cut at or above the target
	walkBogus                         // fail
	walkIndeterminate                 // degraded
	walkBlocked                       // block
	walkStopped                       // st: a CNAME or NXDOMAIN above the target ended the walk in this secure zone
)

type walkResult struct {
	kind     walkKind
	st       *zoneState
	fail     *failure
	degraded string
	block    any
	// checked: the client's own DNSKEY RRset (walk with answerKeys) was
	// checked against the zone's DS; expires is the earliest RRSIG
	// expiration involved.
	checked bool
	expires time.Time
}

// walk establishes the key state of target, or proves an insecure cut above
// it (docs/ARCHITECTURE.md 7.6, zone-cut discovery): it starts at the
// deepest ancestor with a known state for the route (the root is
// established from the anchors on first use) and asks, top-down, "L DS"
// for every label L from there down to the target. answerKeys is the
// client's own DNSKEY RRset of target: it is checked against target's DS
// instead of a DNSKEY lookup.
func (c *vctx) walk(target string, answerKeys *rrset) walkResult {
	target = canon(target)
	anc, st, stop := c.deepest(target)
	if stop != nil {
		return *stop
	}
	if !st.secure {
		return walkResult{kind: walkInsecure, st: st}
	}
	z := st
	route, maxTTL := c.req.Route, c.req.MaxTTL
	for _, l := range ancestorsBelow(anc, target) {
		if f := c.v.fails.get(cacheKey{route, l}, c.now); f != nil {
			return walkResult{kind: walkBogus, fail: f}
		}
		zone, keys := z.zone, z.keys
		r := c.step(dns.TypeDS, l, func(sc *sctx, lookup LookupFunc) stepResult {
			res := sc.runDS(lookup, l, zone, keys)
			sc.v.cacheStep(sc, route, l, res, maxTTL)
			return res
		})
		switch r.kind {
		case stepNotCut:
			continue
		case stepCNAME, stepNXDomain:
			return walkResult{kind: walkStopped, st: z}
		case stepSecureDS:
		default:
			return c.stepOutcome(l, r)
		}
		var kr stepResult
		if answerKeys != nil && l == target {
			kr = c.keysFromSet(answerKeys, l, r.ds, r.ttl, r.expires)
			c.v.cacheStep(&c.sctx, route, l, kr, maxTTL)
		} else {
			ds, ttl, exp := r.ds, r.ttl, r.expires
			kr = c.step(dns.TypeDNSKEY, l, func(sc *sctx, lookup LookupFunc) stepResult {
				res := sc.runDNSKEY(lookup, l, ds, ttl, exp)
				sc.v.cacheStep(sc, route, l, res, maxTTL)
				return res
			})
		}
		if kr.kind != stepKeys {
			return c.stepOutcome(l, kr)
		}
		z = &zoneState{zone: l, secure: true, keys: kr.keys}
		c.zones[l] = z
		if answerKeys != nil && l == target {
			return walkResult{kind: walkSecure, st: z, checked: true, expires: kr.expires}
		}
	}
	return walkResult{kind: walkSecure, st: z}
}

// stepOutcome converts a step result that ends the walk.
func (c *vctx) stepOutcome(name string, r stepResult) walkResult {
	switch r.kind {
	case stepInsecure:
		st := &zoneState{zone: canon(name), reason: r.reason, ede: r.ede, reportZone: r.reportZone}
		c.zones[st.zone] = st
		return walkResult{kind: walkInsecure, st: st}
	case stepIndeterminate:
		return walkResult{kind: walkIndeterminate, degraded: r.degraded}
	case stepBlocked:
		return walkResult{kind: walkBlocked, block: r.block}
	case stepBogus:
		return walkResult{kind: walkBogus, fail: r.fail}
	}
	return walkResult{kind: walkBogus, fail: fail(name, FailBadSignature, EDEBogus)}
}

// deepest returns the deepest ancestor of target (or target itself) with a
// known state: established in this context, in the key cache, else the
// root, established from the anchors (a failure or another outcome ends
// the walk: stop).
func (c *vctx) deepest(target string) (string, *zoneState, *walkResult) {
	for n := target; n != ""; n = parentName(n) {
		if st := c.zones[n]; st != nil {
			return n, st, nil
		}
		if st := c.v.keys.get(cacheKey{c.req.Route, n}, c.now); st != nil {
			c.zones[n] = st
			return n, st, nil
		}
	}
	route, maxTTL := c.req.Route, c.req.MaxTTL
	if f := c.v.fails.get(cacheKey{route, "."}, c.now); f != nil {
		return "", nil, &walkResult{kind: walkBogus, fail: f}
	}
	r := c.step(dns.TypeDNSKEY, ".", func(sc *sctx, lookup LookupFunc) stepResult {
		res := sc.runRoot(lookup)
		sc.v.cacheStep(sc, route, ".", res, maxTTL)
		return res
	})
	if r.kind != stepKeys {
		w := c.stepOutcome(".", r)
		return "", nil, &w
	}
	st := &zoneState{zone: ".", secure: true, keys: r.keys}
	c.zones["."] = st
	return ".", st, nil
}

// shortErr returns a short description of a lookup error for a failure
// phrase.
func shortErr(err error) string {
	s := err.Error()
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline") {
		return "timeout"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return strings.ToValidUTF8(s, "")
}

// lookupReply runs a chain lookup and turns every reply that is not an
// answer into a step result (ok false).
func (sc *sctx) lookupReply(lookup LookupFunc, name string, qtype uint16) (*dns.Msg, stepResult, bool) {
	k := stepKey{name: canon(name), qtype: qtype}
	if lookup == nil {
		return nil, lookupFailure(k, "no route"), false
	}
	rep, err := lookup(sc.ctx, k.name, qtype)
	switch {
	case errors.Is(err, ErrRateLimited):
		r := lookupFailure(k, ErrRateLimited.Error())
		r.fail.local = true
		return nil, r, false
	case err != nil:
		return nil, lookupFailure(k, shortErr(err)), false
	case rep.Block != nil:
		return nil, stepResult{kind: stepBlocked, block: rep.Block}, false
	case rep.Degraded != "":
		return nil, stepResult{kind: stepIndeterminate, degraded: rep.Degraded}, false
	}
	if p := replyProblem(rep.Msg); p != "" {
		return nil, lookupFailure(k, p), false
	}
	if qtype == dns.TypeDNSKEY && k.name == "." && rep.AnchorMismatch != nil {
		sc.onMismatch = rep.AnchorMismatch
	}
	return rep.Msg, stepResult{}, true
}

// runDS is the chain step "name DS" below the secure zone (its keys): a
// signed DS RRset (secure delegation, or insecure when no entry is
// supported), a signed CNAME (no zone cut), or a signed denial: an
// insecure delegation, no zone cut, an opt-out span (insecure) or NXDOMAIN.
// Anything else is bogus.
func (sc *sctx) runDS(lookup LookupFunc, name, zone string, keys []key) stepResult {
	m, r, ok := sc.lookupReply(lookup, name, dns.TypeDS)
	if !ok {
		return r
	}
	ans, auth := groupRRsets(m.Answer), groupRRsets(m.Ns)
	if ds := find(ans, name, dns.TypeDS); ds != nil && m.Rcode == dns.RcodeSuccess {
		if len(ds.rrs) > maxDSRecords {
			return stepResult{kind: stepBogus, fail: limitFail(name)}
		}
		v, f := sc.verifyRRset(ds, zone, keys)
		if f != nil {
			return stepResult{kind: stepBogus, fail: f}
		}
		kept, reason, ede := filterDS(ds.rrs)
		ttl := min(ds.minTTL(), v.origTTL)
		if len(kept) == 0 {
			return stepResult{kind: stepInsecure, reason: reason, ede: ede, ttl: ttl, expires: v.expires}
		}
		return stepResult{kind: stepSecureDS, ds: kept, ttl: ttl, expires: v.expires}
	}
	if cn := find(ans, name, dns.TypeCNAME); cn != nil && m.Rcode == dns.RcodeSuccess {
		if _, f := sc.verifyRRset(cn, zone, keys); f != nil {
			return stepResult{kind: stepBogus, fail: f}
		}
		return stepResult{kind: stepCNAME}
	}
	d, f := sc.newDenial(auth, zone, keys)
	if f != nil {
		return stepResult{kind: stepBogus, fail: f}
	}
	if len(d.nsec) == 0 && len(d.nsec3) == 0 {
		return stepResult{kind: stepBogus, fail: fail(zone, FailNSEC, EDENSECMissing)}
	}
	if d.highParams {
		return stepResult{kind: stepInsecure, reason: ReasonNSEC3Iterations, ede: EDEUnsupportedNSEC3Params,
			reportZone: canon(zone), ttl: d.ttl, expires: d.expires}
	}
	if m.Rcode == dns.RcodeNameError {
		pr, f := d.nxdomain(name)
		switch {
		case f != nil:
			return stepResult{kind: stepBogus, fail: f}
		case pr == proofOK:
			return stepResult{kind: stepNXDomain}
		case pr == proofOptOut:
			return stepResult{kind: stepInsecure, reason: ReasonOptOut, ede: EDENone, ttl: d.ttl, expires: d.expires}
		}
		return stepResult{kind: stepBogus, fail: fail(zone, FailNSEC, EDENSECMissing)}
	}
	pr, f := d.delegation(name)
	if f != nil {
		return stepResult{kind: stepBogus, fail: f}
	}
	switch pr {
	case proofInsecure:
		return stepResult{kind: stepInsecure, reason: ReasonNoDS, ede: EDENone, ttl: d.ttl, expires: d.expires}
	case proofOptOut:
		return stepResult{kind: stepInsecure, reason: ReasonOptOut, ede: EDENone, ttl: d.ttl, expires: d.expires}
	case proofNotCut:
		return stepResult{kind: stepNotCut}
	}
	return stepResult{kind: stepBogus, fail: fail(zone, FailNSEC, EDENSECMissing)}
}

// filterDS keeps the DS records with a supported algorithm and digest type
// (SHA-1 never counts). With none kept, the reason is the algorithms when
// no record has a supported one, else the digests.
func filterDS(rrs []dns.RR) (kept []*dns.DS, reason string, ede int) {
	algOK := false
	for _, rr := range rrs {
		ds, ok := rr.(*dns.DS)
		if !ok {
			continue
		}
		if algSupported(ds.Algorithm) {
			algOK = true
			if digestSupported(ds.DigestType) {
				kept = append(kept, ds)
			}
		}
	}
	switch {
	case len(kept) > 0:
		return kept, "", EDENone
	case !algOK:
		return nil, ReasonUnsupportedAlgorithm, EDEUnsupportedAlgorithm
	}
	return nil, ReasonUnsupportedDigest, EDEUnsupportedDigest
}

// runDNSKEY is the chain step "name DNSKEY" of a zone whose DS entries
// (ds; their TTL and RRSIG expiration) are known.
func (sc *sctx) runDNSKEY(lookup LookupFunc, name string, ds []*dns.DS, ttl uint32, expires time.Time) stepResult {
	m, r, ok := sc.lookupReply(lookup, name, dns.TypeDNSKEY)
	if !ok {
		return r
	}
	set := find(groupRRsets(m.Answer), name, dns.TypeDNSKEY)
	if set == nil || m.Rcode != dns.RcodeSuccess {
		return lookupFailure(stepKey{name: canon(name), qtype: dns.TypeDNSKEY}, "no DNSKEY records")
	}
	return sc.keysFromSet(set, name, ds, ttl, expires)
}

// keysFromSet checks a zone's DNSKEY RRset against its DS entries: a key
// that matches a DS (digest over owner and RDATA; algorithm and key tag
// equal), is supported, has the zone-key flag and no REVOKE flag must sign
// the set. Every DS-matched key unsupported: insecure (EDE 1); no match:
// bogus (EDE 9). The zone's keys are then every usable key of the set.
func (sc *sctx) keysFromSet(set *rrset, zone string, ds []*dns.DS, ttl uint32, expires time.Time) stepResult {
	if len(set.rrs) > maxDNSKEYs {
		return stepResult{kind: stepBogus, fail: limitFail(zone)}
	}
	var all, signing []key
	unsupported, noZone, matched := 0, 0, 0
	for _, rr := range set.rrs {
		k, ok := rr.(*dns.DNSKEY)
		if !ok {
			continue
		}
		kk := newKey(k)
		all = append(all, kk)
		if !matchesDS(kk, ds) {
			continue
		}
		matched++
		switch {
		case !keySupported(k):
			unsupported++
		case k.Flags&dns.ZONE == 0 || k.Protocol != 3:
			noZone++
		case k.Flags&dns.REVOKE != 0:
		default:
			signing = append(signing, kk)
		}
	}
	switch {
	case matched == 0:
		return stepResult{kind: stepBogus, fail: fail(zone, FailNoDNSKEYMatch, EDEDNSKEYMissing)}
	case len(signing) == 0 && noZone > 0:
		return stepResult{kind: stepBogus, fail: fail(zone, FailNoZoneKey, EDENoZoneKeyBit)}
	case len(signing) == 0 && unsupported == matched:
		return stepResult{kind: stepInsecure, reason: ReasonUnsupportedKey, ede: EDEUnsupportedAlgorithm, ttl: ttl, expires: expires}
	case len(signing) == 0:
		return stepResult{kind: stepBogus, fail: fail(zone, FailNoDNSKEYMatch, EDEDNSKEYMissing)}
	}
	v, f := sc.verifyRRset(set, zone, signing)
	if f != nil {
		return stepResult{kind: stepBogus, fail: f}
	}
	return stepResult{kind: stepKeys, keys: usableKeys(all), ttl: min(ttl, set.minTTL(), v.origTTL), expires: earliest(expires, v.expires)}
}

// matchesDS reports whether k matches one of the DS entries.
func matchesDS(k key, ds []*dns.DS) bool {
	for _, d := range ds {
		if d.KeyTag != k.tag || d.Algorithm != k.k.Algorithm {
			continue
		}
		if got := k.k.ToDS(d.DigestType); got != nil && strings.EqualFold(got.Digest, d.Digest) {
			return true
		}
	}
	return false
}

// usableKeys returns the keys that may sign the zone's data.
func usableKeys(all []key) []key {
	out := make([]key, 0, len(all))
	for _, k := range all {
		if usableKey(k.k) {
			out = append(out, k)
		}
	}
	return out
}

func earliest(a, b time.Time) time.Time {
	switch {
	case a.IsZero():
		return b
	case b.IsZero() || a.Before(b):
		return a
	}
	return b
}

// runRoot is the chain step ". DNSKEY": the root DNSKEY RRset against the
// anchors.
func (sc *sctx) runRoot(lookup LookupFunc) stepResult {
	m, r, ok := sc.lookupReply(lookup, ".", dns.TypeDNSKEY)
	if !ok {
		return r
	}
	set := find(groupRRsets(m.Answer), ".", dns.TypeDNSKEY)
	if set == nil || m.Rcode != dns.RcodeSuccess {
		return lookupFailure(stepKey{name: ".", qtype: dns.TypeDNSKEY}, "no DNSKEY records")
	}
	return sc.rootKeys(set)
}

// rootKeys authenticates the root DNSKEY RRset: an RRSIG of an anchored key
// must verify (no anchored key: bogus, EDE 9, and the answering upstream
// is re-probed). A SEP key that matches no anchor marks a new root key,
// only while time checks are active (a replayed old key set must not).
func (sc *sctx) rootKeys(set *rrset) stepResult {
	if len(set.rrs) > maxDNSKEYs {
		return stepResult{kind: stepBogus, fail: limitFail(".")}
	}
	var all, trusted []key
	for _, rr := range set.rrs {
		if k, ok := rr.(*dns.DNSKEY); ok {
			kk := newKey(k)
			all = append(all, kk)
			if anchored(k, sc.v.anchors) {
				trusted = append(trusted, kk)
			}
		}
	}
	if len(trusted) == 0 {
		if sc.onMismatch != nil {
			sc.onMismatch()
		}
		return stepResult{kind: stepBogus, fail: fail(".", FailNoDNSKEYMatch, EDEDNSKEYMissing)}
	}
	v, f := sc.verifyRRset(set, ".", trusted)
	if f != nil {
		return stepResult{kind: stepBogus, fail: f}
	}
	if sc.timeChecks {
		sc.v.checkNewRootKey(all)
	}
	return stepResult{kind: stepKeys, keys: usableKeys(all), ttl: min(set.minTTL(), v.origTTL), expires: v.expires}
}

// checkNewRootKey marks a new root key: a SEP key (zone-key and SEP flags,
// not revoked, supported) of an authenticated root DNSKEY RRset that
// matches no anchor.
func (v *Validator) checkNewRootKey(all []key) bool {
	for _, k := range all {
		f := k.k.Flags
		if f&dns.ZONE != 0 && f&dns.SEP != 0 && f&dns.REVOKE == 0 && keySupported(k.k) && !anchored(k.k, v.anchors) {
			if !v.newRootKey.Swap(true) && v.cfg.NewRootKey != nil {
				v.cfg.NewRootKey()
			}
			return true
		}
	}
	return false
}
