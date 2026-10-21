// Package prefill is the prefill service (picache prefill,
// docs/ARCHITECTURE.md 20): it signs in to game stores without a password
// (Steam: a QR code scanned with the Steam app), lists the account's games
// and downloads the selected ones through the download cache, so that the
// first download in the LAN is already served from the cache.
//
// It runs as its own process under its own user, separate from PiCache:
// the account tokens stay in its state directory and never cross its
// socket. PiCache's web UI talks to it over a Unix socket that admits only
// PiCache's user and root.
package prefill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// Config configures the service (cmd/picache prefill: PICACHE_PREFILL_*).
type Config struct {
	// StateDir holds the state files (0700).
	StateDir string
	// Socket is the Unix socket of the API.
	Socket string
	// CacheURL is the download cache: http://host[:port].
	CacheURL string
	// ClientUIDs may use the socket besides root and the service's own
	// user (PiCache's user).
	ClientUIDs []int
	// DeviceName is how a sign-in appears in the store's device list.
	DeviceName string
	Version    string
	Log        *slog.Logger

	// Tests replace these.
	now   func() time.Time
	steam steamConnector
	epic  epicConnector
	bnet  bnetConnector
}

// ConfigFromEnv reads the PICACHE_PREFILL_* variables.
func ConfigFromEnv(version string, log *slog.Logger) (Config, error) {
	c := Config{
		StateDir: os.Getenv("PICACHE_PREFILL_STATE_DIR"),
		Socket:   os.Getenv("PICACHE_PREFILL_SOCKET"),
		CacheURL: os.Getenv("PICACHE_PREFILL_CACHE"),
		Version:  version,
		Log:      log,
	}
	if c.StateDir == "" {
		// systemd's StateDirectory= (DynamicUser: /var/lib/private/…).
		if d := os.Getenv("STATE_DIRECTORY"); d != "" {
			c.StateDir = filepath.SplitList(d)[0]
		} else {
			c.StateDir = "/var/lib/picache-prefill"
		}
	}
	if c.Socket == "" {
		c.Socket = prefillapi.DefaultSocket
	}
	if c.CacheURL == "" {
		c.CacheURL = "http://127.0.0.1:80"
	}
	if v := os.Getenv("PICACHE_PREFILL_CLIENT_UID"); v != "" {
		for _, s := range splitComma(v) {
			uid, err := strconv.Atoi(s)
			if err != nil || uid < 0 {
				return c, fmt.Errorf("PICACHE_PREFILL_CLIENT_UID: %q is not a user ID", s)
			}
			c.ClientUIDs = append(c.ClientUIDs, uid)
		}
	} else {
		name := os.Getenv("PICACHE_PREFILL_CLIENT_USER")
		if name == "" {
			name = "picache"
		}
		if u, err := user.Lookup(name); err == nil {
			if uid, err := strconv.Atoi(u.Uid); err == nil {
				c.ClientUIDs = append(c.ClientUIDs, uid)
			}
		} else if log != nil {
			log.Warn("PiCache's user was not found: only root can use the prefill socket", "user", name)
		}
	}
	host, _ := os.Hostname()
	c.DeviceName = "PiCache Prefill"
	if host != "" {
		c.DeviceName += " (" + host + ")"
	}
	return c, nil
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if v := s[start:i]; v != "" {
				out = append(out, v)
			}
			start = i + 1
		}
	}
	return out
}

// Daemon is the running service.
type Daemon struct {
	cfg   Config
	log   *slog.Logger
	st    *store
	cache *cacheClient
	steam *steamService
	bnet  *bnetService
	epic  *epicService
	now   func() time.Time

	started time.Time
	wake    chan struct{}
	restart context.CancelFunc

	mu         sync.Mutex
	settings   prefillapi.Settings
	hist       history
	cacheSt    prefillapi.Cache
	hosts      map[string]*prefillapi.Host
	run        *jobRun
	nextID     int64
	stopping   bool
	restarting bool

	preMu sync.Mutex // a leaf lock: taken last
	pre   prefilledRec
}

// ErrRestart: the binary was replaced by an update; the caller exits with
// ExitRestart and systemd starts the new binary.
var ErrRestart = errors.New("the picache binary was updated: restart")

// ExitRestart is the exit code for ErrRestart (systemd restarts the unit
// on it: Restart=on-failure).
const ExitRestart = 75

