package dnssec

import (
	"time"

	"github.com/miekg/dns"
)

// scrub keeps in the Answer section only the answer to (qname, qtype),
// class IN: following the chain from qname (at most maxChainHops hops), at
// each chain name the data of qtype (any type for ANY) or else its CNAME
// (a DNAME's synthesised CNAME included), the closest DNAME above it, and
// the RRSIGs covering these. Every other record (an RRset of another zone
// or class that an upstream or someone on the path appended) is removed,
// so it neither reaches the client nor changes the verdict.
func scrub(m *dns.Msg, qname string, qtype uint16) {
	type key struct {
		name string
		typ  uint16
	}
	ans := groupRRsets(m.Answer)
	keep := map[key]bool{}
	name := canon(qname)
	seen := map[string]bool{}
	for range maxChainHops + 1 {
		if seen[name] {
			break
		}
		seen[name] = true
		d := dnameAbove(ans, name)
		if d != nil {
			keep[key{d.name, dns.TypeDNAME}] = true
		}
		found := false
		for _, s := range ans {
			if s.name == name && (s.typ == qtype || qtype == dns.TypeANY) {
				keep[key{s.name, s.typ}] = true
				found = true
			}
		}
		if found {
			break
		}
		if cn := find(ans, name, dns.TypeCNAME); cn != nil {
			keep[key{name, dns.TypeCNAME}] = true
			if t, ok := cn.rrs[0].(*dns.CNAME); ok {
				name = canon(t.Target)
				continue
			}
			break
		}
		if d != nil {
			if t, ok := substitute(d, name); ok {
				name = t
				continue
			}
		}
		break
	}
	out := m.Answer[:0]
	for _, rr := range m.Answer {
		h := rr.Header()
		typ := h.Rrtype
		if sig, ok := rr.(*dns.RRSIG); ok {
			typ = sig.TypeCovered
		}
		if h.Class == dns.ClassINET && keep[key{canon(h.Name), typ}] {
			out = append(out, rr)
		}
	}
	m.Answer = out
}

// answer validates the answer shapes of docs/ARCHITECTURE.md 7.6: every
// RRset of the (scrubbed) Answer section in its own zone (CNAME chains
// across zones, DNAMEs with their synthesised CNAMEs, wildcard expansions
// with their no-closer-match proof), and for a negative answer at the end
// of the chain the denial proof. The worst part decides. A secure answer
// loses the unvalidated RRsets of its Authority and Additional sections.
func (c *vctx) answer() Result {
	m := c.req.Msg
	qname, qtype := canon(c.req.Name), c.req.Type
	ans, auth := groupRRsets(m.Answer), groupRRsets(m.Ns)
	hops := 0
	for _, s := range ans {
		if s.typ == dns.TypeCNAME || s.typ == dns.TypeDNAME {
			hops++
		}
	}
	if hops > maxChainHops {
		return limitFail(qname).result()
	}
	var parts []Result
	dnames := map[*rrset]Result{}
	for _, s := range ans {
		if s.typ == dns.TypeDNAME {
			r := c.rrsetPart(s, auth)
			dnames[s] = r
			parts = append(parts, r)
		}
	}
	for _, s := range ans {
		switch {
		case s.typ == dns.TypeDNAME:
			continue
		case s.typ == dns.TypeCNAME && len(s.sigs) == 0:
			if d := dnameAbove(ans, s.name); d != nil {
				parts = append(parts, c.synthesised(s, d, dnames[d]))
				continue
			}
		}
		parts = append(parts, c.rrsetPart(s, auth))
	}
	if final, positive := chainEnd(ans, qname, qtype); !positive || m.Rcode != dns.RcodeSuccess {
		parts = append(parts, c.denialPart(final, qtype, m.Rcode == dns.RcodeNameError, auth))
	}
	res := combine(parts)
	if res.Status == Secure {
		c.trim(m)
	}
	return res
}

