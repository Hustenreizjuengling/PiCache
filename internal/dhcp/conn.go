package dhcp

import (
	"net/netip"
	"sync"
	"time"
)

// v4Conn is the DHCPv4 server socket (UDP port 67 on all IPv4 addresses).
type v4Conn interface {
	// ReadFrom reads one datagram with its ingress interface (0 if the
	// kernel did not say).
	ReadFrom(b []byte) (n, ifIndex int, src netip.AddrPort, err error)
	// WriteTo sends b out of interface ifIndex with source address src
	// (IP_PKTINFO), also to the limited broadcast address.
	WriteTo(b []byte, ifIndex int, src netip.Addr, dst netip.AddrPort) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// v6Conn is the DHCPv6 socket (UDP port 547 on all IPv6 addresses).
type v6Conn interface {
	// ReadFrom reads one datagram with its ingress interface and
	// destination address.
	ReadFrom(b []byte) (n, ifIndex int, src netip.AddrPort, dst netip.Addr, err error)
	WriteTo(b []byte, ifIndex int, dst netip.AddrPort) error
	JoinGroup(ifIndex int, group netip.Addr) error
	LeaveGroup(ifIndex int, group netip.Addr) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// icmpConn is the raw ICMPv6 socket for router advertisements. Its filter
// passes router solicitations only; it sends with hop limit 255 and does
// not loop multicast back.
type icmpConn interface {
	// ReadFrom reads one ICMPv6 message with its ingress interface and the
	// hop limit it arrived with (-1 if unknown).
	ReadFrom(b []byte) (n, ifIndex, hopLimit int, src netip.Addr, err error)
	WriteTo(b []byte, ifIndex int, dst netip.Addr) error
	JoinGroup(ifIndex int, group netip.Addr) error
	LeaveGroup(ifIndex int, group netip.Addr) error
	SetReadDeadline(t time.Time) error
	Close() error
}

// Sockets are the DHCP sockets. The app opens them at start, before the
// privilege drop (OpenSockets), because ports 67 and 547 need
// CAP_NET_BIND_SERVICE and the raw ICMPv6 socket CAP_NET_RAW; the service
// uses them afterwards without any privilege.
type Sockets struct {
	// requested: OpenSockets was called (PICACHE_DHCP is on).
	requested bool

	mu   sync.Mutex
	v4   v4Conn
	v6   v6Conn
	icmp icmpConn
	// Why a socket is missing ("" when it is open).
	v4Err, v6Err, icmpErr string
}

// unavailableSockets returns sockets that are all missing for reason.
func unavailableSockets(reason string) *Sockets {
	return &Sockets{v4Err: reason, v6Err: reason, icmpErr: reason}
}

// DisableRouterAdvertisements closes the raw ICMPv6 socket (reason is
// reported in the status), e.g. when CAP_NET_RAW could not be dropped.
func (s *Sockets) DisableRouterAdvertisements(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.icmp != nil {
		_ = s.icmp.Close()
		s.icmp = nil
	}
	s.icmpErr = reason
}

// HasRaw reports whether the raw ICMPv6 socket is open.
func (s *Sockets) HasRaw() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.icmp != nil
}

// Errors returns why each socket is missing ("" when it is open): UDP 67,
// UDP 547, raw ICMPv6.
func (s *Sockets) Errors() (v4, v6, icmp string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v4Err, s.v6Err, s.icmpErr
}

// Close closes all sockets (again: no effect).
func (s *Sockets) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	const closed = "closed"
	if s.v4 != nil {
		_ = s.v4.Close()
		s.v4, s.v4Err = nil, closed
	}
	if s.v6 != nil {
		_ = s.v6.Close()
		s.v6, s.v6Err = nil, closed
	}
	if s.icmp != nil {
		_ = s.icmp.Close()
		s.icmp, s.icmpErr = nil, closed
	}
}

// get returns the open sockets and the reasons for missing ones.
func (s *Sockets) get() (v4 v4Conn, v6 v6Conn, icmp icmpConn, v4Err, v6Err, icmpErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v4, s.v6, s.icmp, s.v4Err, s.v6Err, s.icmpErr
}
