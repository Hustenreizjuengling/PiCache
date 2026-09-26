package dhcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// opener is a fake for listen4/listen6: it counts the opens and fails with
// err while set.
type opener struct {
	mu     sync.Mutex
	err4   error
	err6   error
	opens4 int
	opens6 int
	last4  *fakeConn4
	last6  *fakeConn6
}

func (o *opener) listen4() (v4Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opens4++
	if o.err4 != nil {
		return nil, o.err4
	}
	o.last4 = newFakeConn4()
	return o.last4, nil
}

func (o *opener) listen6() (v6Conn, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.opens6++
	if o.err6 != nil {
		return nil, o.err6
	}
	o.last6 = &fakeConn6{}
	return o.last6, nil
}

// errDenied is what binding port 67 without the capability returns.
var errDenied = &os.SyscallError{Syscall: "bind", Err: os.ErrPermission}

// liveSvc is a service that runs (Start was called: it opens and closes
// sockets and keeps the markers) with the given start sockets, a data
// directory and the fake opener.
func liveSvc(t *testing.T, configure func(*settings.All), socks *Sockets) (*testSvc, *opener, string) {
	t.Helper()
	ts := newTestSvc(t, configure)
	dir := t.TempDir()
	op := &opener{}
	ts.Service.d.Sockets, ts.Service.d.DataDir = socks, dir
	ts.listen4, ts.listen6 = op.listen4, op.listen6
	ts.sockMu.Lock()
	ts.live, ts.runCtx = true, t.Context()
	ts.sockMu.Unlock()
	ts.evaluate(context.Background(), true)
	return ts, op, dir
}

func markers(dir string) (sockets, ra bool) {
	return markerFile(dir, MarkerSockets), markerFile(dir, MarkerRA)
}

func markerFile(dir, name string) bool {
	fi, err := os.Lstat(filepath.Join(dir, name))
	return err == nil && fi.Mode().IsRegular()
}

func (ts *testSvc) update(t *testing.T, fn func(*settings.All)) {
	t.Helper()
	if _, err := ts.set.Update(context.Background(), func(a *settings.All) error { fn(a); return nil }); err != nil {
		t.Fatal(err)
	}
	ts.evaluate(context.Background(), true)
}

