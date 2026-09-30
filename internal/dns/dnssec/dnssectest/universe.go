// Package dnssectest builds signed DNS zones in memory for the tests of the
// DNSSEC validator and its users (internal/dns/dnssec, upstream,
// dnsserver). A Universe answers queries like a recursive resolver that
// returns DNSSEC data: answers with their RRSIGs, CNAME chains followed
// across zones, wildcard expansions, and NODATA and NXDOMAIN answers with
// NSEC or NSEC3 proofs. It is imported only by tests (never linked into
// the binary) and touches no network.
package dnssectest

import (
	"bytes"
	"context"
	"crypto"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Now is the fixed time of the tests; signatures are valid from a day
// before it to 30 days after it.
var Now = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// Signer is a DNSKEY with its private key.
type Signer struct {
	Key  *dns.DNSKEY
	priv crypto.Signer
}

// generated caches the private keys and the public key encodings of the
// test keys by (algorithm, bits, slot): RSA generation is slow.
var (
	keyMu     sync.Mutex
	generated = map[string]generatedKey{}
)

type generatedKey struct {
	priv crypto.Signer
	pub  string
}

// NewSigner returns a key of alg for zone (flags 256 or 257). Private keys
// are generated once per (alg, bits, slot) and reused across zones; the
// owner name is set per zone.
func NewSigner(t testing.TB, zone string, alg uint8, bits int, flags uint16, slot int) *Signer {
	t.Helper()
	id := fmt.Sprintf("%d/%d/%d", alg, bits, slot)
	keyMu.Lock()
	g, ok := generated[id]
	if !ok {
		tmpl := &dns.DNSKEY{Algorithm: alg}
		priv, err := tmpl.Generate(bits)
		if err != nil {
			keyMu.Unlock()
			t.Fatalf("generate %s: %v", id, err)
		}
		g = generatedKey{priv: priv.(crypto.Signer), pub: tmpl.PublicKey}
		generated[id] = g
	}
	keyMu.Unlock()
	k := &dns.DNSKEY{Hdr: Hdr(zone, dns.TypeDNSKEY, 3600), Flags: flags, Protocol: 3, Algorithm: alg, PublicKey: g.pub}
	return &Signer{Key: k, priv: g.priv}
}

// Sig signs rrs with s (valid from inc to exp). It panics on an error
// (it runs in lookups of the validator, outside the test goroutine).
func (s *Signer) Sig(rrs []dns.RR, inc, exp time.Time) *dns.RRSIG {
	r := &dns.RRSIG{Hdr: dns.RR_Header{Ttl: rrs[0].Header().Ttl}, Algorithm: s.Key.Algorithm, SignerName: s.Key.Hdr.Name,
		KeyTag: s.Key.KeyTag(), Inception: uint32(inc.Unix()), Expiration: uint32(exp.Unix())}
	if err := r.Sign(s.priv, rrs); err != nil {
		panic(fmt.Sprintf("sign %s: %v", rrs[0].Header().Name, err))
	}
	return r
}

// Zone is a zone of a Universe.
type Zone struct {
	u      *Universe
	Name   string // canonical (lower-case, fully qualified)
	Signed bool
	KSK    *Signer
	ZSK    *Signer
	// Data holds the RRsets by owner and type (DNSKEY and SOA at the apex
	// are added by AddZone, DS of child zones by AddChild).
	Data map[string]map[uint16][]dns.RR
	// NSEC3 selects NSEC3 proofs (else NSEC); OptOut leaves insecure
	// delegations out of the chain with the opt-out flag on every record;
	// Iterations and Salt are the NSEC3 parameters.
	NSEC3      bool
	OptOut     bool
	Iterations uint16
	Salt       string
	// DSDigest is the digest type of the DS records the parent publishes
	// for this zone (SHA-256); DSAlg overrides their algorithm.
	DSDigest uint8
	DSAlg    uint8
	// SigInc and SigExp are the validity period of new signatures (call
	// ClearSigs after changing them).
	SigInc, SigExp time.Time
	sigCache       map[string]*dns.RRSIG
}

// Universe is a set of zones below a test root.
type Universe struct {
	t     testing.TB
	Zones map[string]*Zone
	Root  *Zone
	// Tamper may change a response (question name canonical) before it is
	// returned.
	Tamper func(qname string, qtype uint16, m *dns.Msg)
	// Delay is added to every Serve (0: none).
	Delay time.Duration
	// RSAKSK: new signed zones get an RSA-2048 KSK (else ECDSA P-256).
	RSAKSK bool
	// base is the time the signatures of new zones are valid around;
	// slot selects the key pair set (0: the shared one).
	base time.Time
	slot int

	mu      sync.Mutex
	counts  map[string]int
	total   atomic.Int64
	resolve sync.Mutex // Resolve is not safe for concurrent use of the caches
}

// NewUniverse returns a universe with a signed root (ECDSA P-256 keys)
// whose signatures are valid around Now.
func NewUniverse(t testing.TB) *Universe { return newUniverse(t, false, Now) }

// NewUniverseAt returns a universe whose signatures are valid from a day
// before base to 30 days after it (tests that run on the clock, or in a
// synctest bubble).
func NewUniverseAt(t testing.TB, base time.Time) *Universe { return newUniverse(t, false, base) }

// NewForeignUniverseAt is NewUniverseAt with keys of its own (a root no
// anchor of the other universes matches).
func NewForeignUniverseAt(t testing.TB, base time.Time) *Universe {
	u := &Universe{t: t, Zones: map[string]*Zone{}, counts: map[string]int{}, base: base, slot: 2}
	u.Root = u.AddZone(".", true)
	return u
}

// NewRSAUniverse returns a universe whose signed zones get RSA-2048 KSKs
// (and ECDSA P-256 ZSKs).
func NewRSAUniverse(t testing.TB) *Universe { return newUniverse(t, true, Now) }

func newUniverse(t testing.TB, rsa bool, base time.Time) *Universe {
	u := &Universe{t: t, Zones: map[string]*Zone{}, counts: map[string]int{}, RSAKSK: rsa, base: base}
	u.Root = u.AddZone(".", true)
	return u
}

// Anchors are the trust anchors of the universe (the root KSK's DS).
func (u *Universe) Anchors() []*dns.DS { return []*dns.DS{u.Root.KSK.Key.ToDS(dns.SHA256)} }

// AddZone adds a zone (signed or not) with a SOA at the apex.
func (u *Universe) AddZone(name string, signed bool) *Zone {
	name = Canon(name)
	z := &Zone{u: u, Name: name, Signed: signed, Data: map[string]map[uint16][]dns.RR{}, sigCache: map[string]*dns.RRSIG{},
		SigInc: u.base.Add(-24 * time.Hour), SigExp: u.base.Add(30 * 24 * time.Hour), DSDigest: dns.SHA256}
	host := strings.TrimPrefix(name, ".")
	z.Add(&dns.SOA{Hdr: Hdr(name, dns.TypeSOA, 3600), Ns: "ns." + host, Mbox: "hostmaster." + host,
		Serial: 1, Refresh: 3600, Retry: 600, Expire: 86400, Minttl: 300})
	if signed {
		if u.RSAKSK {
			z.KSK = NewSigner(u.t, name, dns.RSASHA256, 2048, 257, 1+u.slot)
		} else {
			z.KSK = NewSigner(u.t, name, dns.ECDSAP256SHA256, 256, 257, 1+u.slot)
		}
		z.ZSK = NewSigner(u.t, name, dns.ECDSAP256SHA256, 256, 256, 2+u.slot)
		z.Data[name][dns.TypeDNSKEY] = []dns.RR{z.KSK.Key, z.ZSK.Key}
	}
	u.Zones[name] = z
	return z
}

// AddChild adds a zone below parent with its delegation (NS) and, when both
// are signed, its DS records in the parent.
func (u *Universe) AddChild(parent *Zone, name string, signed bool) *Zone {
	z := u.AddZone(name, signed)
	parent.Add(&dns.NS{Hdr: Hdr(z.Name, dns.TypeNS, 3600), Ns: "ns." + z.Name})
	if signed && parent.Signed {
		parent.SetDS(z)
	}
	return z
}

// SetDS publishes the DS record of child's KSK in its parent zone z
// (replacing earlier ones).
func (z *Zone) SetDS(child *Zone) {
	ds := child.KSK.Key.ToDS(child.DSDigest)
	ds.Hdr.Ttl = 3600
	if child.DSAlg != 0 {
		ds.Algorithm = child.DSAlg
	}
	delete(z.Data[child.Name], dns.TypeDS)
	z.Add(ds)
}

// Hdr returns an RR header of class IN.
func Hdr(name string, t uint16, ttl uint32) dns.RR_Header {
	return dns.RR_Header{Name: dns.Fqdn(name), Rrtype: t, Class: dns.ClassINET, Ttl: ttl}
}

// Add adds a record to the zone.
func (z *Zone) Add(rr dns.RR) {
	h := rr.Header()
	h.Name = Canon(h.Name)
	if z.Data[h.Name] == nil {
		z.Data[h.Name] = map[uint16][]dns.RR{}
	}
	z.Data[h.Name][h.Rrtype] = append(z.Data[h.Name][h.Rrtype], rr)
	z.ClearSigs()
}

// A adds an A record (TTL 300).
func (z *Zone) A(name, ip string) { z.Add(MustRR(name + " 300 IN A " + ip)) }

// ClearSigs drops the cached signatures (after SigInc or SigExp changed).
func (z *Zone) ClearSigs() {
	z.u.resolve.Lock()
	defer z.u.resolve.Unlock()
	clear(z.sigCache)
}

// MustRR parses a record in presentation form (panics on an error).
func MustRR(s string) dns.RR {
	rr, err := dns.NewRR(s)
	if err != nil {
		panic(err)
	}
	return rr
}

// Sign returns the RRSIG over rrs: the KSK signs the DNSKEY RRset, the ZSK
// everything else.
func (z *Zone) Sign(rrs []dns.RR) *dns.RRSIG {
	h := rrs[0].Header()
	var key strings.Builder
	fmt.Fprintf(&key, "%s/%d", h.Name, h.Rrtype)
	for _, rr := range rrs {
		key.WriteString("/" + rr.String())
	}
	if s, ok := z.sigCache[key.String()]; ok {
		return dns.Copy(s).(*dns.RRSIG)
	}
	k := z.ZSK
	if h.Rrtype == dns.TypeDNSKEY {
		k = z.KSK
	}
	s := k.Sig(rrs, z.SigInc, z.SigExp)
	z.sigCache[key.String()] = s
	return dns.Copy(s).(*dns.RRSIG)
}

// WithSig returns copies of rrs with their RRSIG (rrs alone in an unsigned
// zone).
func (z *Zone) WithSig(rrs []dns.RR) []dns.RR {
	out := CopyRRs(rrs)
	if z.Signed {
		out = append(out, z.Sign(rrs))
	}
	return out
}

// CopyRRs returns deep copies of rrs.
func CopyRRs(rrs []dns.RR) []dns.RR {
	out := make([]dns.RR, len(rrs))
	for i, rr := range rrs {
		out[i] = dns.Copy(rr)
	}
	return out
}

// cut reports whether name is a delegation point of z (NS below the apex).
func (z *Zone) cut(name string) bool { return name != z.Name && z.Data[name][dns.TypeNS] != nil }

// insecureCut reports a delegation without DS.
func (z *Zone) insecureCut(name string) bool { return z.cut(name) && z.Data[name][dns.TypeDS] == nil }

// owners returns the names of the zone's chain: every owner that is not
// below a delegation point (occluded), sorted canonically.
func (z *Zone) owners() []string {
	var out []string
	for n := range z.Data {
		occluded := false
		for p := Parent(n); p != "" && Below(z.Name, p) && p != z.Name; p = Parent(p) {
			if z.cut(p) {
				occluded = true
				break
			}
		}
		if !occluded {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return Compare(out[i], out[j]) < 0 })
	return out
}

// exists reports whether name exists in the zone (records or an empty
// non-terminal).
func (z *Zone) exists(name string) bool {
	for _, o := range z.owners() {
		if Below(name, o) {
			return true
		}
	}
	return false
}

// types returns the type bitmap of name in the chain.
func (z *Zone) types(name string) []uint16 {
	var out []uint16
	for t := range z.Data[name] {
		if z.cut(name) && t != dns.TypeNS && t != dns.TypeDS {
			continue
		}
		out = append(out, t)
	}
	switch {
	case !z.NSEC3: // the NSEC record itself is signed
		out = append(out, dns.TypeNSEC, dns.TypeRRSIG)
	case len(out) > 0 && (!z.cut(name) || z.Data[name][dns.TypeDS] != nil):
		out = append(out, dns.TypeRRSIG)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// nsecOf returns the NSEC record of owner (which must be in the chain).
func (z *Zone) nsecOf(owner string) *dns.NSEC {
	owners := z.owners()
	i := slices.Index(owners, owner)
	next := owners[(i+1)%len(owners)]
	return &dns.NSEC{Hdr: Hdr(owner, dns.TypeNSEC, 300), NextDomain: next, TypeBitMap: z.types(owner)}
}

// nsecCovering returns the NSEC record whose span covers name.
func (z *Zone) nsecCovering(name string) *dns.NSEC {
	owners := z.owners()
	prev := owners[len(owners)-1]
	for _, o := range owners {
		if Compare(o, name) >= 0 {
			break
		}
		prev = o
	}
	return z.nsecOf(prev)
}

// chain3 returns the NSEC3 chain: the sorted hashes and hash → name.
func (z *Zone) chain3() ([]string, map[string]string) {
	names := map[string]bool{}
	for _, o := range z.owners() {
		if z.OptOut && z.insecureCut(o) {
			continue
		}
		for n := o; n != "" && Below(z.Name, n); n = Parent(n) {
			names[n] = true
			if n == z.Name {
				break
			}
		}
	}
	byHash := map[string]string{}
	var hashes []string
	for n := range names {
		h := dns.HashName(n, dns.SHA1, z.Iterations, z.Salt)
		byHash[h] = n
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	return hashes, byHash
}

// nsec3Rec builds the NSEC3 record at position i of the chain.
func (z *Zone) nsec3Rec(hashes []string, byHash map[string]string, i int) *dns.NSEC3 {
	var flags uint8
	if z.OptOut {
		flags = 1
	}
	owner := strings.ToLower(hashes[i]) + "." + z.Name
	if z.Name == "." {
		owner = strings.ToLower(hashes[i]) + "."
	}
	return &dns.NSEC3{Hdr: Hdr(owner, dns.TypeNSEC3, 300), Hash: dns.SHA1, Flags: flags,
		Iterations: z.Iterations, SaltLength: uint8(len(z.Salt) / 2), Salt: z.Salt, HashLength: 20,
		NextDomain: hashes[(i+1)%len(hashes)], TypeBitMap: z.types(byHash[hashes[i]])}
}

// nsec3Match returns the NSEC3 record of name (nil if name is not in the
// chain).
func (z *Zone) nsec3Match(name string) *dns.NSEC3 {
	hashes, byHash := z.chain3()
	h := dns.HashName(name, dns.SHA1, z.Iterations, z.Salt)
	if i := slices.Index(hashes, h); i >= 0 {
		return z.nsec3Rec(hashes, byHash, i)
	}
	return nil
}

// nsec3Cover returns the NSEC3 record whose span covers the hash of name.
func (z *Zone) nsec3Cover(name string) *dns.NSEC3 {
	hashes, byHash := z.chain3()
	h := dns.HashName(name, dns.SHA1, z.Iterations, z.Salt)
	i := sort.SearchStrings(hashes, h) - 1
	if i < 0 {
		i = len(hashes) - 1
	}
	return z.nsec3Rec(hashes, byHash, i)
}

// closestEncloser returns the longest existing ancestor of name and the
// next closer name.
func (z *Zone) closestEncloser(name string) (string, string) {
	next := name
	for n := Parent(name); n != ""; n = Parent(n) {
		if z.exists(n) || n == z.Name {
			return n, next
		}
		next = n
	}
	return z.Name, next
}

// proof adds records with their RRSIGs to the authority section.
func (z *Zone) proof(m *dns.Msg, rrs ...dns.RR) {
	seen := map[string]bool{}
	for _, rr := range m.Ns {
		seen[rr.String()] = true
	}
	for _, rr := range rrs {
		if rr == nil || seen[rr.String()] {
			continue
		}
		seen[rr.String()] = true
		m.Ns = append(m.Ns, z.WithSig([]dns.RR{rr})...)
	}
}

// negative fills the authority section of a negative answer for (name,
// qtype): the SOA and the NSEC/NSEC3 proof.
func (z *Zone) negative(m *dns.Msg, name string, qtype uint16, nx bool) {
	m.Ns = append(m.Ns, z.WithSig(z.Data[z.Name][dns.TypeSOA])...)
	if !z.Signed {
		return
	}
	ce, nc := z.closestEncloser(name)
	wildcardNoData := !nx && !z.exists(name)
	if !z.NSEC3 {
		switch {
		case nx:
			z.proof(m, z.nsecCovering(name), z.nsecCovering(wildcard(ce)))
		case wildcardNoData:
			z.proof(m, z.nsecCovering(name), z.nsecOf(wildcard(ce)))
		case z.Data[name] != nil:
			z.proof(m, z.nsecOf(name))
		default: // empty non-terminal
			z.proof(m, z.nsecCovering(name))
		}
		return
	}
	var rrs []dns.RR
	switch {
	case nx:
		rrs = []dns.RR{z.nsec3Match(ce), z.nsec3Cover(nc), z.nsec3Cover(wildcard(ce))}
	case wildcardNoData:
		rrs = []dns.RR{z.nsec3Match(ce), z.nsec3Cover(nc), z.nsec3Match(wildcard(ce))}
	default:
		if r := z.nsec3Match(name); r != nil {
			rrs = []dns.RR{r}
		} else { // an insecure delegation left out of an opt-out chain
			ce, nc := z.closestEncloser(name)
			rrs = []dns.RR{z.nsec3Match(ce), z.nsec3Cover(nc)}
		}
	}
	for _, rr := range rrs {
		if r, ok := rr.(*dns.NSEC3); ok && r != nil {
			z.proof(m, r)
		}
	}
}

// zoneFor returns the zone that answers (name, qtype): the deepest zone at
// or above name; DS at a zone apex is answered by the parent.
func (u *Universe) zoneFor(name string, qtype uint16) *Zone {
	for n := name; n != ""; n = Parent(n) {
		if z := u.Zones[n]; z != nil {
			if qtype == dns.TypeDS && n == name && n != "." {
				continue
			}
			return z
		}
	}
	return u.Root
}

// Resolve answers a query for (qname, qtype) like a recursive resolver
// with DNSSEC data (then Tamper runs).
func (u *Universe) Resolve(qname string, qtype uint16) *dns.Msg {
	u.resolve.Lock()
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(qname), qtype)
	m.Response, m.RecursionAvailable = true, true
	name := Canon(qname)
	for range 10 {
		z := u.zoneFor(name, qtype)
		data := z.Data[name]
		if rrs := data[qtype]; rrs != nil && (qtype != dns.TypeNS || z.cut(name) || name == z.Name) {
			m.Answer = append(m.Answer, z.WithSig(rrs)...)
			break
		}
		if rrs := data[dns.TypeCNAME]; rrs != nil && qtype != dns.TypeCNAME {
			m.Answer = append(m.Answer, z.WithSig(rrs)...)
			name = Canon(rrs[0].(*dns.CNAME).Target)
			continue
		}
		if z.exists(name) {
			z.negative(m, name, qtype, false)
			break
		}
		ce, nc := z.closestEncloser(name)
		if wc := z.Data[wildcard(ce)]; wc != nil {
			if rrs := wc[qtype]; rrs != nil {
				sig := z.Sign(rrs)
				for _, rr := range CopyRRs(rrs) {
					rr.Header().Name = dns.Fqdn(name)
					m.Answer = append(m.Answer, rr)
				}
				if z.Signed {
					sig.Hdr.Name = dns.Fqdn(name)
					m.Answer = append(m.Answer, sig)
					if z.NSEC3 {
						z.proof(m, z.nsec3Cover(nc))
					} else {
						z.proof(m, z.nsecCovering(name))
					}
				}
				break
			}
			z.negative(m, name, qtype, false)
			break
		}
		m.Rcode = dns.RcodeNameError
		z.negative(m, name, qtype, true)
		break
	}
	u.resolve.Unlock()
	if u.Tamper != nil {
		u.Tamper(Canon(qname), qtype, m)
	}
	return m
}

// Serve answers a lookup (counted per name and type) after Delay.
func (u *Universe) Serve(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	u.mu.Lock()
	u.counts[Canon(name)+"/"+dns.TypeToString[qtype]]++
	u.mu.Unlock()
	u.total.Add(1)
	if u.Delay > 0 {
		select {
		case <-time.After(u.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return u.Resolve(name, qtype), nil
}

// Reply answers a query message like an upstream (counted): the answer of
// Resolve with q's ID and question; the OPT echoes DO. It is the handler of
// fake upstream transports.
func (u *Universe) Reply(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	qq := q.Question[0]
	m, err := u.Serve(ctx, qq.Name, qq.Qtype)
	if err != nil {
		return nil, err
	}
	m.Id, m.Question, m.RecursionDesired = q.Id, []dns.Question{qq}, q.RecursionDesired
	do := false
	if opt := q.IsEdns0(); opt != nil {
		do = opt.Do()
		m.SetEdns0(1232, do)
	}
	if !do {
		m.Answer, m.Ns = dropDNSSEC(m.Answer), dropDNSSEC(m.Ns)
	}
	return m, nil
}

func dropDNSSEC(rrs []dns.RR) []dns.RR {
	var out []dns.RR
	for _, rr := range rrs {
		switch rr.Header().Rrtype {
		case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3:
			continue
		}
		out = append(out, rr)
	}
	return out
}

// Count returns the lookups of (name, qtype) so far.
func (u *Universe) Count(name string, qtype uint16) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.counts[Canon(name)+"/"+dns.TypeToString[qtype]]
}

// Total returns the lookups so far.
func (u *Universe) Total() int64 { return u.total.Load() }

// Example builds the common universe: com (NSEC3) with the signed
// example.com (NSEC; A records at the apex and www) and the insecure
// delegation unsigned.com (www).
func Example(t testing.TB) (u *Universe, com, ex *Zone) { return ExampleAt(t, Now) }

// ExampleAt is Example with signatures valid around base.
func ExampleAt(t testing.TB, base time.Time) (u *Universe, com, ex *Zone) {
	u = NewUniverseAt(t, base)
	com = u.AddChild(u.Root, "com", true)
	com.NSEC3 = true
	ex = u.AddChild(com, "example.com", true)
	ex.A("example.com", "192.0.2.1")
	ex.A("www.example.com", "192.0.2.2")
	un := u.AddChild(com, "unsigned.com", false)
	un.A("www.unsigned.com", "192.0.2.3")
	return u, com, ex
}

// --- names ---

// Canon lower-cases the ASCII letters of a name and makes it fully
// qualified.
func Canon(name string) string { return dns.CanonicalName(name) }

// Parent returns the parent of name ("" for the root).
func Parent(name string) string {
	if name == "." || name == "" {
		return ""
	}
	off, end := dns.NextLabel(name, 0)
	if end || off >= len(name) {
		return "."
	}
	return name[off:]
}

// wildcard returns the wildcard name at the closest encloser ce ("*." at
// the root).
func wildcard(ce string) string {
	if ce == "." {
		return "*."
	}
	return "*." + ce
}

// Below reports whether child is equal to or below zone.
func Below(zone, child string) bool { return dns.IsSubDomain(zone, child) }

// Compare orders names canonically (RFC 4034 6.1).
func Compare(a, b string) int {
	la, lb := labels(a), labels(b)
	i, j := len(la)-1, len(lb)-1
	for i >= 0 && j >= 0 {
		if c := bytes.Compare(la[i], lb[j]); c != 0 {
			return c
		}
		i--
		j--
	}
	switch {
	case i < 0 && j < 0:
		return 0
	case i < 0:
		return -1
	}
	return 1
}

func labels(name string) [][]byte {
	var buf [256]byte
	n, err := dns.PackDomainName(dns.Fqdn(name), buf[:], 0, nil, false)
	if err != nil {
		return nil
	}
	var out [][]byte
	for off := 0; off < n && buf[off] != 0; off += 1 + int(buf[off]) {
		l := slices.Clone(buf[off+1 : off+1+int(buf[off])])
		for i, c := range l {
			if 'A' <= c && c <= 'Z' {
				l[i] = c + 'a' - 'A'
			}
		}
		out = append(out, l)
	}
	return out
}
