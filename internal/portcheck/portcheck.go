// Package portcheck checks which TCP ports the devices of the local network
// accept (clients.portChecks, off by default; docs/ARCHITECTURE.md 17 "Port
// checks"). Every clients.portChecks.intervalHours (±10 %), and when an
// admin asks for it, PiCache tries a TCP connection (connect, then close;
// nothing is sent) to a built-in list of common ports plus
// clients.portChecks.extraPorts on every IPv4 device of the kernel's
// neighbour table that is inside a private network this machine is
// connected to (never its own addresses), at most 256 devices. It keeps
// the ports that accepted the connection and reports changes: a port that
// accepts is open; a device that accepted or refused (RST) at least one
// port answered; a device that answered nothing (asleep, gone, or a
// firewall that drops everything) keeps its last result. A port that looks
// closed is tried once more with a longer timeout before it is recorded as
// closed. The first check of a device only records its ports.
//
// Bounds: 16 connection attempts at a time (4 per device), at most 100 per
// second, 1.5 s per attempt (3 s for the second try); a check at most
// once a minute; changes are kept 180 days (at most 20 000), devices that
// did not answer for 90 days are forgotten. An ordinary unprivileged TCP
// socket: no raw sockets, no capabilities.
//
// Tables (logs.db, component "portcheck"): portcheck_devices,
// portcheck_open, portcheck_changes. They are runtime data: never part of
// a backup.
package portcheck

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/notify"
)

// Bounds and intervals.
const (
	MaxTargets      = 256
	parallel        = 16
	perDevice       = 4
	attemptsPerSec  = 100
	dialTimeout     = 1500 * time.Millisecond
	recheckTimeout  = 3 * time.Second
	runCooldown     = time.Minute
	firstDelay      = 10 * time.Minute // after the start, without an earlier check
	enabledDelay    = time.Minute      // after the checks were switched on
	changeRetention = 180 * 24 * time.Hour
	maxChanges      = 20_000
	deviceRetention = 90 * 24 * time.Hour
	maxChangePage   = 500
	maxNotifyLines  = 20
)

// DefaultPorts are the ports every check tries, with the usual service.
var DefaultPorts = []Port{
	{21, "FTP"}, {22, "SSH"}, {23, "Telnet"}, {25, "SMTP"}, {53, "DNS"}, {80, "HTTP"}, {110, "POP3"},
	{111, "RPC"}, {135, "MS RPC"}, {139, "NetBIOS"}, {143, "IMAP"}, {443, "HTTPS"}, {445, "SMB"},
	{548, "AFP"}, {554, "RTSP"}, {631, "IPP"}, {993, "IMAPS"}, {995, "POP3S"}, {1883, "MQTT"},
	{2049, "NFS"}, {3000, ""}, {3306, "MySQL"}, {3389, "RDP"}, {5000, ""}, {5432, "PostgreSQL"},
	{5900, "VNC"}, {6379, "Redis"}, {7000, "AirPlay"}, {8000, "HTTP"}, {8008, "HTTP"},
	{8080, "HTTP"}, {8081, "HTTP"}, {8123, "Home Assistant"}, {8443, "HTTPS"}, {8883, "MQTTS"},
	{9000, ""}, {9090, ""}, {9100, "Printer"}, {10000, ""}, {27017, "MongoDB"}, {32400, "Plex"},
	{49152, "UPnP"}, {62078, "iOS sync"},
}

var serviceNames = func() map[int]string {
	m := make(map[int]string, len(DefaultPorts))
	for _, p := range DefaultPorts {
		m[p.Port] = p.Service
	}
	return m
}()

// Service returns the usual service of a port ("" if none is known).
func Service(port int) string { return serviceNames[port] }

var migrations = []string{
	`CREATE TABLE portcheck_devices (
		device      TEXT    PRIMARY KEY,
		mac         TEXT    NOT NULL DEFAULT '',
		ip          TEXT    NOT NULL,
		first_check INTEGER NOT NULL,
		last_check  INTEGER NOT NULL,
		last_answer INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID;
	CREATE TABLE portcheck_open (
		device TEXT    NOT NULL,
		port   INTEGER NOT NULL,
		since  INTEGER NOT NULL,
		PRIMARY KEY (device, port)
	) WITHOUT ROWID;
	CREATE TABLE portcheck_changes (
		id     INTEGER PRIMARY KEY,
		time   INTEGER NOT NULL,
		device TEXT    NOT NULL,
		ip     TEXT    NOT NULL,
		port   INTEGER NOT NULL,
		opened INTEGER NOT NULL
	);
	CREATE INDEX portcheck_changes_device ON portcheck_changes (device, time);
	CREATE INDEX portcheck_changes_time ON portcheck_changes (time);`,
}

