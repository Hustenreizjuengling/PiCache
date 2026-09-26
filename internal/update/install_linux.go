//go:build linux

package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// oNoFollow refuses to open a symbolic link as the final path component.
const oNoFollow = syscall.O_NOFOLLOW

// SystemHost is the real host of Apply: root only, systemctl, the given
// health probe (the checks of `picache healthcheck`), a lock against
// concurrent updates and the ownership checks of a root run.
func SystemHost(health func(ctx context.Context) error) Host {
	return Host{
		CheckPrivileges: func() error {
			if os.Geteuid() != 0 {
				return errors.New("must be run as root (sudo)")
			}
			return nil
		},
		ServiceBinary: serviceBinary,
		Systemctl:     runSystemctl,
		Health:        health,
		Lock:          lockUpdates,
		UnitDir:       systemdUnitDir(),
		UnitFragment:  unitFragment,
		Strict:        true,
	}
}

// systemdUnitDir is DefaultUnitDir on a host that runs systemd, else ""
// (the unit step is skipped).
func systemdUnitDir() string {
	if fileExists("/run/systemd/system") {
		return DefaultUnitDir
	}
	return ""
}

// unitFragment returns the file systemd loaded picache.service from.
func unitFragment(ctx context.Context) (string, error) {
	out, err := systemctl(ctx, "show", "-p", "FragmentPath", "--value", Service)
	return strings.TrimSpace(out), err
}

// systemBinary returns /usr/bin/<name>, or /bin/<name> on systems without merged /usr.
func systemBinary(name string) string {
	if bin := "/usr/bin/" + name; fileExists(bin) {
		return bin
	}
	return "/bin/" + name
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func runSystemctl(ctx context.Context, args ...string) error {
	_, err := systemctl(ctx, args...)
	return err
}

// systemctl runs systemctl by absolute path with a fixed, minimal
// environment (no shell, no PATH lookup) and a timeout, and returns its
// standard output; a failure carries systemctl's messages.
func systemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, systemBinary("systemctl"), args...)
	cmd.Env = helperEnv
	stdout, all := &cappedBuffer{max: 16 << 10}, &cappedBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = io.MultiWriter(stdout, all), all
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(all.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), sanitizeMessage(msg))
	}
	return stdout.String(), nil
}

// serviceBinary returns the executable in picache.service's ExecStart=.
func serviceBinary(ctx context.Context) (string, error) {
	out, err := systemctl(ctx, "show", "--property=ExecStart", "--value", Service)
	if err != nil {
		return "", err
	}
	return execStartPath(out)
}

// lockUpdates takes an exclusive lock on the data directory for the time
// of an update (the CLI and the root helper). The kernel drops it when the
// process ends, so a killed run never blocks the next one.
func lockUpdates(dataDir string) (func(), error) {
	f, err := os.Open(dataDir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another update is running")
		}
		return nil, fmt.Errorf("lock %s: %w", dataDir, err)
	}
	return func() { f.Close() }, nil
}

// checkRootOwnedChain requires dir and all its parents to be real
// directories owned by root and not writable by group or others, so nobody
// else can swap the verified binary before it is renamed into place.
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

// fileOwner returns the owner of a file.
func fileOwner(fi fs.FileInfo) (uid, gid int, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1, false
	}
	return int(st.Uid), int(st.Gid), true
}

func fchown(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }

// linkCount returns the number of hard links of a file.
func linkCount(fi fs.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

// userFreeBytes returns the free space of dir's file system that
// unprivileged users may still use (root may also use the reserved blocks).
func userFreeBytes(dir string) (uint64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}

// syncDir makes a rename in dir durable.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
