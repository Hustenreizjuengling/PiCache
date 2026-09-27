package clients

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestClientIDIdentifiers(t *testing.T) {
	for in, want := range map[string]string{
		"clientid:phone": "clientid:phone", " ClientID:Kid-Tablet ": "clientid:kid-tablet", "CLIENTID:A1": "clientid:a1",
	} {
		id, ok := parseIdentifier(in)
		if !ok || id.kind != kindClientID || id.value != want {
			t.Fatalf("%q: %+v %v", in, id, ok)
		}
	}
	for _, in := range []string{"clientid:", "clientid:-a", "clientid:a.b", "clientid:" + strings.Repeat("x", 64), "client:abc"} {
		if _, ok := parseIdentifier(in); ok {
			t.Fatalf("%q accepted", in)
		}
	}

	r := newTestRegistry(t, false)
	ctx := context.Background()
	kids := mustGroup(t, r, "Kids", true)
	c := mustClient(t, r, "Tablet", []int64{kids.ID}, "ClientID:Tablet", "clientid:tablet")
	if len(c.Identifiers) != 1 || c.Identifiers[0] != "clientid:tablet" {
		t.Fatalf("identifiers %v", c.Identifiers)
	}
	// A ClientID belongs to one client.
	_, err := r.CreateClient(ctx, ClientInput{Name: "Other", Identifiers: []string{"clientid:TABLET"}})
	var ce *apperr.Error
	if !errors.As(err, &ce) || ce.Kind != apperr.KindConflict || !strings.Contains(ce.Message, "already used") {
		t.Fatalf("duplicate: %v", err)
	}
	_, err = r.CreateClient(ctx, ClientInput{Name: "Bad", Identifiers: []string{"clientid:bad_id"}})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Field != "identifiers[0]" ||
		ae.Message != "must be an IP address, a CIDR (e.g. 192.168.1.0/24), a MAC address, clientid:<ClientID>, iface:<interface> or host:<name>" {
		t.Fatalf("invalid: %v", err)
	}
	_, err = r.CreateClient(ctx, ClientInput{Name: "None"})
	if !errors.As(err, &ae) || ae.Message != "at least one IP address, CIDR, MAC address or ClientID is required" {
		t.Fatalf("none: %v", err)
	}

	id, ok := r.IdentifyDNSClientID("tablet")
	if !ok || id.ClientID != c.ID || id.Name != "Tablet" || len(id.GroupIDs) != 1 || id.GroupIDs[0] != kids.ID || id.IP.IsValid() || id.MAC != "" {
		t.Fatalf("identity %+v %v", id, ok)
	}
	if _, ok := r.IdentifyDNSClientID("unknown"); ok {
		t.Fatal("unknown ClientID identified")
	}
	if _, ok := r.IdentifyDNSClientID(""); ok {
		t.Fatal("empty ClientID identified")
	}
	// A client with only a ClientID never matches an address.
	if got := r.Identify(ip("192.168.1.77")); got.ClientID != 0 {
		t.Fatalf("address identified as %d", got.ClientID)
	}
}

func TestSeenDNSClientIDs(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	tv := mustClient(t, r, "TV", nil, "clientid:tv")
	laptop := mustClient(t, r, "Laptop", nil, "192.168.1.9")
	src := ip("192.168.1.50")
	r.Seen(src)
	r.SeenDNSClientID(src, "tv")
	r.SeenDNSClientID(src, "tv")
	r.Seen(ip("192.168.1.9"))
	r.SeenDNSClientID(ip("192.168.1.9"), "tv")
	r.SeenTransient(ip("192.168.1.60"))
	r.SeenDNSClientID(ip("192.168.1.60"), "stranger")

	list := r.DNSClientIDs()
	if len(list) != 2 || list[0].DNSClientID != "stranger" || list[1].DNSClientID != "tv" {
		t.Fatalf("list %+v", list)
	}
	if tvRow := list[1]; tvRow.ClientID != tv.ID || tvRow.Name != "TV" || tvRow.Queries != 3 || tvRow.Address != "192.168.1.9" {
		t.Fatalf("tv row %+v", tvRow)
	}
	if list[0].ClientID != 0 || list[0].Name != "" {
		t.Fatalf("unknown ClientID row %+v", list[0])
	}
	// The API encodes with encoding/json/v2 (omitempty keeps a zero
	// number): the row of an unknown ClientID has no clientId or name.
	if b, err := json.Marshal(list[0]); err != nil || strings.Contains(string(b), `"clientId"`) || strings.Contains(string(b), `"name"`) {
		t.Fatalf("unknown ClientID row encoded as %s (%v)", b, err)
	}

	known, err := r.Known(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	byIP := map[string]Known{}
	for _, k := range known {
		byIP[k.IP] = k
	}
	// An address that identifies no client by itself takes the client of
	// its last ClientID; one that does keeps its own client.
	if k := byIP["192.168.1.50"]; k.DNSClientID != "tv" || k.ClientID != tv.ID || k.Name != "TV" {
		t.Fatalf("50: %+v", k)
	}
	if k := byIP["192.168.1.9"]; k.DNSClientID != "tv" || k.ClientID != laptop.ID {
		t.Fatalf("9: %+v", k)
	}
	if k := byIP["192.168.1.60"]; k.DNSClientID != "stranger" || k.ClientID != 0 {
		t.Fatalf("60: %+v", k)
	}
	devs, err := r.Devices(ctx, []string{"192.168.1.50", "192.168.1.60"})
	if err != nil {
		t.Fatal(err)
	}
	if d := devs["192.168.1.50"]; d.ClientID != tv.ID || d.Key != fmt.Sprintf("client:%d", tv.ID) {
		t.Fatalf("device %+v", d)
	}
	if d := devs["192.168.1.60"]; d.ClientID != 0 {
		t.Fatalf("device %+v", d)
	}
}

// TestSeenDNSClientIDBound: at most 1024 ClientIDs are kept; the least
// recently seen is evicted.
func TestSeenDNSClientIDBound(t *testing.T) {
	r := newTestRegistry(t, false)
	src := netip.MustParseAddr("192.168.1.2")
	for i := range maxSeenClientIDs + 10 {
		r.SeenDNSClientID(src, fmt.Sprintf("id%d", i))
	}
	r.SeenDNSClientID(src, "id10") // refreshed: survives the next evictions
	for i := range 5 {
		r.SeenDNSClientID(src, fmt.Sprintf("new%d", i))
	}
	list := r.DNSClientIDs()
	if len(list) != maxSeenClientIDs {
		t.Fatalf("len %d", len(list))
	}
	seen := map[string]bool{}
	for _, e := range list {
		seen[e.DNSClientID] = true
	}
	if !seen["id10"] || seen["id0"] || seen["id11"] || !seen["new4"] {
		t.Fatalf("eviction order wrong: id10 %v id0 %v id11 %v", seen["id10"], seen["id0"], seen["id11"])
	}
}