// Port is a checked port.
type Port struct {
	Port    int    `json:"port"`
	Service string `json:"service,omitempty"`
}

// Config is the part of the settings the checks follow.
type Config struct {
	Enabled    bool
	Interval   time.Duration
	ExtraPorts []int
}

// Target is a device to check.
type Target struct {
	IP  netip.Addr
	MAC string // "" if unknown
}

// Deps are the checker's inputs.
type Deps struct {
	DB     *db.DB // logs.db
	Config func() Config
	// Targets returns the devices to check (apperr.Unavailable when checks
	// cannot run here, e.g. in a container bridge network).
	Targets func(ctx context.Context) ([]Target, error)
	// Name returns a device's display name ("" if none).
	Name func(ip netip.Addr, mac string) string
	Emit func(notify.Message)
	Log  *slog.Logger
}

// State is GET /network/ports.
type State struct {
	Enabled   bool      `json:"enabled"`
	Running   bool      `json:"running"`
	StartedAt time.Time `json:"startedAt,omitzero"` // of the running or last check
	LastRun   time.Time `json:"lastRun,omitzero"`   // end of the last check
	NextRun   time.Time `json:"nextRun,omitzero"`   // while enabled
	LastError string    `json:"lastError,omitempty"`
	Ports     []Port    `json:"ports"`
	Devices   []Device  `json:"devices"`
}

// Device is a checked device.
type Device struct {
	Device     string     `json:"device"` // mac:<mac> or ip:<address>
	MAC        string     `json:"mac,omitempty"`
	IP         string     `json:"ip"`
	FirstCheck time.Time  `json:"firstCheck"`
	LastCheck  time.Time  `json:"lastCheck"`
	LastAnswer time.Time  `json:"lastAnswer,omitzero"` // the last check it answered
	Open       []OpenPort `json:"open"`
}

// OpenPort is a port that accepted the last connection.
type OpenPort struct {
	Port    int       `json:"port"`
	Service string    `json:"service,omitempty"`
	Since   time.Time `json:"since"`
}

// Change is a port that opened or closed.
type Change struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Device  string    `json:"device"`
	IP      string    `json:"ip"`
	Port    int       `json:"port"`
	Service string    `json:"service,omitempty"`
	Opened  bool      `json:"opened"`
}

// probe results of one port.
const (
	noAnswer = iota
	closed   // refused (RST)
	open
)

// Checker runs the port checks. It implements api.PortChecks.
type Checker struct {
	d Deps

	// Replaced in tests.
	dial  func(ctx context.Context, addr string, timeout time.Duration) int
	now   func() time.Time
	pace  func(ctx context.Context) bool // waits for the next attempt
	start time.Time

	kick chan struct{}

	mu        sync.Mutex
	runCtx    context.Context
	running   bool
	pending   bool // a check was asked for
	startedAt time.Time
	lastRun   time.Time
	lastErr   string
	enabledAt time.Time // when the checks were last switched on
	wasOn     bool
	jitter    float64
	rounds    sync.WaitGroup
}

// New migrates the tables and returns the checker.
func New(ctx context.Context, d Deps) (*Checker, error) {
	if err := d.DB.Migrate(ctx, "portcheck", migrations); err != nil {
		return nil, fmt.Errorf("portcheck: %w", err)
	}
	c := &Checker{d: d, dial: dialTCP, now: time.Now, kick: make(chan struct{}, 1), jitter: 0.9 + rand.Float64()*0.2}
	c.start = c.now()
	c.pace = newPacer(attemptsPerSec)
	var last int64
	if err := d.DB.R.QueryRowContext(ctx, `SELECT coalesce(max(last_check), 0) FROM portcheck_devices`).Scan(&last); err != nil {
		return nil, fmt.Errorf("portcheck: %w", err)
	}
	if last > 0 {
		c.lastRun = time.UnixMilli(last)
	}
	c.wasOn = d.Config().Enabled
	return c, nil
}

