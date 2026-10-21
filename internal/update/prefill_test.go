package update

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// prefillHost is a fake host: systemctl answers from a table.
type prefillHost struct {
	calls     []string
	fragment  string
	props     string
	enableErr error
}

func (f *prefillHost) host(unitDir string) PrefillHost {
	return PrefillHost{
		Systemctl: func(_ context.Context, args ...string) (string, error) {
			c := strings.Join(args, " ")
			f.calls = append(f.calls, c)
			switch {
			case strings.HasPrefix(c, "show -p FragmentPath"):
				return f.fragment + "\n", nil
			case strings.HasPrefix(c, "show -p ActiveState"):
				return f.props, nil
			case strings.HasPrefix(c, "enable"):
				return "", f.enableErr
			}
			return "", nil
		},
		Journal: func(_ context.Context, unit string, _ time.Time) string {
			return "picache[1]: level=ERROR msg=\"picache prefill exited with error\" err=\"\x1b[31mboom\""
		},
		UnitDir: unitDir,
		Sleep:   func(context.Context, time.Duration) error { return nil },
	}
}

func TestPrefillApplyPending(t *testing.T) {
	data, units := t.TempDir(), t.TempDir()
	o := func(f *prefillHost) PrefillOptions {
		return PrefillOptions{DataDir: data, UnitFile: "[Service]\nExecStart=/x/picache prefill\n", Host: f.host(units)}
	}
	ctx := context.Background()

	// Nothing queued: nothing happens.
	f := &prefillHost{}
	if err := PrefillApplyPending(ctx, o(f)); err != nil || len(f.calls) != 0 || ReadPrefillStatus(data, time.Now()) != nil {
		t.Fatalf("empty queue: %v %v", err, f.calls)
	}

	// Enable on a host without the unit: installed, enabled, checked.
	if err := QueuePrefill(data, PrefillRequest{Action: PrefillEnable, RequestedAt: time.Now(), RequestedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	if st := ReadPrefillStatus(data, time.Now()); st == nil || st.State != StateRunning || st.Action != PrefillEnable || st.Message != msgPrefillWaiting {
		t.Fatalf("queued %+v", st)
	}
	if st := ReadPrefillStatus(data, time.Now().Add(4*time.Minute)); st.State != StateFailed || st.Message != msgPrefillNotPickedUp {
		t.Fatalf("stale %+v", st)
	}
	f = &prefillHost{props: "ActiveState=active\nSubState=running\nNRestarts=0\n"}
	if err := PrefillApplyPending(ctx, o(f)); err != nil {
		t.Fatal(err)
	}
	want := []string{"show -p FragmentPath --value " + PrefillUnit, "daemon-reload", "reset-failed " + PrefillUnit,
		"enable --now " + PrefillUnit, "show -p ActiveState -p SubState -p NRestarts " + PrefillUnit}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("calls %q", f.calls)
	}
	b, err := os.ReadFile(filepath.Join(units, PrefillUnit))
	if err != nil || string(b) != "[Service]\nExecStart=/x/picache prefill\n" {
		t.Fatalf("unit %q %v", b, err)
	}
	st := ReadPrefillStatus(data, time.Now())
	if st == nil || st.State != StateSucceeded || st.Action != PrefillEnable || !strings.Contains(st.Message, "installed "+filepath.Join(units, PrefillUnit)) {
		t.Fatalf("status %+v", st)
	}
	for _, n := range []string{requestFile, claimFile} {
		if _, err := os.Lstat(filepath.Join(PrefillRequestsDir(data), n)); !os.IsNotExist(err) {
			t.Errorf("%s left behind", n)
		}
	}

	// The unit exists but the service keeps failing: the journal is reported.
	_ = QueuePrefill(data, PrefillRequest{Action: PrefillEnable})
	f = &prefillHost{fragment: "/usr/lib/systemd/system/" + PrefillUnit, props: "ActiveState=activating\nSubState=auto-restart\nNRestarts=2\n"}
	if err := PrefillApplyPending(ctx, o(f)); err == nil {
		t.Fatal("a failing start succeeded")
	}
	if slices.Contains(f.calls, "daemon-reload") {
		t.Error("an existing unit was installed again")
	}
	st = ReadPrefillStatus(data, time.Now())
	if st.State != StateFailed || !strings.Contains(st.Message, "does not keep running (activating/auto-restart, restarts 2)") ||
		!strings.Contains(st.Message, "\npicache[1]: level=ERROR") || strings.Contains(st.Message, "\x1b") {
		t.Fatalf("failure %+v", st)
	}

	// Disable.
	_ = QueuePrefill(data, PrefillRequest{Action: PrefillDisable})
	f = &prefillHost{}
	if err := PrefillApplyPending(ctx, o(f)); err != nil || !slices.Equal(f.calls, []string{"disable --now " + PrefillUnit}) {
		t.Fatalf("disable %v %q", err, f.calls)
	}
	if st := ReadPrefillStatus(data, time.Now()); st.State != StateSucceeded || st.Action != PrefillDisable {
		t.Fatalf("disabled %+v", st)
	}

	// An invalid request fails without a systemctl call; a stale claim is
	// reported first.
	dir := PrefillRequestsDir(data)
	if err := os.WriteFile(filepath.Join(dir, claimFile), []byte(`{"action":"enable"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if st := ReadPrefillStatus(data, time.Now().Add(6*time.Minute)); st.State != StateFailed || st.Message != msgPrefillInterrupted {
		t.Fatalf("stale claim %+v", st)
	}
	if err := os.WriteFile(filepath.Join(dir, requestFile), []byte(`{"action":"reboot"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	f = &prefillHost{}
	if err := PrefillApplyPending(ctx, o(f)); err == nil || len(f.calls) != 0 {
		t.Fatalf("invalid request %v %q", err, f.calls)
	}
	if st := ReadPrefillStatus(data, time.Now()); st.State != StateFailed || !strings.Contains(st.Message, "unknown action") {
		t.Fatalf("invalid %+v", st)
	}
	if err := QueuePrefill(data, PrefillRequest{Action: "reboot"}); err == nil {
		t.Error("an unknown action was queued")
	}
}
