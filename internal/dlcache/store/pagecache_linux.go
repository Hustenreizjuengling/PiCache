//go:build linux

package cachestore

import (
	"os"

	"golang.org/x/sys/unix"
)

// DropPageCache asks the kernel to drop the cached pages of f
// (posix_fadvise POSIX_FADV_DONTNEED), so that the next read comes from the
// storage. Dirty pages stay: sync the file first. It reports whether the
// call succeeded.
func DropPageCache(f *os.File) bool {
	return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED) == nil
}