// Nothing is held while DHCP is off: switching it on opens UDP 67 and 547
// and writes the marker; router advertisements add their marker; switching
// them off removes it and closes the raw socket; switching DHCP off closes
// everything and removes both markers.
func TestSocketLifeCycle(t *testing.T) {
	icmp := &fakeICMP{}
	ts, op, dir := liveSvc(t, nil, &Sockets{bindCapable: true, rawCapable: true, icmp: icmp})
	// Off: the raw socket a legacy start opened is closed right away.
	if st := ts.d2().state(); st.v4Open || st.v6Open || st.icmpOpen {
		t.Fatalf("sockets while off: %+v", st)
	}
	if s, r := markers(dir); s || r {
		t.Fatal("markers while off")
	}
	if st := ts.Status(); st.State != StateOff || !st.Available || st.ReasonCode != "" {
		t.Fatalf("off: %+v", st)
	}
	ts.update(t, func(a *settings.All) { enabledDHCP(a) })
	if st := ts.d2().state(); !st.v4Open || !st.v6Open || op.opens4 != 1 || op.opens6 != 1 {
		t.Fatalf("switched on: %+v, opens %d/%d", st, op.opens4, op.opens6)
	}
	if s, r := markers(dir); !s || r {
		t.Fatalf("markers %v %v", s, r)
	}
	if st := ts.Status(); st.State != StateServing {
		t.Fatalf("serving: %+v", st)
	}
	// Router advertisements on: marker written; the raw socket opens only at
	// start (restart-required).
	ts.update(t, func(a *settings.All) { a.DHCP.IPv6.RouterAdvertisements = true })
	if s, r := markers(dir); !s || !r {
		t.Fatal("RA marker missing")
	}
	if r := ts.Status().IPv6.RouterAdvertisements; r.Available || r.ReasonCode != RAReasonRestart {
		t.Fatalf("RA after switching on: %+v", r)
	}
	ts.update(t, func(a *settings.All) { a.DHCP.IPv6.RouterAdvertisements = false })
	if _, r := markers(dir); r {
		t.Fatal("RA marker kept")
	}
	if r := ts.Status().IPv6.RouterAdvertisements; !r.Available || r.ReasonCode != "" {
		t.Fatalf("RA off with CAP_NET_RAW at start: %+v", r)
	}
	// Off: sockets closed (their readers end), markers removed.
	c4, c6 := op.last4, op.last6
	ts.update(t, func(a *settings.All) { a.DHCP.Enabled = false })
	if st := ts.d2().state(); st.v4Open || st.v6Open {
		t.Fatalf("sockets after switching off: %+v", st)
	}
	select {
	case <-c4.closed:
	default:
		t.Fatal("UDP 67 not closed")
	}
	if !c6.isClosed() {
		t.Fatal("UDP 547 not closed")
	}
	if s, r := markers(dir); s || r {
		t.Fatal("markers after switching off")
	}
	// A restore (settings replaced, then a new evaluation) reconciles too.
	ts.update(t, func(a *settings.All) { enabledDHCP(a); a.DHCP.IPv6.RouterAdvertisements = true })
	if s, r := markers(dir); !s || !r {
		t.Fatal("markers after a restore")
	}
	// A marker that is a symbolic link (or anything else) is replaced by a
	// regular file, never followed.
	target := filepath.Join(t.TempDir(), "target")
	os.Remove(filepath.Join(dir, MarkerRA))
	if err := os.Symlink(target, filepath.Join(dir, MarkerRA)); err == nil {
		ts.evaluate(context.Background(), true)
		if !markerFile(dir, MarkerRA) {
			t.Fatal("linked marker not replaced")
		}
		if _, err := os.Lstat(target); err == nil {
			t.Fatal("the link was followed")
		}
	} else if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
}

// With PICACHE_DHCP=off and in a bridge network no marker exists and no
// socket is opened, whatever the settings say.
func TestMarkersOptOutAndBridge(t *testing.T) {
	ts, op, dir := liveSvc(t, enabledDHCP, &Sockets{optOut: true})
	if s, r := markers(dir); s || r || op.opens4 != 0 {
		t.Fatalf("opt-out: markers %v %v, opens %d", s, r, op.opens4)
	}
	st := ts.Status()
	if st.State != StateUnavailable || st.ReasonCode != ReasonOptOut || st.Reason != "PICACHE_DHCP=off is set" || st.Available {
		t.Fatalf("opt-out: %+v", st)
	}
	if r := st.IPv6.RouterAdvertisements; r.Available || r.ReasonCode != RAReasonDHCPUnavailable {
		t.Fatalf("RA with opt-out: %+v", r)
	}
	// A stale marker (the opt-out came later) is removed.
	os.WriteFile(filepath.Join(dir, MarkerSockets), nil, 0o640)
	ts.evaluate(context.Background(), true)
	if s, _ := markers(dir); s {
		t.Fatal("stale marker kept with the opt-out")
	}

	ts, op, dir = liveSvc(t, enabledDHCP, &Sockets{bindCapable: true})
	if s, _ := markers(dir); !s || op.opens4 != 1 {
		t.Fatal("not switched on")
	}
	ts.env.bridge = func() bool { return true }
	ts.evaluate(context.Background(), true)
	if s, r := markers(dir); s || r {
		t.Fatal("markers in a bridge network")
	}
	if st := ts.Status(); st.ReasonCode != ReasonBridge || ts.d2().state().v4Open {
		t.Fatalf("bridge: %+v", st)
	}
}

