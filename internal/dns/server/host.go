package dnsserver

import (
	"cmp"
	"context"
	"net"
	"net/netip"
	"slices"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// interfaceIPv4 is a private IPv4 address of a physical-looking interface.
type interfaceIPv4 struct {
	iface string
	ip    netip.Addr
}

// hostEnv abstracts the facts about this machine that the server detects
// (replaced in tests).
type hostEnv struct {
	container string                         // "docker", "podman", "lxc" or ""
	primary   func() (netip.Addr, error)     // source address towards the Internet
	ifaces    func() ([]interfaceIPv4, bool) // private IPv4s; bool = a docker0 interface exists
	host      func() *hostInfo               // interface addresses and search domains
	gateway   func() (netip.Addr, error)     // IPv4 default gateway
	gateway6  func() (netip.Addr, error)     // IPv6 default gateway (link-local ones with zone)
	// neighbours reads the kernel's neighbour table (the router's other
	// addresses); nil = none.
	neighbours func(ctx context.Context) ([]clients.Neighbour, error)
}

func defaultHostEnv(container string, neighbours func(ctx context.Context) ([]clients.Neighbour, error)) hostEnv {
	return hostEnv{container: container, primary: netutil.PrimaryIPv4, ifaces: localPrivateIPv4s,
		host: loadHostInfo, gateway: netutil.DefaultGatewayIPv4, gateway6: netutil.DefaultGatewayIPv6,
		neighbours: neighbours}
}

// localPrivateIPv4s lists RFC 1918 addresses of up, non-loopback interfaces
// (virtual bridges excluded) and whether docker0 exists.
func localPrivateIPv4s() ([]interfaceIPv4, bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	var out []interfaceIPv4
	docker0 := false
	for _, ifc := range ifaces {
		if ifc.Name == "docker0" {
			docker0 = true
		}
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || netutil.VirtualInterface(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if pn, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(pn.IP); ok && netutil.IsRFC1918(ip) {
					out = append(out, interfaceIPv4{iface: ifc.Name, ip: netutil.Canon(ip)})
				}
			}
		}
	}
	return out, docker0
}

// hostIface is one interface of this machine that is up, with its usable
// unicast addresses and their on-link prefix lengths.
type hostIface struct {
	name     string
	prefixes []netip.Prefix // address/on-link prefix length (not masked); no deprecated or tentative addresses
}

// hostInfo describes this machine (refreshed every minute).
type hostInfo struct {
	ifaces    []hostIface         // all up interfaces incl. loopback and virtual bridges (server-name answers)
	own       []netip.Addr        // unicast interface addresses without loopback/link-local (loop protection, own PTR)
	temporary map[netip.Addr]bool // IPv6 temporary (privacy) addresses: answered only when no stable one exists
	primary4  netip.Addr          // netutil.PrimaryIPv4 (invalid if none)
	primary6  netip.Addr          // netutil.PrimaryIPv6, a stable address of its /64 instead of a temporary one (invalid if none)
	search    []string            // resolv.conf search domains
}

// loadHostInfo reads the interface addresses (with their IPv6 flags on
// Linux), the primary addresses and the resolv.conf search domains.
func loadHostInfo() *hostInfo {
	var p4, p6 netip.Addr
	if ip, err := netutil.PrimaryIPv4(); err == nil {
		p4 = ip
	}
	if ip, err := netutil.PrimaryIPv6(); err == nil {
		p6 = ip
	}
	return newHostInfo(netutil.HostAddrs(), p4, p6, netutil.ResolvConfSearch())
}

// newHostInfo builds the host information from the interface addresses:
// interfaces that are down are left out; deprecated and tentative (or
// duplicate) addresses are never answered but still count as this
// machine's; temporary IPv6 addresses are marked. A temporary primary IPv6
// address is replaced by a stable address of the same /64 if there is one.
func newHostInfo(addrs []netutil.HostAddr, primary4, primary6 netip.Addr, search []string) *hostInfo {
	h := &hostInfo{primary4: primary4, primary6: primary6, search: search, temporary: map[netip.Addr]bool{}}
	index := map[string]int{}
	for _, a := range addrs {
		ip := netutil.Canon(a.Prefix.Addr())
		if !a.Up || !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !slices.Contains(h.own, ip) {
			h.own = append(h.own, ip)
		}
		if a.Deprecated || a.Tentative {
			continue
		}
		if a.Temporary && ip.Is6() {
			h.temporary[ip] = true
		}
		i, ok := index[a.Iface]
		if !ok {
			i = len(h.ifaces)
			index[a.Iface] = i
			h.ifaces = append(h.ifaces, hostIface{name: a.Iface})
		}
		h.ifaces[i].prefixes = append(h.ifaces[i].prefixes, netip.PrefixFrom(ip, a.Prefix.Bits()))
	}
	if p6 := netutil.Canon(primary6); h.temporary[p6] {
		net64, _ := p6.Prefix(64)
		for _, ifc := range h.ifaces {
			for _, p := range ifc.prefixes {
				if a := p.Addr(); a.Is6() && !h.temporary[a] && net64.Contains(a) {
					h.primary6 = a
					return h
				}
			}
		}
	}
	return h
}

