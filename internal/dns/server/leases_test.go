package dnsserver

import (
	"context"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"
)

// fakeLeases answers laptop.lan → 192.168.1.100 (TTL 120) and its PTR.
type fakeLeases struct{ calls []string }

func (f *fakeLeases) LeaseAddr(name string) (netip.Addr, uint32, bool) {
	f.calls = append(f.calls, name)
	if name == "laptop.lan" {
		return netip.MustParseAddr("192.168.1.100"), 120, true
	}
	return netip.Addr{}, 0, false
}

func (f *fakeLeases) LeasePTR(ip netip.Addr) (string, uint32, bool) {
	if ip == netip.MustParseAddr("192.168.1.100") {
		return "laptop.lan", 120, true
	}
	return "", 0, false
}

// The names of DHCP leases are part of the local zone (step 7): A and PTR
// with the lease's TTL, NODATA for other types, never forwarded; local
// records win; CNAME targets can be lease names; Lookup traces them.
func TestLeaseNames(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.d.Leases = &fakeLeases{}
	e.serve()

	r := e.query("udp", "laptop.lan", dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || !r.Authoritative || !slices.Equal(answerIPs(r.Answer), []string{"192.168.1.100"}) || r.Answer[0].Header().Ttl != 120 {
		t.Fatalf("A: %v", r)
	}
	if r := e.query("udp", "laptop.lan", dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 || len(r.Ns) != 1 {
		t.Fatalf("AAAA: %v", r)
	}
	r = e.query("udp", "100.1.168.192.in-addr.arpa", dns.TypePTR)
	if len(r.Answer) != 1 || r.Answer[0].(*dns.PTR).Ptr != "laptop.lan." || r.Answer[0].Header().Ttl != 120 {
		t.Fatalf("PTR: %v", r)
	}
	if calls := e.up.callsFor("laptop.lan"); len(calls) != 0 {
		t.Fatalf("lease name forwarded: %v", calls)
	}
	// A local CNAME pointing at a lease name.
	e.addRecord("printer.lan", "CNAME", "laptop.lan")
	if r := e.query("udp", "printer.lan", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"192.168.1.100"}) {
		t.Fatalf("CNAME to a lease name: %v", r)
	}
	// Local records win: the same name, and the auto-PTR of the address.
	e.addRecord("laptop.lan", "A", "10.9.9.9")
	e.addRecord("other.lan", "A", "192.168.1.100")
	if r := e.query("udp", "laptop.lan", dns.TypeA); !slices.Equal(answerIPs(r.Answer), []string{"10.9.9.9"}) {
		t.Fatalf("local record lost: %v", r)
	}
	r = e.query("udp", "100.1.168.192.in-addr.arpa", dns.TypePTR)
	if len(r.Answer) != 1 || r.Answer[0].(*dns.PTR).Ptr != "other.lan." {
		t.Fatalf("auto-PTR lost: %v", r)
	}
}

func TestLeaseNameLookupTrace(t *testing.T) {
	e := newEnv(t, nil)
	e.srv.d.Leases = &fakeLeases{}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "laptop.lan"}, netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusLocal || !slices.Equal(res.Answers, []string{"laptop.lan.\t120\tIN\tA\t192.168.1.100"}) ||
		!slices.ContainsFunc(res.Steps, func(s string) bool { return strings.Contains(s, "DHCP lease name") }) {
		t.Fatalf("%+v", res)
	}
}