// combine returns the worst part (the first of equally bad ones, the
// last of secure ones: the zone of the final data); a part ended by an
// upstream block wins. A secure result expires with its earliest part.
func combine(parts []Result) Result {
	if len(parts) == 0 {
		return Result{Status: Bogus, Reason: FailNSEC, EDE: EDENSECMissing}
	}
	w := parts[0]
	var exp time.Time
	for i, p := range parts {
		if p.Block != nil {
			return Result{Block: p.Block, EDE: EDENone}
		}
		if i > 0 && (rank(p.Status) > rank(w.Status) || p.Status == Secure && w.Status == Secure) {
			w = p
		}
		if p.Status == Secure {
			exp = earliest(exp, p.Expires)
		}
	}
	if w.Status == Secure {
		w.Expires = exp
	}
	return w
}

// chainEnd follows the CNAME (and DNAME) chain of qname through the answer
// (at most maxChainHops hops) and reports the last name and whether the
// answer holds data of qtype there (a CNAME query is answered by the
// CNAME itself; ANY by any RRset).
func chainEnd(ans []*rrset, qname string, qtype uint16) (string, bool) {
	name := qname
	seen := map[string]bool{}
	for range maxChainHops + 1 {
		if seen[name] {
			break
		}
		seen[name] = true
		for _, s := range ans {
			if s.name == name && (s.typ == qtype || qtype == dns.TypeANY) {
				return name, true
			}
		}
		if cn := find(ans, name, dns.TypeCNAME); cn != nil {
			if t, ok := cn.rrs[0].(*dns.CNAME); ok {
				name = canon(t.Target)
				continue
			}
		}
		if d := dnameAbove(ans, name); d != nil {
			if t, ok := substitute(d, name); ok {
				name = t
				continue
			}
		}
		break
	}
	return name, false
}

// dnameAbove returns the DNAME RRset of the answer whose owner is the
// closest proper ancestor of name (nil if none).
func dnameAbove(ans []*rrset, name string) *rrset {
	var best *rrset
	for _, s := range ans {
		if s.typ == dns.TypeDNAME && strictlyBelow(s.name, name) &&
			(best == nil || dns.CountLabel(s.name) > dns.CountLabel(best.name)) {
			best = s
		}
	}
	return best
}

// substitute returns the name a DNAME RRset maps name (a name strictly
// below its owner) to (RFC 6672 2.2): the owner's suffix replaced by the
// target; a root owner keeps the whole name as the prefix. False when the
// result is not a valid name.
func substitute(d *rrset, name string) (string, bool) {
	t, ok := d.rrs[0].(*dns.DNAME)
	if !ok {
		return "", false
	}
	name = canon(name)
	owner := dns.CountLabel(d.name)
	idx := dns.Split(name)
	if owner >= len(idx) || !strictlyBelow(d.name, name) {
		return "", false
	}
	prefix := name // the owner is the root: every label is kept
	if owner > 0 {
		prefix = name[:idx[len(idx)-owner]]
	}
	out := prefix
	if target := canon(t.Target); target != "." {
		out += target
	}
	if _, ok := dns.IsDomainName(out); !ok {
		return "", false
	}
	return out, true
}

// synthesised judges an unsigned CNAME below a DNAME of the answer: it is
// accepted (with the DNAME's verdict) only when it equals the substitution.
func (c *vctx) synthesised(cn, d *rrset, dname Result) Result {
	want, ok := substitute(d, cn.name)
	got, isCNAME := cn.rrs[0].(*dns.CNAME)
	if ok && isCNAME && len(cn.rrs) == 1 && equalName(got.Target, want) {
		return dname
	}
	if dname.Status != Secure {
		return dname
	}
	return fail(dname.Zone, FailBadSignature, EDEBogus).result()
}

// insecureResult is the verdict of a zone state below an insecure cut.
func insecureResult(st *zoneState) Result {
	zone := st.zone
	if st.reportZone != "" {
		zone = st.reportZone
	}
	return Result{Status: Insecure, Zone: display(zone), Reason: st.reason, EDE: st.ede}
}

