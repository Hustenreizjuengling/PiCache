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
	ip, iface, err := parseProcRoute(strings.NewReader(in))
	if err != nil || ip.String() != "192.168.178.1" || iface != "eth0" {
		t.Fatalf("got %v %v", ip, err)
	}
}

func TestParseIPv6Route(t *testing.T) {
	const zero = "00000000000000000000000000000000"
	in := "fd000000000000000000000000000000 40 " + zero + " 00 " + zero + " 00000100 00000001 00000000 00000001 eth0\n" +
		zero + " 00 " + zero + " 00 fe80000000000000021122fffe334455 00000400 00000001 00000000 00000003 eth0\n" +
		zero + " 00 " + zero + " 00 fe800000000000000000000000000001 00000100 00000001 00000000 00000003 wlan0\n" +
		zero + " 00 " + zero + " 00 " + zero + " ffffffff 00000001 00000000 00200200 lo\n" +
		"garbage\n"
	ip, err := parseIPv6Route(strings.NewReader(in))
	if err != nil || ip.String() != "fe80::1%wlan0" {
		t.Fatalf("got %v %v, want the default route with the lowest metric", ip, err)
	}
	if _, err := parseIPv6Route(strings.NewReader(zero + " 00 " + zero + " 00 " + zero + " ffffffff 00000001 00000000 00200200 lo\n")); err == nil {
		t.Fatal("an unreachable default route is no gateway")
	}
}

func TestParseResolvConfSearch(t *testing.T) {
	in := "# generated\nnameserver 127.0.0.53\nsearch fritz.box Home.ARPA. # comment\ndomain fritz.box\n"
	got := parseResolvConfSearch(strings.NewReader(in))
	if !slices.Equal(got, []string{"fritz.box", "home.arpa"}) {
		t.Fatalf("got %v", got)
	}
}
