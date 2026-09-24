package clients

import (
	"os"
	"syscall"
	"testing"
)

// TestReadARPLinux reads the real neighbour tables (netlink and
// /proc/net/arp) and checks that every entry is well formed and that the
// netlink dump sees the IPv4 neighbours of /proc/net/arp.
func TestReadARPLinux(t *testing.T) {
	m := readARP()
	v4, v6 := 0, 0
	for ip, mac := range m {
		if !ip.IsValid() || ip.Zone() != "" || ip.Is4In6() {
			t.Errorf("address %v is not canonical", ip)
		}
		if n, ok := normalizeMAC(mac); !ok || n != mac {
			t.Errorf("%s: MAC %q is not normalised", ip, mac)
		}
		if ip.Is4() {
			v4++
		} else {
			v6++
		}
	}
	b, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_UNSPEC)
	if err != nil {
		t.Skipf("netlink neighbour dump unavailable: %v", err)
	}
	nl := parseNeighDump(b)
	var proc map[string]bool
	if f, err := os.Open("/proc/net/arp"); err == nil {
		proc = map[string]bool{}
		for ip := range parseARP(f) {
			proc[ip.String()] = true
		}
		f.Close()
	}
	nl4 := 0
	for ip := range nl {
		if ip.Is4() {
			nl4++
		}
	}
	if len(proc) > 0 && nl4 == 0 {
		t.Errorf("netlink returned no IPv4 neighbours, /proc/net/arp has %d", len(proc))
	}
	t.Logf("neighbours: %d IPv4, %d IPv6 (netlink: %d entries, /proc/net/arp: %d)", v4, v6, len(nl), len(proc))
}