// establish returns the keys of zone (the signer of an RRset), or the
// verdict when the walk ends otherwise; a zone that the walk does not
// reach as a zone cut makes the signer fail.
func (c *vctx) establish(zone string, answerKeys *rrset) (*zoneState, walkResult, *Result) {
	w := c.walk(zone, answerKeys)
	if w.kind == walkSecure && equalName(w.st.zone, zone) {
		return w.st, w, nil
	}
	r := w.result()
	if w.kind == walkSecure || w.kind == walkStopped {
		r = signerFail(zone).result()
	}
	return nil, w, &r
}

// unsigned judges data without RRSIGs at owner: the walk down its owner
// name must prove an insecure cut; otherwise the data is bogus (text, ede).
func (c *vctx) unsigned(owner, text string, ede int) Result {
	w := c.walk(owner, nil)
	if w.kind == walkSecure || w.kind == walkStopped {
		return fail(w.st.zone, text, ede).result()
	}
	return w.result()
}

// rrsetPart validates one RRset of the Answer section.
func (c *vctx) rrsetPart(s *rrset, auth []*rrset) Result {
	if len(s.sigs) == 0 {
		return c.unsigned(s.name, FailNoRRSIG, EDERRSIGsMissing)
	}
	signer := canon(s.sigs[0].SignerName)
	if !below(signer, s.name) {
		return signerFail(signer).result()
	}
	if s.typ == dns.TypeDNSKEY && equalName(signer, s.name) {
		return c.dnskeyPart(s)
	}
	st, _, r := c.establish(signer, nil)
	if r != nil {
		return *r
	}
	v, f := c.verifyRRset(s, signer, st.keys)
	if f != nil {
		return f.result()
	}
	res := Result{Status: Secure, Zone: display(signer), EDE: EDENone, Expires: v.expires}
	if labels := int(v.sig.Labels); labels < sigLabels(s.name) {
		d, f := c.newDenial(auth, signer, st.keys)
		if f != nil {
			return f.result()
		}
		if d.highParams {
			return Result{Status: Insecure, Zone: display(signer), Reason: ReasonNSEC3Iterations, EDE: EDEUnsupportedNSEC3Params}
		}
		pr, f := d.wildcard(s.name, labels)
		switch {
		case f != nil:
			return f.result()
		case pr == proofOptOut:
			return Result{Status: Insecure, Zone: display(signer), Reason: ReasonOptOut, EDE: EDENone}
		case pr != proofOK:
			return fail(signer, FailNSEC, EDENSECMissing).result()
		}
		c.keep(d.used...)
		res.Expires = earliest(res.Expires, d.expires)
	}
	return res
}

// dnskeyPart validates a zone's own DNSKEY RRset in an answer (a DNSKEY
// query) from the answer itself: the root's against the anchors, another
// zone's against its DS from the parent.
func (c *vctx) dnskeyPart(s *rrset) Result {
	if s.name == "." {
		r := c.rootKeys(s)
		c.v.cacheStep(&c.sctx, c.req.Route, ".", r, c.req.MaxTTL)
		if r.kind != stepKeys {
			return c.stepOutcome(".", r).result()
		}
		return Result{Status: Secure, Zone: ".", EDE: EDENone, Expires: r.expires}
	}
	st, w, r := c.establish(s.name, s)
	if r != nil {
		return *r
	}
	exp := w.expires
	if !w.checked {
		// The zone's keys were known: the answer is authenticated by them.
		v, f := c.verifyRRset(s, s.name, st.keys)
		if f != nil {
			return f.result()
		}
		exp = v.expires
	}
	return Result{Status: Secure, Zone: display(s.name), EDE: EDENone, Expires: exp}
}

