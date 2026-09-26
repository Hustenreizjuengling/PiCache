//go:build !linux

package hostinfo

// diskUsage is not available on this system (PiCache runs on Linux).
func diskUsage(string) (total, free uint64, ok bool) { return 0, 0, false }
