package dnsserver

import (
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// virtualIfacePrefixes name container/VM bridges whose addresses clients
// cannot reach; they are never auto-detected as cache IPs.
var virtualIfacePrefixes = []string{"docker", "br-", "veth", "virbr", "cni", "podman", "flannel", "cali", "vnet", "lxcbr", "lxdbr", "tun", "tap", "wg", "zt"}

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
}

func defaultHostEnv(container string) hostEnv {
	return hostEnv{container: container, primary: netutil.PrimaryIPv4, ifaces: localPrivateIPv4s,
		host: loadHostInfo, gateway: netutil.DefaultGatewayIPv4}
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
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || virtualIface(ifc.Name) {
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

func virtualIface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// hostIface is one interface of this machine that is up, with its unicast
// addresses and their on-link prefix lengths.
type hostIface struct {
	name     string
	prefixes []netip.Prefix // address/on-link prefix length (not masked)
}

// hostInfo describes this machine (refreshed every 5 minutes).
type hostInfo struct {
	ifaces   []hostIface  // all up interfaces incl. loopback and virtual bridges (server-name answers)
	own      []netip.Addr // unicast interface addresses without loopback/link-local (loop protection, own PTR)
	primary4 netip.Addr   // netutil.PrimaryIPv4 (invalid if none)
	primary6 netip.Addr   // netutil.PrimaryIPv6 (invalid if none)
	search   []string     // resolv.conf search domains
}

// loadHostInfo reads the interface addresses, the primary addresses and
// the resolv.conf search domains.
func loadHostInfo() *hostInfo {
	h := &hostInfo{search: netutil.ResolvConfSearch()}
	if ip, err := netutil.PrimaryIPv4(); err == nil {
		h.primary4 = ip
	}
	if ip, err := netutil.PrimaryIPv6(); err == nil {
		h.primary6 = ip
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return h
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		hi := hostIface{name: ifc.Name}
		for _, a := range addrs {
			pn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(pn.IP)
			if !ok {
				continue
			}
			ip = netutil.Canon(ip)
			if ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			ones, bits := pn.Mask.Size()
			if ip.Is4() && bits == 128 {
				ones -= 96
			}
			if ones < 0 || ones > ip.BitLen() {
				continue
			}
			hi.prefixes = append(hi.prefixes, netip.PrefixFrom(ip, ones))
			if !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				h.own = append(h.own, ip)
			}
		}
		if len(hi.prefixes) > 0 {
			h.ifaces = append(h.ifaces, hi)
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
// addresses never (they are ambiguous without a zone).
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
