package portcheck

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/notify"
)

// fakeNet answers connection attempts from a table: addr → state, and
// optionally a different state for the second try of a port.
type fakeNet struct {
	mu       sync.Mutex
	states   map[string]int
	second   map[string]int
	tries    map[string]int
	timeouts []time.Duration
}

func (f *fakeNet) set(ip string, ports map[int]int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.states {
		if strings.HasPrefix(k, ip+":") {
			delete(f.states, k)
		}
	}
	for p, s := range ports {
		f.states[net.JoinHostPort(ip, strconv.Itoa(p))] = s
	}
}

func (f *fakeNet) dial(_ context.Context, addr string, timeout time.Duration) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries[addr]++
	f.timeouts = append(f.timeouts, timeout)
	if s, ok := f.second[addr]; ok && f.tries[addr] > 1 {
		return s
	}
	return f.states[addr]
}

type env struct {
	c        *Checker
	net      *fakeNet
	db       *db.DB
	cfg      Config
	targets  []Target
	msgs     []notify.Message
	mu       sync.Mutex
	clock    time.Time
	unusable error
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "logs.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	e := &env{db: d, cfg: Config{Enabled: true, Interval: 24 * time.Hour},
		net:   &fakeNet{states: map[string]int{}, second: map[string]int{}, tries: map[string]int{}},
		clock: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	e.targets = []Target{{netip.MustParseAddr("192.168.1.20"), "aa:bb:cc:00:00:01"}, {netip.MustParseAddr("192.168.1.30"), ""}}
	c, err := New(context.Background(), Deps{DB: d,
		Config: func() Config {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.cfg
		},
		Targets: func(context.Context) ([]Target, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return slices.Clone(e.targets), e.unusable
		},
		Name: func(ip netip.Addr, mac string) string {
			if mac == "aa:bb:cc:00:00:01" {
				return "Laptop"
			}
			return ""
		},
		Emit: func(m notify.Message) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.msgs = append(e.msgs, m)
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	c.dial = e.net.dial
	c.pace = func(ctx context.Context) bool { return ctx.Err() == nil }
	c.now = func() time.Time {
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.clock
	}
	c.start = e.clock
	e.c = c
	return e
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.clock = e.clock.Add(d)
}

func (e *env) messages() []notify.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.msgs)
}

func (e *env) device(t *testing.T, key string) Device {
	t.Helper()
	st, err := e.c.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range st.Devices {
		if d.Device == key {
			return d
		}
	}
	t.Fatalf("device %s missing: %+v", key, st.Devices)
	return Device{}
}

func openList(d Device) []int {
	var out []int
	for _, p := range d.Open {
		out = append(out, p.Port)
	}
	return out
}

