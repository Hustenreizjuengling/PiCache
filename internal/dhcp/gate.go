package dhcp

import (
	"fmt"
	"net"
	"net/netip"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Blockers of the DHCPv4 server (docs/ARCHITECTURE.md 18), in the order
// they are reported.
const (
	BlockerDynamicAddress = "dynamic-address" // PiCache's own IPv4 has a finite lifetime (DHCP client)
	BlockerOtherServer    = "other-server"    // another DHCP server answered a probe or was named by a client
	BlockerNoInterface    = "no-interface"    // the interface is missing or has no single RFC 1918 IPv4 address
	BlockerRange          = "range"           // the range does not fit the subnet
)

// Blockers of the IPv6 announcements.
const (
	BlockerNoULA       = "no-ula"        // no stable ULA on the interface
	BlockerNoRawSocket = "no-raw-socket" // the raw ICMPv6 socket is missing (router advertisements)
	BlockerNoSocket    = "no-socket"     // UDP port 547 is missing (DHCPv6)
)

// env are the facts about this machine the service reads (replaced in
// tests).
type env struct {
	interfaces func() ([]net.Interface, error)
	addrs      func() []netutil.HostAddr
	gateway4   func() (netip.Addr, error)
	bridge     func() bool // PiCache runs in a container bridge network
}

func liveEnv(bridge func() bool) env {
	if bridge == nil {
		bridge = func() bool { return false }
	}
	return env{interfaces: net.Interfaces, addrs: netutil.HostAddrs, gateway4: netutil.DefaultGatewayIPv4, bridge: bridge}
}

// ifaceState is what the service knows about the configured interface.
type ifaceState struct {
	name    string
	index   int
	mac     [6]byte
	self    netip.Prefix // PiCache's IPv4 with the on-link prefix length (the served subnet)
	dynamic bool         // self has a finite valid lifetime
	ula     netip.Addr   // stable ULA (invalid if none)
	problem string       // why the interface cannot be served ("" = usable)
}

// lookupIface reads the interface name: it must exist, be up, not be a
// loopback or virtual interface, have an Ethernet address and exactly one
// RFC 1918 IPv4 address.
func (e env) lookupIface(name string) ifaceState {
	st := ifaceState{name: name}
	ifs, err := e.interfaces()
	if err != nil {
		st.problem = "the network interfaces cannot be read: " + err.Error()
		return st
	}
	i := slices.IndexFunc(ifs, func(ifc net.Interface) bool { return ifc.Name == name })
	if i < 0 {
		st.problem = fmt.Sprintf("the interface %s does not exist", name)
		return st
	}
	ifc := ifs[i]
	st.index = ifc.Index
	switch {
	case ifc.Flags&net.FlagLoopback != 0 || netutil.VirtualInterface(name):
		st.problem = fmt.Sprintf("%s is a loopback, bridge, virtual or tunnel interface", name)
		return st
	case ifc.Flags&net.FlagUp == 0:
		st.problem = fmt.Sprintf("the interface %s is down", name)
		return st
	case len(ifc.HardwareAddr) != 6:
		st.problem = fmt.Sprintf("the interface %s has no Ethernet address", name)
		return st
	}
	st.mac = [6]byte(ifc.HardwareAddr)
	var v4 []netutil.HostAddr
	var ulas []netutil.HostAddr
	for _, a := range e.addrs() {
		if a.Iface != name {
			continue
		}
		ip := netutil.Canon(a.Prefix.Addr())
		switch {
		case ip.Is4() && netutil.IsRFC1918(ip) && !a.Tentative:
			v4 = append(v4, a)
		case ip.Is6() && netutil.IsULA(ip) && !a.Temporary && !a.Deprecated && !a.Tentative:
			ulas = append(ulas, a)
		}
	}
	if len(ulas) > 0 {
		// A statically configured ULA first, then the lowest.
		slices.SortFunc(ulas, func(a, b netutil.HostAddr) int {
			if a.Dynamic != b.Dynamic {
				if a.Dynamic {
					return 1
				}
				return -1
			}
			return a.Prefix.Addr().Compare(b.Prefix.Addr())
		})
		st.ula = netutil.Canon(ulas[0].Prefix.Addr())
	}
	switch len(v4) {
	case 0:
		st.problem = fmt.Sprintf("the interface %s has no private (RFC 1918) IPv4 address", name)
	case 1:
		st.self = netip.PrefixFrom(netutil.Canon(v4[0].Prefix.Addr()), v4[0].Prefix.Bits())
		st.dynamic = v4[0].Dynamic
	default:
		st.problem = fmt.Sprintf("the interface %s has %d private IPv4 addresses; PiCache serves an interface with exactly one", name, len(v4))
	}
	return st
}

// gate is the evaluation of the safety gates for the settings.
type gate struct {
	iface    ifaceState
	pool     *pool
	router   netip.Addr
	dns      netip.Addr
	domain   string
	blockers []string // IPv4 blockers except other-server (evaluated by the caller)
	err      string   // a configuration problem that is not a blocker (no router)
}

// evalGate checks the interface, the range and the options for set.
// lastGateway is the last IPv4 default gateway seen in the subnet (used
// while the routing table has none).
func (e env) evalGate(set *settings.All, lastGateway netip.Addr) gate {
	h := set.DHCP
	g := gate{domain: h.Domain}
	if g.domain == "" {
		g.domain = set.DNS.LocalDomain
	}
	if h.Interface == "" {
		g.iface.problem = "no interface is configured"
		g.blockers = []string{BlockerNoInterface}
		return g
	}
	g.iface = e.lookupIface(h.Interface)
	if g.iface.problem != "" {
		g.blockers = []string{BlockerNoInterface}
		return g
	}
	if g.iface.dynamic {
		g.blockers = append(g.blockers, BlockerDynamicAddress)
	}
	self := g.iface.self.Addr()
	subnet := g.iface.self.Masked()
	start, err1 := netip.ParseAddr(h.RangeStart)
	end, err2 := netip.ParseAddr(h.RangeEnd)
	g.dns = self
	if ip, err := netip.ParseAddr(h.DNSServer); err == nil && ip.Is4() {
		g.dns = ip
	}
	g.router, g.err = e.routerFor(h.Router, subnet, self, lastGateway)
	skip := []netip.Addr{self, g.router}
	if g.dns != self {
		skip = append(skip, g.dns)
	}
	if err1 != nil || err2 != nil || subnet.Bits() > 30 || !subnet.Contains(start) || !subnet.Contains(end) || end.Less(start) {
		g.blockers = append(g.blockers, BlockerRange)
		return g
	}
	g.pool = newPool(subnet, start, end, skip...)
	if g.pool.size() == 0 {
		g.blockers = append(g.blockers, BlockerRange)
	}
	return g
}

// routerFor resolves the router option: an explicit address must be in
// the subnet; "" takes the IPv4 default gateway if it is in the subnet
// (else the last one seen there).
func (e env) routerFor(configured string, subnet netip.Prefix, self, last netip.Addr) (netip.Addr, string) {
	if configured != "" {
		ip, err := netip.ParseAddr(configured)
		if err != nil || !subnet.Contains(ip) || ip == self {
			return netip.Addr{}, fmt.Sprintf("the router %s is not an address of %s other than PiCache's own", configured, subnet)
		}
		return ip, ""
	}
	if gw, err := e.gateway4(); err == nil && subnet.Contains(netutil.Canon(gw)) && netutil.Canon(gw) != self {
		return netutil.Canon(gw), ""
	}
	if last.IsValid() && subnet.Contains(last) {
		return last, ""
	}
	return netip.Addr{}, fmt.Sprintf("the IPv4 default gateway is not in %s; set the router address in the DHCP settings", subnet)
}
