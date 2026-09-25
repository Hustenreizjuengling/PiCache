//go:build !linux

package netutil

// readHostAddrs reads the interface addresses through net.Interfaces on
// systems other than Linux (no address flags).
func readHostAddrs() []HostAddr { return ifaceHostAddrs() }
