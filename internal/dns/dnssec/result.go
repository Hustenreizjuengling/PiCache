package dnssec

import (
	"time"

	"github.com/miekg/dns"
)

// Statuses of a validated answer (docs/ARCHITECTURE.md 7.6).
const (
	Secure        = "secure"
	Insecure      = "insecure"
	Bogus         = "bogus"
	Indeterminate = "indeterminate"
)

// rank orders the statuses for combining the parts of an answer: the worst
// part decides (bogus > indeterminate > insecure > secure).
func rank(status string) int {
	switch status {
	case Bogus:
		return 3
	case Indeterminate:
		return 2
	case Insecure:
		return 1
	}
	return 0
}

// Failure phrases of bogus verdicts (fixed English; the API, the query log
// and the tests compare them). The lookup failures read "DNSKEY lookup
// failed (<error>)" and "DS lookup failed (<error>)", a signer that is not
// the zone "signer <name> is not the zone".
const (
	FailSignatureExpired  = "signature expired"
	FailSignatureNotValid = "signature not yet valid"
	FailBadSignature      = "bad signature"
	FailNoRRSIG           = "no RRSIG"
	FailNoDNSKEYMatch     = "no DNSKEY matches the DS"
	FailNoZoneKey         = "no zone key bit"
	FailNSEC              = "missing or invalid NSEC proof"
	FailChainLoop         = "chain loop"
	FailLimit             = "validation limit reached"
)

// Reasons of insecure verdicts.
const (
	ReasonNoDS                 = "no DS"
	ReasonOptOut               = "NSEC3 opt-out"
	ReasonUnsupportedAlgorithm = "unsupported algorithm"
	ReasonUnsupportedDigest    = "unsupported digest"
	ReasonUnsupportedKey       = "unsupported key"
	ReasonNSEC3Iterations      = "NSEC3 iterations above 50"
)

// Reasons of indeterminate verdicts that the validator sets itself (the
// upstream package adds "<upstream> returns no DNSSEC data", "the trust
// anchors do not match the root zone" and "stale answer").
const (
	ReasonTimeSuspended = "time checks suspended"
)

// Extended DNS Error codes (RFC 8914) of the verdicts.
const (
	EDENone                   = -1
	EDEUnsupportedAlgorithm   = int(dns.ExtendedErrorCodeUnsupportedDNSKEYAlgorithm) // 1
	EDEUnsupportedDigest      = int(dns.ExtendedErrorCodeUnsupportedDSDigestType)    // 2
	EDEBogus                  = int(dns.ExtendedErrorCodeDNSBogus)                   // 6
	EDESignatureExpired       = int(dns.ExtendedErrorCodeSignatureExpired)           // 7
	EDESignatureNotYetValid   = int(dns.ExtendedErrorCodeSignatureNotYetValid)       // 8
	EDEDNSKEYMissing          = int(dns.ExtendedErrorCodeDNSKEYMissing)              // 9
	EDERRSIGsMissing          = int(dns.ExtendedErrorCodeRRSIGsMissing)              // 10
	EDENoZoneKeyBit           = int(dns.ExtendedErrorCodeNoZoneKeyBitSet)            // 11
	EDENSECMissing            = int(dns.ExtendedErrorCodeNSECMissing)                // 12
	EDEUnsupportedNSEC3Params = int(dns.ExtendedErrorCodeUnsupportedNSEC3IterValue)  // 27
)

// Result is the verdict of a validation.
type Result struct {
	// Status is Secure, Insecure, Bogus or Indeterminate; "" when the
	// answer was not validated because a chain reply was the upstream's
	// own block (Block).
	Status string
	// Zone is the signer (secure, bogus) or the insecure cut: lower-case,
	// without the trailing dot, "." for the root; "" when unknown.
	Zone string
	// Reason is the failure phrase (bogus), the reason of an insecure or
	// indeterminate verdict, "" for secure.
	Reason string
	// EDE is the Extended DNS Error code for clients (EDENone: none).
	EDE int
	// Lookup marks a bogus verdict after a lookup failure or an exhausted
	// budget (cached 5 s); otherwise it is cryptographic (cached at most
	// 30 s).
	Lookup bool
	// Local marks a lookup failure that a local bound refused (a chain
	// rate limit, the chain flight cap): it says nothing about the zone,
	// so the answer must not be cached.
	Local bool
	// Expires is the earliest expiration of the RRSIGs a secure verdict
	// used (zero while time checks are suspended): the answer must not be
	// cached beyond it.
	Expires time.Time
	// Block is the Reply.Block of a chain reply the upstream classified as
	// its own block.
	Block any
}

// failure is a bogus verdict under construction.
type failure struct {
	zone   string // canonical
	text   string
	ede    int
	lookup bool
	local  bool // a local refusal (Result.Local): never in the failure cache
}

func (f *failure) result() Result {
	return Result{Status: Bogus, Zone: display(f.zone), Reason: f.text, EDE: f.ede, Lookup: f.lookup, Local: f.local}
}

// fail builds a cryptographic failure.
func fail(zone, text string, ede int) *failure {
	return &failure{zone: canon(zone), text: text, ede: ede}
}

// lookupFail builds a lookup failure (a failed lookup, an exhausted budget).
func lookupFail(zone, text string, ede int) *failure {
	return &failure{zone: canon(zone), text: text, ede: ede, lookup: true}
}

// limitFail is the failure of an exhausted bound.
func limitFail(zone string) *failure { return lookupFail(zone, FailLimit, EDEBogus) }

// signerFail is the failure of a signer that is not the zone.
func signerFail(signer string) *failure {
	return fail(signer, "signer "+display(signer)+" is not the zone", EDEBogus)
}
