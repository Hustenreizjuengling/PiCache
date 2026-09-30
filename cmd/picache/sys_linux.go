//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hustenreizjuengling/picache/internal/dns/filter"
)

// becomeOwnerOf switches to the owner of dir when running as root, so files
// the CLI creates (SQLite -wal/-shm) stay writable for the service.
func becomeOwnerOf(dir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(int(st.Gid)); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(int(st.Uid)); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	return nil
}

// memoryLimit returns the memory limit of the own cgroup or MemTotal (bytes), 0 if unknown.
func memoryLimit() int64 {
	if n := cgroupMemoryMax(); n > 0 {
		return n
	}
	return memTotal()
}

// budgetMemory is the memory the entry budget of the blocklists follows
// (config.Config.MemoryLimit, filter.BudgetFor): the limit of the own
// cgroup as it is, else MemTotal rounded up to the machine's nominal size
// (filter.NominalMemory); 0 if unknown.
func budgetMemory() uint64 {
	if n := cgroupMemoryMax(); n > 0 {
		return uint64(n)
	}
	return filter.NominalMemory(uint64(max(memTotal(), 0)))
}

// cgroupMemoryMax returns the memory limit of PiCache's own cgroup (bytes),
// 0 without one.
func cgroupMemoryMax() int64 { return cgroupMemoryLimit("/") }

// cgroupMemoryLimit returns the lowest memory limit of the process's cgroup
// and its ancestors, read below root (tests: a directory with proc and
// sys): cgroup v2 memory.max along the path of the "0::" line of
// /proc/self/cgroup (a MemoryMax= of picache.service, a container's limit
// at the root of its cgroup namespace), else cgroup v1
// memory.limit_in_bytes along the memory controller's path; 0 without one.
// A path that does not exist below the mount (a v1 container that sees its
// own cgroup as the root) is walked up to the mount.
func cgroupMemoryLimit(root string) int64 {
	b, _ := os.ReadFile(filepath.Join(root, "proc/self/cgroup"))
	v2, v1 := "/", "/"
	for line := range strings.SplitSeq(string(b), "\n") {
		id, rest, _ := strings.Cut(strings.TrimSpace(line), ":")
		controllers, p, ok := strings.Cut(rest, ":")
		switch {
		case !ok:
		case id == "0" && controllers == "":
			v2 = p
		case slices.Contains(strings.Split(controllers, ","), "memory"):
			v1 = p
		}
	}
	if n := lowestLimit(filepath.Join(root, "sys/fs/cgroup"), v2, "memory.max"); n > 0 {
		return n
	}
	return lowestLimit(filepath.Join(root, "sys/fs/cgroup/memory"), v1, "memory.limit_in_bytes")
}

// lowestLimit returns the lowest limit in file of the cgroup p below mount
// and of its ancestors up to mount (never above it); 0 without one ("max",
// cgroup v1's "unlimited" of about 2^63).
func lowestLimit(mount, p, file string) int64 {
	var lowest int64
	for dir := path.Clean("/" + p); ; dir = path.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(mount, filepath.FromSlash(dir), file)); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n > 0 && n < 1<<62 &&
				(lowest == 0 || n < lowest) {
				lowest = n
			}
		}
		if dir == "/" {
			return lowest
		}
	}
}

// memTotal returns MemTotal of /proc/meminfo (bytes), 0 if unknown.
func memTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return kb * 1024
			}
		}
	}
	return 0
}

// oNoFollow refuses to open a symbolic link as the final path component.
const oNoFollow = syscall.O_NOFOLLOW
