package clients

import (
	"fmt"
	"net"
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

// readNeighbourTable reads the neighbour tables for Neighbours: the netlink
// dump (IPv4 and IPv6, with interface names), else /proc/net/arp (IPv4
// only).
func readNeighbourTable() ([]Neighbour, error) {
	b, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_UNSPEC)
	if err == nil {
		names := map[int]string{}
		if ifs, err := net.Interfaces(); err == nil {
			for _, ifc := range ifs {
				names[ifc.Index] = ifc.Name
			}
		}
		return parseNeighbours(b, func(i int) string { return names[i] }), nil
	}
	f, ferr := os.Open("/proc/net/arp")
	if ferr != nil {
		return nil, fmt.Errorf("read the neighbour table: %w", err)
	}
	defer f.Close()
	return parseARPNeighbours(f), nil
}