// Run starts the service and blocks until ctx ends. It also ends with
// ErrRestart, once no job and no sign-in runs, when the binary was
// replaced by an update.
func Run(ctx context.Context, cfg Config) error {
	d, err := newDaemon(cfg)
	if err != nil {
		return err
	}
	ctx, d.restart = context.WithCancel(ctx)
	defer d.restart()
	defer d.st.close()
	ln, err := listenSocket(cfg.Socket)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           d.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
		ConnContext:       d.connContext,
		ErrorLog:          slog.NewLogLogger(d.log.Handler(), slog.LevelDebug),
	}
	d.log.Info("prefill service started", "socket", cfg.Socket, "cache", d.cache.base.String(), "version", cfg.Version)
	var wg sync.WaitGroup
	wg.Go(func() { d.scheduler(ctx) })
	wg.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("prefill socket stopped", "err", err)
		}
	})
	<-ctx.Done()
	d.mu.Lock()
	d.stopping = true
	r := d.run
	d.mu.Unlock()
	if r != nil {
		r.cancel(errShutdown)
		<-r.done
	}
	d.steam.shutdown()
	d.epic.shutdown()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	wg.Wait()
	_ = os.Remove(cfg.Socket)
	d.cache.close()
	d.mu.Lock()
	restart := d.restarting
	d.mu.Unlock()
	if restart {
		d.log.Info("prefill service stopped to start the updated binary")
		return ErrRestart
	}
	d.log.Info("prefill service stopped")
	return nil
}

func newDaemon(cfg Config) (*Daemon, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	log := cfg.Log.With("component", "prefill")
	base, err := parseCacheURL(cfg.CacheURL)
	if err != nil {
		return nil, fmt.Errorf("PICACHE_PREFILL_CACHE: %w", err)
	}
	st, err := openStore(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("state directory: %w", err)
	}
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	d := &Daemon{
		cfg: cfg, log: log, st: st, now: now,
		started: now(),
		wake:    make(chan struct{}, 1),
		hosts:   map[string]*prefillapi.Host{},
		cacheSt: prefillapi.Cache{URL: base.String()},
	}
	d.settings = defaultSettings()
	if err := st.load(fileSettings, &d.settings); err != nil {
		log.Warn("settings.json is unreadable: using the defaults", "err", err)
		d.settings = defaultSettings()
	}
	if s, err := validateSettings(d.settings); err != nil {
		log.Warn("settings.json is invalid: using the defaults", "err", err)
		d.settings = defaultSettings()
	} else {
		d.settings = s
	}
	if err := st.load(fileHistory, &d.hist); err != nil {
		log.Warn("history.json is unreadable: starting a new history", "err", err)
		d.hist = history{}
	}
	for _, j := range d.hist.Jobs {
		d.nextID = max(d.nextID, j.ID)
	}
	// A job that was running when the service stopped did not finish.
	for i := range d.hist.Jobs {
		if d.hist.Jobs[i].State == prefillapi.JobRunning {
			d.hist.Jobs[i].State = prefillapi.JobStopped
			d.hist.Jobs[i].Error = "the prefill service stopped"
		}
	}
	d.cache = newCacheClient(base, maxConnections)
	conn := cfg.steam
	if conn == nil {
		conn = liveSteam{}
	}
	d.pre = prefilledRec{}
	if err := st.load(filePrefilled, &d.pre); err != nil || d.pre == nil {
		d.pre = prefilledRec{}
	}
	d.steam = newSteamService(d, conn)
	bconn := cfg.bnet
	if bconn == nil {
		bconn = liveBattleNet{}
	}
	d.bnet = newBattleNetService(d, bconn)
	econn := cfg.epic
	if econn == nil {
		econn = liveEpic{}
	}
	d.epic = newEpicService(d, econn)
	return d, nil
}

// prefilled returns a copy of prefilled.json.
func (d *Daemon) prefilled() prefilledRec {
	d.preMu.Lock()
	defer d.preMu.Unlock()
	return maps.Clone(d.pre)
}

// markPrefilled records a complete download of key ("steam:<app>",
// "battlenet:<product>", "epic:<app>").
func (d *Daemon) markPrefilled(key string, p prefilledApp) {
	d.preMu.Lock()
	d.pre[key] = p
	cp := maps.Clone(d.pre)
	d.preMu.Unlock()
	if err := d.st.save(filePrefilled, cp); err != nil {
		d.log.Warn("prefilled.json could not be written", "err", err)
	}
}

