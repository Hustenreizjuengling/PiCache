// Package app wires all components together and runs the process:
// bind listeners → drop privileges → open databases → start components →
// serve → graceful shutdown (docs/ARCHITECTURE.md 2, 6.2).
package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
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

// App is the running process state. It implements api.Runtime.
type App struct {
	cfg        *config.Config
	paths      config.Paths
	log        *slog.Logger
	started    time.Time
	instanceID string

	cdb      *db.DB
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

	store      atomic.Pointer[cachestore.Store]
	storeMu    sync.Mutex // serialises store open/close
	storeState atomic.Pointer[api.StoreState]
	storeKick  chan struct{}

	verifyMu sync.Mutex
	verify   api.VerifyState
}

// Run starts PiCache and blocks until ctx is cancelled or a fatal error occurs.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	a := &App{cfg: cfg, paths: cfg.Paths(), log: log, started: time.Now(), storeKick: make(chan struct{}, 1)}
	info := version.Get()
	log.Info("starting PiCache", slog.String("version", info.Version), slog.String("commit", info.Commit),
		slog.String("go", info.GoVersion), slog.String("data_dir", cfg.DataDir), slog.String("cache_dir", cfg.CacheDir))

	// 1. Bind every listener while we may still be privileged.
	if err := a.bindListeners(); err != nil {
		return err
	}
	defer a.ln.closeAll()

	// 2. Prepare directories, then drop privileges (Docker: root → 65532).
	if err := a.prepareDirs(); err != nil {
		return err
	}
	if err := a.dropPrivileges(); err != nil {
		return err
	}
	if os.Geteuid() == 0 {
		log.Warn("running as root; set PICACHE_RUN_AS or run under the provided systemd unit (only needed for in-process NAS mounts)")
	}

	// 3. Open state and build components.
	if err := a.applyStagedRestore(); err != nil {
		return err
	}
	if err := a.build(ctx); err != nil {
		a.closeState()
		return err
	}
	defer a.closeState()

	// 4. Start background loops and servers.
	return a.serve(ctx)
}

func (a *App) prepareDirs() error {
	for _, d := range []string{a.cfg.DataDir, a.paths.CacheIndexDir, a.paths.ListsDir, a.paths.CacheDomainsDir, a.paths.TLSDir, filepath.Join(a.cfg.DataDir, "tmp")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}
	if err := os.MkdirAll(a.paths.KeysDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", a.paths.KeysDir, err)
	}
	if err := os.MkdirAll(a.cfg.CacheDir, 0o750); err != nil {
		return fmt.Errorf("create cache dir %s: %w", a.cfg.CacheDir, err)
	}
	return nil
}

