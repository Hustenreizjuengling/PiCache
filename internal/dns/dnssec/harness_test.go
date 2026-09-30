package dnssec

import (
	"context"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/dns/dnssec/dnssectest"
)

// The tests validate answers of signed zones built in memory
// (dnssectest): no test touches the network.

var testNow = dnssectest.Now

// lookupOf is the validator's LookupFunc over a universe.
func lookupOf(u *dnssectest.Universe) LookupFunc {
	return func(ctx context.Context, name string, qtype uint16) (Reply, error) {
		m, err := u.Serve(ctx, name, qtype)
		return Reply{Msg: m}, err
	}
}

// validatorFor returns a Validator anchored at the universe's root.
func validatorFor(u *dnssectest.Universe) *Validator { return New(Config{Anchors: u.Anchors()}) }

// request builds a validation request for (name, qtype) answered by the
// universe.
func request(u *dnssectest.Universe, name string, qtype uint16) *Request {
	return &Request{Route: "test", Name: dns.Fqdn(name), Type: qtype, Msg: u.Resolve(name, qtype), Lookup: lookupOf(u),
		Now: testNow, TimeChecks: true}
}

// check validates (name, qtype) and compares status, EDE and reason.
func check(t *testing.T, u *dnssectest.Universe, v *Validator, name string, qtype uint16, status string, ede int, reason string) Result {
	t.Helper()
	res := v.Validate(context.Background(), request(u, name, qtype))
	if res.Status != status || res.EDE != ede || (reason != "" && res.Reason != reason) {
		t.Fatalf("%s %s: got %s EDE %d %q (zone %q), want %s EDE %d %q", name, dns.TypeToString[qtype], res.Status, res.EDE,
			res.Reason, res.Zone, status, ede, reason)
	}
	return res
}

var mustRR = dnssectest.MustRR
