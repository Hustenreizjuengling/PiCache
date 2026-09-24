// Package app wires all components together and runs the process:
// bind listeners → drop privileges → prepare directories → open databases →
// start components → serve → graceful shutdown (docs/ARCHITECTURE.md 2, 6.2).
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/lancache/proxy"
	"github.com/hustenreizjuengling/picache/internal/lancache/services"
	"github.com/hustenreizjuengling/picache/internal/lancache/sni"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
	"github.com/hustenreizjuengling/picache/internal/version"
	"github.com/hustenreizjuengling/picache/internal/webui"
)

// ErrRestart is returned by Run when a restart was requested through the
// API; the command exits with ExitRestart so systemd/Docker restart it.
var ErrRestart = errors.New("restart requested")

// ExitRestart is the process exit code for a requested restart.
const ExitRestart = 75

// App is the running process state. It implements api.Runtime.
type App struct {
	cfg        *config.Config
	paths      config.Paths
	log        *slog.Logger
	started    time.Time
	instanceID string

	cdb      *db.DB
	ldb      *db.DB // nil if logs are disabled
	set      *settings.Store
	box      *secrets.Box
	acl      *netutil.ACLWatcher
	logs     *logs.Store
	up       *upstream.Resolver
	clients  *clients.Registry
	filter   *filter.Engine
	services *services.Registry
	auth     *auth.Service
	storage  *storage.Manager
	dns      *dnsserver.Server
	proxy    *proxy.Server
	sni      *sni.Server
	api      *api.Server

	ln listeners

	store          atomic.Pointer[cachestore.Store]
	storeMu        sync.Mutex // guards publishing and closing the active store and storeClosed
	storeClosed    bool       // closeState ran: no store is published any more
	reconcileMu    sync.Mutex // serialises reconcileStore (held while a store opens)
	storeOpening   atomic.Bool
	openCacheStore func(context.Context, cachestore.Options) (*cachestore.Store, error) // nil: cachestore.Open (tests)
	storeState     atomic.Pointer[api.StoreState]
	storeKick      chan struct{}
	storeFull      atomic.Bool
	lastStoreErr   string
	lastStoreErrAt time.Time

	evictKick chan struct{} // low free space: evict now
	evictSem  chan struct{} // serialises EvictNow
	free      freeSpace

	verifyMu sync.Mutex
	verify   api.VerifyState

	health     atomic.Pointer[api.Health]
	restoredAt time.Time // set when a staged restore was applied at this start
	restart    chan struct{}
}

// Run starts PiCache and blocks until ctx is cancelled, a restart is
// requested (ErrRestart) or a fatal error occurs.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	a := newApp(cfg, log)
	info := version.Get()
	log.Info("starting PiCache", slog.String("version", info.Version), slog.String("commit", info.Commit),
		slog.String("go", info.GoVersion), slog.String("data_dir", cfg.DataDir), slog.String("cache_dir", cfg.CacheDir))
	if cfg.AdminPasswordFromEnv {
		log.Warn("PICACHE_ADMIN_PASSWORD is set in the environment; prefer PICACHE_ADMIN_PASSWORD_FILE (environment variables are visible to other processes and `docker inspect`)")
	}

	// 1. Bind listeners while we may still be privileged. DNS and at least
	//    one web listener are mandatory; the others fail softly.
	if err := a.bindListeners(); err != nil {
		return err
	}
	defer a.ln.closeAll()

	// 2. Drop privileges (Docker: root → PICACHE_RUN_AS) before touching files.
	if err := a.dropPrivileges(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		log.Warn("running as root; use the provided systemd unit or set PICACHE_RUN_AS (Docker)")
	}
	if err := a.prepareDirs(); err != nil {
		return err
	}

	// 3. Open state (with restore rollback) and build components.
	restored, err := a.applyStagedRestore()
	if err != nil {
		return err
	}
	if err := a.build(ctx); err != nil {
		a.closeState()
		if restored {
			if rbErr := a.rollbackRestore(); rbErr == nil {
				log.Error("the restored backup could not be started; the previous configuration was put back", slog.Any("err", err))
				if err2 := a.build(ctx); err2 == nil {
					defer a.closeState()
					return a.serve(ctx)
				}
			}
		}
		return err
	}
	defer a.closeState()

	// 4. Start background loops and servers.
	return a.serve(ctx)
}

