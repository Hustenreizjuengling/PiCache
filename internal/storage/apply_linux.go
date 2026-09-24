//go:build linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// oNoFollow refuses to open a symbolic link as the final path component.
const oNoFollow = syscall.O_NOFOLLOW

// systemHostEnv is the real root helper environment.
func systemHostEnv() hostEnv {
	return hostEnv{
		credDir: CredentialsDir,
		unitDir: "/etc/systemd/system",
		strict:  true,
		checkPrivileges: func() error {
			if os.Geteuid() != 0 {
				return errors.New("must be run as root (sudo)")
			}
			return nil
		},
		systemctl:    runSystemctl,
		serviceOwner: ownerOf,
	}
}

// runSystemctl runs systemctl by absolute path with a fixed, minimal
// environment (no shell, no PATH lookup) and a timeout.
func runSystemctl(ctx context.Context, args ...string) error {
	bin := "/usr/bin/systemctl"
	if !fileExists(bin) {
		bin = "/bin/systemctl"
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	out := &cappedBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

// ownerOf returns the owner of the data directory: the PiCache user.
func ownerOf(dir string) (uid, gid int, err error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return 0, 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("no ownership information")
	}
	return int(st.Uid), int(st.Gid), nil
}

// checkRootOwnedChain requires dir and all its parents to be real
// directories owned by root and not writable by group or others (like
// sshd's StrictModes), so no unprivileged user can redirect root's writes.
func checkRootOwnedChain(dir string) error {
	p := filepath.Clean(dir)
	for {
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !ok || !fi.IsDir():
			return fmt.Errorf("%s must be a directory, not a symbolic link", p)
		case st.Uid != 0:
			return fmt.Errorf("%s must be owned by root (chown root:root %s)", p, p)
		case fi.Mode().Perm()&0o022 != 0:
			return fmt.Errorf("%s must not be writable by group or others (chmod go-w %s)", p, p)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return nil
		}
		p = parent
	}
}

// chownPath gives the mountpoint to the PiCache user.
func chownPath(p string, uid, gid int) error { return os.Lchown(p, uid, gid) }
