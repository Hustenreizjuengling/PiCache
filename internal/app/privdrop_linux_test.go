package app

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
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

// dropNetRaw removes CAP_NET_RAW from every thread; a raw socket opened
// before keeps working, a new one cannot be opened. It runs in a child
// process (the test binary again), because it changes the process for
// good, and is skipped without CAP_NET_RAW.
func TestDropNetRaw(t *testing.T) {
	if os.Getenv("PICACHE_TEST_DROP_NET_RAW") == "1" {
		dropNetRawChild(t)
		return
	}
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6)
	if err != nil {
		t.Skipf("no CAP_NET_RAW (or no IPv6) in this environment: %v", err)
	}
	syscall.Close(fd)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDropNetRaw$", "-test.v")
	cmd.Env = append(os.Environ(), "PICACHE_TEST_DROP_NET_RAW=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "SKIP") {
		t.Skipf("child skipped:\n%s", out)
	}
	t.Logf("%s", out)
}

func dropNetRawChild(t *testing.T) {
	fd, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6)
	if err != nil {
		t.Skipf("no raw socket: %v", err)
	}
	defer syscall.Close(fd)
	if held, err := threadsHoldCap(unix.CAP_NET_RAW); err != nil || !held {
		t.Skipf("CAP_NET_RAW not in the thread sets: %v %v", held, err)
	}
	if err := dropNetRaw(); err != nil {
		if errors.Is(err, syscall.ENOTSUP) {
			t.Skip("built with cgo: AllThreadsSyscall is not available")
		}
		t.Fatal(err)
	}
	if held, err := threadsHoldCap(unix.CAP_NET_RAW); err != nil || held {
		t.Fatalf("still held: %v %v", held, err)
	}
	if fd2, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_ICMPV6); err == nil {
		syscall.Close(fd2)
		t.Fatal("a new raw socket was opened after the drop")
	}
	// The socket opened before still sends (an echo request to ::1).
	echo := []byte{128, 0, 0, 0, 0, 1, 0, 1}
	if err := syscall.Sendto(fd, echo, 0, &syscall.SockaddrInet6{Addr: [16]byte{15: 1}}); err != nil {
		t.Fatalf("the open raw socket stopped working: %v", err)
	}
	// Dropping again finds nothing to drop.
	if err := dropNetRaw(); err != nil {
		t.Fatalf("second drop: %v", err)
	}
}
