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
		diagnose:     unitDiagnostics,
		serviceOwner: ownerOf,
		hasHelper:    func(name string) bool { return mountHelpers()[name] },
	}
}

// helperEnv is the fixed, minimal environment of the commands the helper runs.
var helperEnv = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}

// systemBinary returns /usr/bin/<name>, or /bin/<name> on systems without merged /usr.
func systemBinary(name string) string {
	if bin := "/usr/bin/" + name; fileExists(bin) {
		return bin
	}
	return "/bin/" + name
}

// runSystemctl runs systemctl by absolute path with a fixed, minimal
// environment (no shell, no PATH lookup) and a timeout. It returns the
// standard output; a failure is a *systemctlError with systemctl's messages.
func runSystemctl(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, systemBinary("systemctl"), args...)
	cmd.Env = helperEnv
	stdout, all := &cappedBuffer{max: 4 << 10}, &cappedBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = splitWriter{stdout, all}, all
	if err := cmd.Run(); err != nil {
		return stdout.String(), &systemctlError{args: args, err: err, output: systemctlOutput(all.String())}
	}
	return stdout.String(), nil
}

// splitWriter writes to both buffers (stdout also goes into the combined log).
type splitWriter struct{ a, b *cappedBuffer }

func (w splitWriter) Write(p []byte) (int, error) {
	w.a.Write(p)
	return w.b.Write(p)
}

// unitDiagnostics returns systemd's result of the unit and its journal
// messages since the job started (fixed arguments; errors are ignored: this
// only improves the message).
func unitDiagnostics(ctx context.Context, unit string, since time.Time) unitDiag {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var d unitDiag
	if out, err := runSystemctl(ctx, "show", "-p", "Result", "--value", unit); err == nil {
		d.result = strings.TrimSpace(out)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 { // journald may not have written the mount helper's last lines yet
			select {
			case <-time.After(500 * time.Millisecond):
			case <-ctx.Done():
				return d
			}
		}
		cmd := exec.CommandContext(ctx, systemBinary("journalctl"), "--no-pager", "--quiet", "--output=cat",
			"--lines=30", "--unit="+unit, fmt.Sprintf("--since=@%d.%06d", since.Unix(), since.Nanosecond()/1000))
		cmd.Env = helperEnv
		out := &cappedBuffer{max: 16 << 10}
		cmd.Stdout = out
		if cmd.Run() != nil {
			return d
		}
		d.lines = strings.Split(out.String(), "\n")
		if len(relevantJournalLines(d.lines)) > 0 {
			break
		}
	}
	return d
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