// newApp returns the process state before anything is opened.
func newApp(cfg *config.Config, log *slog.Logger) *App {
	return &App{cfg: cfg, paths: cfg.Paths(), log: log, started: time.Now(),
		storeKick: make(chan struct{}, 1), evictKick: make(chan struct{}, 1), evictSem: make(chan struct{}, 1),
		restart: make(chan struct{}, 1)}
}

func (a *App) prepareDirs() error {
	for _, d := range []string{a.cfg.DataDir, a.paths.CacheIndexDir, a.paths.ListsDir, a.paths.CacheDomainsDir,
		a.paths.TLSDir, filepath.Join(a.cfg.DataDir, "tmp"), filepath.Join(a.cfg.DataDir, "backups"),
		filepath.Join(a.cfg.DataDir, "storage-requests")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("create %s: %w (the data directory must be writable by the PiCache user)", d, err)
		}
	}
	if err := os.MkdirAll(a.paths.KeysDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", a.paths.KeysDir, err)
	}
	if err := os.MkdirAll(a.cfg.CacheDir, 0o750); err != nil {
		return fmt.Errorf("create cache dir %s: %w (it must be writable by the PiCache user)", a.cfg.CacheDir, err)
	}
	return nil
}

func (a *App) build(ctx context.Context) error {
	var err error
	log := a.log
	a.storeMu.Lock()
	a.storeClosed = false // Run builds again after a failed restore (closeState ran)
	a.storeMu.Unlock()
	if a.cdb, err = db.Open(a.paths.ConfigDB, 4); err != nil {
		return err
	}
	if err := a.removePlantedSchema(ctx); err != nil {
		return err
	}
	if err := a.preUpgradeBackup(ctx); err != nil {
		log.Warn("pre-upgrade backup failed", slog.Any("err", err))
	}
	if a.set, err = settings.Open(ctx, a.cdb, log); err != nil {
		return err
	}
	if a.set.Created() {
		a.applyDetectedDefaults(ctx)
	}
	if a.box, err = secrets.Open(a.paths.MasterKeyFile); err != nil {
		return err
	}
	if a.instanceID, err = loadInstanceID(filepath.Join(a.cfg.DataDir, "instance-id")); err != nil {
		return err
	}
	a.acl = netutil.NewACLWatcher(a.set)

	a.openLogs(ctx)

	if a.up, err = upstream.New(a.set, log); err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	lookup46 := func(ctx context.Context, host string) ([]netip.Addr, error) { return a.up.LookupIP(ctx, host, true) }
	lookup4 := func(ctx context.Context, host string) ([]netip.Addr, error) { return a.up.LookupIP(ctx, host, false) }
	fetch := newFetchClient(lookup46)

	if a.clients, err = clients.New(ctx, a.cdb, a.ldb, log); err != nil {
		return fmt.Errorf("clients: %w", err)
	}
	a.clients.SetPTRResolver(a.lookupClientName)
	if a.filter, err = filter.New(ctx, a.cdb, a.set, fetch, a.paths.ListsDir, log); err != nil {
		return fmt.Errorf("filter: %w", err)
	}
	// Group deletions/renumbering must reach the filter's source→groups table.
	a.clients.OnChange(func() {
		if err := a.filter.ReloadGroups(context.Background()); err != nil {
			log.Warn("reload filter groups", slog.Any("err", err))
		}
	})
	if a.services, err = services.New(ctx, a.cdb, a.set, fetch, a.paths.CacheDomainsDir, log); err != nil {
		return fmt.Errorf("services: %w", err)
	}
	if a.auth, err = auth.New(ctx, a.cdb, a.set, a.box, a.paths.SetupTokenFile, log); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if !a.restoredAt.IsZero() {
		// CarryOverAccounts already emptied the sessions; a failure here
		// rolls the restore back.
		if err := a.cdb.Tx(ctx, func(tx *sqlTx) error { return auth.PurgeSessions(ctx, tx) }); err != nil {
			return fmt.Errorf("restore: end sessions: %w", err)
		}
		log.Warn("configuration restored from backup: accounts, API tokens and the audit log were kept; everyone has to sign in again")
	}
	if pw := a.cfg.AdminPassword; pw != "" {
		err := a.auth.Provision(ctx, a.cfg.AdminUser, pw)
		a.cfg.AdminPassword = ""
		if err != nil {
			return fmt.Errorf("provision admin: %w", err)
		}
	}
	sliceSize := func() int64 { return a.set.Get().Cache.SliceSizeBytes }
	if a.storage, err = storage.New(ctx, a.cdb, a.box, a.cfg, sliceSize, log); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	a.storage.OnStatusChange(func(string, storage.Status) { a.kickStore() })
	a.set.Subscribe(func(o, n *settings.All) {
		if o.Cache.ActiveStoreID != n.Cache.ActiveStoreID || o.Cache.MinFreeBytes != n.Cache.MinFreeBytes {
			a.kickStore()
		}
		if o.Cache.MaxSizeBytes != n.Cache.MaxSizeBytes || o.Cache.MaxAgeDays != n.Cache.MaxAgeDays {
			a.kickEvict() // apply a lowered limit now, not within the next minute
		}
	})

	if a.dns, err = dnsserver.New(ctx, dnsserver.Deps{
		DB: a.cdb, Settings: a.set, Upstream: a.up, Filter: a.filter, Clients: a.clients,
		Services: a.services, Logs: a.logs, ACL: a.acl, LanCacheReady: a.lanCacheReady,
		Container: a.storage.Capabilities().Container, Log: log,
	}); err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	if a.proxy, err = proxy.New(ctx, proxy.Deps{
		DB: a.cdb, Settings: a.set, Services: a.services, Lookup: lookup4, Store: a.proxyStore,
		StoreFull: a.storeFull.Load, Clients: a.clients, Logs: a.logs, ACL: a.acl,
		InstanceID: a.instanceID, Log: log,
	}); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	a.sni = sni.New(sni.Deps{Settings: a.set, Services: a.services, Lookup: lookup4, Clients: a.clients,
		Logs: a.logs, ACL: a.acl, Log: log})
	a.api = api.New(api.Deps{
		Config: a.cfg, Settings: a.set, Auth: a.auth, DNS: a.dns, Upstream: a.up, Filter: a.filter,
		Clients: a.clients, Services: a.services, Proxy: a.proxy, SNI: a.sni, Storage: a.storage,
		Logs: a.logs, Runtime: a, UI: webui.Handler(), Log: log,
	})
	a.storeState.Store(&api.StoreState{TargetID: a.set.Get().Cache.ActiveStoreID, Reason: "starting"})
	return nil
}

