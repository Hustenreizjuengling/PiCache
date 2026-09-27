package dnsserver

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"testing"

	"github.com/miekg/dns"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// A known ClientID wins over a client found by a host: identifier (host:
// comes last); DoH of a client a trusted proxy forwarded is identified
// without iface: (IdentifyForwarded).
func TestClientIDBeforeHostIdentifier(t *testing.T) {
	e, _ := encEnv(t, nil)
	const kids, adults = 5, 6
	byName := &clients.Identity{IP: netip.MustParseAddr("192.168.1.70"), ClientID: 7, Name: "Tablet by name", GroupIDs: []int64{kids}, ByHost: true}
	e.cl.set(byName)
	e.cl.byID = map[string]*clients.Identity{"dad": {ClientID: 2, Name: "Dad", GroupIDs: []int64{adults}}}
	e.serveVia(queryConn{proto: ProtoDoT, source: byName.IP, clientID: "dad"}, question("a.example", dns.TypeA))
	if got := e.flt.lastChecked("a.example"); !slices.Equal(got, []int64{adults}) {
		t.Fatalf("a ClientID must win over host:: groups %v", got)
	}
	e.serveVia(queryConn{proto: ProtoDoT, source: byName.IP}, question("b.example", dns.TypeA))
	if got := e.flt.lastChecked("b.example"); !slices.Equal(got, []int64{kids}) {
		t.Fatalf("without a ClientID host: decides: groups %v", got)
	}
	res, err := e.srv.Lookup(context.Background(), LookupRequest{Name: "x.example", ClientIP: "192.168.1.70", DNSClientID: "dad"}, netip.Addr{})
	if err != nil || !slices.Contains(res.Steps, "ClientID dad: client Dad") {
		t.Fatalf("%v %v", res.Steps, err)
	}
	e.serveVia(queryConn{proto: ProtoDoH, source: netip.MustParseAddr("192.168.1.71"), forwarded: true}, question("c.example", dns.TypeA))
	e.cl.mu.Lock()
	fw := slices.Clone(e.cl.forwarded)
	e.cl.mu.Unlock()
	if !slices.Contains(fw, netip.MustParseAddr("192.168.1.71")) {
		t.Fatalf("a forwarded client must be identified with IdentifyForwarded: %v", fw)
	}
}

// An IPv6 link-local source reaches Identify with the zone of its
// transport (the interface it arrived on: iface:) over UDP, TCP, DoT and
// DoH; the ACL and everything else use the canonical address.
func TestLinkLocalZoneReachesIdentify(t *testing.T) {
	d := newDoHEnv(t, nil)
	e := d.testEnv
	const guests = 5
	e.cl.set(&clients.Identity{IP: netip.MustParseAddr("fe80::5%vlan20"), ClientID: 3, Name: "Guest VLAN", GroupIDs: []int64{guests}})
	src := netip.MustParseAddr("fe80::5")
	for _, proto := range []string{ProtoUDP, ProtoTCP, ProtoDoT} {
		name := proto + ".example"
		e.serveVia(queryConn{proto: proto, source: src, zone: "vlan20"}, question(name, dns.TypeA))
		if got := e.flt.lastChecked(name); !slices.Equal(got, []int64{guests}) {
			t.Fatalf("%s: groups %v", proto, got)
		}
	}
	res, _ := d.post(t, "/dns-query", packQuery(t, "doh.example", 1), map[string]string{"X-Test-Source": "fe80::5%vlan20"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("DoH: %d", res.StatusCode)
	}
	if got := e.flt.lastChecked("doh.example"); !slices.Equal(got, []int64{guests}) {
		t.Fatalf("DoH: groups %v", got)
	}
	// ServeDNS takes the zone from the transport's peer address.
	if z := netutil.PeerZone(&net.UDPAddr{IP: net.ParseIP("fe80::5"), Port: 53, Zone: "vlan20"}); z != "vlan20" {
		t.Fatalf("zone %q", z)
	}
}
