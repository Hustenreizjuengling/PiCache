package clients

import (
	"net/netip"
	"os"
)

// readARP reads the kernel's IPv4 neighbour table (nil on error).
func readARP() map[netip.Addr]string {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil
	}
	defer f.Close()
	return parseARP(f)
}
