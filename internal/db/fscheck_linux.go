//go:build linux

package db

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// checkLocalFS refuses network filesystems (SQLite WAL is unsafe there).
func checkLocalFS(dir string) error {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("db: statfs %s: %w", dir, err)
	}
	switch uint32(st.Type) { // uint32: Type is int32 on 32-bit targets
	case unix.NFS_SUPER_MAGIC, unix.CIFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC:
		return fmt.Errorf("db: %s is on a network filesystem; databases must be on local disk (set PICACHE_DATA_DIR)", dir)
	}
	return nil
}
