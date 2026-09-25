package netutil

import (
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HostAddr is a unicast address of one of this machine's interfaces.
type HostAddr struct {
	Iface    string       // interface name
	Prefix   netip.Prefix // the address with its on-link prefix length (not masked)
	Up       bool         // the interface is up
	Loopback bool         // the interface is a loopback interface
	// IPv6 address state (read via netlink on Linux; always false
	// elsewhere).
	Temporary  bool // a temporary (privacy) address, RFC 8981
	Deprecated bool // its preferred lifetime has ended: not used for new connections
	Tentative  bool // duplicate address detection is running or failed: not usable yet
}

// HostAddrs returns the unicast addresses of this machine's interfaces: on
// Linux from a netlink RTM_GETADDR dump (with the IPv6 address flags), else
// (and when netlink fails) from net.Interfaces.
func HostAddrs() []HostAddr { return hostAddrs() }

// hostAddrs is the address source (replaced in tests).
var hostAddrs = readHostAddrs

// virtualIfacePrefixes name container/VM bridges, virtual Ethernet pairs
// and tunnels. Clients cannot reach this machine through their addresses
// (or they lead to other hosts than the LAN), and they have no neighbour
// table worth reading.
var virtualIfacePrefixes = []string{"docker", "br-", "veth", "virbr", "cni", "podman", "flannel", "cali", "vnet",
	"lxcbr", "lxdbr", "tun", "tap", "wg", "zt"}

// VirtualInterface reports whether an interface name belongs to a
// container/VM bridge, a virtual Ethernet pair or a tunnel.
func VirtualInterface(name string) bool {
	for _, p := range virtualIfacePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// ConnectedPrefix returns the masked on-link network of an interface
// address if it may be trusted as a connected network: IPv4 of /8 or
// longer, IPv6 of /48 or longer, never loopback, multicast or unspecified.
func ConnectedPrefix(p netip.Prefix) (netip.Prefix, bool) {
	a := Canon(p.Addr())
	if !p.IsValid() || !a.IsValid() || a.IsLoopback() || a.IsMulticast() || a.IsUnspecified() {
		return netip.Prefix{}, false
	}
	minBits := 8
	if a.Is6() {
		minBits = 48
	}
	if p.Bits() < minBits {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(a, p.Bits()).Masked(), true
}

// ConnectedSubnets returns the networks this machine is connected to, as
// dns.trustConnectedNetworks trusts them: the on-link prefixes of the
// addresses of interfaces that are up, public or private, without loopback
// and virtual bridges or tunnels (VirtualInterface), IPv4 of /8 or longer
// and IPv6 of /48 or longer.
func ConnectedSubnets() []netip.Prefix { return connectedSubnets(hostAddrs()) }

func connectedSubnets(addrs []HostAddr) []netip.Prefix {
	var out []netip.Prefix
	for _, a := range addrs {
		if !a.Up || a.Loopback || VirtualInterface(a.Iface) {
			continue
		}
		if p, ok := ConnectedPrefix(a.Prefix); ok && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// OnLink reports whether ip is a link-local address or inside one of the
// ConnectedSubnets and not an address of this machine, i.e. a neighbour
// whose MAC address the kernel's neighbour table knows once it talked to
// this machine. The subnets are re-read at most once a minute.
func OnLink(ip netip.Addr) bool {
	ip = Canon(ip)
	if !ip.IsValid() || ip.IsLoopback() {
		return false
	}
	ps := onLinkAll.get()
	if slices.Contains(ps, netip.PrefixFrom(ip, ip.BitLen())) {
		return false // this machine
	}
	return ip.IsLinkLocalUnicast() || inAny(ip, ps)
}

// onLinkAll caches the ConnectedSubnets and this machine's own addresses
// (as single-address prefixes) for OnLink (hot path).
var onLinkAll = prefixCache{load: func() []netip.Prefix {
	addrs := hostAddrs()
	ps := connectedSubnets(addrs)
	for _, a := range addrs {
		ip := Canon(a.Prefix.Addr())
		ps = append(ps, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return ps
}}

// prefixCache is a list of prefixes that is re-read at most once per
// onLinkTTL by one caller; other callers keep using the previous list
// meanwhile.
type prefixCache struct {
	mu      sync.Mutex // one refresh at a time
	ps      atomic.Pointer[[]netip.Prefix]
	expires atomic.Int64 // unix nanoseconds
	load    func() []netip.Prefix
}

func (c *prefixCache) get() []netip.Prefix {
	ps := c.ps.Load()
	if ps != nil && time.Now().UnixNano() < c.expires.Load() {
		return *ps
	}
	if ps == nil {
		c.mu.Lock()
	} else if !c.mu.TryLock() {
		return *ps
	}
	defer c.mu.Unlock()
	if cur := c.ps.Load(); cur != nil && time.Now().UnixNano() < c.expires.Load() {
		return *cur // refreshed while we waited
	}
	fresh := c.load()
	c.ps.Store(&fresh)
	c.expires.Store(time.Now().Add(onLinkTTL).UnixNano())
	return fresh
}

// reset drops the cached list (tests).
func (c *prefixCache) reset() { c.ps.Store(nil) }

// ifaceHostAddrs reads the interface addresses through net.Interfaces (no
// address flags).
func ifaceHostAddrs() []HostAddr {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []HostAddr
	for _, ifc := range ifs {
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
			ones, bits := pn.Mask.Size()
			if p, ok := ifacePrefix(ip, ones, bits); ok {
				out = append(out, HostAddr{Iface: ifc.Name, Prefix: p, Up: ifc.Flags&net.FlagUp != 0,
					Loopback: ifc.Flags&net.FlagLoopback != 0})
			}
		}
	}
	return out
}

// ifacePrefix builds the canonical address/prefix-length pair of an
// interface address (unicast only).
func ifacePrefix(ip netip.Addr, ones, bits int) (netip.Prefix, bool) {
	ip = Canon(ip)
	if ip.Is4() && bits == 128 { // IPv4 with an IPv6-length mask
		ones -= 96
	}
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsMulticast() || ones < 0 || ones > ip.BitLen() {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(ip, ones), true
}

// Netlink address dump layout (linux/netlink.h, linux/if_addr.h). Header
// fields are in host byte order.
const (
	nlmsgHdrLen    = 16 // nlmsghdr: len u32, type u16, flags u16, seq u32, pid u32
	nlmsgDone      = 3
	rtmNewAddr     = 20
	ifaMsgLen      = 8 // ifaddrmsg: family u8, prefixlen u8, flags u8, scope u8, index u32
	ifaAddress     = 1
	ifaLocal       = 2
	ifaFlags       = 8 // u32, supersedes ifaddrmsg.flags
	ifaFTemporary  = 0x01
	ifaFDadFailed  = 0x08
	ifaFDeprecated = 0x20
	ifaFTentative  = 0x40
	maxHostAddrs   = 4096
)

// addrMsg is one address of a netlink address dump.
type addrMsg struct {
	index  int
	prefix netip.Prefix
	flags  uint32
}

// parseAddrDump parses the reply of a netlink RTM_GETADDR dump (at most
// 4096 unicast addresses). IPv4 uses IFA_LOCAL (the peer's address is in
// IFA_ADDRESS on point-to-point links), IPv6 IFA_ADDRESS; malformed data
// ends the parse.
func parseAddrDump(b []byte) []addrMsg {
	var out []addrMsg
	for len(b) >= nlmsgHdrLen && len(out) < maxHostAddrs {
		l := int(binary.NativeEndian.Uint32(b[0:4]))
		typ := binary.NativeEndian.Uint16(b[4:6])
		if l < nlmsgHdrLen || l > len(b) || typ == nlmsgDone {
			break
		}
		if typ == rtmNewAddr {
			if m, ok := parseAddrMsg(b[nlmsgHdrLen:l]); ok {
				out = append(out, m)
			}
		}
		n := (l + 3) &^ 3
		if n >= len(b) {
			break
		}
		b = b[n:]
	}
	return out
}

func parseAddrMsg(m []byte) (addrMsg, bool) {
	if len(m) < ifaMsgLen {
		return addrMsg{}, false
	}
	bits := int(m[1])
	msg := addrMsg{flags: uint32(m[2]), index: int(binary.NativeEndian.Uint32(m[4:8]))}
	var addr, local netip.Addr
	for a := m[ifaMsgLen:]; len(a) >= 4; {
		l := int(binary.NativeEndian.Uint16(a[0:2]))
		if l < 4 || l > len(a) {
			return addrMsg{}, false
		}
		v := a[4:l]
		switch binary.NativeEndian.Uint16(a[2:4]) & 0x3fff { // without NLA_F_NESTED / NLA_F_NET_BYTEORDER
		case ifaAddress:
			addr, _ = netip.AddrFromSlice(v)
		case ifaLocal:
			local, _ = netip.AddrFromSlice(v)
		case ifaFlags:
			if len(v) == 4 {
				msg.flags = binary.NativeEndian.Uint32(v)
			}
		}
		n := (l + 3) &^ 3
		if n >= len(a) {
			break
		}
		a = a[n:]
	}
	if local.IsValid() {
		addr = local
	}
	p, ok := ifacePrefix(addr, bits, addr.BitLen())
	if !ok {
		return addrMsg{}, false
	}
	msg.prefix = p
	return msg, true
}

// netlinkHostAddrs combines an address dump with the interface names and
// flags (index → interface).
func netlinkHostAddrs(msgs []addrMsg, ifs map[int]net.Interface) []HostAddr {
	out := make([]HostAddr, 0, len(msgs))
	for _, m := range msgs {
		ifc, ok := ifs[m.index]
		if !ok {
			continue
		}
		v6 := m.prefix.Addr().Is6()
		out = append(out, HostAddr{
			Iface: ifc.Name, Prefix: m.prefix,
			Up: ifc.Flags&net.FlagUp != 0, Loopback: ifc.Flags&net.FlagLoopback != 0,
			Temporary:  v6 && m.flags&ifaFTemporary != 0, // for IPv4 the same bit means "secondary"
			Deprecated: m.flags&ifaFDeprecated != 0,
			Tentative:  m.flags&(ifaFTentative|ifaFDadFailed) != 0,
		})
	}
	return out
}
