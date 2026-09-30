package dnssec

import (
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// denial holds the authenticated NSEC and NSEC3 records of one response
// (signed by zone) and proves the non-existence of names and types with
// them (RFC 4035 5.4, RFC 5155 8, RFC 6840 4.1).
type denial struct {
	c     *sctx
	zone  string
	nsec  []*dns.NSEC
	nsec3 []*dns.NSEC3
	// highParams: an authenticated NSEC3 record has more than 50
	// iterations or a salt of more than 64 bytes (RFC 9276 3.2): the
	// proof's zone is insecure and no hash is computed.
	highParams bool
	hashes     map[hashKey]string
	// used are the authenticated RRsets (kept in a secure answer); ttl and
	// expires the smallest TTL (RRSIG original TTLs included) and the
	// earliest RRSIG expiration among them.
	used    []*rrset
	ttl     uint32
	expires time.Time
}

type hashKey struct {
	name string
	iter uint16
	salt string
}

// newDenial authenticates the NSEC and NSEC3 RRsets of a section that are
// signed by zone (at most maxProofRecords records; the others are
// ignored). A record whose signature fails makes the whole proof fail.
func (c *sctx) newDenial(auth []*rrset, zone string, keys []key) (*denial, *failure) {
	d := &denial{c: c, zone: canon(zone), hashes: map[hashKey]string{}, ttl: ^uint32(0)}
	n := 0
	for _, s := range auth {
		if (s.typ != dns.TypeNSEC && s.typ != dns.TypeNSEC3) || !signedBy(s, zone) || !below(zone, s.name) {
			continue
		}
		if n += len(s.rrs); n > maxProofRecords {
			return nil, limitFail(zone)
		}
		v, f := c.verifyRRset(s, zone, keys)
		if f != nil {
			return nil, f
		}
		d.use(s, v)
		for _, rr := range s.rrs {
			switch r := rr.(type) {
			case *dns.NSEC:
				d.nsec = append(d.nsec, r)
			case *dns.NSEC3:
				if !nsec3Usable(r, zone) {
					continue
				}
				if r.Iterations > maxNSEC3Iterations || r.SaltLength > maxNSEC3Salt {
					d.highParams = true
				}
				d.nsec3 = append(d.nsec3, r)
			}
		}
	}
	return d, nil
}

// use records an authenticated RRset of the proof.
func (d *denial) use(s *rrset, v verified) {
	d.used = append(d.used, s)
	d.ttl = min(d.ttl, s.minTTL(), v.origTTL)
	if d.expires.IsZero() || v.expires.Before(d.expires) {
		d.expires = v.expires
	}
}

// signedBy reports whether an RRSIG of s names zone as its signer.
func signedBy(s *rrset, zone string) bool {
	return slices.ContainsFunc(s.sigs, func(sig *dns.RRSIG) bool { return equalName(sig.SignerName, zone) })
}

// nsec3Usable reports an NSEC3 record of hash algorithm 1 (SHA-1) whose
// owner is <32 base32hex characters>.<zone> (<hash>. in the root zone) and
// whose next hashed owner has the same form (other hash algorithms are
// ignored, RFC 5155 8.1).
func nsec3Usable(r *dns.NSEC3, zone string) bool {
	if r.Hash != dns.SHA1 || !validHash(r.NextDomain) {
		return false
	}
	off, end := dns.NextLabel(r.Hdr.Name, 0)
	if off < 2 || !validHash(r.Hdr.Name[:off-1]) {
		return false
	}
	if end { // a single label: the root zone's
		return equalName(zone, ".")
	}
	return equalName(r.Hdr.Name[off:], zone)
}

// validHash reports a SHA-1 hash in base32hex without padding (RFC 4648
// 7): 32 characters of 0-9 and A-V (either case).
func validHash(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'v' || 'A' <= c && c <= 'V') {
			return false
		}
	}
	return true
}

func ownerHash(r *dns.NSEC3) string {
	off, _ := dns.NextLabel(r.Hdr.Name, 0)
	return strings.ToUpper(r.Hdr.Name[:off-1])
}

// proofResult is the outcome of a denial proof.
type proofResult int

const (
	proofFailed   proofResult = iota
	proofOK                   // the non-existence is proven
	proofOptOut               // proven only up to an NSEC3 opt-out span: insecure
	proofNotCut               // walk: the name exists and is no zone cut (or an empty non-terminal)
	proofInsecure             // walk: an insecure delegation (NS bit, no DS bit)
)

// hash returns the NSEC3 hash of name with the parameters of r, computed at
// most once per proof and within the budget (256 per context).
func (d *denial) hash(name string, r *dns.NSEC3) (string, *failure) {
	k := hashKey{canon(name), r.Iterations, strings.ToUpper(r.Salt)}
	if h, ok := d.hashes[k]; ok {
		return h, nil
	}
	if d.c.b.hashes >= maxNSEC3Hashes {
		return "", limitFail(d.zone)
	}
	d.c.b.hashes++
	h := dns.HashName(k.name, dns.SHA1, r.Iterations, r.Salt)
	d.hashes[k] = h
	return h, nil
}