// openLogs opens logs.db. A broken database is moved aside and recreated;
// if that fails too, logging is disabled (DNS must never depend on logs).
func (a *App) openLogs(ctx context.Context) {
	open := func() (*db.DB, *logs.Store, error) {
		d, err := db.Open(a.paths.LogsDB, 4)
		if err != nil {
			return nil, nil, err
		}
		st, err := logs.New(ctx, d, a.set, a.log)
		if err != nil {
			d.Close()
			return nil, nil, err
		}
		return d, st, nil
	}
	d, st, err := open()
	if err != nil {
		a.log.Error("logs.db cannot be opened; moving it aside and starting a fresh one", slog.Any("err", err))
		ts := time.Now().UTC().Format("20060102T150405")
		for _, sfx := range []string{"", "-wal", "-shm"} {
			_ = os.Rename(a.paths.LogsDB+sfx, a.paths.LogsDB+sfx+".broken-"+ts)
		}
		d, st, err = open()
	}
	if err != nil {
		a.log.Error("logging disabled: logs.db unusable", slog.Any("err", err))
		a.logs = logs.Discard(err.Error(), a.log)
		return
	}
	a.ldb, a.logs = d, st
}

// proxyStore returns the active store as the proxy interface (nil stays nil).
func (a *App) proxyStore() proxy.SliceStore {
	if s := a.store.Load(); s != nil {
		return s
	}
	return nil
}

