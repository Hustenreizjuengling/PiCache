package netutil

import (
	"net"
	"syscall"
)

// readHostAddrs reads the interface addresses with their flags from a
// netlink RTM_GETADDR dump (unprivileged) and the interface names and
// states from net.Interfaces; net.Interfaces alone if netlink fails.
func readHostAddrs() []HostAddr {
	b, err := syscall.NetlinkRIB(syscall.RTM_GETADDR, syscall.AF_UNSPEC)
	if err != nil {
		return ifaceHostAddrs()
	}
	list, err := net.Interfaces()
	if err != nil {
		return ifaceHostAddrs()
	}
	ifs := make(map[int]net.Interface, len(list))
	for _, ifc := range list {
		ifs[ifc.Index] = ifc
	}
	return netlinkHostAddrs(parseAddrDump(b), ifs)
}
