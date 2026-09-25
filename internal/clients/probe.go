package clients

import (
	"net"
	"net/netip"
)

// probeNeighbour sends one empty UDP datagram to the discard port of ip.
// Sending anything to an on-link address makes the kernel resolve its
// link-layer address (ARP or neighbour discovery), so the address appears
// in the neighbour table; the datagram itself is ignored or answered with
// an ICMP error. Unprivileged; errors are irrelevant.
func probeNeighbour(ip netip.Addr) {
	c, err := net.ListenUDP("udp", nil)
	if err != nil {
		return
	}
	defer c.Close()
	_, _ = c.WriteToUDPAddrPort(nil, netip.AddrPortFrom(ip, 9))
}