func (a *App) build(ctx context.Context) error {
	var err error
	log := a.log
	if a.cdb, err = db.Open(a.paths.ConfigDB, 4); err != nil {
		return err
	}
	if a.set, err = settings.Open(ctx, a.cdb, log); err != nil {
		return err
	}
	if a.box, err = secrets.Open(a.paths.MasterKeyFile); err != nil {
		return err
	}
	if a.instanceID, err = loadInstanceID(filepath.Join(a.cfg.DataDir, "instance-id")); err != nil {
		return err
	}
	a.acl = netutil.NewACLWatcher(a.set)

	if a.logs, err = logs.Open(ctx, a.paths.LogsDB, a.set, log); err != nil {
		return fmt.Errorf("logs: %w", err)
	}
	if a.up, err = upstream.New(a.set, log); err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	lookup46 := func(ctx context.Context, host string) ([]netip.Addr, error) { return a.up.LookupIP(ctx, host, true) }
	lookup4 := func(ctx context.Context, host string) ([]netip.Addr, error) { return a.up.LookupIP(ctx, host, false) }
	fetch := newFetchClient(lookup46)

	if a.clients, err = clients.New(ctx, a.cdb, log); err != nil {
		return fmt.Errorf("clients: %w", err)
	}
	a.clients.SetPTRResolver(func(ctx context.Context, ip netip.Addr) (string, error) {
		servers := a.set.Get().DNS.LocalPTRUpstreams
		if len(servers) == 0 {
			return "", nil
		}
		return a.up.LookupPTR(ctx, ip, servers)
	})
	if a.filter, err = filter.New(ctx, a.cdb, a.set, fetch, a.paths.ListsDir, log); err != nil {
		return fmt.Errorf("filter: %w", err)
	}
	a.clients.OnChange(a.filter.Recompile)
	if a.services, err = services.New(ctx, a.cdb, a.set, fetch, a.paths.CacheDomainsDir, log); err != nil {
		return fmt.Errorf("services: %w", err)
	}
	if a.auth, err = auth.New(ctx, a.cdb, a.set, a.paths.SetupTokenFile, log); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if a.cfg.AdminPassword != "" {
		if err := a.auth.Provision(ctx, a.cfg.AdminUser, a.cfg.AdminPassword); err != nil {
			return fmt.Errorf("provision admin: %w", err)
		}
	}
	if a.storage, err = storage.New(ctx, a.cdb, a.box, a.cfg, lookup46, log); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	a.storage.OnStatusChange(func(string, storage.Status) { a.kickStore() })
	a.set.Subscribe(func(o, n *settings.All) {
		if o.Cache.ActiveStoreID != n.Cache.ActiveStoreID {
			a.kickStore()
		}
	})

	if a.dns, err = dnsserver.New(ctx, dnsserver.Deps{
		DB: a.cdb, Settings: a.set, Upstream: a.up, Filter: a.filter, Clients: a.clients,
		Services: a.services, Logs: a.logs, ACL: a.acl, Log: log,
	}); err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	if a.proxy, err = proxy.New(ctx, proxy.Deps{
		DB: a.cdb, Settings: a.set, Services: a.services, Lookup: lookup4, Store: a.ActiveStore,
		Clients: a.clients, Logs: a.logs, ACL: a.acl, InstanceID: a.instanceID, Log: log,
	}); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	a.sni = sni.New(sni.Deps{Settings: a.set, Services: a.services, Lookup: lookup4, Logs: a.logs, ACL: a.acl, Log: log})
	a.api = api.New(api.Deps{
		Config: a.cfg, Settings: a.set, Auth: a.auth, DNS: a.dns, Upstream: a.up, Filter: a.filter,
		Clients: a.clients, Services: a.services, Proxy: a.proxy, SNI: a.sni, Storage: a.storage,
		Logs: a.logs, Runtime: a, UI: webui.Handler(), Log: log,
	})
	a.storeState.Store(&api.StoreState{TargetID: a.set.Get().Cache.ActiveStoreID, Reason: "starting"})
	return nil
}

func (a *App) serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 16)
	var wg sync.WaitGroup
	goRun := func(name string, fn func() error) {
		wg.Go(func() {
			if err := fn(); err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		})
	}

	go a.logs.Start(ctx)
	go a.up.Start(ctx)
	go a.clients.Start(ctx)
	go a.filter.Start(ctx)
	go a.services.Start(ctx)
	go a.auth.Start(ctx)
	go a.storage.Start(ctx)
	go a.acl.Run(ctx.Done(), 5*time.Minute)
	go a.storeLoop(ctx)
	go a.evictLoop(ctx)

	goRun("dns", func() error { return a.dns.Serve(ctx, a.ln.dnsUDP, a.ln.dnsTCP) })
	for _, ln := range a.ln.sni {
		goRun("sni", func() error { return a.sni.Serve(ctx, ln) })
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
		goRun("cache", func() error { return cacheSrv.Serve(ln) })
	}

	webSrv := &http.Server{
		Handler:           a.api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
	}
	servers = append(servers, webSrv)
	for _, ln := range a.ln.web {
		goRun("web", func() error { return webSrv.Serve(ln) })
	}
	if len(a.ln.webTLS) > 0 {
		tlsCfg, err := a.webTLSConfig()
		if err != nil {
			return err
		}
		webTLS := &http.Server{
			Handler:           a.api.Handler(),
			TLSConfig:         tlsCfg,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      120 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    32 << 10,
			ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
		}
		servers = append(servers, webTLS)
		for _, ln := range a.ln.webTLS {
			goRun("web-tls", func() error { return webTLS.ServeTLS(ln, "", "") })
		}
	}

	a.log.Info("PiCache is running", slog.Any("listeners", a.Listeners()), slog.String("instance", a.instanceID))
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
		a.log.Error("fatal error, shutting down", slog.Any("err", runErr))
	}
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	for _, s := range servers {
		_ = s.Shutdown(shutCtx)
	}
	a.ln.closeAll()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-shutCtx.Done():
		a.log.Warn("shutdown timed out")
	}
	a.log.Info("stopped")
	return runErr
}

