//go:build !linux

package netutil

// readRouteTable: no routing table outside Linux (identifiers iface:
// never match).
func readRouteTable() []Route { return nil }