// newPacer returns a wait of 1/rate s between attempts.
func newPacer(rate int) func(ctx context.Context) bool {
	var mu sync.Mutex
	var next time.Time
	gap := time.Second / time.Duration(rate)
	return func(ctx context.Context) bool {
		mu.Lock()
		now := time.Now()
		if next.Before(now) {
			next = now
		}
		wait := next.Sub(now)
		next = next.Add(gap)
		mu.Unlock()
		if wait <= 0 {
			return ctx.Err() == nil
		}
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
}

// dialTCP tries a TCP connection and closes it at once.
func dialTCP(ctx context.Context, addr string, timeout time.Duration) int {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return open
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return closed
	}
	return noAnswer
}

// SettingsChanged is called when the settings change: switching the
// checks on schedules the first one a minute later.
func (c *Checker) SettingsChanged() {
	on := c.d.Config().Enabled
	c.mu.Lock()
	if on && !c.wasOn {
		c.enabledAt = c.now()
	}
	c.wasOn = on
	c.mu.Unlock()
	c.signal()
}

func (c *Checker) signal() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// ports returns the checked ports, sorted.
func (c *Checker) ports(cfg Config) []Port {
	out := slices.Clone(DefaultPorts)
	for _, p := range cfg.ExtraPorts {
		if p > 0 && p <= 65535 && !slices.ContainsFunc(out, func(x Port) bool { return x.Port == p }) {
			out = append(out, Port{Port: p})
		}
	}
	slices.SortFunc(out, func(a, b Port) int { return cmp.Compare(a.Port, b.Port) })
	return out
}

// nextRun returns when the next scheduled check is due (zero: none).
func (c *Checker) nextRunLocked(cfg Config) time.Time {
	if !cfg.Enabled {
		return time.Time{}
	}
	var due time.Time
	if !c.lastRun.IsZero() {
		due = c.lastRun.Add(time.Duration(float64(cfg.Interval) * c.jitter))
	} else {
		due = c.start.Add(firstDelay)
	}
	// Switched on and not checked since: a minute later.
	if !c.enabledAt.IsZero() {
		due = c.enabledAt.Add(enabledDelay)
	}
	return due
}

