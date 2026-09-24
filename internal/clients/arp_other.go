//go:build !linux

package clients

import "net/netip"

// readARP returns no neighbours on systems without /proc/net/arp; MAC-based
// identification is then unavailable.
func readARP() map[netip.Addr]string { return nil }
