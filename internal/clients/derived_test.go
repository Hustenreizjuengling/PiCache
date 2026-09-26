package clients

import (
	"net/netip"
	"slices"
	"testing"
)

// IdentifyDerived: the usual order for an address from EDNS (with the EDNS
// MAC, else the neighbour table's MAC of the address), only the MAC steps
// for a MAC alone, and nothing is learned from EDNS MACs.
func TestIdentifyDerived(t *testing.T) {
	r := newTestRegistry(t, false)
	r.gateways = func() []netip.Addr { return nil }
	kids := mustGroup(t, r, "Kids", true)
	laptop := mustClient(t, r, "laptop", []int64{kids.ID}, "192.168.1.5")
	tv := mustClient(t, r, "tv", nil, "aa:00:00:00:00:10")
	lan := mustClient(t, r, "guest net", nil, "192.168.9.0/24")
	learnedPhone := mustClient(t, r, "phone", nil, "192.168.1.6")
	setNeighbours(r, map[netip.Addr]string{
		ip("192.168.1.6"): "aa:00:00:00:00:06", ip("fd00::6"): "aa:00:00:00:00:06", // learned MAC of the phone
		ip("192.168.1.20"): "aa:00:00:00:00:10", // the TV's address in the neighbour table
	})
	for _, tc := range []struct {
		name   string
		ip     string
		mac    string
		client int64
		gotIP  string
		gotMAC string
	}{
		{"exact IP", "192.168.1.5", "", laptop.ID, "192.168.1.5", ""},
		{"exact IP beats the MAC", "192.168.1.5", "aa:00:00:00:00:10", laptop.ID, "192.168.1.5", "aa:00:00:00:00:10"},
		{"CIDR", "192.168.9.44", "", lan.ID, "192.168.9.44", ""},
		{"EDNS MAC", "192.168.1.99", "aa:00:00:00:00:10", tv.ID, "192.168.1.99", "aa:00:00:00:00:10"},
		{"neighbour MAC of the address", "192.168.1.20", "", tv.ID, "192.168.1.20", "aa:00:00:00:00:10"},
		{"learned MAC", "fd00::99", "aa:00:00:00:00:06", learnedPhone.ID, "fd00::99", "aa:00:00:00:00:06"},
		{"unknown address", "192.168.1.77", "", 0, "192.168.1.77", ""},
		{"MAC only", "", "aa:00:00:00:00:10", tv.ID, "", "aa:00:00:00:00:10"},
		{"MAC only, learned", "", "aa:00:00:00:00:06", learnedPhone.ID, "", "aa:00:00:00:00:06"},
		{"unknown MAC only", "", "aa:00:00:00:00:99", 0, "", "aa:00:00:00:00:99"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a netip.Addr
			if tc.ip != "" {
				a = ip(tc.ip)
			}
			id := r.IdentifyDerived(a, tc.mac)
			gotIP := ""
			if id.IP.IsValid() {
				gotIP = id.IP.String()
			}
			if id.ClientID != tc.client || gotIP != tc.gotIP || id.MAC != tc.gotMAC {
				t.Fatalf("got %+v", id)
			}
			if tc.client == 0 && !slices.Equal(id.GroupIDs, []int64{DefaultGroupID}) {
				t.Errorf("groups %v, want Default", id.GroupIDs)
			}
		})
	}
	if id := r.IdentifyDerived(ip("192.168.1.5"), ""); !slices.Equal(id.GroupIDs, []int64{kids.ID}) || id.Name != "laptop" {
		t.Errorf("groups/name of the laptop: %+v", id)
	}
	// Nothing was learned: the EDNS MAC of the TV is not in the neighbour
	// table, and the address it was claimed for is not identified by it.
	if _, ok := r.NeighbourMAC(ip("192.168.1.99")); ok {
		t.Error("an EDNS MAC entered the neighbour table")
	}
	if id := r.Identify(ip("192.168.1.99")); id.ClientID != 0 || id.MAC != "" {
		t.Errorf("Identify after IdentifyDerived: %+v", id)
	}
}
