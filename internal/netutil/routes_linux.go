package netutil

import "syscall"

// readRouteTable reads the unicast routes of the main table (netlink
// RTM_GETROUTE dump of IPv4 and IPv6, unprivileged) and adds the connected
// prefixes of the interfaces' addresses.
func readRouteTable() []Route {
	var out []Route
	if b, err := syscall.NetlinkRIB(syscall.RTM_GETROUTE, syscall.AF_UNSPEC); err == nil {
		out = ParseRouteDump(b, interfaceNames())
	}
	return append(out, connectedRoutes(HostAddrs())...)
}
