//go:build linux

package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
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
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "max" {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
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