// lanCacheReady gates DNS overrides: the cache listener must be bound.
// (dnsserver additionally checks that a valid cache IP is known.)
func (a *App) lanCacheReady() (bool, string) {
	if len(a.ln.cache) == 0 {
		if msg, ok := a.ln.failed["cache"]; ok {
			return false, "cache HTTP listener not bound: " + msg
		}
		return false, "cache HTTP listener is disabled (PICACHE_CACHE_LISTEN)"
	}
	return true, ""
}

func (a *App) lookupClientName(ctx context.Context, ip netip.Addr) (string, error) {
	servers := a.set.Get().DNS.LocalPTRUpstreams
	if len(servers) == 0 {
		if r := a.dns.Router(); r.Address != "" && r.Answers {
			servers = []string{r.Address}
		}
	}
	if len(servers) == 0 {
		return "", nil
	}
	return a.up.LookupPTR(ctx, ip, servers)
}

// applyDetectedDefaults adapts first-start defaults to the environment
// (local domain from /etc/resolv.conf).
func (a *App) applyDetectedDefaults(ctx context.Context) {
	search := netutil.ResolvConfSearch()
	if len(search) == 0 {
		return
	}
	d := search[0]
	if _, err := a.set.Update(ctx, func(s *settings.All) error { s.DNS.LocalDomain = d; return nil }); err == nil {
		a.log.Info("detected local domain", slog.String("domain", d))
	}
}

func (a *App) serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 16)
	var srv, bg sync.WaitGroup
	goRun := func(name string, fn func() error) {
		srv.Go(func() {
			if err := fn(); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		})
	}
	for _, fn := range []func(context.Context){
		a.logs.Start, a.up.Start, a.clients.Start, a.filter.Start, a.services.Start, a.auth.Start,
		a.storage.Start, a.proxy.Start, a.storeLoop, a.evictLoop, a.healthLoop,
		func(ctx context.Context) { a.acl.Run(ctx.Done(), 5*time.Minute) },
	} {
		bg.Go(func() { fn(ctx) })
	}

	aclFn := a.acl.Get
	var dnsTCP []netListener
	for _, ln := range a.ln.dnsTCP {
		dnsTCP = append(dnsTCP, netutil.LimitListener(ln, aclFn, 32, 1024))
	}
	goRun("dns", func() error { return a.dns.Serve(ctx, a.ln.dnsUDP, dnsTCP) })
	for _, ln := range a.ln.sni {
		limited := netutil.LimitListener(ln, aclFn, 256, 4096)
		goRun("sni", func() error { return a.sni.Serve(ctx, limited) })
	}

	var servers []*http.Server
	cacheSrv := &http.Server{
		Handler:           a.proxy.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
	}
	servers = append(servers, cacheSrv)
	for _, ln := range a.ln.cache {
		limited := netutil.LimitListener(ln, aclFn, 256, 4096)
		goRun("cache", func() error { return cacheSrv.Serve(limited) })
	}

	newWeb := func() *http.Server {
		return &http.Server{
			Handler:           a.api.Handler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      120 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    32 << 10,
			ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
		}
	}
	webSrv := newWeb()
	servers = append(servers, webSrv)
	for _, ln := range a.ln.web {
		goRun("web", func() error { return webSrv.Serve(ln) })
	}
	if len(a.ln.webTLS) > 0 {
		tlsCfg, err := a.webTLSConfig()
		if err != nil {
			a.log.Error("HTTPS web listener disabled", slog.Any("err", err))
		} else {
			webTLS := newWeb()
			webTLS.TLSConfig = tlsCfg
			servers = append(servers, webTLS)
			for _, ln := range a.ln.webTLS {
				goRun("web-tls", func() error { return webTLS.ServeTLS(ln, "", "") })
			}
		}
	}

	a.log.Info("PiCache is running", slog.Any("listeners", a.Listeners()), slog.String("instance", a.instanceID))
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		a.log.Error("fatal error, shutting down", slog.Any("err", runErr))
	case <-a.restart:
		a.log.Warn("restart requested via the API")
		runErr = ErrRestart
	}
	cancel()
	a.shutdown(servers, &srv, &bg)
	a.log.Info("stopped")
	return runErr
}

