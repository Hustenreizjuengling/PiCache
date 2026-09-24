//go:build linux

package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// mountState reports whether path (whose opened directory is fi) is a mount
// point: its st_dev differs from the parent's, or it is listed in
// /proc/self/mountinfo (bind mounts of the same file system).
func mountState(path string, fi os.FileInfo) (mounted, supported bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, false
	}
	if pfi, err := os.Stat(filepath.Dir(path)); err == nil {
		if pst, ok := pfi.Sys().(*syscall.Stat_t); ok && pst.Dev != st.Dev {
			return true, true
		}
	}
	entries, err := readMountinfo()
	if err != nil {
		return false, true
	}
	resolved, _ := filepath.EvalSymlinks(path)
	for _, e := range entries {
		if e.mountPoint == path || (resolved != "" && e.mountPoint == resolved) {
			return true, true
		}
	}
	return false, true
}

func readMountinfo() ([]mountEntry, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseMountinfo(f)
}

// statDir runs fstatfs on the opened directory (the handle keeps pointing at
// the file system that was verified, even if it is unmounted meanwhile).
func statDir(r *os.Root) (fsInfo, error) {
	f, err := r.Open(".")
	if err != nil {
		return fsInfo{}, err
	}
	defer f.Close()
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return fsInfo{}, &os.PathError{Op: "statfs", Path: f.Name(), Err: err}
	}
	bsize := uint64(st.Frsize)
	if bsize == 0 {
		bsize = uint64(st.Bsize)
	}
	return fsInfo{typ: uint32(st.Type), total: st.Blocks * bsize, free: st.Bavail * bsize}, nil
}

// sameDevice reports whether two files live on the same file system.
func sameDevice(a, b os.FileInfo) (same, ok bool) {
	sa, ok1 := a.Sys().(*syscall.Stat_t)
	sb, ok2 := b.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 {
		return false, false
	}
	return sa.Dev == sb.Dev, true
}

// blockDevice names the block device behind fi (e.g. "mmcblk0p2"): via
// /sys/dev/block/<major>:<minor> (which also resolves /dev/root), else the
// mount source in mountinfo.
func blockDevice(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	dev := uint64(st.Dev) // uint32 on some targets
	majMin := fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev))
	if link, err := os.Readlink("/sys/dev/block/" + majMin); err == nil {
		return filepath.Base(link)
	}
	entries, err := readMountinfo()
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.majMin == majMin && filepath.Dir(e.source) == "/dev" {
			return filepath.Base(e.source)
		}
	}
	return ""
}