func (a *App) closeState() {
	a.storeMu.Lock()
	if st := a.store.Swap(nil); st != nil {
		_ = st.Close()
	}
	a.storeMu.Unlock()
	if a.logs != nil {
		_ = a.logs.Close()
	}
	if a.up != nil {
		_ = a.up.Close()
	}
	if a.cdb != nil {
		_ = a.cdb.Close()
	}
}

// newFetchClient is the HTTP client for list and cache-domains downloads:
// resolution via the bypass resolver, private destinations allowed (users may
// host lists locally), no environment proxy.
func newFetchClient(lookup netutil.Resolver) *http.Client {
	d := &netutil.SafeDialer{Resolve: lookup, AllowPrivate: func() bool { return true }, Timeout: 15 * time.Second,
		OwnAddrs: func() []netip.Addr { return nil }}
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
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && via[0].URL.Scheme == "https" {
				return errors.New("refusing redirect from https to http")
			}
			return nil
		},
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

func (a *App) StartedAt() time.Time        { return a.started }
func (a *App) InstanceID() string          { return a.instanceID }
func (a *App) Config() *config.Config      { return a.cfg }
func (a *App) ActiveStore() *cachestore.Store { return a.store.Load() }

func (a *App) Listeners() map[string][]string {
	addrs := func(ls []net.Listener) []string {
		out := make([]string, 0, len(ls))
		for _, l := range ls {
			out = append(out, l.Addr().String())
		}
		return out
	}
	udp := make([]string, 0, len(a.ln.dnsUDP))
	for _, pc := range a.ln.dnsUDP {
		udp = append(udp, pc.LocalAddr().String())
	}
	return map[string][]string{
		"dns-udp": udp, "dns-tcp": addrs(a.ln.dnsTCP), "cache": addrs(a.ln.cache),
		"sni": addrs(a.ln.sni), "web": addrs(a.ln.web), "web-tls": addrs(a.ln.webTLS),
	}
}

func (a *App) Health(ctx context.Context) api.Health {
	h := api.Health{OK: true, Checks: map[string]string{}}
	fail := func(k, v string) { h.OK = false; h.Checks[k] = v }
	h.Checks["dns"] = "ok"
	healthy := false
	for _, s := range a.up.Stats() {
		if s.Healthy {
			healthy = true
		}
	}
	if healthy || len(a.up.Stats()) == 0 {
		h.Checks["upstreams"] = "ok"
	} else {
		fail("upstreams", "no healthy upstream")
	}
	if st := a.services.Status(); st.Ready {
		h.Checks["cache-domains"] = "ok"
	} else if a.set.Get().LanCache.Enabled {
		fail("cache-domains", "no cache-domains snapshot loaded: "+st.Error)
	}
	if ss := a.StoreState(); ss.Online {
		h.Checks["cache-store"] = "ok"
	} else {
		fail("cache-store", "offline: "+ss.Reason)
	}
	if m := a.logs.Metrics(); m.Dropped > 0 {
		h.Checks["logs"] = fmt.Sprintf("ok (%d events dropped)", m.Dropped)
	} else {
		h.Checks["logs"] = "ok"
	}
	return h
}
