package clients

import (
	"net/netip"
	"os"
	"syscall"
)

// readARP reads the kernel's neighbour tables: IPv4 and IPv6 (NDP) via a
// netlink RTM_GETNEIGH dump, plus /proc/net/arp for IPv4 in case netlink is
// unavailable. nil if neither can be read.
func readARP() map[netip.Addr]string {
	var out map[netip.Addr]string
	if f, err := os.Open("/proc/net/arp"); err == nil {
		out = parseARP(f)
		f.Close()
	}
	if b, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_UNSPEC); err == nil {
		if out == nil {
			out = map[netip.Addr]string{}
		}
		mergeNeighbours(out, parseNeighDump(b))
	}
	return out
}
