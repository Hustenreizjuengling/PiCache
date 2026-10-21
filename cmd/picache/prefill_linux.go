package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill"
	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

const (
	prefillUnit    = "picache-prefill.service"
	prefillUnitDir = "/usr/local/lib/systemd/system"
	// prefillState is the state directory of the unit (DynamicUser): the
	// sign-ins, the selection and the history.
	prefillState = "/var/lib/private/picache-prefill"
)

// prefillEnable installs the unit when the host has none (an update from
// the web UI does not add units), then enables and starts the service.
func prefillEnable(args []string) int {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "usage: sudo picache prefill enable")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "picache prefill enable: run it as root (sudo)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	frag, _ := systemctlOut(ctx, "show", "-p", "FragmentPath", "--value", prefillUnit)
	if frag = strings.TrimSpace(frag); frag == "" {
		exe, err := os.Executable()
		if err == nil {
			exe, err = filepath.EvalSymlinks(exe)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "picache prefill enable: cannot find this binary:", err)
			return 1
		}
		unit := strings.Replace(prefill.UnitFile, "ExecStart="+prefill.UnitBinary+" ", "ExecStart="+exe+" ", 1)
		if err := writeUnitFile(prefillUnitDir, prefillUnit, []byte(unit)); err != nil {
			fmt.Fprintln(os.Stderr, "picache prefill enable:", err)
			return 1
		}
		fmt.Printf("installed %s (ExecStart=%s prefill)\n", filepath.Join(prefillUnitDir, prefillUnit), exe)
		if _, err := systemctlOut(ctx, "daemon-reload"); err != nil {
			fmt.Fprintln(os.Stderr, "picache prefill enable:", err)
			return 1
		}
	}
	if _, err := systemctlOut(ctx, "enable", "--now", prefillUnit); err != nil {
		fmt.Fprintln(os.Stderr, "picache prefill enable:", err)
		return 1
	}
	socket := os.Getenv("PICACHE_PREFILL_SOCKET")
	if socket == "" {
		socket = prefillapi.DefaultSocket
	}
	c := prefillapi.NewClient(socket)
	for range 20 {
		if st, err := c.Status(ctx); err == nil {
			fmt.Printf("the prefill service %s runs (cache %s): open Cache -> Prefill in the web UI\n", st.Version, st.Cache.URL)
			fmt.Println("stop it again with: sudo picache prefill disable")
			return 0
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "picache prefill enable: the service was started but does not answer; see: journalctl -u picache-prefill")
	return 1
}

// prefillDisable stops and disables the service; --forget also deletes its
// state (the sign-ins: sign out in the web UI first, which revokes them).
func prefillDisable(args []string) int {
	forget := false
	for _, a := range args {
		switch a {
		case "--forget":
			forget = true
		default:
			fmt.Fprintln(os.Stderr, "usage: sudo picache prefill disable [--forget]")
			return 2
		}
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "picache prefill disable: run it as root (sudo)")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := systemctlOut(ctx, "disable", "--now", prefillUnit); err != nil {
		fmt.Fprintln(os.Stderr, "picache prefill disable:", err)
		return 1
	}
	fmt.Println("the prefill service is stopped and disabled")
	if !forget {
		fmt.Printf("its state (sign-ins, selection, history) stays in %s; delete it with: sudo picache prefill disable --forget\n", prefillState)
		return 0
	}
	if err := os.RemoveAll(prefillState); err != nil {
		fmt.Fprintln(os.Stderr, "picache prefill disable:", err)
		return 1
	}
	_ = os.Remove("/var/lib/picache-prefill") // the symbolic link of DynamicUser
	fmt.Printf("deleted %s; a sign-in that was not signed out in the web UI stays valid at Steam until you revoke it there\n", prefillState)
	return 0
}

// writeUnitFile writes a unit file (0644, root) through a temporary file;
// an existing file is never replaced.
func writeUnitFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer r.Close()
	if _, err := r.Lstat(name); err == nil {
		return fmt.Errorf("%s exists already", filepath.Join(dir, name))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp := "." + name + ".tmp"
	f, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.Rename(tmp, name)
	}
	if err != nil {
		_ = r.Remove(tmp)
	}
	return err
}

// systemctlOut runs systemctl by absolute path with a minimal environment.
func systemctlOut(ctx context.Context, args ...string) (string, error) {
	bin := "/usr/bin/systemctl"
	if _, err := os.Stat(bin); err != nil {
		bin = "/bin/systemctl"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "SYSTEMD_PAGER=", "SYSTEMD_COLORS=0"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 500 {
			msg = msg[:500]
		}
		return out.String(), fmt.Errorf("systemctl %s: %v %s", strings.Join(args, " "), err, msg)
	}
	return out.String(), nil
}