// PICACHE_DHCP=on opened everything at start: the service closes what the
// settings do not need right after build.
func TestLegacyOnClosesUnneeded(t *testing.T) {
	c4, c6, icmp := newFakeConn4(), &fakeConn6{}, &fakeICMP{}
	ts, _, dir := liveSvc(t, func(a *settings.All) { enabledDHCP(a) }, &Sockets{legacy: true, bindCapable: true, rawCapable: true,
		v4: c4, v6: c6, icmp: icmp})
	if st := ts.d2().state(); !st.v4Open || !st.v6Open || st.icmpOpen {
		t.Fatalf("enabled without RA: %+v", st)
	}
	if s, r := markers(dir); !s || r {
		t.Fatal("markers")
	}
	if !icmp.isClosed() {
		t.Fatal("raw socket not closed")
	}
}

// Opening UDP 67 while running: permission denied although PiCache could
// bind the ports at start means restart-required (Docker; the marker is
// written so the next start opens them); without the capability at start,
// and for a port in use, it is socket, retried with every evaluation.
func TestRuntimeOpenFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		bindCapable bool
		code        string
		health      string
	}{
		{"docker", errDenied, true, ReasonRestartRequired, "warn"},
		{"no capability", errDenied, false, ReasonSocket, "fail"},
		{"in use", errors.New("bind: address already in use"), true, ReasonSocket, "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := newTestSvc(t, nil)
			dir := t.TempDir()
			op := &opener{err4: tc.err}
			ts.Service.d.Sockets, ts.Service.d.DataDir = &Sockets{bindCapable: tc.bindCapable, rawCapable: true}, dir
			ts.listen4, ts.listen6 = op.listen4, op.listen6
			ts.sockMu.Lock()
			ts.live = true
			ts.sockMu.Unlock()
			ts.update(t, enabledDHCP)
			st := ts.Status()
			if st.State != StateUnavailable || st.ReasonCode != tc.code || st.Reason == "" || st.Available {
				t.Fatalf("status %+v", st)
			}
			if tc.code == ReasonRestartRequired && st.Reason != reasonRestart {
				t.Fatalf("reason %q", st.Reason)
			}
			if h, msg, hint, show := ts.Health(); h != tc.health || !show || msg == "" || hint == "" {
				t.Fatalf("health %s %q %q", h, msg, hint)
			}
			if s, _ := markers(dir); !s {
				t.Fatal("marker not written: the next start would not open the ports")
			}
			if r := st.IPv6.RouterAdvertisements; r.ReasonCode != RAReasonDHCPUnavailable {
				t.Fatalf("RA %+v", r)
			}
			// Retried with every evaluation; stopping the other server is
			// picked up.
			ts.evaluate(context.Background(), true)
			if op.opens4 != 2 {
				t.Fatalf("opens %d", op.opens4)
			}
			op.mu.Lock()
			op.err4 = nil
			op.mu.Unlock()
			ts.evaluate(context.Background(), true)
			if st := ts.Status(); st.State != StateServing || st.ReasonCode != "" {
				t.Fatalf("after the port became free: %+v", st)
			}
			if h, _, _, _ := ts.Health(); h != "ok" {
				t.Fatalf("health after recovery %s", h)
			}
		})
	}
	// A failed UDP 547 alone blocks DHCPv6 with no-socket.
	ts := newTestSvc(t, nil)
	op := &opener{err6: errors.New("in use")}
	ts.Service.d.Sockets = &Sockets{bindCapable: true, rawCapable: true}
	ts.listen4, ts.listen6 = op.listen4, op.listen6
	ts.sockMu.Lock()
	ts.live = true
	ts.sockMu.Unlock()
	ts.update(t, func(a *settings.All) { enabledDHCP(a); a.DHCP.IPv6.DHCPv6 = true })
	if st := ts.Status(); st.State != StateServing || st.IPv6.DHCPv6.State != StateBlocked || st.IPv6.DHCPv6.Blockers[0] != BlockerNoSocket {
		t.Fatalf("547 failed: %+v %+v", st.State, st.IPv6.DHCPv6)
	}
}

