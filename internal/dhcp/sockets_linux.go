package dhcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// OpenAtStart opens the DHCP sockets the markers ask for (call it before
// the privilege drop, like the listeners): with MarkerSockets UDP
// 0.0.0.0:67 (SO_BROADCAST, IP_PKTINFO) and [::]:547 (IPV6_RECVPKTINFO, no
// multicast loopback); with MarkerRA and CAP_NET_RAW the raw ICMPv6 socket
// for router advertisements (hop limit 255, a filter that passes router
// solicitations and advertisements only, no multicast loopback). The
// markers are checked with Lstat only: never opened or read, only a regular
// file counts. It also records whether the process may bind ports below
// 1024 and open raw sockets now, before the drop. A socket that cannot be
// opened is reported in the status; the others are used anyway.
func OpenAtStart(o StartOptions) *Sockets {
	if o.OptOut {
		return OptOutSockets()
	}
	s := &Sockets{legacy: o.Legacy}
	s.bindCapable, s.rawCapable = startCapabilities()
	if o.Legacy || markerExists(o.DataDir, MarkerSockets) {
		if c, err := listen4(); err != nil {
			s.v4Err, s.v4Code = socketErr("UDP port 67", err), ReasonSocket
		} else {
			s.v4 = c
		}
		if c, err := listen6(); err != nil {
			s.v6Err = socketErr("UDP port 547", err)
		} else {
			s.v6 = c
		}
	}
	if (o.Legacy || markerExists(o.DataDir, MarkerRA)) && s.rawCapable {
		if c, err := listenICMP(); err != nil {
			s.icmpErr = socketErr("the raw ICMPv6 socket", err)
			s.icmpStartErr = s.icmpErr
		} else {
			s.icmp = c
		}
	}
	return s
}

// markerExists reports whether dir/name is a regular file (Lstat only).
func markerExists(dir, name string) bool {
	if dir == "" {
		return false
	}
	fi, err := os.Lstat(filepath.Join(dir, name))
	return err == nil && fi.Mode().IsRegular()
}

// startCapabilities reports whether this thread may bind ports below 1024
// (euid 0 or CAP_NET_BIND_SERVICE effective) and holds CAP_NET_RAW in its
// effective set.
func startCapabilities() (bindCapable, rawCapable bool) {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return os.Geteuid() == 0, false
	}
	has := func(c int) bool { return data[c/32].Effective&(1<<(uint(c)%32)) != 0 }
	return os.Geteuid() == 0 || has(unix.CAP_NET_BIND_SERVICE), has(unix.CAP_NET_RAW)
}

// bindDenied reports whether opening a socket failed for lack of
// permission (EACCES or EPERM).
func bindDenied(err error) bool { return errors.Is(err, os.ErrPermission) }

// socketErr explains a failed socket.
func socketErr(what string, err error) string {
	switch {
	case errors.Is(err, syscall.EADDRINUSE):
		return what + " is in use by another program (another DHCP server on this host, e.g. dnsmasq)"
	case bindDenied(err):
		return what + ": permission denied (binding needs CAP_NET_BIND_SERVICE)"
	}
	return fmt.Sprintf("%s: %v", what, err)
}

// platformListen4 and platformListen6 open UDP 67 and 547 while the
// process runs (the service; tests replace them).
func platformListen4() (v4Conn, error) {
	c, err := listen4()
	if err != nil {
		return nil, err
	}
	return c, nil
}

func platformListen6() (v6Conn, error) {
	c, err := listen6()
	if err != nil {
		return nil, err
	}
	return c, nil
}

// setBroadcast enables SO_BROADCAST (Go sets it for UDP sockets already;
// set here explicitly because replies go to 255.255.255.255).
func setBroadcast(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return serr
}

type udp4 struct {
	pc net.PacketConn
	p  *ipv4.PacketConn
}

func listen4() (*udp4, error) {
	lc := net.ListenConfig{Control: setBroadcast}
	pc, err := lc.ListenPacket(context.Background(), "udp4", "0.0.0.0:67")
	if err != nil {
		return nil, err
	}
	p := ipv4.NewPacketConn(pc)
	if err := p.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true); err != nil {
		pc.Close()
		return nil, err
	}
	return &udp4{pc: pc, p: p}, nil
}