// Start runs the scheduled checks until ctx is done (blocks).
func (c *Checker) Start(ctx context.Context) {
	c.mu.Lock()
	c.runCtx = ctx
	c.mu.Unlock()
	defer c.rounds.Wait()
	for {
		c.mu.Lock()
		due := c.nextRunLocked(c.d.Config())
		pending := c.pending
		c.mu.Unlock()
		var fire <-chan time.Time
		var timer *time.Timer
		if !pending && !due.IsZero() {
			timer = time.NewTimer(max(0, due.Sub(c.now())))
			fire = timer.C
		}
		run := pending
		if !run {
			select {
			case <-ctx.Done():
			case <-fire:
				run = true
			case <-c.kick:
			}
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		run = run || c.pending
		if run && !c.running && c.d.Config().Enabled {
			c.running, c.pending = true, false
			c.startedAt = c.now()
			c.rounds.Add(1)
			c.mu.Unlock()
			c.round(ctx)
			c.rounds.Done()
			continue
		}
		c.pending = false
		c.mu.Unlock()
	}
}

// Run asks for a check now: apperr.Unavailable while the checks are off
// or cannot run here, apperr.Conflict while one runs, apperr.TooMany within
// a minute of the previous start.
func (c *Checker) Run(ctx context.Context) error {
	if !c.d.Config().Enabled {
		return apperr.Unavailable("port checks are off: switch them on first")
	}
	if _, err := c.d.Targets(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	switch {
	case c.runCtx == nil:
		return apperr.Unavailable("port checks are not available while PiCache starts or stops")
	case c.running || c.pending:
		return apperr.Conflict("a port check is already running")
	case !c.startedAt.IsZero() && now.Sub(c.startedAt) < runCooldown:
		wait := int((runCooldown - now.Sub(c.startedAt) + time.Second - 1) / time.Second)
		return apperr.TooMany("a port check started less than a minute ago; try again in %d s", wait)
	}
	c.pending = true
	c.signal()
	return nil
}

// deviceKey identifies a device: by MAC when known.
func deviceKey(t Target) string {
	if t.MAC != "" {
		return "mac:" + t.MAC
	}
	return "ip:" + t.IP.String()
}

// result is what one check saw of a device.
type result struct {
	target   Target
	answered bool
	open     map[int]bool
}

// round runs one check.
func (c *Checker) round(ctx context.Context) {
	cfg := c.d.Config()
	c.mu.Lock()
	c.enabledAt = time.Time{} // the check after switching on runs now
	c.mu.Unlock()
	errText := ""
	defer func() {
		c.mu.Lock()
		c.running = false
		c.lastRun = c.now()
		c.lastErr = errText
		c.mu.Unlock()
	}()
	targets, err := c.d.Targets(ctx)
	if err != nil {
		errText = err.Error()
		return
	}
	if len(targets) > MaxTargets {
		targets = targets[:MaxTargets]
	}
	ports := c.ports(cfg)
	results := c.probe(ctx, targets, ports, dialTimeout)
	if ctx.Err() != nil {
		return
	}
	if err := c.apply(ctx, results); err != nil && ctx.Err() == nil {
		errText = err.Error()
		c.d.Log.Warn("port check: storing the results failed", slog.Any("err", err))
	}
	if err := c.prune(ctx); err != nil && ctx.Err() == nil {
		c.d.Log.Warn("port check: pruning failed", slog.Any("err", err))
	}
}

// probe tries every port of every target.
func (c *Checker) probe(ctx context.Context, targets []Target, ports []Port, timeout time.Duration) []*result {
	results := make([]*result, len(targets))
	sems := make([]chan struct{}, len(targets))
	for i, t := range targets {
		results[i] = &result{target: t, open: map[int]bool{}}
		sems[i] = make(chan struct{}, perDevice)
	}
	type job struct{ ti, port int }
	jobs := make(chan job)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range min(parallel, len(targets)*len(ports)) {
		wg.Go(func() {
			for j := range jobs {
				sems[j.ti] <- struct{}{}
				state := noAnswer
				if c.pace(ctx) {
					addr := net.JoinHostPort(targets[j.ti].IP.String(), strconv.Itoa(j.port))
					state = c.dial(ctx, addr, timeout)
				}
				<-sems[j.ti]
				if state == noAnswer {
					continue
				}
				mu.Lock()
				results[j.ti].answered = true
				if state == open {
					results[j.ti].open[j.port] = true
				}
				mu.Unlock()
			}
		})
	}
	// Port by port, so the devices are visited in turn.
send:
	for _, p := range ports {
		for ti := range targets {
			select {
			case jobs <- job{ti, p.Port}:
			case <-ctx.Done():
				break send
			}
		}
	}
	close(jobs)
	wg.Wait()
	return results
}

// apply stores the results, records and reports changes.
func (c *Checker) apply(ctx context.Context, results []*result) error {
	now := c.now()
	type devChange struct {
		target         Target
		opened, closed []int
	}
	var changes []devChange
	for _, r := range results {
		key := deviceKey(r.target)
		ip := r.target.IP.String()
		var lastAnswer int64
		known := true
		switch err := c.d.DB.R.QueryRowContext(ctx, `SELECT last_answer FROM portcheck_devices WHERE device = ?`, key).Scan(&lastAnswer); {
		case errors.Is(err, sql.ErrNoRows):
			known = false
		case err != nil:
			return err
		}
		if !r.answered {
			if _, err := c.d.DB.W.ExecContext(ctx, `INSERT INTO portcheck_devices (device, mac, ip, first_check, last_check) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(device) DO UPDATE SET ip = excluded.ip, last_check = excluded.last_check`,
				key, r.target.MAC, ip, now.UnixMilli(), now.UnixMilli()); err != nil {
				return err
			}
			continue
		}
		old, err := c.openPorts(ctx, key)
		if err != nil {
			return err
		}
		var opened, gone []int
		for p := range r.open {
			if !old[p] {
				opened = append(opened, p)
			}
		}
		for p := range old {
			if !r.open[p] {
				gone = append(gone, p)
			}
		}
		// A port that looks closed is tried once more.
		if len(gone) > 0 {
			ports := make([]Port, len(gone))
			for i, p := range gone {
				ports[i] = Port{Port: p}
			}
			again := c.probe(ctx, []Target{r.target}, ports, recheckTimeout)[0]
			gone = slices.DeleteFunc(gone, func(p int) bool {
				if again.open[p] {
					r.open[p] = true
					return true
				}
				return false
			})
		}
		slices.Sort(opened)
		slices.Sort(gone)
		// A first answer only records the ports (no change, no message).
		first := !known || lastAnswer == 0
		if err := c.store(ctx, key, r, now, opened, gone, first); err != nil {
			return err
		}
		if !first && (len(opened) > 0 || len(gone) > 0) {
			changes = append(changes, devChange{r.target, opened, gone})
		}
	}
	if len(changes) == 0 || c.d.Emit == nil {
		return nil
	}
	sev := notify.SeverityInfo
	var lines []string
	for i, ch := range changes {
		if len(ch.opened) > 0 {
			sev = notify.SeverityWarning
		}
		if i == maxNotifyLines {
			lines = append(lines, fmt.Sprintf("… and %d more devices", len(changes)-i))
			break
		}
		line := c.deviceName(ch.target) + ":"
		if len(ch.opened) > 0 {
			line += " opened " + portList(ch.opened)
		}
		if len(ch.closed) > 0 {
			if len(ch.opened) > 0 {
				line += ";"
			}
			line += " closed " + portList(ch.closed)
		}
		lines = append(lines, line)
	}
	title := fmt.Sprintf("Open ports changed on %d devices", len(changes))
	if len(changes) == 1 {
		title = "Open ports changed on " + c.deviceName(changes[0].target)
	}
	c.d.Emit(notify.Message{Event: notify.EventDevicePorts, Severity: sev, Title: title, Message: strings.Join(lines, "\n"), Time: now})
	return nil
}

// deviceName is "<name> (<ip>)" or the address.
func (c *Checker) deviceName(t Target) string {
	if c.d.Name != nil {
		if n := c.d.Name(t.IP, t.MAC); n != "" {
			return n + " (" + t.IP.String() + ")"
		}
	}
	return t.IP.String()
}

// portList is "22 (SSH), 8080 (HTTP), 3000".
func portList(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
		if s := Service(p); s != "" {
			parts[i] += " (" + s + ")"
		}
	}
	return strings.Join(parts, ", ")
}

// openPorts returns the stored open ports of a device.
func (c *Checker) openPorts(ctx context.Context, key string) (map[int]bool, error) {
	rows, err := c.d.DB.R.QueryContext(ctx, `SELECT port FROM portcheck_open WHERE device = ?`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// store writes the result of a device that answered.
func (c *Checker) store(ctx context.Context, key string, r *result, now time.Time, opened, gone []int, first bool) error {
	tx, err := c.d.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ms, ip := now.UnixMilli(), r.target.IP.String()
	if _, err := tx.ExecContext(ctx, `INSERT INTO portcheck_devices (device, mac, ip, first_check, last_check, last_answer) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(device) DO UPDATE SET ip = excluded.ip, last_check = excluded.last_check, last_answer = excluded.last_answer`,
		key, r.target.MAC, ip, ms, ms, ms); err != nil {
		return err
	}
	for _, p := range gone {
		if _, err := tx.ExecContext(ctx, `DELETE FROM portcheck_open WHERE device = ? AND port = ?`, key, p); err != nil {
			return err
		}
	}
	add := opened
	if first {
		add = add[:0:0]
		for p := range r.open {
			add = append(add, p)
		}
	}
	for _, p := range add {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO portcheck_open (device, port, since) VALUES (?, ?, ?)`, key, p, ms); err != nil {
			return err
		}
	}
	if !first {
		for _, ch := range []struct {
			ports  []int
			opened bool
		}{{opened, true}, {gone, false}} {
			for _, p := range ch.ports {
				if _, err := tx.ExecContext(ctx, `INSERT INTO portcheck_changes (time, device, ip, port, opened) VALUES (?, ?, ?, ?, ?)`,
					ms, key, ip, p, ch.opened); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

// prune drops old changes and devices that did not answer for long.
func (c *Checker) prune(ctx context.Context) error {
	now := c.now()
	if _, err := c.d.DB.W.ExecContext(ctx, `DELETE FROM portcheck_changes WHERE time < ?`, now.Add(-changeRetention).UnixMilli()); err != nil {
		return err
	}
	if _, err := c.d.DB.W.ExecContext(ctx, `DELETE FROM portcheck_changes WHERE id IN (SELECT id FROM portcheck_changes
		ORDER BY id DESC LIMIT -1 OFFSET ?)`, maxChanges); err != nil {
		return err
	}
	cut := now.Add(-deviceRetention).UnixMilli()
	if _, err := c.d.DB.W.ExecContext(ctx, `DELETE FROM portcheck_open WHERE device IN
		(SELECT device FROM portcheck_devices WHERE max(last_answer, first_check) < ?)`, cut); err != nil {
		return err
	}
	_, err := c.d.DB.W.ExecContext(ctx, `DELETE FROM portcheck_devices WHERE max(last_answer, first_check) < ?`, cut)
	return err
}

// State returns the state and the stored devices (GET /network/ports).
func (c *Checker) State(ctx context.Context) (State, error) {
	cfg := c.d.Config()
	c.mu.Lock()
	st := State{Enabled: cfg.Enabled, Running: c.running || c.pending, StartedAt: c.startedAt, LastRun: c.lastRun,
		NextRun: c.nextRunLocked(cfg), LastError: c.lastErr, Ports: c.ports(cfg)}
	c.mu.Unlock()
	rows, err := c.d.DB.R.QueryContext(ctx, `SELECT device, mac, ip, first_check, last_check, last_answer FROM portcheck_devices
		ORDER BY last_check DESC LIMIT ?`, 4*MaxTargets)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	byKey := map[string]int{}
	st.Devices = []Device{}
	for rows.Next() {
		var d Device
		var first, last, answer int64
		if err := rows.Scan(&d.Device, &d.MAC, &d.IP, &first, &last, &answer); err != nil {
			return st, err
		}
		d.FirstCheck, d.LastCheck = time.UnixMilli(first), time.UnixMilli(last)
		if answer > 0 {
			d.LastAnswer = time.UnixMilli(answer)
		}
		d.Open = []OpenPort{}
		byKey[d.Device] = len(st.Devices)
		st.Devices = append(st.Devices, d)
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	rows2, err := c.d.DB.R.QueryContext(ctx, `SELECT device, port, since FROM portcheck_open ORDER BY device, port`)
	if err != nil {
		return st, err
	}
	defer rows2.Close()
	for rows2.Next() {
		var key string
		var p int
		var since int64
		if err := rows2.Scan(&key, &p, &since); err != nil {
			return st, err
		}
		if i, ok := byKey[key]; ok {
			st.Devices[i].Open = append(st.Devices[i].Open, OpenPort{Port: p, Service: Service(p), Since: time.UnixMilli(since)})
		}
	}
	return st, rows2.Err()
}

// Changes returns the recorded changes, newest first, optionally of one
// device (GET /network/ports/changes).
func (c *Checker) Changes(ctx context.Context, device string, limit, offset int) (listing.Page[Change], error) {
	if limit <= 0 || limit > maxChangePage {
		limit = 100
	}
	offset = max(offset, 0)
	where, args := "", []any{}
	if device != "" {
		where, args = " WHERE device = ?", append(args, device)
	}
	page := listing.Page[Change]{Items: []Change{}}
	if err := c.d.DB.R.QueryRowContext(ctx, `SELECT count(*) FROM portcheck_changes`+where, args...).Scan(&page.Total); err != nil {
		return page, err
	}
	rows, err := c.d.DB.R.QueryContext(ctx, `SELECT id, time, device, ip, port, opened FROM portcheck_changes`+where+
		` ORDER BY time DESC, id DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var ch Change
		var t int64
		if err := rows.Scan(&ch.ID, &t, &ch.Device, &ch.IP, &ch.Port, &ch.Opened); err != nil {
			return page, err
		}
		ch.Time, ch.Service = time.UnixMilli(t), Service(ch.Port)
		page.Items = append(page.Items, ch)
	}
	return page, rows.Err()
}

// Forget removes a device with its open ports and changes.
func (c *Checker) Forget(ctx context.Context, device string) error {
	tx, err := c.d.DB.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM portcheck_devices WHERE device = ?`, device)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.NotFound("device", device)
	}
	for _, q := range []string{`DELETE FROM portcheck_open WHERE device = ?`, `DELETE FROM portcheck_changes WHERE device = ?`} {
		if _, err := tx.ExecContext(ctx, q, device); err != nil {
			return err
		}
	}
	return tx.Commit()
}
