package clients

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// Neighbours lists entries in REACHABLE, STALE, DELAY, PROBE and PERMANENT
// only, with a unicast MAC and the interface name.
func TestParseNeighbours(t *testing.T) {
	mac := func(last byte) []byte { return []byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, last} }
	var dump []byte
	for _, n := range []struct {
		state  uint16
		ip     string
		lladdr []byte
	}{
		{nudReachable, "192.168.1.30", mac(1)},
		{nudStale, "fd00::30", mac(1)},
		{nudDelay, "192.168.1.31", mac(2)},
		{nudProbe, "fe80::31", mac(2)},
		{nudPermanent, "192.168.1.1", mac(3)},
		{0x00, "192.168.1.40", mac(4)}, // NUD_NONE
		{nudIncomplete, "192.168.1.41", nil},
		{nudFailed, "192.168.1.42", mac(5)},
		{nudReachable, "192.168.1.44", make([]byte, 6)},                            // zero MAC
		{nudReachable, "192.168.1.45", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}}, // broadcast
		{nudReachable, "192.168.1.46", []byte{0x01, 0x00, 0x5e, 0x00, 0x00, 0xfb}}, // multicast
		{nudNoARP, "ff02::fb", []byte{0x33, 0x33, 0x00, 0x00, 0x00, 0xfb}},
		{nudStale, "192.168.1.30", mac(9)}, // duplicate address: the first entry wins
	} {
		dump = append(dump, nlNeigh(rtmNewNeigh, n.state, ip(n.ip), n.lladdr)...)
	}
	names := func(i int) string {
		if i == 2 {
			return "eth0"
		}
		return ""
	}
	got := parseNeighbours(dump, names)
	var ips []string
	for _, n := range got {
		ips = append(ips, n.IP.String())
		if n.Iface != "eth0" || n.MAC == "" {
			t.Errorf("entry %+v", n)
		}
	}
	want := []string{"192.168.1.30", "fd00::30", "192.168.1.31", "fe80::31", "192.168.1.1"}
	if !slices.Equal(ips, want) {
		t.Fatalf("got %v, want %v", ips, want)
	}
	if got[0].MAC != "aa:bb:cc:dd:ee:01" {
		t.Errorf("MAC %q", got[0].MAC)
	}
	for i := range dump {
		parseNeighbours(dump[:i], names) // truncated input never panics
	}
}

func TestParseARPNeighbours(t *testing.T) {
	const table = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.1      0x1         0x2         AA:BB:CC:DD:EE:01     *        eth0
192.168.1.50     0x1         0x0         00:00:00:00:00:00     *        eth0
192.168.1.51     0x1         0x2         01:00:5e:00:00:fb     *        eth0
192.168.1.52     0x1         0x6         aa:bb:cc:dd:ee:02     *        wlan0
192.168.1.53     0x1         0x2         aa:bb:cc:dd:ee:03
garbage
`
	got := parseARPNeighbours(strings.NewReader(table))
	want := []Neighbour{
		{IP: ip("192.168.1.1"), MAC: "aa:bb:cc:dd:ee:01", Iface: "eth0"},
		{IP: ip("192.168.1.52"), MAC: "aa:bb:cc:dd:ee:02", Iface: "wlan0"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestNeighboursAndDescribe(t *testing.T) {
	r := newTestRegistry(t, false)
	ctx := context.Background()
	r.readNeighbours = func() ([]Neighbour, error) {
		return []Neighbour{{IP: ip("192.168.1.30"), MAC: "aa:bb:cc:dd:ee:01", Iface: "eth0"}}, nil
	}
	got, err := r.Neighbours(ctx)
	if err != nil || len(got) != 1 || got[0].IP != ip("192.168.1.30") {
		t.Fatalf("neighbours %+v %v", got, err)
	}
	r.readNeighbours = func() ([]Neighbour, error) { return nil, errors.New("netlink: permission denied") }
	if got, err := r.Neighbours(ctx); err == nil || got == nil || len(got) != 0 {
		t.Fatalf("error: %+v %v", got, err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Neighbours(cctx); err == nil {
		t.Fatal("a cancelled context must fail")
	}

	tv := mustClient(t, r, "tv", nil, "aa:bb:cc:dd:ee:02")
	nas := mustClient(t, r, "nas", nil, "192.168.1.5")
	r.names.put(ip("192.168.1.99"), hostName{name: "printer.lan", at: time.Now()})
	for _, tc := range []struct {
		ip, mac  string
		id       int64
		name, hn string
	}{
		{"fd00::1234", "aa:bb:cc:dd:ee:02", tv.ID, "tv", ""}, // by MAC, whatever the address
		{"192.168.1.5", "", nas.ID, "nas", ""},               // by IP
		{"::ffff:192.168.1.5", "", nas.ID, "nas", ""},        // canonical
		{"192.168.1.99", "aa:bb:cc:dd:ee:09", 0, "", "printer.lan"},
		{"192.168.1.98", "", 0, "", ""},
	} {
		id, name, hn := r.Describe(ip(tc.ip), tc.mac)
		if id != tc.id || name != tc.name || hn != tc.hn {
			t.Errorf("Describe(%s, %s) = %d %q %q", tc.ip, tc.mac, id, name, hn)
		}
	}
}