// listenSocket creates the Unix socket. An old socket file left by a
// crash is removed; any other file in its place is refused. The mode is
// 0666: who may talk is decided by the peer's user ID (connContext).
func listenSocket(path string) (net.Listener, error) {
	// sockaddr_un holds 108 bytes with the terminating NUL.
	if len(path) > 107 {
		return nil, fmt.Errorf("the socket path %s is longer than 107 bytes", path)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o666); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

type ctxKeyPeer struct{}

// connContext records the peer's user ID; the handler refuses every other
// user than root, the service's own and ClientUIDs.
func (d *Daemon) connContext(ctx context.Context, c net.Conn) context.Context {
	uid, err := peerUID(c)
	if err != nil {
		uid = -1
	}
	return context.WithValue(ctx, ctxKeyPeer{}, uid)
}

func (d *Daemon) peerAllowed(ctx context.Context) bool {
	uid, ok := ctx.Value(ctxKeyPeer{}).(int)
	if !ok || uid < 0 {
		return false
	}
	return uid == 0 || uid == os.Getuid() || slices.Contains(d.cfg.ClientUIDs, uid)
}

// usedHost records that the service talked to host for purpose.
func (d *Daemon) usedHost(host, protocol, purpose string) {
	t := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	key := protocol + "://" + host
	h := d.hosts[key]
	if h == nil {
		if len(d.hosts) >= 256 {
			return
		}
		h = &prefillapi.Host{Host: host, Protocol: protocol, Purpose: purpose}
		d.hosts[key] = h
	}
	h.LastUsed = &t
	if d.run != nil && !slices.Contains(d.run.job.Hosts, key) && len(d.run.job.Hosts) < 64 {
		d.run.job.Hosts = append(d.run.job.Hosts, key)
	}
}

// checkCache runs the heartbeat and records the result.
func (d *Daemon) checkCache(ctx context.Context) error {
	id, err := d.cache.heartbeat(ctx)
	t := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cacheSt.CheckedAt = &t
	d.cacheSt.OK = err == nil
	d.cacheSt.InstanceID = id
	d.cacheSt.Error = ""
	if err != nil {
		d.cacheSt.Error = err.Error()
	}
	return err
}

func (d *Daemon) status() prefillapi.Status {
	d.mu.Lock()
	st := prefillapi.Status{
		Version:  d.cfg.Version,
		Started:  d.started,
		Cache:    d.cacheSt,
		Settings: cloneSettings(d.settings),
		Hosts:    []prefillapi.Host{},
	}
	if d.run != nil {
		j := cloneJob(d.run.job)
		st.Job = &j
	}
	if start, _, ok := window(d.settings.Schedule, d.now()); ok {
		if start.Before(d.now()) {
			start = d.now()
		}
		st.NextRun = &start
	}
	for _, h := range d.hosts {
		st.Hosts = append(st.Hosts, *h)
	}
	d.mu.Unlock()
	slices.SortFunc(st.Hosts, func(a, b prefillapi.Host) int {
		if a.Purpose != b.Purpose {
			if a.Purpose < b.Purpose {
				return -1
			}
			return 1
		}
		if a.Host < b.Host {
			return -1
		}
		if a.Host > b.Host {
			return 1
		}
		return 0
	})
	st.Steam = d.steam.status()
	st.Epic = d.epic.status()
	return st
}

func cloneSettings(s prefillapi.Settings) prefillapi.Settings {
	s.Schedule.Days = slices.Clone(s.Schedule.Days)
	s.Steam.OS = slices.Clone(s.Steam.OS)
	s.Steam.Languages = slices.Clone(s.Steam.Languages)
	s.Steam.Apps = slices.Clone(s.Steam.Apps)
	s.BattleNet.Products = slices.Clone(s.BattleNet.Products)
	s.Epic.Apps = slices.Clone(s.Epic.Apps)
	return s
}

func (d *Daemon) currentSettings() prefillapi.Settings {
	d.mu.Lock()
	defer d.mu.Unlock()
	return cloneSettings(d.settings)
}

// setSettings validates, stores and applies settings.
func (d *Daemon) setSettings(s prefillapi.Settings) (prefillapi.Settings, error) {
	s, err := validateSettings(s)
	if err != nil {
		return s, invalid(err.Error())
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.st.save(fileSettings, s); err != nil {
		return s, err
	}
	d.settings = s
	select {
	case d.wake <- struct{}{}:
	default:
	}
	return cloneSettings(s), nil
}
