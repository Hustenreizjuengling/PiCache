//go:build linux

package hostinfo

import "golang.org/x/sys/unix"

// diskUsage returns the size and the free bytes (for unprivileged users)
// of the filesystem of dir.
func diskUsage(dir string) (total, free uint64, ok bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, 0, false
	}
	return st.Blocks * uint64(st.Bsize), st.Bavail * uint64(st.Bsize), true
}
