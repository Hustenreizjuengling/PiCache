package netutil

import (
	"cmp"
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
)

// Route is a unicast route of the main routing table with an output
// interface (docs/ARCHITECTURE.md 7.1 step 5, identifiers iface:).
type Route struct {
	Prefix  netip.Prefix // masked
	Iface   string
	Gateway netip.Addr // next hop; invalid for an on-link route
	Metric  uint32
}

// RouteTable is an immutable snapshot of the routes: the non-default
// routes, most specific first (among equal prefixes the lower metric
// first), and the default routes.
type RouteTable struct {
	routes   []Route
	defaults []Route
}

// maxRoutes bounds a route snapshot; the rest of a larger table is
// ignored (RefreshRoutes reports it).
const maxRoutes = 4096

// NewRouteTable builds a snapshot from routes (default routes, 0.0.0.0/0
// and ::/0, are kept apart; IPv6 link-local prefixes, which every
// interface has, are left out). It reports whether more than 4096 routes
// were given (the rest is ignored).
func NewRouteTable(in []Route) (*RouteTable, bool) {
	t := &RouteTable{}
	truncated := false
	for _, r := range in {
		if !r.Prefix.IsValid() || r.Iface == "" {
			continue
		}
		r.Prefix = r.Prefix.Masked()
		if r.Prefix.Bits() == 0 {
			t.defaults = append(t.defaults, r)
			continue
		}
		if r.Prefix.Addr().Is6() && r.Prefix.Addr().IsLinkLocalUnicast() {
			continue
		}
		if len(t.routes) == maxRoutes {
			truncated = true
			break
		}
		t.routes = append(t.routes, r)
	}
	slices.SortStableFunc(t.routes, func(a, b Route) int {
		if c := cmp.Compare(b.Prefix.Bits(), a.Prefix.Bits()); c != 0 {
			return c
		}
		if c := a.Prefix.Addr().Compare(b.Prefix.Addr()); c != 0 {
			return c
		}
		return cmp.Compare(a.Metric, b.Metric)
	})
	// One route per prefix: the lowest metric.
	t.routes = slices.CompactFunc(t.routes, func(a, b Route) bool { return a.Prefix == b.Prefix })
	slices.SortStableFunc(t.defaults, func(a, b Route) int { return cmp.Compare(a.Metric, b.Metric) })
	return t, truncated
}

// InterfaceOf returns the output interface of the most specific
// non-default route to addr ("" if none): the interface a reply to that
// source leaves by.
func (t *RouteTable) InterfaceOf(addr netip.Addr) string {
	if t == nil {
		return ""
	}
	addr = Canon(addr)
	for _, r := range t.routes {
		if r.Prefix.Contains(addr) {
			return r.Iface
		}
	}
	return ""
}

// Routes returns the non-default routes (shared: read only).
func (t *RouteTable) Routes() []Route {
	if t == nil {
		return nil
	}
	return t.routes
}

// DefaultRoutes returns the default routes, the lowest metric first
// (shared: read only).
func (t *RouteTable) DefaultRoutes() []Route {
	if t == nil {
		return nil
	}
	return t.defaults
}

// equal reports whether two snapshots hold the same routes.
func (t *RouteTable) equal(o *RouteTable) bool {
	if t == nil || o == nil {
		return t == o
	}
	return slices.Equal(t.routes, o.routes) && slices.Equal(t.defaults, o.defaults)
}

// routes is the current snapshot (RefreshRoutes).
var routes atomic.Pointer[RouteTable]

// readRoutes is the route source (replaced in tests).
var readRoutes = readRouteTable

// RefreshRoutes reads the routes again (Linux: a netlink RTM_GETROUTE dump
// of the main table plus the connected prefixes of the interfaces'
// addresses; elsewhere none) and swaps the snapshot. It reports whether
// the snapshot changed and whether the table had more than 4096 routes.
func RefreshRoutes() (changed, truncated bool) {
	t, truncated := NewRouteTable(readRoutes())
	old := routes.Swap(t)
	return !t.equal(old), truncated
}

// Routes returns the current route snapshot (nil before RefreshRoutes).
func Routes() *RouteTable { return routes.Load() }

// InterfaceOf returns the interface a reply to addr leaves by: the output
// interface of the most specific non-default route of the current
// snapshot ("" if none, or on systems other than Linux). An in-memory
// lookup, never a system call.
func InterfaceOf(addr netip.Addr) string { return routes.Load().InterfaceOf(addr) }

