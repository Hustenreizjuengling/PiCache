//go:build !linux

package logs

// diskFree is unknown on other platforms: raw inserts never pause there.
func diskFree(string) (uint64, bool) { return 0, false }