// result converts a walk result that ended a walk (bogus, insecure,
// indeterminate, blocked).
func (w walkResult) result() Result {
	switch w.kind {
	case walkInsecure:
		return insecureResult(w.st)
	case walkIndeterminate:
		return Result{Status: Indeterminate, Reason: w.degraded, EDE: EDENone}
	case walkBlocked:
		return Result{Block: w.block, EDE: EDENone}
	case walkBogus:
		return w.fail.result()
	}
	return Result{Status: Bogus, Reason: FailBadSignature, EDE: EDEBogus}
}

// denialPart validates the negative end of an answer: NXDOMAIN or NODATA
// for (name, qtype) with the NSEC or NSEC3 records of the Authority
// section and their SOA, all signed by one zone: the deepest signer of an
// NSEC, NSEC3 or SOA RRset that is an ancestor of name (for DS a proper
// one, the parent's side), so the proofs of other zones in the section (a
// wildcard CNAME's no-closer-name proof) are left to their own parts.
// Without such a signer the walk down the name must prove an insecure cut
// (else EDE 12).
func (c *vctx) denialPart(name string, qtype uint16, nx bool, auth []*rrset) Result {
	signer := ""
	for _, s := range auth {
		if s.typ != dns.TypeNSEC && s.typ != dns.TypeNSEC3 && s.typ != dns.TypeSOA {
			continue
		}
		for _, sig := range s.sigs {
			z := canon(sig.SignerName)
			if !below(z, name) || (qtype == dns.TypeDS && name != "." && equalName(z, name)) {
				continue
			}
			if signer == "" || dns.CountLabel(z) > dns.CountLabel(signer) {
				signer = z
			}
		}
	}
	if signer == "" {
		return c.unsigned(name, FailNSEC, EDENSECMissing)
	}
	st, _, r := c.establish(signer, nil)
	if r != nil {
		return *r
	}
	res := Result{Status: Secure, Zone: display(signer), EDE: EDENone}
	if soa := find(auth, signer, dns.TypeSOA); soa != nil && signedBy(soa, signer) {
		v, f := c.verifyRRset(soa, signer, st.keys)
		if f != nil {
			return f.result()
		}
		c.keep(soa)
		res.Expires = v.expires
	}
	d, f := c.newDenial(auth, signer, st.keys)
	if f != nil {
		return f.result()
	}
	if d.highParams {
		return Result{Status: Insecure, Zone: display(signer), Reason: ReasonNSEC3Iterations, EDE: EDEUnsupportedNSEC3Params}
	}
	var pr proofResult
	if nx {
		pr, f = d.nxdomain(name)
	} else {
		pr, f = d.nodata(name, qtype)
	}
	switch {
	case f != nil:
		return f.result()
	case pr == proofOptOut:
		return Result{Status: Insecure, Zone: display(signer), Reason: ReasonOptOut, EDE: EDENone}
	case pr != proofOK:
		return fail(signer, FailNSEC, EDENSECMissing).result()
	}
	c.keep(d.used...)
	res.Expires = earliest(res.Expires, d.expires)
	return res
}

// keep marks authenticated RRsets of the Authority section that a secure
// answer keeps.
func (c *vctx) keep(sets ...*rrset) {
	if c.kept == nil {
		c.kept = map[dns.RR]bool{}
	}
	for _, s := range sets {
		for _, rr := range s.rrs {
			c.kept[rr] = true
		}
		for _, sig := range s.sigs {
			c.kept[sig] = true
		}
	}
}

// trim removes the RRsets of the Authority and Additional sections of a
// secure answer that were not validated (the OPT record stays), so AD
// covers the whole message.
func (c *vctx) trim(m *dns.Msg) {
	ns := m.Ns[:0]
	for _, rr := range m.Ns {
		if c.kept[rr] {
			ns = append(ns, rr)
		}
	}
	m.Ns = ns
	extra := m.Extra[:0]
	for _, rr := range m.Extra {
		if rr.Header().Rrtype == dns.TypeOPT {
			extra = append(extra, rr)
		}
	}
	m.Extra = extra
}
