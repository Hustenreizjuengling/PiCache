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
// passes router solicitations and advertisements only; it sends with hop
// limit 255 and does not loop multicast back.
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

// Marker files in the data directory, kept by the service (markers.go):
// what the app opens at the next start, before the privilege drop.
const (
	MarkerSockets = "dhcp.sockets" // UDP 67 and 547: exists while dhcp.enabled is true
	MarkerRA      = "dhcp.ra"      // the raw ICMPv6 socket: exists while router advertisements are on too
)

// StartOptions tell OpenAtStart what to open (PICACHE_DHCP and the data
// directory with the markers).
type StartOptions struct {
	OptOut  bool   // PICACHE_DHCP=off: nothing, ever
	Legacy  bool   // PICACHE_DHCP=on: everything, as if both markers existed
	DataDir string // where the markers are (checked with Lstat only)
}

// Sockets are the DHCP sockets and what the process could do at its start.
// The app opens the sockets the markers ask for before the privilege drop
// (OpenAtStart); the service opens UDP 67 and 547 later itself when DHCP is
// switched on (systemd keeps CAP_NET_BIND_SERVICE for the process lifetime)
// and closes whatever the settings do not need. The raw ICMPv6 socket can
// only be opened at start (CAP_NET_RAW is dropped right after).
type Sockets struct {
	optOut      bool   // PICACHE_DHCP=off
	unsupported string // why this platform cannot serve DHCP ("" on Linux)
	legacy      bool   // PICACHE_DHCP=on
	// bindCapable: euid 0 or CAP_NET_BIND_SERVICE effective at start;
	// rawCapable: CAP_NET_RAW effective at start.
	bindCapable, rawCapable bool

	mu   sync.Mutex
	v4   v4Conn
	v6   v6Conn
	icmp icmpConn
	// Why a socket is missing ("" when it is open or was never tried).
	v4Err, v6Err, icmpErr string
	// v4Code is the reason code of a failed UDP 67 (ReasonSocket or
	// ReasonRestartRequired).
	v4Code string
	// icmpStartErr: opening the raw socket at start failed for a reason
	// other than a missing capability (RA reason code socket).
	icmpStartErr string
	// dropUnverified: the app could not verify that CAP_NET_RAW was
	// dropped; the raw socket was closed.
	dropUnverified string
	// probeHold: a search for DHCP servers uses a UDP 67 socket opened
	// just for it (the service must not close it meanwhile).
	probeHold bool
}

// unsupportedSockets are the sockets of a platform without DHCP support.
func unsupportedSockets(reason string) *Sockets { return &Sockets{unsupported: reason} }

// OptOutSockets are the sockets with PICACHE_DHCP=off: none, ever.
func OptOutSockets() *Sockets { return &Sockets{optOut: true} }

// SetDropUnverified closes the raw ICMPv6 socket because the app could not
// verify that CAP_NET_RAW was dropped (reported as drop-unverified and by
// the health check).
func (s *Sockets) SetDropUnverified(reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.icmp != nil {
		_ = s.icmp.Close()
		s.icmp = nil
	}
	s.icmpErr, s.dropUnverified = reason, reason
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

// Errors returns why each socket is missing ("" when it is open or was
// not asked for): UDP 67, UDP 547, raw ICMPv6.
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
	s.closeV4Locked()
	s.closeV6Locked()
	s.closeICMPLocked()
}

// closeV4Locked closes UDP 67 (s.mu held).
func (s *Sockets) closeV4Locked() {
	if s.v4 != nil {
		_ = s.v4.Close()
		s.v4 = nil
	}
	s.v4Err, s.v4Code = "", ""
}

func (s *Sockets) closeV6Locked() {
	if s.v6 != nil {
		_ = s.v6.Close()
		s.v6 = nil
	}
	s.v6Err = ""
}

// closeICMPLocked closes the raw socket: it can be opened again only at
// the next start.
func (s *Sockets) closeICMPLocked() {
	if s.icmp != nil {
		_ = s.icmp.Close()
		s.icmp = nil
		s.icmpErr = "closed"
	}
}

// get returns the open sockets and the reasons for missing ones.
func (s *Sockets) get() (v4 v4Conn, v6 v6Conn, icmp icmpConn, v4Err, v6Err, icmpErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v4, s.v6, s.icmp, s.v4Err, s.v6Err, s.icmpErr
}

// sockState is a consistent copy of the state the evaluation reads.
type sockState struct {
	optOut, legacy           bool
	unsupported              string
	bindCapable, rawCapable  bool
	v4Open, v6Open, icmpOpen bool
	v4Err, v4Code, v6Err     string
	icmpErr, icmpStartErr    string
	dropUnverified           string
}

func (s *Sockets) state() sockState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sockState{optOut: s.optOut, legacy: s.legacy, unsupported: s.unsupported, bindCapable: s.bindCapable,
		rawCapable: s.rawCapable, v4Open: s.v4 != nil, v6Open: s.v6 != nil, icmpOpen: s.icmp != nil,
		v4Err: s.v4Err, v4Code: s.v4Code, v6Err: s.v6Err, icmpErr: s.icmpErr, icmpStartErr: s.icmpStartErr,
		dropUnverified: s.dropUnverified}
}