func TestChecksRecordAndReportChanges(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	laptop := "mac:aa:bb:cc:00:00:01"
	e.net.set("192.168.1.20", map[int]int{22: open, 80: open, 443: closed})

	// The first answer only records the ports; a silent device is listed
	// without an answer.
	e.c.round(ctx)
	if len(e.messages()) != 0 {
		t.Fatalf("first check reported: %+v", e.messages())
	}
	d := e.device(t, laptop)
	if !slices.Equal(openList(d), []int{22, 80}) || d.IP != "192.168.1.20" || d.MAC != "aa:bb:cc:00:00:01" ||
		!d.LastAnswer.Equal(e.clock) || d.Open[0].Service != "SSH" {
		t.Fatalf("laptop = %+v", d)
	}
	if s := e.device(t, "ip:192.168.1.30"); !s.LastAnswer.IsZero() || len(s.Open) != 0 {
		t.Fatalf("silent device = %+v", s)
	}
	if n := len(e.c.ports(e.cfg)) * 2; len(e.net.timeouts) != n {
		t.Fatalf("%d attempts, want %d", len(e.net.timeouts), n)
	}

	// 8080 opens, 80 closes; 22 times out once but answers the second try.
	e.advance(24 * time.Hour)
	e.net.set("192.168.1.20", map[int]int{22: noAnswer, 443: closed, 8080: open})
	e.net.second["192.168.1.20:22"] = open
	e.c.round(ctx)
	msgs := e.messages()
	if len(msgs) != 1 || msgs[0].Event != notify.EventDevicePorts || msgs[0].Severity != notify.SeverityWarning ||
		msgs[0].Title != "Open ports changed on Laptop (192.168.1.20)" ||
		msgs[0].Message != "Laptop (192.168.1.20): opened 8080 (HTTP); closed 80 (HTTP)" {
		t.Fatalf("messages = %+v", msgs)
	}
	if d := e.device(t, laptop); !slices.Equal(openList(d), []int{22, 8080}) || !d.Open[0].Since.Equal(d.FirstCheck) {
		t.Fatalf("after the change: %+v", d)
	}
	page, err := e.c.Changes(ctx, laptop, 0, 0)
	if err != nil || page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("changes = %+v, %v", page, err)
	}
	got := map[int]bool{}
	for _, ch := range page.Items {
		got[ch.Port] = ch.Opened
	}
	if len(got) != 2 || !got[8080] || got[80] {
		t.Fatalf("changes = %+v", page.Items)
	}
	// The second try of a port that looked closed waits longer.
	if !slices.Contains(e.net.timeouts, recheckTimeout) {
		t.Fatal("no second try with the longer timeout")
	}

	// A device that does not answer keeps its ports; the silent device's
	// first answer only records.
	e.advance(24 * time.Hour)
	e.net.set("192.168.1.20", nil)
	delete(e.net.second, "192.168.1.20:22")
	e.net.set("192.168.1.30", map[int]int{9100: open})
	e.c.round(ctx)
	if len(e.messages()) != 1 {
		t.Fatalf("reported: %+v", e.messages()[1:])
	}
	if d := e.device(t, laptop); !slices.Equal(openList(d), []int{22, 8080}) || d.LastAnswer.Equal(e.clock) || !d.LastCheck.Equal(e.clock) {
		t.Fatalf("silent laptop = %+v", d)
	}

	// Only closed ports: info; two devices: one message.
	e.advance(24 * time.Hour)
	e.net.set("192.168.1.20", map[int]int{22: open, 8080: closed})
	e.net.set("192.168.1.30", map[int]int{80: closed})
	e.c.round(ctx)
	msgs = e.messages()
	if len(msgs) != 2 || msgs[1].Severity != notify.SeverityInfo || msgs[1].Title != "Open ports changed on 2 devices" ||
		msgs[1].Message != "Laptop (192.168.1.20): closed 8080 (HTTP)\n192.168.1.30: closed 9100 (Printer)" {
		t.Fatalf("messages = %+v", msgs[1:])
	}
	if all, _ := e.c.Changes(ctx, "", 0, 0); all.Total != 4 || all.Items[0].Time.Before(all.Items[3].Time) {
		t.Fatalf("all changes = %+v", all)
	}

	// Forget removes the device with its ports and changes.
	if err := e.c.Forget(ctx, laptop); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Forget(ctx, laptop); apperr.KindOf(err) != apperr.KindNotFound {
		t.Fatalf("forget twice: %v", err)
	}
	if all, _ := e.c.Changes(ctx, "", 0, 0); all.Total != 1 {
		t.Fatalf("changes after forget = %d", all.Total)
	}
}

func TestChecksExtraPortsAndPrune(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.cfg.ExtraPorts = []int{22, 25565}
	ports := e.c.ports(e.cfg)
	if !slices.IsSortedFunc(ports, func(a, b Port) int { return a.Port - b.Port }) || len(ports) != len(DefaultPorts)+1 ||
		!slices.Contains(ports, Port{Port: 25565}) {
		t.Fatalf("ports = %v", ports)
	}
	e.net.set("192.168.1.20", map[int]int{25565: open})
	e.c.round(ctx)
	e.advance(24 * time.Hour)
	e.net.set("192.168.1.20", map[int]int{25565: closed})
	e.c.round(ctx)
	// Changes older than 180 days and devices without an answer for 90 days
	// go.
	e.advance(91 * 24 * time.Hour)
	e.net.set("192.168.1.20", nil)
	e.c.round(ctx)
	st, _ := e.c.State(ctx)
	if len(st.Devices) != 0 {
		t.Fatalf("devices after 91 days: %+v", st.Devices)
	}
	if all, _ := e.c.Changes(ctx, "", 0, 0); all.Total != 1 {
		t.Fatalf("changes after 91 days = %d", all.Total)
	}
	e.advance(90 * 24 * time.Hour)
	e.c.round(ctx)
	if all, _ := e.c.Changes(ctx, "", 0, 0); all.Total != 0 {
		t.Fatalf("changes after 181 days = %d", all.Total)
	}
}

