package app

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
)

func TestStatusHoldsCap(t *testing.T) {
	status := "Name:\tpicache\nCapInh:\t0000000000002400\nCapPrm:\t0000000000002400\nCapEff:\t0000000000002400\nCapBnd:\t0000000000002400\nCapAmb:\t0000000000002400\n"
	if held, err := statusHoldsCap(status, unix.CAP_NET_RAW); err != nil || !held {
		t.Fatalf("NET_RAW (bit 13) in 0x2400: %v %v", held, err)
	}
	dropped := strings.ReplaceAll(status, "2400", "0400")
	dropped = strings.Replace(dropped, "CapBnd:\t0000000000000400", "CapBnd:\t0000000000002400", 1)
	if held, err := statusHoldsCap(dropped, unix.CAP_NET_RAW); err != nil || held {
		t.Fatalf("after the drop (bounding set ignored): %v %v", held, err)
	}
	if _, err := statusHoldsCap("Name:\tx\n", unix.CAP_NET_RAW); err == nil {
		t.Fatal("status without capability sets accepted")
	}
	if _, err := statusHoldsCap("CapEff:\tzz\nCapPrm:\t0\n", unix.CAP_NET_RAW); err == nil {
		t.Fatal("garbage accepted")
	}
}

// The start sequence (bind the listeners and the DHCP sockets the markers
// ask for, drop privileges, drop CAP_NET_RAW) with CAP_NET_RAW held in the
// ambient and effective sets of every thread leaves bit 13 out of CapEff,
// CapPrm and CapAmb on every thread, and a new raw socket fails: with the
// raw-socket marker (the raw socket opened before keeps working), without
// it, and with PICACHE_DHCP=off. A failed drop makes the start fail. Each
// case runs in a child process (the test binary again), because it changes
// the process for good; it is skipped without CAP_NET_RAW (run the Linux
// tests as root in a container to cover it).
func TestDropNetRaw(t *testing.T) {
	if c := os.Getenv("PICACHE_TEST_DROP_NET_RAW"); c != "" {
		dropNetRawChild(t, c)
		return
	}
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6)
	if err != nil {
		t.Skipf("no CAP_NET_RAW (or no IPv6) in this environment: %v", err)
	}
	syscall.Close(fd)
	for _, c := range []string{"marker", "no-marker", "opt-out", "fail"} {
		t.Run(c, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestDropNetRaw$", "-test.v")
			cmd.Env = append(os.Environ(), "PICACHE_TEST_DROP_NET_RAW="+c)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child: %v\n%s", err, out)
			}
			if strings.Contains(string(out), "SKIP") {
				t.Skipf("child skipped:\n%s", out)
			}
			t.Logf("%s", out)
		})
	}
}

// raiseNetRaw puts CAP_NET_RAW into the inheritable and ambient sets of
// every thread (it is in the permitted and effective sets of root).
func raiseNetRaw() error {
	hdr := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := &[2]unix.CapUserData{}
	if err := unix.Capget(hdr, &data[0]); err != nil {
		return err
	}
	data[unix.CAP_NET_RAW/32].Inheritable |= 1 << (unix.CAP_NET_RAW % 32)
	if _, _, e := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(hdr)), uintptr(unsafe.Pointer(&data[0])), 0); e != 0 {
		return e
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_RAISE, unix.CAP_NET_RAW); e != 0 {
		return e
	}
	return nil
}

// ambientHeld reports whether CapAmb lists CAP_NET_RAW on this thread.
func ambientHeld() bool {
	b, err := os.ReadFile("/proc/thread-self/status")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "CapAmb:\t"); ok {
			return strings.TrimSpace(v) != "0000000000000000"
		}
	}
	return false
}

func dropNetRawChild(t *testing.T, c string) {
	if err := raiseNetRaw(); err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			t.Skip("built with cgo: AllThreadsSyscall is not available")
		}
		t.Skipf("cannot raise the ambient CAP_NET_RAW: %v", err)
	}
	if held, err := threadsHoldCap(unix.CAP_NET_RAW); err != nil || !held || !ambientHeld() {
		t.Skipf("CAP_NET_RAW not in the thread sets: %v %v", held, err)
	}
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, CacheDir: filepath.Join(dir, "cache"), DNSListen: []string{"127.0.0.1:0"},
		WebListen: []string{"127.0.0.1:0"}, LogFormat: "text"}
	switch c {
	case "marker":
		for _, m := range []string{dhcp.MarkerSockets, dhcp.MarkerRA} {
			if err := os.WriteFile(filepath.Join(dir, m), nil, 0o640); err != nil {
				t.Fatal(err)
			}
		}
	case "opt-out":
		cfg.DHCP = config.DHCPOff
		os.WriteFile(filepath.Join(dir, dhcp.MarkerRA), nil, 0o640) // ignored
	case "fail":
		clearNetRaw = func() error { return errors.New("capset refused") }
	}
	a := newApp(cfg, slog.New(slog.DiscardHandler))
	if err := a.bindListeners(); err != nil {
		t.Fatal(err)
	}
	defer a.ln.closeAll()
	if err := a.dropPrivileges(); err != nil {
		t.Fatal(err)
	}
	err := a.dropRawCapability()
	if c == "fail" {
		if err == nil || !strings.Contains(err.Error(), "refusing to run") {
			t.Fatalf("a failed drop: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if held, err := threadsHoldCap(unix.CAP_NET_RAW); err != nil || held {
		t.Fatalf("still held: %v %v", held, err)
	}
	if fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6); err == nil {
		syscall.Close(fd)
		t.Fatal("a new raw socket was opened after the drop")
	}
	if got, want := a.ln.dhcp.HasRaw(), c == "marker"; got != want {
		t.Fatalf("raw socket open: %v, want %v", got, want)
	}
	if c == "opt-out" {
		if v4, v6, raw := a.ln.dhcp.Errors(); v4 != "" || v6 != "" || raw != "" {
			t.Fatalf("opt-out opened something: %q %q %q", v4, v6, raw)
		}
	}
	// Dropping again finds nothing to drop.
	if unverified, err := dropNetRaw(); err != nil || unverified != nil {
		t.Fatalf("second drop: %v %v", unverified, err)
	}
}
