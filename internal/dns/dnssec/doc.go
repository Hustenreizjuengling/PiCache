// Package dnssec validates DNS answers locally (docs/ARCHITECTURE.md 7.6):
// the root trust anchors (anchors.go), the algorithm policy (policy.go),
// RRset, key and denial checks (verify.go, denial.go), the walk that
// discovers zone cuts over a lookup function it is given (walk.go), the
// answer shapes (answer.go), the key and failure caches (cache.go) and the
// budgets. It imports no PiCache package; only the upstream package uses
// it, which owns the routes, the probes, the clock state and the verdicts
// in the response cache.
//
// Standards: RFC 4033–4035, 5155 (NSEC3), 6672 (DNAME), 6840, 8624
// (algorithms), 9276 (NSEC3 parameters), 9520 (failure caching).
// Cryptography only through miekg's RRSIG.Verify, DNSKEY.ToDS and HashName
// (the NSEC3 parameters are checked before any hash is computed) and the
// standard library. Out of scope: RFC 5011 rollover tracking (the anchors
// change only with releases and are never fetched), RFC 8198 aggressive
// use of NSEC/NSEC3 (nothing is synthesised from cached proofs), negative
// trust anchors.
//
// The Answer section is scrubbed first: only the answer to the question
// stays (the data or CNAME at each name of the chain from the question
// name, the DNAMEs above them, their RRSIGs; class IN), so a record of
// another zone or class neither changes the verdict nor rides along.
//
// Verdicts: secure (every RRset of the answer and of its denial proof
// verified along a chain of trust from an anchor), insecure (a validated
// proof that the answer lies below an insecure delegation: no DS, an NSEC3
// opt-out span, DS entries or keys of unsupported algorithms, digests or
// sizes, NSEC3 parameters above the limit), bogus (anything inside a zone
// proven signed that cannot be verified, including failed chain lookups
// and exhausted budgets) and indeterminate (a degraded upstream, time
// checks suspended). A lookup that fails or cannot be proven is bogus,
// never insecure.
//
// Bounds (per validation context: the validation of one fetched answer, or
// of one chain step, with its own budget): 32 chain lookups (every step
// that is not a key-cache hit, coalesced waits included), 4 s, the first 8
// RRSIGs per RRset, 4 DNSKEYs per RRSIG (same key tag and algorithm), 32
// records in a DNSKEY RRset and 8 in a DS RRset, 16 signature
// verifications of which at most 2 fail, 256 NSEC3 hashes, 16 NSEC/NSEC3
// records per proof, 8 CNAME/DNAME hops; beyond them: bogus, EDE 6
// "validation limit reached". NSEC3 iterations above 50 or a salt above
// 64 bytes: the proof's zone is insecure (EDE 27), no hash is computed.
// Global: 1024 chain steps in flight, GOMAXPROCS concurrent signature
// verifications (a wait ends with the context's deadline); the key cache
// holds 16 384 zone states per (route, zone) and 8 MiB (packed wire size
// of the kept DNSKEY RRsets plus the names; a larger RRset than 16 KiB is
// not kept), LRU; the failure cache 4096 entries (30 s after a
// cryptographic failure, 5 s after a lookup failure or an exhausted
// budget; never a local refusal: the flight cap, or a lookup that failed
// with ErrRateLimited, sets Result.Local instead).
package dnssec
