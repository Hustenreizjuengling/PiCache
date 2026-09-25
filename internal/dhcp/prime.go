package dhcp

import (
	"net"
	"net/netip"
)

// primeNeighbour sends one empty UDP datagram to the discard port of ip,
// so the kernel resolves its link-layer address (ARP) and the neighbour
// table shows whether a device uses it. Unprivileged; errors do not
// matter.
func primeNeighbour(ip netip.Addr) {
	c, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.WriteToUDPAddrPort(nil, netip.AddrPortFrom(ip, 9))
}