// connectedRoutes returns the on-link prefixes of the interfaces'
// addresses as routes (metric maxUint32: a kernel route of the same
// prefix wins).
func connectedRoutes(addrs []HostAddr) []Route {
	var out []Route
	for _, a := range addrs {
		if a.Loopback || !a.Up || a.Iface == "" {
			continue
		}
		out = append(out, Route{Prefix: a.Prefix.Masked(), Iface: a.Iface, Metric: ^uint32(0)})
	}
	return out
}

// Netlink route dump layout (linux/rtnetlink.h).
const (
	rtmNewRoute    = 24
	rtMsgLen       = 12 // rtmsg: family, dst_len, src_len, tos, table, protocol, scope, type u8; flags u32
	rtaDst         = 1
	rtaOIF         = 4
	rtaGateway     = 5
	rtaPriority    = 6
	rtaTable       = 15
	rtTableMain    = 254
	rtnUnicast     = 1
	maxDumpEntries = 16384
)

// ParseRouteDump parses the reply of a netlink RTM_GETROUTE dump: the
// unicast routes of the main table that have an output interface (names
// maps interface indexes to names; unknown indexes are skipped). Local,
// broadcast, anycast, multicast, unreachable, blackhole and prohibit routes
// and routes of other tables are skipped; malformed data ends the parse.
func ParseRouteDump(b []byte, names map[int]string) []Route {
	var out []Route
	for n := 0; len(b) >= nlmsgHdrLen && n < maxDumpEntries; n++ {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		typ := binary.NativeEndian.Uint16(b[4:6])
		if l < nlmsgHdrLen || l > len(b) || typ == nlmsgDone {
			break
		}
		if typ == rtmNewRoute {
			if r, ok := parseRouteMsg(b[nlmsgHdrLen:l], names); ok {
				out = append(out, r)
			}
		}
		next := (l + 3) &^ 3
		if next >= len(b) {
			break
		}
		b = b[next:]
	}
	return out
}

func parseRouteMsg(m []byte, names map[int]string) (Route, bool) {
	if len(m) < rtMsgLen {
		return Route{}, false
	}
	family, dstLen, table, rtype := m[0], int(m[1]), uint32(m[4]), m[7]
	if rtype != rtnUnicast {
		return Route{}, false
	}
	var r Route
	var dst netip.Addr
	oif := 0
	for a := m[rtMsgLen:]; len(a) >= 4; {
		l := int(binary.NativeEndian.Uint16(a[0:2]))
		if l < 4 || l > len(a) {
			return Route{}, false
		}
		v := a[4:l]
		switch binary.NativeEndian.Uint16(a[2:4]) & 0x3fff {
		case rtaDst:
			dst, _ = netip.AddrFromSlice(v)
		case rtaOIF:
			if len(v) == 4 {
				oif = int(binary.NativeEndian.Uint32(v))
			}
		case rtaGateway:
			r.Gateway, _ = netip.AddrFromSlice(v)
		case rtaPriority:
			if len(v) == 4 {
				r.Metric = binary.NativeEndian.Uint32(v)
			}
		case rtaTable:
			if len(v) == 4 {
				table = binary.NativeEndian.Uint32(v)
			}
		}
		next := (l + 3) &^ 3
		if next >= len(a) {
			break
		}
		a = a[next:]
	}
	if table != rtTableMain || oif == 0 {
		return Route{}, false
	}
	if r.Iface = names[oif]; r.Iface == "" {
		return Route{}, false
	}
	switch {
	case !dst.IsValid() && family == 2: // AF_INET: no RTA_DST = 0.0.0.0/0
		dst = netip.IPv4Unspecified()
	case !dst.IsValid() && family == 10: // AF_INET6
		dst = netip.IPv6Unspecified()
	case !dst.IsValid():
		return Route{}, false
	}
	dst = dst.Unmap()
	if dstLen > dst.BitLen() {
		return Route{}, false
	}
	r.Prefix = netip.PrefixFrom(dst, dstLen).Masked()
	if r.Gateway.IsValid() {
		r.Gateway = r.Gateway.Unmap()
	}
	return r, true
}

// interfaceNames maps interface indexes to names.
func interfaceNames() map[int]string {
	list, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make(map[int]string, len(list))
	for _, ifc := range list {
		out[ifc.Index] = ifc.Name
	}
	return out
}