// The reason codes of the router advertisements in every case.
func TestRAReasonCodes(t *testing.T) {
	avail := &view{available: true}
	for _, tc := range []struct {
		name string
		v    *view
		st   sockState
		on   bool
		ok   bool
		code string
	}{
		{"dhcp unavailable", &view{reason: "x"}, sockState{rawCapable: true, icmpOpen: true}, true, false, RAReasonDHCPUnavailable},
		{"drop unverified", avail, sockState{rawCapable: true, dropUnverified: "x"}, true, false, RAReasonDropUnverified},
		{"open", avail, sockState{rawCapable: true, icmpOpen: true}, true, true, ""},
		{"no cap, off", avail, sockState{}, false, false, RAReasonNoCapNetRaw},
		{"no cap, on", avail, sockState{}, true, false, RAReasonNoCapNetRaw},
		{"cap, off", avail, sockState{rawCapable: true}, false, true, ""},
		{"cap, on, failed at start", avail, sockState{rawCapable: true, icmpStartErr: "x"}, true, false, RAReasonSocket},
		{"cap, on, not opened", avail, sockState{rawCapable: true}, true, false, RAReasonRestart},
	} {
		ok, code, reason := raAvailability(tc.v, tc.st, tc.on)
		if ok != tc.ok || code != tc.code || (!ok && reason == "") {
			t.Errorf("%s: %v %q %q", tc.name, ok, code, reason)
		}
	}
}

// Could not verify that CAP_NET_RAW was dropped: the raw socket is closed
// and the health check fails, whether or not DHCP is enabled.
func TestDropUnverified(t *testing.T) {
	ts := newTestSvc(t, nil)
	icmp := &fakeICMP{}
	ts.Service.d.Sockets = &Sockets{bindCapable: true, rawCapable: true, v4: ts.conn, icmp: icmp}
	ts.d2().SetDropUnverified("could not verify that CAP_NET_RAW was dropped: no CapAmb")
	if ts.d2().HasRaw() || !icmp.isClosed() {
		t.Fatal("raw socket kept")
	}
	ts.evaluate(context.Background(), true)
	if h, msg, _, show := ts.Health(); h != "fail" || !show || msg == "" {
		t.Fatalf("health while off: %s %q %v", h, msg, show)
	}
	ts.update(t, func(a *settings.All) { enabledDHCP(a); a.DHCP.IPv6.RouterAdvertisements = true })
	if r := ts.Status().IPv6.RouterAdvertisements; r.ReasonCode != RAReasonDropUnverified || r.Available {
		t.Fatalf("RA %+v", r)
	}
	if h, _, _, _ := ts.Health(); h != "fail" {
		t.Fatalf("health while on: %s", h)
	}
}

// POST /dhcp/probe while DHCP is off opens UDP 67 for the probe only; in an
// installation that opens the ports only at start the search needs DHCP
// switched on.
func TestProbeWhileOff(t *testing.T) {
	ts, op, _ := liveSvc(t, func(a *settings.All) { enabledDHCP(a); a.DHCP.Enabled = false }, &Sockets{bindCapable: true})
	res, err := ts.Probe(context.Background())
	if err != nil || len(res.Servers) != 0 || op.opens4 != 1 {
		t.Fatalf("probe %+v %v, opens %d", res, err, op.opens4)
	}
	if len(op.last4.take()) != 1 {
		t.Fatal("no probe sent")
	}
	select {
	case <-op.last4.closed:
	default:
		t.Fatal("UDP 67 kept open after the probe")
	}
	if ts.d2().state().v4Open {
		t.Fatal("socket still registered")
	}
	op.mu.Lock()
	op.err4 = errDenied
	op.mu.Unlock()
	ts.clk.Add(11 * time.Second)
	_, err = ts.Probe(context.Background())
	if apperr.KindOf(err) != apperr.KindUnavailable || !containsAll(err.Error(), "switched on", "only at start") {
		t.Fatalf("docker probe: %v", err)
	}
	op.mu.Lock()
	op.err4 = errors.New("address already in use")
	op.mu.Unlock()
	ts.clk.Add(11 * time.Second)
	if _, err := ts.Probe(context.Background()); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("in use: %v", err)
	}
	// Opt-out: unavailable without trying.
	ts.d2().optOut = true
	ts.clk.Add(11 * time.Second)
	opens := op.opens4
	if _, err := ts.Probe(context.Background()); apperr.KindOf(err) != apperr.KindUnavailable || op.opens4 != opens {
		t.Fatalf("opt-out probe: %v", err)
	}
}