func TestChecksSchedule(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	start := e.c.start
	if due := e.c.nextRunLocked(e.cfg); !due.Equal(start.Add(firstDelay)) {
		t.Fatalf("first check due %v, want %v", due, start.Add(firstDelay))
	}
	off := e.cfg
	off.Enabled = false
	if !e.c.nextRunLocked(off).IsZero() {
		t.Fatal("a check is due while off")
	}
	// Switched on: a minute later.
	e.mu.Lock()
	e.cfg.Enabled = false
	e.mu.Unlock()
	e.c.SettingsChanged()
	e.mu.Lock()
	e.cfg.Enabled = true
	e.mu.Unlock()
	e.advance(time.Hour)
	e.c.SettingsChanged()
	if due := e.c.nextRunLocked(e.cfg); !due.Equal(e.clock.Add(enabledDelay)) {
		t.Fatalf("due after switching on %v", due)
	}
	e.c.round(ctx)
	e.c.mu.Lock()
	due := e.c.nextRunLocked(e.cfg)
	e.c.mu.Unlock()
	if d := due.Sub(e.clock); d < 21*time.Hour || d > 27*time.Hour {
		t.Fatalf("next check in %v", d)
	}
	// After a restart the last check is read back.
	c2, err := New(ctx, e.c.d)
	if err != nil {
		t.Fatal(err)
	}
	if !c2.lastRun.Equal(e.clock) {
		t.Fatalf("restored last run %v, want %v", c2.lastRun, e.clock)
	}
}

func TestChecksRunAndLoop(t *testing.T) {
	e := newEnv(t)
	if err := e.c.Run(context.Background()); apperr.KindOf(err) != apperr.KindUnavailable {
		t.Fatalf("before Start: %v", err)
	}
	e.net.set("192.168.1.20", map[int]int{22: open})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.c.Start(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := e.c.Run(ctx)
		if err == nil {
			break
		}
		if apperr.KindOf(err) != apperr.KindUnavailable || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(time.Millisecond)
	}
	for {
		st, err := e.c.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !st.Running && !st.LastRun.IsZero() {
			if len(st.Devices) != 2 || !st.Enabled || st.LastError != "" {
				t.Fatalf("state = %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the check did not run")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.c.Run(ctx); apperr.KindOf(err) != apperr.KindTooMany {
		t.Fatalf("within a minute: %v", err)
	}
	e.mu.Lock()
	e.cfg.Enabled = false
	e.mu.Unlock()
	if err := e.c.Run(ctx); apperr.KindOf(err) != apperr.KindUnavailable || !strings.Contains(err.Error(), "off") {
		t.Fatalf("while off: %v", err)
	}
	e.mu.Lock()
	e.cfg.Enabled = true
	e.unusable = apperr.Unavailable("bridge network")
	e.mu.Unlock()
	if err := e.c.Run(ctx); err == nil || !strings.Contains(err.Error(), "bridge") {
		t.Fatalf("unusable: %v", err)
	}
}

// The real dialer: an accepting port is open, a refusing one closed.
func TestDialTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if s := dialTCP(context.Background(), addr, time.Second); s != open {
		t.Fatalf("listening port: %d", s)
	}
	_ = ln.Close()
	if s := dialTCP(context.Background(), addr, time.Second); s != closed {
		t.Fatalf("closed port: %d", s)
	}
}
