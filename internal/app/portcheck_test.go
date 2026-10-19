package app

import (
	"context"
	"net/netip"
	"slices"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/portcheck"
)

// Only IPv4 neighbours inside this machine's private networks are checked,
// one per MAC, never this machine's own addresses.
func TestPortTargets(t *testing.T) {
	prefixes := []netip.Prefix{netip.MustParsePrefix("192.168.1.10/24"), netip.MustParsePrefix("203.0.113.5/24"),
		netip.MustParsePrefix("fd00::10/64")}
	nbs := []clients.Neighbour{
		{IP: netip.MustParseAddr("192.168.1.30"), MAC: "aa:00:00:00:00:30"},
		{IP: netip.MustParseAddr("192.168.1.20"), MAC: "aa:00:00:00:00:20"},
		{IP: netip.MustParseAddr("192.168.1.21"), MAC: "aa:00:00:00:00:20"}, // same device
		{IP: netip.MustParseAddr("192.168.1.10"), MAC: "aa:00:00:00:00:10"}, // this machine
		{IP: netip.MustParseAddr("192.168.2.5"), MAC: "aa:00:00:00:00:05"},  // not a connected network
		{IP: netip.MustParseAddr("203.0.113.7"), MAC: "aa:00:00:00:00:07"},  // public
		{IP: netip.MustParseAddr("fd00::20"), MAC: "aa:00:00:00:00:20"},     // IPv6
		{IP: netip.MustParseAddr("::ffff:192.168.1.40"), MAC: ""},
	}
	got, err := portTargetsOf(context.Background(), prefixes, func(context.Context) ([]clients.Neighbour, error) { return nbs, nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []portcheck.Target{{IP: netip.MustParseAddr("192.168.1.20"), MAC: "aa:00:00:00:00:20"},
		{IP: netip.MustParseAddr("192.168.1.30"), MAC: "aa:00:00:00:00:30"}, {IP: netip.MustParseAddr("192.168.1.40")}}
	if !slices.Equal(got, want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	_, err = portTargetsOf(context.Background(), prefixes[1:], func(context.Context) ([]clients.Neighbour, error) { return nbs, nil })
	if apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("without a private network: %v", err)
	}
}