// markers are written through an os.Root; setMarkers creates 0640 files and
// removes what is not wanted.
func TestSetMarkers(t *testing.T) {
	dir := t.TempDir()
	if err := setMarkers(dir, map[string]bool{MarkerSockets: true, MarkerRA: false}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dir, MarkerSockets))
	if err != nil || fi.Size() != 0 || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640) {
		t.Fatalf("marker %v %v", fi, err)
	}
	// A directory in the marker's place is removed (if empty) and replaced.
	os.Mkdir(filepath.Join(dir, MarkerRA), 0o750)
	if err := setMarkers(dir, map[string]bool{MarkerSockets: false, MarkerRA: true}); err != nil {
		t.Fatal(err)
	}
	if s, r := markers(dir); s || !r {
		t.Fatalf("markers %v %v", s, r)
	}
	// A failure is reported (and shown in the status by the service).
	if err := setMarkers(filepath.Join(dir, "missing"), map[string]bool{MarkerSockets: true}); err == nil {
		t.Fatal("no error for a missing directory")
	}
	ts, _, _ := liveSvc(t, enabledDHCP, &Sockets{bindCapable: true})
	ts.Service.d.DataDir = filepath.Join(dir, "missing")
	ts.evaluate(context.Background(), true)
	if st := ts.Status(); st.State != StateServing || !containsAll(st.Reason, "marker") || st.MarkerError != st.Reason {
		t.Fatalf("marker failure: %+v", st)
	}
	if h, msg, hint, _ := ts.Health(); h != "warn" || !containsAll(msg, "marker") || hint == "" {
		t.Fatalf("health %s %q %q", h, msg, hint)
	}

	// With restart-required the marker error is shown too (a restart would
	// not open the ports), and the health check fails.
	ts = newTestSvc(t, nil)
	op := &opener{err4: errDenied}
	ts.Service.d.Sockets, ts.Service.d.DataDir = &Sockets{bindCapable: true, rawCapable: true}, filepath.Join(dir, "missing")
	ts.listen4, ts.listen6 = op.listen4, op.listen6
	ts.sockMu.Lock()
	ts.live = true
	ts.sockMu.Unlock()
	ts.update(t, enabledDHCP)
	st := ts.Status()
	if st.ReasonCode != ReasonRestartRequired || st.Reason != reasonRestart || !containsAll(st.MarkerError, "marker") {
		t.Fatalf("restart-required with a marker failure: %+v", st)
	}
	if h, msg, _, _ := ts.Health(); h != "fail" || !containsAll(msg, "restart will not", "marker") {
		t.Fatalf("health %s %q", h, msg)
	}
}

// OpenAtStart with the opt-out opens nothing, whatever the markers say.
func TestOpenAtStartOptOut(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, MarkerSockets), nil, 0o640)
	s := OpenAtStart(StartOptions{OptOut: true, DataDir: dir})
	if st := s.state(); !st.optOut || st.v4Open || st.v6Open || st.icmpOpen {
		t.Fatalf("opt-out %+v", st)
	}
	if runtime.GOOS != "linux" {
		if st := OpenAtStart(StartOptions{DataDir: dir}).state(); st.unsupported == "" || st.v4Open {
			t.Fatalf("not linux %+v", st)
		}
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
