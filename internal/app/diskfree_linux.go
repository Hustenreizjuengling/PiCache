//go:build linux

package app

import "golang.org/x/sys/unix"

func diskFree(path string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}
