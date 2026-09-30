package dnssec

import (
	"encoding/base64"
	"math/bits"

	"github.com/miekg/dns"
)

// Algorithm policy (RFC 8624): the algorithms PiCache validates. Every other
// algorithm is unsupported (never bogus by itself), including 5 RSASHA1 and
// 7 RSASHA1-NSEC3-SHA1 (SHA-1 is not trusted) and 16 ED448.
func algSupported(alg uint8) bool {
	switch alg {
	case dns.RSASHA256, dns.RSASHA512, dns.ECDSAP256SHA256, dns.ECDSAP384SHA384, dns.ED25519:
		return true
	}
	return false
}

// digestSupported reports the DS digest types PiCache uses: 2 SHA-256 and 4
// SHA-384. 1 SHA-1 is never used (ignored next to a SHA-256/384 DS, RFC 4509
// downgrade protection; alone it counts as unsupported).
func digestSupported(t uint8) bool { return t == dns.SHA256 || t == dns.SHA384 }

// RSA key limits: a modulus of 1024 to 4096 bits and an exponent of at most
// 2^31-1 (Go's crypto/rsa and miekg refuse the others; the validator
// classifies them before verifying).
const (
	minRSABits = 1024
	maxRSABits = 4096
	maxRSAExp  = 1<<31 - 1
)

// keySupported reports whether k can be used: a supported algorithm and a
// public key of that algorithm's form (RSA within the limits above, ECDSA
// P-256 64 bytes, P-384 96 bytes, Ed25519 32 bytes). Protocol and flags
// are checked by the callers.
func keySupported(k *dns.DNSKEY) bool {
	if !algSupported(k.Algorithm) {
		return false
	}
	pub, err := base64.StdEncoding.DecodeString(k.PublicKey)
	if err != nil {
		return false
	}
	switch k.Algorithm {
	case dns.RSASHA256, dns.RSASHA512:
		return rsaKeyOK(pub)
	case dns.ECDSAP256SHA256:
		return len(pub) == 64
	case dns.ECDSAP384SHA384:
		return len(pub) == 96
	case dns.ED25519:
		return len(pub) == 32
	}
	return false
}

// rsaKeyOK checks an RSA public key in DNSKEY form (RFC 3110 2): the
// exponent length (one byte, or zero and two bytes), the exponent and the
// modulus.
func rsaKeyOK(pub []byte) bool {
	if len(pub) < 3 {
		return false
	}
	explen, off := int(pub[0]), 1
	if explen == 0 {
		explen, off = int(pub[1])<<8|int(pub[2]), 3
	}
	if explen == 0 || explen > 4 || off+explen >= len(pub) || pub[off] == 0 {
		return false
	}
	var exp uint64
	for _, b := range pub[off : off+explen] {
		exp = exp<<8 | uint64(b)
	}
	if exp > maxRSAExp {
		return false
	}
	mod := pub[off+explen:]
	if mod[0] == 0 {
		return false // a leading zero (Go and miekg refuse it)
	}
	n := (len(mod)-1)*8 + bits.Len8(mod[0])
	return n >= minRSABits && n <= maxRSABits
}

// usableKey reports whether k may sign the zone's data: the zone-key flag,
// protocol 3, no REVOKE flag and a supported key.
func usableKey(k *dns.DNSKEY) bool {
	return k.Flags&dns.ZONE != 0 && k.Flags&dns.REVOKE == 0 && k.Protocol == 3 && keySupported(k)
}
