package netutil

import (
	"slices"
	"strings"
	"testing"
)

func TestParseProcRoute(t *testing.T) {
	in := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n" +
		"eth0\t00000000\t01B2A8C0\t0003\t0\t0\t100\t00000000\n"
	ip, err := parseProcRoute(strings.NewReader(in))
	if err != nil || ip.String() != "192.168.178.1" {
		t.Fatalf("got %v %v", ip, err)
	}
}

func TestParseResolvConfSearch(t *testing.T) {
	in := "# generated\nnameserver 127.0.0.53\nsearch fritz.box Home.ARPA. # comment\ndomain fritz.box\n"
	got := parseResolvConfSearch(strings.NewReader(in))
	if !slices.Equal(got, []string{"fritz.box", "home.arpa"}) {
		t.Fatalf("got %v", got)
	}
}
