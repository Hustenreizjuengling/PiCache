//go:build linux

package logs

import "golang.org/x/sys/unix"

// diskFree returns the bytes available to unprivileged users on the
// filesystem holding dir.
func diskFree(dir string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}
