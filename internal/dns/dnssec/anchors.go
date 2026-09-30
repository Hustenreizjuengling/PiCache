package dnssec

import (
	"strings"

	"github.com/miekg/dns"
)

// rootAnchors are the trust anchors of the root zone: the DS records of the
// root key-signing keys, copied from IANA's root-anchors.xml
// (https://data.iana.org/root-anchors/root-anchors.xml, TrustAnchor
// 0C05FDD6-422C-4910-8ED6-430ED15E11C2, retrieved 2026-09-29):
//
//   - KSK-2017 (KeyDigest Klajeyz, validFrom 2017-02-02): 20326 8 2
//     E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D
//   - KSK-2024 (KeyDigest Kmyv6jo, validFrom 2024-07-18): 38696 8 2
//     683D2D0ACB8C9B712A1948B27F741219298D0A450D612C483AF444A4C0FB2B16
//
// KSK-2024 signs the root DNSKEY RRset from 2026-10-11; KSK-2017 gets the
// REVOKE bit on 2027-01-11 (its changed RDATA then matches no digest). The
// anchors are never fetched at runtime (no RFC 5011 tracking): a rollover
// to a key not listed here needs a PiCache update. The KSK of 2010
// (19036, validUntil 2019-01-11) is left out.
var rootAnchors = []*dns.DS{
	{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDS, Class: dns.ClassINET}, KeyTag: 20326, Algorithm: dns.RSASHA256,
		DigestType: dns.SHA256, Digest: "E06D44B80B8F1D39A95C0B0D7C65D08458E880409BBC683457104237C7F8EC8D"},
	{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDS, Class: dns.ClassINET}, KeyTag: 38696, Algorithm: dns.RSASHA256,
		DigestType: dns.SHA256, Digest: "683D2D0ACB8C9B712A1948B27F741219298D0A450D612C483AF444A4C0FB2B16"},
}

// RootAnchors returns copies of the built-in root trust anchors.
func RootAnchors() []*dns.DS {
	out := make([]*dns.DS, len(rootAnchors))
	for i, a := range rootAnchors {
		c := *a
		out[i] = &c
	}
	return out
}

// anchored reports whether k is a trusted root key: the SHA-256 digest over
// its owner name and RDATA (flags included) equals an anchor's digest
// (never by key tag alone), its zone-key and SEP flags are set, its REVOKE
// flag is clear and its algorithm is the anchor's.
func anchored(k *dns.DNSKEY, anchors []*dns.DS) bool {
	if k.Flags&dns.ZONE == 0 || k.Flags&dns.SEP == 0 || k.Flags&dns.REVOKE != 0 || k.Protocol != 3 {
		return false
	}
	var digests map[uint8]string
	for _, a := range anchors {
		if a.Algorithm != k.Algorithm {
			continue
		}
		if digests == nil {
			digests = map[uint8]string{}
		}
		d, ok := digests[a.DigestType]
		if !ok {
			if ds := k.ToDS(a.DigestType); ds != nil {
				d = ds.Digest
			}
			digests[a.DigestType] = d
		}
		if d != "" && strings.EqualFold(d, a.Digest) {
			return true
		}
	}
	return false
}
