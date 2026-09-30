package dnssec

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
)

func TestNewRootKeyNeedsValidDates(t *testing.T) {
	// A root DNSKEY RRset with a SEP key no anchor matches marks a new root
	// key only when its signature is valid now: an old key set replayed
	// to a probe, or validated while time checks are suspended, does not.
	u := dnssectest.NewUniverse(t)
	extra := dnssectest.NewSigner(t, ".", dns.ECDSAP256SHA256, 256, 257, 7)
	u.Root.Data["."][dns.TypeDNSKEY] = append(u.Root.Data["."][dns.TypeDNSKEY], extra.Key)
	u.Root.ClearSigs()
	k, s := u.Resolve(".", dns.TypeDNSKEY), u.Resolve(".", dns.TypeSOA)
	late := testNow.Add(60 * 24 * time.Hour)
	v := validatorFor(u)
	r := v.CheckRoot(context.Background(), k, s, late)
	if r.State != StateCapable || r.TimeOK || r.NewRootKey || v.NewRootKey() {
		t.Fatalf("expired: %+v, validator %v", r, v.NewRootKey())
	}
	if RootValidAt(r.Inception, r.Expiration, late) || !RootValidAt(r.Inception, r.Expiration, testNow) {
		t.Fatalf("validity %v – %v", r.Inception, r.Expiration)
	}
	req := request(u, ".", dns.TypeDNSKEY)
	req.Now, req.TimeChecks = late, false
	if res := v.Validate(context.Background(), req); res.Status != Indeterminate || v.NewRootKey() {
		t.Fatalf("suspended: %+v, validator %v", res, v.NewRootKey())
	}
	if r := v.CheckRoot(context.Background(), k, s, testNow); !r.TimeOK || !r.NewRootKey || !v.NewRootKey() {
		t.Fatalf("in time: %+v, validator %v", r, v.NewRootKey())
	}
}
