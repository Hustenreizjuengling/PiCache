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

// hostInfo describes this machine (refreshed every 5 minutes).
type hostInfo struct {
	addrs  []netip.Prefix // unicast addresses of non-virtual interfaces with their subnet (answers)
	own    []netip.Addr   // all unicast interface addresses (loop protection)
	search []string       // resolv.conf search domains
}

// loadHostInfo reads the interface addresses and resolv.conf search domains.
func loadHostInfo() *hostInfo {
	h := &hostInfo{search: netutil.ResolvConfSearch()}
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
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			h.own = append(h.own, ip)
			if !virtualIface(ifc.Name) {
				ones, _ := pn.Mask.Size()
				h.addrs = append(h.addrs, netip.PrefixFrom(ip, ones))
			}
		}
	}
	return h
}

// isOwn reports whether ip is an address of this machine.
func (h *hostInfo) isOwn(ip netip.Addr) bool { return slices.Contains(h.own, ip) }

// addrsFor returns this machine's addresses of one family for a client:
// those on the client's subnet if any, otherwise all of them.
func (h *hostInfo) addrsFor(client netip.Addr, v6 bool) []netip.Addr {
	var same, all []netip.Addr
	for _, p := range h.addrs {
		if p.Addr().Is6() != v6 {
			continue
		}
		all = append(all, p.Addr())
		if p.Masked().Contains(client) {
			same = append(same, p.Addr())
		}
	}
	if len(same) > 0 {
		return same
	}
	return all
}