func (c *udp4) ReadFrom(b []byte) (int, int, netip.AddrPort, error) {
	n, cm, src, err := c.p.ReadFrom(b)
	if err != nil {
		return 0, 0, netip.AddrPort{}, err
	}
	idx := 0
	if cm != nil {
		idx = cm.IfIndex
	}
	var ap netip.AddrPort
	if u, ok := src.(*net.UDPAddr); ok {
		ap = u.AddrPort()
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	return n, idx, ap, nil
}

func (c *udp4) WriteTo(b []byte, ifIndex int, src netip.Addr, dst netip.AddrPort) error {
	cm := &ipv4.ControlMessage{IfIndex: ifIndex}
	if src.Is4() {
		cm.Src = src.AsSlice()
	}
	_, err := c.p.WriteTo(b, cm, net.UDPAddrFromAddrPort(dst))
	return err
}

func (c *udp4) SetReadDeadline(t time.Time) error { return c.pc.SetReadDeadline(t) }
func (c *udp4) Close() error                      { return c.pc.Close() }

type udp6 struct {
	pc net.PacketConn
	p  *ipv6.PacketConn
}

// listen6 opens UDP [::]:547 without multicast loopback: PiCache's own
// relayed search (K8, ARCHITECTURE 18.5) must not come back to it.
func listen6() (*udp6, error) {
	pc, err := net.ListenPacket("udp6", "[::]:547")
	if err != nil {
		return nil, err
	}
	p := ipv6.NewPacketConn(pc)
	for _, set := range []func() error{
		func() error { return p.SetControlMessage(ipv6.FlagInterface|ipv6.FlagDst, true) },
		func() error { return p.SetMulticastLoopback(false) },
	} {
		if err := set(); err != nil {
			pc.Close()
			return nil, err
		}
	}
	return &udp6{pc: pc, p: p}, nil
}

func (c *udp6) ReadFrom(b []byte) (int, int, netip.AddrPort, netip.Addr, error) {
	n, cm, src, err := c.p.ReadFrom(b)
	if err != nil {
		return 0, 0, netip.AddrPort{}, netip.Addr{}, err
	}
	idx := 0
	var dst netip.Addr
	if cm != nil {
		idx = cm.IfIndex
		dst, _ = netip.AddrFromSlice(cm.Dst)
	}
	var ap netip.AddrPort
	if u, ok := src.(*net.UDPAddr); ok {
		ap = u.AddrPort()
	}
	return n, idx, ap, dst, nil
}

func (c *udp6) WriteTo(b []byte, ifIndex int, dst netip.AddrPort) error {
	_, err := c.p.WriteTo(b, &ipv6.ControlMessage{IfIndex: ifIndex}, net.UDPAddrFromAddrPort(dst))
	return err
}

func (c *udp6) JoinGroup(ifIndex int, group netip.Addr) error {
	ifi, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return err
	}
	return c.p.JoinGroup(ifi, &net.UDPAddr{IP: group.AsSlice()})
}

func (c *udp6) LeaveGroup(ifIndex int, group netip.Addr) error {
	ifi, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return err
	}
	return c.p.LeaveGroup(ifi, &net.UDPAddr{IP: group.AsSlice()})
}

func (c *udp6) SetReadDeadline(t time.Time) error { return c.pc.SetReadDeadline(t) }
func (c *udp6) Close() error                      { return c.pc.Close() }

type rawICMP struct {
	pc net.PacketConn
	p  *ipv6.PacketConn
}

func listenICMP() (*rawICMP, error) {
	pc, err := net.ListenPacket("ip6:ipv6-icmp", "::")
	if err != nil {
		return nil, err
	}
	c := &rawICMP{pc: pc, p: ipv6.NewPacketConn(pc)}
	var f ipv6.ICMPFilter
	f.SetAll(true)
	f.Accept(ipv6.ICMPTypeRouterSolicitation)
	f.Accept(ipv6.ICMPTypeRouterAdvertisement) // other routers' (K8)
	for _, set := range []func() error{
		func() error { return c.p.SetICMPFilter(&f) },
		func() error { return c.p.SetControlMessage(ipv6.FlagHopLimit|ipv6.FlagInterface|ipv6.FlagDst, true) },
		func() error { return c.p.SetMulticastHopLimit(255) },
		func() error { return c.p.SetHopLimit(255) },
		func() error { return c.p.SetMulticastLoopback(false) },
	} {
		if err := set(); err != nil {
			pc.Close()
			return nil, err
		}
	}
	return c, nil
}

func (c *rawICMP) ReadFrom(b []byte) (int, int, int, netip.Addr, error) {
	n, cm, src, err := c.p.ReadFrom(b)
	if err != nil {
		return 0, 0, 0, netip.Addr{}, err
	}
	idx, hops := 0, -1
	if cm != nil {
		idx, hops = cm.IfIndex, cm.HopLimit
	}
	var ip netip.Addr
	if a, ok := src.(*net.IPAddr); ok {
		ip, _ = netip.AddrFromSlice(a.IP)
	}
	return n, idx, hops, ip, nil
}

func (c *rawICMP) WriteTo(b []byte, ifIndex int, dst netip.Addr) error {
	_, err := c.p.WriteTo(b, &ipv6.ControlMessage{IfIndex: ifIndex, HopLimit: 255}, &net.IPAddr{IP: dst.AsSlice()})
	return err
}

func (c *rawICMP) JoinGroup(ifIndex int, group netip.Addr) error {
	ifi, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return err
	}
	return c.p.JoinGroup(ifi, &net.IPAddr{IP: group.AsSlice()})
}

func (c *rawICMP) LeaveGroup(ifIndex int, group netip.Addr) error {
	ifi, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return err
	}
	return c.p.LeaveGroup(ifi, &net.IPAddr{IP: group.AsSlice()})
}

func (c *rawICMP) SetReadDeadline(t time.Time) error { return c.pc.SetReadDeadline(t) }
func (c *rawICMP) Close() error                      { return c.pc.Close() }
