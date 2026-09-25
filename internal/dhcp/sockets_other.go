//go:build !linux

package dhcp

// OpenSockets reports the DHCP server as unavailable on systems other
// than Linux (the ingress interface of a broadcast and the capability
// model are Linux-specific).
func OpenSockets() *Sockets {
	s := unavailableSockets(reasonNotLinux)
	s.requested = true
	return s
}
