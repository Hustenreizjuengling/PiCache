//go:build linux

package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

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

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }
func isPermission(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}
