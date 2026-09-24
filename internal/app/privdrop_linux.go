//go:build linux

package app

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// dropPrivileges switches from root to PICACHE_RUN_AS after all listeners
// are bound. Ownership of the (small) data dir is fixed first; the cache dir
// only at its top level (it may be large or on a NAS).
func (a *App) dropPrivileges() error {
	if a.cfg.RunAs == "" || os.Geteuid() != 0 {
		return nil
	}
	uid, gid, err := config.ParseRunAs(a.cfg.RunAs)
	if err != nil {
		return err
	}
	chownTree(a.cfg.DataDir, uid, gid, a.log)
	if err := os.Lchown(a.cfg.CacheDir, uid, gid); err != nil {
		a.log.Warn("cannot chown cache dir; make sure it is writable by the run-as user",
			slog.String("dir", a.cfg.CacheDir), slog.Int("uid", uid), slog.Any("err", err))
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

func chownTree(root string, uid, gid int, log *slog.Logger) {
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		return os.Lchown(p, uid, gid)
	})
	if err != nil {
		log.Warn("cannot chown data dir", slog.String("dir", root), slog.Any("err", err))
	}
}

func isAddrInUse(err error) bool  { return errors.Is(err, syscall.EADDRINUSE) }
func isPermission(err error) bool { return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) }
