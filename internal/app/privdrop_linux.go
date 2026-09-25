//go:build linux

package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// dropPrivileges switches from root to PICACHE_RUN_AS after all listeners
// are bound and before any file is touched. PiCache never chowns: the data
// and cache directories must already belong to the run-as user (the Docker
// image creates /data and /cache owned by 65532; bind mounts must be chowned
// on the host).
func (a *App) dropPrivileges() error {
	if a.cfg.RunAs == "" || os.Geteuid() != 0 {
		return nil
	}
	uid, gid, err := config.ParseRunAs(a.cfg.RunAs)
	if err != nil {
		return err
	}
	for _, dir := range []string{a.cfg.DataDir, a.cfg.CacheDir} {
		fi, err := os.Stat(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue // created after the drop (parent must be writable)
		}
		if err != nil {
			return fmt.Errorf("stat %s: %w", dir, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if ok && (int(st.Uid) != uid) {
			return fmt.Errorf("%s is owned by %d:%d but PiCache runs as %d:%d; run `chown -R %d:%d <host directory>` "+
				"for this bind mount (named Docker volumes get the right owner automatically)", dir, st.Uid, st.Gid, uid, gid, uid, gid)
		}
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("drop privileges: setgroups: %w", err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return fmt.Errorf("drop privileges: setgid: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("drop privileges: setuid: %w", err)
	}
	if os.Getuid() != uid || os.Geteuid() != uid || os.Getgid() != gid {
		return errors.New("drop privileges: uid/gid did not change")
	}
	if err := syscall.Setuid(0); err == nil {
		return errors.New("drop privileges: regained root, refusing to continue")
	}
	a.log.Info("dropped privileges", slog.Int("uid", uid), slog.Int("gid", gid))
	return nil
}

// dropNetRaw removes CAP_NET_RAW from the process for good once the DHCP
// sockets are open: the systemd drop-in of install.sh --with-dhcp grants
// it only for the raw ICMPv6 socket of the router advertisements.
// Capabilities belong to threads, so the ambient set is lowered and the
// effective, permitted and inheritable sets are cleared on every thread
// (syscall.AllThreadsSyscall; needs capset, which the drop-in allows).
// Afterwards no thread may list it (/proc/self/task/*/status) and opening
// another raw socket must fail. Nothing needs dropping when the process
// does not hold it (Docker after the switch to PICACHE_RUN_AS, which
// clears every capability; the unit without the drop-in).
func dropNetRaw() error {
	held, err := threadsHoldCap(unix.CAP_NET_RAW)
	if err != nil {
		return err
	}
	if held {
		if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_LOWER, unix.CAP_NET_RAW); e != 0 {
			return fmt.Errorf("lower the ambient CAP_NET_RAW: %w", e)
		}
		hdr := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		data := &[2]unix.CapUserData{}
		if err := unix.Capget(hdr, &data[0]); err != nil {
			return fmt.Errorf("read the capabilities: %w", err)
		}
		bit := uint32(1) << (unix.CAP_NET_RAW % 32)
		d := &data[unix.CAP_NET_RAW/32]
		d.Effective &^= bit
		d.Permitted &^= bit
		d.Inheritable &^= bit
		hdr.Pid = 0 // each thread changes itself
		if _, _, e := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
			return fmt.Errorf("drop CAP_NET_RAW: %w", e)
		}
		if held, err := threadsHoldCap(unix.CAP_NET_RAW); err != nil || held {
			return fmt.Errorf("CAP_NET_RAW is still held after dropping it (%v)", err)
		}
	}
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6)
	if err == nil {
		syscall.Close(fd)
		return errors.New("a raw socket can still be opened after dropping CAP_NET_RAW")
	}
	if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EAFNOSUPPORT) {
		return fmt.Errorf("check that raw sockets are refused: %w", err)
	}
	return nil
}

// threadsHoldCap reports whether any thread of the process has capability
// c in its effective, permitted or ambient set (/proc/self/task/*/status).
func threadsHoldCap(c int) (bool, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return false, fmt.Errorf("read the threads: %w", err)
	}
	for _, t := range tasks {
		b, err := os.ReadFile("/proc/self/task/" + t.Name() + "/status")
		if errors.Is(err, os.ErrNotExist) {
			continue // the thread ended
		}
		if err != nil {
			return false, fmt.Errorf("read the capabilities of thread %s: %w", t.Name(), err)
		}
		held, err := statusHoldsCap(string(b), c)
		if err != nil || held {
			return held, err
		}
	}
	return false, nil
}

// statusHoldsCap reports whether capability c is set in the CapEff,
// CapPrm or CapAmb line of a /proc/<pid>/status file.
func statusHoldsCap(status string, c int) (bool, error) {
	found := 0
	for line := range strings.SplitSeq(status, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || (key != "CapEff" && key != "CapPrm" && key != "CapAmb") {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSpace(val), 16, 64)
		if err != nil {
			return false, fmt.Errorf("parse %s: %w", key, err)
		}
		found++
		if v&(1<<uint(c)) != 0 {
			return true, nil
		}
	}
	if found < 2 { // CapAmb is missing on kernels before 4.3
		return false, errors.New("no capability sets in the thread status")
	}
	return false, nil
}

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
func isPermission(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}