// isOwn reports whether ip is an address of this machine.
func (h *hostInfo) isOwn(ip netip.Addr) bool { return slices.Contains(h.own, ip) }

// addrsFor returns the addresses of one family that answer a client asking
// for this server's name: the addresses that share a connected subnet with
// the client (for the other family: the addresses of the interfaces that
// share a subnet with it), otherwise the primary address. Loopback
// addresses are returned only to loopback clients, IPv6 link-local
// addresses never (they are ambiguous without a zone). IPv6: stable
// addresses only if there are any (temporary ones otherwise), ULA before
// global addresses.
func (h *hostInfo) addrsFor(client netip.Addr, v6 bool) []netip.Addr {
	client = netutil.Canon(client)
	loop := client.IsLoopback()
	usable := func(ip netip.Addr, sameSubnet bool) bool {
		return ip.Is6() == v6 && (loop || !ip.IsLoopback()) &&
			(!ip.IsLinkLocalUnicast() || (sameSubnet && ip.Is4()))
	}
	var out []netip.Addr
	add := func(ip netip.Addr) {
		if !slices.Contains(out, ip) {
			out = append(out, ip)
		}
	}
	for _, ifc := range h.ifaces {
		matched := false
		for _, p := range ifc.prefixes {
			a := p.Addr()
			if a.Is6() && a.IsLinkLocalUnicast() {
				continue // fe80::/64 exists on every link
			}
			if p.Masked().Contains(client) {
				matched = true
				if usable(a, true) {
					add(a)
				}
			}
		}
		if matched && client.Is6() != v6 {
			for _, p := range ifc.prefixes {
				if usable(p.Addr(), false) {
					add(p.Addr())
				}
			}
		}
	}
	if v6 && len(out) > 0 {
		if stable := slices.DeleteFunc(slices.Clone(out), func(a netip.Addr) bool { return h.temporary[a] }); len(stable) > 0 {
			out = stable
		}
		slices.SortStableFunc(out, func(a, b netip.Addr) int {
			return cmp.Compare(v6Rank(a), v6Rank(b))
		})
	}
	if len(out) > 0 {
		return out
	}
	primary := h.primary4
	if v6 {
		primary = h.primary6
	}
	if primary.IsValid() && (loop || !primary.IsLoopback()) {
		return []netip.Addr{primary}
	}
	return nil
}

// v6Rank orders IPv6 answer addresses: loopback, ULA (stable across
// prefix changes), then global.
func v6Rank(a netip.Addr) int {
	switch {
	case a.IsLoopback():
		return 0
	case netutil.IsULA(a):
		return 1
	}
	return 2
}

// HostNetwork describes this machine's network for the network check.
type HostNetwork struct {
	// Bridge: PiCache runs in a container bridge network; its interfaces
	// and neighbours are those of the bridge, not of the LAN.
	Bridge bool
	// Prefixes are the unicast addresses of the interfaces that are up,
	// with their on-link prefix lengths. Loopback addresses, virtual
	// bridges (docker0, veth…, virbr…, VPN tunnels) and deprecated or
	// tentative addresses are left out.
	Prefixes []netip.Prefix
}

// HostNetwork reads the interface addresses now.
func (s *Server) HostNetwork() HostNetwork {
	out := HostNetwork{Bridge: s.cacheIPs.Load().bridge}
	for _, ifc := range s.env.host().ifaces {
		if netutil.VirtualInterface(ifc.name) {
			continue
		}
		for _, p := range ifc.prefixes {
			if !p.Addr().IsLoopback() {
				out.Prefixes = append(out.Prefixes, p)
			}
		}
	}
	return out
}