// match3 returns the NSEC3 record whose owner is the hash of name.
func (d *denial) match3(name string) (*dns.NSEC3, *failure) {
	for _, r := range d.nsec3 {
		h, f := d.hash(name, r)
		if f != nil {
			return nil, f
		}
		if h == ownerHash(r) {
			return r, nil
		}
	}
	return nil, nil
}

// cover3 returns the NSEC3 record whose span covers the hash of name.
func (d *denial) cover3(name string) (*dns.NSEC3, *failure) {
	for _, r := range d.nsec3 {
		h, f := d.hash(name, r)
		if f != nil {
			return nil, f
		}
		o, x := ownerHash(r), strings.ToUpper(r.NextDomain)
		switch {
		case o == x:
			if h != o {
				return r, nil
			}
		case o < x:
			if o < h && h < x {
				return r, nil
			}
		default: // the last span of the chain wraps around
			if h > o || h < x {
				return r, nil
			}
		}
	}
	return nil, nil
}

// closestEncloser3 runs the closest encloser proof of RFC 5155 8.3 for
// name: the closest encloser (an ancestor with a matching NSEC3 record that
// is no delegation point and no DNAME) and the record covering the next
// closer name. ok is false when there is no such proof (also when name
// itself has a matching record).
func (d *denial) closestEncloser3(name string) (ce string, cover *dns.NSEC3, ok bool, f *failure) {
	if !below(d.zone, name) {
		return "", nil, false, nil
	}
	next := ""
	for s := canon(name); s != ""; s = parentName(s) {
		m, f := d.match3(s)
		if f != nil {
			return "", nil, false, f
		}
		if m != nil {
			if next == "" || delegationOrDNAME(m.TypeBitMap) {
				return "", nil, false, nil
			}
			cover, f := d.cover3(next)
			if f != nil || cover == nil {
				return "", nil, false, f
			}
			return s, cover, true, nil
		}
		if equalName(s, d.zone) {
			break
		}
		next = s
	}
	return "", nil, false, nil
}

// delegationOrDNAME reports a type bitmap of a zone cut seen from the
// parent (NS without SOA) or of a DNAME: such a record proves nothing
// below its owner.
func delegationOrDNAME(types []uint16) bool {
	return has(types, dns.TypeNS) && !has(types, dns.TypeSOA) || has(types, dns.TypeDNAME)
}

func has(types []uint16, t uint16) bool { return slices.Contains(types, t) }

// nodataBits checks the bitmap of a record matching the name of a NODATA
// answer for qtype: neither the type nor CNAME; for DS the record must not
// be the child's apex (SOA), for other types not the parent side of a cut
// (NS without SOA).
func nodataBits(types []uint16, qtype uint16, root bool) bool {
	if has(types, qtype) || has(types, dns.TypeCNAME) {
		return false
	}
	if qtype == dns.TypeDS {
		return root || !has(types, dns.TypeSOA)
	}
	return !(has(types, dns.TypeNS) && !has(types, dns.TypeSOA))
}

// nsecCovers reports whether r proves that name does not exist: name lies
// strictly between the owner and the next name in canonical order (the
// last record wraps around to the apex), inside the zone, and the owner is
// not a zone cut or DNAME above name.
func (d *denial) nsecCovers(r *dns.NSEC, name string) bool {
	if !below(d.zone, name) || equalName(r.Hdr.Name, name) {
		return false
	}
	if strictlyBelow(r.Hdr.Name, name) && delegationOrDNAME(r.TypeBitMap) {
		return false
	}
	o, x := r.Hdr.Name, r.NextDomain
	if compareNames(o, x) < 0 {
		return compareNames(o, name) < 0 && compareNames(name, x) < 0
	}
	return compareNames(o, name) < 0 || compareNames(name, x) < 0
}

// nsecCE returns the closest encloser of name that the covering record r
// implies: the longer of the common ancestors of name with its owner and
// with its next name.
func nsecCE(r *dns.NSEC, name string) string {
	a, b := commonAncestor(name, r.Hdr.Name), commonAncestor(name, r.NextDomain)
	if dns.CountLabel(b) > dns.CountLabel(a) {
		return b
	}
	return a
}

func (d *denial) matchNSEC(name string) *dns.NSEC {
	for _, r := range d.nsec {
		if equalName(r.Hdr.Name, name) {
			return r
		}
	}
	return nil
}

func (d *denial) coverNSEC(name string) *dns.NSEC {
	for _, r := range d.nsec {
		if d.nsecCovers(r, name) {
			return r
		}
	}
	return nil
}

