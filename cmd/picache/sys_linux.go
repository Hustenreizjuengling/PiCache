//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
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

// memoryLimit returns the cgroup v2 memory limit or MemTotal (bytes), 0 if unknown.
func memoryLimit() int64 {
	if n := cgroupMemoryMax(); n > 0 {
		return n
	}
	return memTotal()
}

// budgetMemory is the memory the entry budget of the blocklists follows
// (config.Config.MemoryLimit, filter.BudgetFor): the cgroup v2 limit as it
// is, else MemTotal rounded up to the machine's nominal size
// (filter.NominalMemory); 0 if unknown.
func budgetMemory() uint64 {
	if n := cgroupMemoryMax(); n > 0 {
		return uint64(n)
	}
	return filter.NominalMemory(uint64(max(memTotal(), 0)))
}

// cgroupMemoryMax returns the cgroup v2 memory limit (bytes), 0 without one.
func cgroupMemoryMax() int64 {
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "max" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
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