// Shutdown budgets. HTTP servers get httpShutdownGrace to finish their
// requests and are then closed. The background components, cancelled when
// the shutdown starts, get their own componentStopWait after that, so a long
// download on the cache port cannot use up the time they need to flush
// before the databases close. Together with closing the cache store (at most
// 10 s for running operations) this stays below Docker's stop_grace_period
// of 30 s.
var (
	httpShutdownGrace = 12 * time.Second
	serverStopWait    = time.Second
	componentStopWait = 5 * time.Second
)

// shutdown stops the HTTP servers, closes the listeners and waits for the
// server goroutines (srv) and the background components (bg), each within
// its own budget.
func (a *App) shutdown(servers []*http.Server, srv, bg *sync.WaitGroup) {
	shutCtx, shutCancel := context.WithTimeout(context.Background(), httpShutdownGrace)
	defer shutCancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() {
			if err := s.Shutdown(shutCtx); err != nil {
				_ = s.Close() // grace period over: drop the remaining connections
			}
		})
	}
	wg.Wait()
	a.ln.closeAll()
	if !waitFor(srv, serverStopWait) {
		a.log.Warn("some servers did not stop in time")
	}
	if !waitFor(bg, componentStopWait) {
		a.log.Warn("some components did not stop in time; closing the databases anyway")
	}
}

// waitFor waits for wg at most d; false on timeout.
func waitFor(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

// closeState closes the active store and the databases. It never waits for
// a store that is being opened (reconcileStore holds only reconcileMu while
// it opens); such a store is closed as soon as its open returns.
func (a *App) closeState() {
	a.storeMu.Lock()
	a.storeClosed = true
	st := a.store.Swap(nil)
	a.storeMu.Unlock()
	if st != nil {
		_ = st.Close()
	}
	if a.logs != nil {
		_ = a.logs.Close()
		a.logs = nil
	}
	if a.ldb != nil {
		_ = a.ldb.Close()
		a.ldb = nil
	}
	if a.up != nil {
		_ = a.up.Close()
		a.up = nil
	}
	if a.cdb != nil {
		_ = a.cdb.Close()
		a.cdb = nil
	}
}

// newFetchClient is the HTTP client for list and cache-domains downloads:
// resolution via the bypass resolver, SSRF guard (private destinations only
// with netutil.WithAllowPrivate), no redirects (callers follow them
// manually), no environment proxy.
func newFetchClient(lookup netutil.Resolver) *http.Client {
	d := &netutil.SafeDialer{Resolve: lookup, Timeout: 15 * time.Second}
	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       60 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func loadInstanceID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	buf := make([]byte, 6)
	rand.Read(buf)
	id := "picache-" + hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(id+"\n"), 0o640); err != nil {
		return "", fmt.Errorf("write instance id: %w", err)
	}
	return id, nil
}

// --- api.Runtime ---

func (a *App) StartedAt() time.Time           { return a.started }
func (a *App) InstanceID() string             { return a.instanceID }
func (a *App) Config() *config.Config         { return a.cfg }
func (a *App) ActiveStore() *cachestore.Store { return a.store.Load() }
func (a *App) MasterKeySource() string        { return a.box.Source }

// Restart asks Run to exit with ErrRestart (after the current response).
func (a *App) Restart() {
	go func() {
		time.Sleep(500 * time.Millisecond)
		select {
		case a.restart <- struct{}{}:
		default:
		}
	}()
}

func (a *App) Listeners() api.ListenerInfo {
	return a.ln.info()
}