// nxdomain proves that name does not exist and that no wildcard could have
// answered it: an NSEC covering the name and one covering the wildcard at
// the closest encloser, or the NSEC3 closest encloser proof with the
// wildcard covered (a next closer name covered by an opt-out span:
// proofOptOut).
func (d *denial) nxdomain(name string) (proofResult, *failure) {
	if d.matchNSEC(name) == nil {
		if cover := d.coverNSEC(name); cover != nil {
			wc := wildcardAt(nsecCE(cover, name))
			if d.matchNSEC(wc) == nil && d.coverNSEC(wc) != nil {
				return proofOK, nil
			}
		}
	}
	if len(d.nsec3) == 0 {
		return proofFailed, nil
	}
	ce, cover, ok, f := d.closestEncloser3(name)
	if f != nil || !ok {
		return proofFailed, f
	}
	if cover.Flags&1 != 0 {
		return proofOptOut, nil
	}
	wc, f := d.cover3(wildcardAt(ce))
	if f != nil || wc == nil {
		return proofFailed, f
	}
	return proofOK, nil
}

// nodata proves that name exists without qtype (and without a CNAME): a
// record matching the name, an empty non-terminal (NSEC: a covering
// record whose next name is below the name), a wildcard NODATA proof, or
// for DS an NSEC3 opt-out span covering the next closer name
// (proofOptOut).
func (d *denial) nodata(name string, qtype uint16) (proofResult, *failure) {
	root := equalName(name, ".")
	if m := d.matchNSEC(name); m != nil {
		if nodataBits(m.TypeBitMap, qtype, root) {
			return proofOK, nil
		}
		return proofFailed, nil
	}
	if cover := d.coverNSEC(name); cover != nil {
		if strictlyBelow(name, cover.NextDomain) {
			return proofOK, nil // empty non-terminal
		}
		if w := d.matchNSEC(wildcardAt(nsecCE(cover, name))); w != nil && nodataBits(w.TypeBitMap, qtype, false) {
			return proofOK, nil // wildcard NODATA
		}
	}
	if len(d.nsec3) == 0 {
		return proofFailed, nil
	}
	m, f := d.match3(name)
	if f != nil {
		return proofFailed, f
	}
	if m != nil {
		if nodataBits(m.TypeBitMap, qtype, root) {
			return proofOK, nil
		}
		return proofFailed, nil
	}
	ce, cover, ok, f := d.closestEncloser3(name)
	if f != nil || !ok {
		return proofFailed, f
	}
	if qtype == dns.TypeDS && cover.Flags&1 != 0 {
		return proofOptOut, nil
	}
	w, f := d.match3(wildcardAt(ce))
	if f != nil || w == nil || !nodataBits(w.TypeBitMap, qtype, false) {
		return proofFailed, f
	}
	return proofOK, nil
}

// delegation classifies the NODATA answer to the chain lookup "name DS"
// (the walk): a record matching the name with the NS bit and without the
// DS bit is an insecure delegation; one without the NS bit (or an empty
// non-terminal) is no zone cut; an NSEC3 opt-out span covering the next
// closer name is insecure (proofOptOut). A matching record with the DS bit
// or from the child's apex (SOA) fails.
func (d *denial) delegation(name string) (proofResult, *failure) {
	classify := func(types []uint16) proofResult {
		switch {
		case has(types, dns.TypeDS), has(types, dns.TypeSOA):
			return proofFailed
		case has(types, dns.TypeNS):
			return proofInsecure
		}
		return proofNotCut
	}
	if m := d.matchNSEC(name); m != nil {
		return classify(m.TypeBitMap), nil
	}
	if cover := d.coverNSEC(name); cover != nil && strictlyBelow(name, cover.NextDomain) {
		return proofNotCut, nil // empty non-terminal
	}
	if len(d.nsec3) == 0 {
		return proofFailed, nil
	}
	m, f := d.match3(name)
	if f != nil {
		return proofFailed, f
	}
	if m != nil {
		return classify(m.TypeBitMap), nil
	}
	_, cover, ok, f := d.closestEncloser3(name)
	if f != nil || !ok || cover.Flags&1 == 0 {
		return proofFailed, f
	}
	return proofOptOut, nil
}

// wildcard proves for a wildcard expansion (an RRSIG with fewer labels than
// its owner) that no closer name exists: an NSEC covering the owner whose
// closest encloser is the wildcard's, or an NSEC3 record covering the next
// closer name (opt-out: proofOptOut).
func (d *denial) wildcard(owner string, labels int) (proofResult, *failure) {
	ce := lastLabels(owner, labels)
	for _, r := range d.nsec {
		if d.nsecCovers(r, owner) && equalName(nsecCE(r, owner), ce) {
			return proofOK, nil
		}
	}
	if len(d.nsec3) == 0 {
		return proofFailed, nil
	}
	cover, f := d.cover3(lastLabels(owner, labels+1))
	if f != nil || cover == nil {
		return proofFailed, f
	}
	if cover.Flags&1 != 0 {
		return proofOptOut, nil
	}
	return proofOK, nil
}
