// Package app wires all components together and runs the process:
// bind listeners → drop privileges → prepare directories → open databases →
// start components → serve → graceful shutdown (docs/ARCHITECTURE.md 2, 6.2).
package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
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
	"github.com/hustenreizjuengling/picache/internal/applog"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/dlcache/proxy"
	"github.com/hustenreizjuengling/picache/internal/dlcache/services"
	"github.com/hustenreizjuengling/picache/internal/dlcache/sni"
	cachestore "github.com/hustenreizjuengling/picache/internal/dlcache/store"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/hostinfo"
	"github.com/hustenreizjuengling/picache/internal/logs"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/ntp"
	"github.com/hustenreizjuengling/picache/internal/secrets"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
	"github.com/hustenreizjuengling/picache/internal/update"
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
	parental *parental.Engine
	services *services.Registry
	auth     *auth.Service
	storage  *storage.Manager
	dns      *dnsserver.Server
	proxy    *proxy.Server
	sni      *sni.Server
	updates  *updater
	notify   *notify.Service // nil in tests that build parts of the App (Emit is nil-safe)
	dhcp     *dhcp.Service   // nil in tests that build parts of the App
	backups  *backupScheduler
	network  *netChecker
	sync     *syncer     // the follower sync
	ntp      *ntp.Server // the NTP server (answers on PICACHE_NTP_LISTEN while ntp.enabled)
	api      *api.Server

	outbound outboundProxy // the tunnel of network.proxy (proxyFor)
	// routesTruncWarned: a route table beyond 4096 routes was logged once.
	routesTruncWarned bool

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
	healthKick chan struct{} // evaluate the health checks now (kickHealth)
	restoredAt time.Time     // set when a staged full restore was applied at this start
	// restoreSections are the sections a staged restore applied at this
	// start (nil: none); restoreBy is "cli" for `picache restore` (the
	// audit row system.restore is written after the start).
	restoreSections []string
	restoreBy       string
	restart         chan struct{}

	appLog      *applog.Log                   // the application log of the logger (nil: another handler)
	logDrops    dropTracker                   // recent drops of the log sinks (health check "logging")
	hostSampler *hostinfo.Sampler             // host resources (built with the storage manager)
	host        atomic.Pointer[hostinfo.Info] // the last sample

	web    *netutil.WebAccess // the web ACL (listeners and API)
	webTLS *webTLS            // the certificate of the TLS listeners (web UI over HTTPS, DoT, DoH)
	// encState is the state of encrypted DNS the DNS server reads
	// (refreshEncrypted); encFailOpen: plain DNS is off but nothing
	// encrypted serves (logged once per change). encMu serialises the
	// refreshes (tick, certificate swap, settings change), so the last one
	// always publishes the latest state; it is taken last (under the
	// settings store's and webTLS's locks) and takes no other lock.
	encMu       sync.Mutex
	encState    atomic.Pointer[dnsserver.EncryptedState]
	encFailOpen atomic.Bool
	hup         <-chan os.Signal // SIGHUP: run the maintenance tick now (nil: never)
	// The web access reset marker: resetApplied once a marker that could
	// not be deleted was applied (once per start); resetMarkerWarned once a
	// marker that is no regular file was logged.
	resetApplied, resetMarkerWarned bool
}

// Run starts PiCache and blocks until ctx is cancelled, a restart is
// requested (ErrRestart) or a fatal error occurs. A value on hup (SIGHUP,
// subscribed by the caller before any listener is bound) runs the
// maintenance tick at once: the web access reset marker, the web ACL and
// the web certificate files are checked; it never stops PiCache.
func Run(ctx context.Context, cfg *config.Config, log *slog.Logger, hup <-chan os.Signal) error {
	a := newApp(cfg, log)
	a.hup = hup
	if h, ok := log.Handler().(*applog.Handler); ok {
		a.appLog = h.Log()
	}
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

	// 2. Drop privileges (Docker: root → PICACHE_RUN_AS) before touching
	//    files, and CAP_NET_RAW (systemd grants it for the DHCP raw socket):
	//    PiCache refuses to run while it holds it.
	if err := a.dropPrivileges(); err != nil {
		return err
	}
	if err := a.dropRawCapability(); err != nil {
		return err
	}
	// The log file and syslog sinks start writing now: a log file is never
	// created as root.
	if a.appLog != nil {
		a.prepareLogDir()
		a.appLog.StartSinks()
	}
	if os.Geteuid() == 0 {
		log.Warn("running as root; use the provided systemd unit or set PICACHE_RUN_AS (Docker)")
	}
	if err := a.prepareDirs(); err != nil {
		return err
	}
	// The saved listeners of this start: listeners.json follows what was
	// bound, listeners.failed.json keeps a set that fell back.
	a.finishListenerFiles()

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

// deployment reports how PiCache runs, for the DHCP page: docker (a
// Docker or Podman container, as for the update mode), systemd (a systemd
// service) or other.
func (a *App) deployment() string {
	if a.storage == nil {
		return dhcp.DeploymentOther
	}
	caps := a.storage.Capabilities()
	switch {
	case caps.Container == "docker" || caps.Container == "podman":
		return dhcp.DeploymentDocker
	case runsAsSystemdService(caps.Systemd):
		return dhcp.DeploymentSystemd
	}
	return dhcp.DeploymentOther
}

// newApp returns the process state before anything is opened.
func newApp(cfg *config.Config, log *slog.Logger) *App {
	return &App{cfg: cfg, paths: cfg.Paths(), log: log, started: time.Now(),
		storeKick: make(chan struct{}, 1), evictKick: make(chan struct{}, 1), evictSem: make(chan struct{}, 1),
		healthKick: make(chan struct{}, 1), restart: make(chan struct{}, 1)}
}

// dropNetRawFn is dropNetRaw (tests replace it).
var dropNetRawFn = dropNetRaw

// dropRawCapability drops CAP_NET_RAW after the DHCP sockets are open, on
// every start (dropNetRaw). Fail closed: when the capability could not be
// dropped PiCache refuses to run (Run returns the error). When the thread
// capabilities cannot be read at all but a raw socket is refused, the raw
// socket is closed (no router advertisements; the health check dhcp
// fails) and DNS keeps running.
func (a *App) dropRawCapability() error {
	unverified, err := dropNetRawFn()
	switch {
	case err != nil:
		return fmt.Errorf("CAP_NET_RAW could not be dropped: %w; refusing to run with it", err)
	case unverified != nil:
		a.ln.dhcp.SetDropUnverified("could not verify that CAP_NET_RAW was dropped: " + unverified.Error())
		a.log.Error("router advertisements disabled: could not verify that CAP_NET_RAW was dropped", slog.Any("err", unverified))
	case a.ln.dhcp.HasRaw():
		a.log.Info("CAP_NET_RAW is not held after opening the ICMPv6 socket for router advertisements")
	}
	return nil
}

func (a *App) prepareDirs() error {
	for _, d := range []string{a.cfg.DataDir, a.paths.CacheIndexDir, a.paths.ListsDir, a.paths.CacheDomainsDir,
		a.paths.TLSDir, filepath.Join(a.cfg.DataDir, "tmp"), filepath.Join(a.cfg.DataDir, "backups"),
		filepath.Join(a.cfg.DataDir, "storage-requests"), filepath.Join(a.cfg.DataDir, update.RequestsDirName)} {
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

// prepareLogDir creates the directory of a PICACHE_LOG_FILE below
// <data dir>/logs (0750). The unit's LogsDirectory creates
// /var/log/picache, and a file directly in the data directory needs none.
// A failure is left to the file sink, which reports it (health check
// logging).
func (a *App) prepareLogDir() {
	logs := filepath.Join(filepath.Clean(a.cfg.DataDir), "logs")
	if a.cfg.LogFile == "" || !strings.HasPrefix(filepath.Clean(a.cfg.LogFile), logs+string(filepath.Separator)) {
		return
	}
	_ = os.MkdirAll(filepath.Dir(filepath.Clean(a.cfg.LogFile)), 0o750)
}

func (a *App) build(ctx context.Context) (err error) {
	// A damaged or unwritable picache.db names its cause and the fix.
	defer func() { err = explainConfigDBError(a.paths.ConfigDB, err) }()
	log := a.log
	a.storeMu.Lock()
	a.storeClosed = false // Run builds again after a failed restore (closeState ran)
	a.storeMu.Unlock()
	if err := checkExistingInstallation(a.paths.ConfigDB, a.cfg.DataDir); err != nil {
		return err
	}
	if a.cdb, err = db.Open(a.paths.ConfigDB, 4); err != nil {
		return err
	}
	if err := a.removePlantedSchema(ctx); err != nil {
		return err
	}
	// Its own errors name the copy; a failed migration of the app schema
	// is no backup error.
	if err := a.preUpgradeBackup(ctx); err != nil {
		return err
	}
	if a.set, err = settings.Open(ctx, a.cdb, log); err != nil {
		return err
	}
	if a.appLog != nil {
		// The application log masks addresses and hides domains in the web
		// UI like the query log (stderr is not covered).
		a.appLog.SetPrivacy(func() bool { return a.set.Get().Logs.AnonymizeClientIPs },
			func() bool { return a.set.Get().Logs.HideDomains })
	}
	if a.set.Created() {
		a.applyDetectedDefaults(ctx)
	}
	if a.box, err = secrets.Open(a.paths.MasterKeyFile); err != nil {
		return err
	}
	if a.box.Created {
		a.checkReplacedKey(ctx)
	}
	// The sync token and the proxy password are sealed with the master
	// key (settings_secrets).
	a.set.SetSealer(a.box.Seal, a.box.Open)
	a.outbound.a = a
	// PICACHE_INITIAL_CONFIG: once, on the first start, before any
	// listener serves and before the admin password is provisioned.
	if err := a.applyInitialConfig(ctx); err != nil {
		return err
	}
	if a.instanceID, err = loadInstanceID(filepath.Join(a.cfg.DataDir, "instance-id")); err != nil {
		return err
	}
	a.acl = netutil.NewACLWatcher(a.set)
	a.web = netutil.NewWebAccess(a.set, log)
	a.webTLS = newWebTLS(a.cfg.DataDir, a.cfg.WebTLSCertFile, a.cfg.WebTLSKeyFile, a.ln.tlsBound(), a.cfg.WebHosts,
		a.instanceID, a.set, func() bool { return a.dns != nil && a.dns.BridgeNetwork() }, log)
	a.webTLS.onSwap = a.refreshEncrypted // the first snapshot is built when the listeners serve
	// The storage (container detection) exists before the listeners serve.
	a.webTLS.container = func() bool { return a.deployment() == dhcp.DeploymentDocker }
	a.set.Subscribe(func(_, _ *settings.All) { a.refreshEncrypted() })
	host, _ := os.Hostname()
	if a.notify, err = notify.New(ctx, a.cdb, a.box,
		notify.Options{InstanceID: a.instanceID, Hostname: host, Version: version.Version, Proxy: a.proxyFor(proxyNotifications)},
		log); err != nil {
		return fmt.Errorf("notify: %w", err)
	}

	a.openLogs(ctx)
	// After a clock jump the retention waits, so a clock set far ahead
	// cannot empty the logs; a synchronised host clock (the NTP server's
	// reader) ends the wait.
	a.logs.SetClockReader(func() (bool, bool) {
		st := ntp.ReadClock()
		return st.Synced, st.Err == nil
	})

	if a.up, err = upstream.New(a.set, log); err != nil {
		return fmt.Errorf("upstream: %w", err)
	}
	// DNSSEC time checks follow the host clock's synchronisation (the NTP
	// server's reader; an unreadable state suspends nothing).
	a.up.SetClockReader(func() (bool, bool) {
		st := ntp.ReadClock()
		return st.Synced, st.Err == nil
	})
	lookup46 := func(ctx context.Context, host string) ([]netip.Addr, error) { return a.up.LookupIP(ctx, host, true) }
	lookup4 := func(ctx context.Context, host string) ([]netip.Addr, error) { return a.up.LookupIP(ctx, host, false) }
	fetch := newFetchClient(lookup46, a.proxyFor(proxyLists))

	if a.clients, err = clients.New(ctx, a.cdb, a.ldb, log); err != nil {
		return fmt.Errorf("clients: %w", err)
	}
	a.clients.SetPTRResolver(a.lookupClientName)
	// The name sources, the seen retention and the WHOIS inputs follow the
	// settings; RDAP lookups go out directly (never through the proxy).
	a.clients.ApplyConfig(clients.ConfigFrom(a.set.Get()))
	a.set.Subscribe(func(_, n *settings.All) { a.clients.ApplyConfig(clients.ConfigFrom(n)) })
	a.clients.SetWhoisClient(newWhoisClient(lookup46), "PiCache/"+version.Version)
	a.refreshRoutes()
	a.clients.SetFlushInterval(func() time.Duration { return time.Duration(a.set.Get().Logs.FlushSeconds) * time.Second })
	if a.filter, err = filter.New(ctx, a.cdb, a.set, fetch, a.paths.ListsDir, log); err != nil {
		return fmt.Errorf("filter: %w", err)
	}
	// The entry budget follows the memory PiCache may use (ARCHITECTURE 7.2).
	a.filter.SetEntryBudget(filter.BudgetFor(a.cfg.MemoryLimit))
	// Group deletions/renumbering must reach the filter's source→groups table.
	a.clients.OnChange(func() {
		if err := a.filter.ReloadGroups(context.Background()); err != nil {
			log.Warn("reload filter groups", slog.Any("err", err))
		}
	})
	if a.parental, err = parental.New(ctx, a.cdb, a.clients, log); err != nil {
		return fmt.Errorf("parental: %w", err)
	}
	// Group renames and deletions reach the parental controls' snapshot
	// (the reasons in the query log name the group).
	a.clients.OnChange(func() {
		if err := a.parental.Reload(context.Background()); err != nil {
			log.Warn("reload parental controls", slog.Any("err", err))
		}
	})
	if a.services, err = services.New(ctx, a.cdb, a.set, fetch, a.paths.CacheDomainsDir, log); err != nil {
		return fmt.Errorf("services: %w", err)
	}
	// The DHCP tables exist on every installation (backups, restores); the
	// server is switched on in the web UI (not with PICACHE_DHCP=off).
	if a.dhcp, err = dhcp.New(ctx, dhcp.Deps{
		DB: a.cdb, Settings: a.set, Sockets: a.ln.dhcp, DataDir: a.cfg.DataDir,
		Bridge:     func() bool { return a.dns != nil && a.dns.BridgeNetwork() },
		Deployment: a.deployment,
		Neighbour:  a.clients.NeighbourMAC, OnNames: a.clients.LeaseNamesChanged, Log: log,
	}); err != nil {
		return fmt.Errorf("dhcp: %w", err)
	}
	a.clients.SetLeaseNames(a.dhcp.LeaseName)
	if a.auth, err = auth.New(ctx, a.cdb, a.set, a.box, a.paths.SetupTokenFile, log); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	a.auth.OnLockout(func(l auth.Lockout) { a.emit(lockoutMessage(l)) })
	if !a.restoredAt.IsZero() {
		// CarryOverAccounts already emptied the sessions; a failure here
		// rolls the restore back.
		if err := a.cdb.Tx(ctx, func(tx *sqlTx) error { return auth.PurgeSessions(ctx, tx) }); err != nil {
			return fmt.Errorf("restore: end sessions: %w", err)
		}
		log.Warn("configuration restored from backup: accounts, API tokens and the audit log were kept; everyone has to sign in again")
	}
	if a.restoreSections != nil {
		// The selection of the staged file is no longer needed; a restore
		// staged by `picache restore` is audited now (username cli).
		if _, err := a.cdb.W.ExecContext(ctx, `DELETE FROM app_meta WHERE key IN (?, ?)`, metaRestoreSections, metaRestoreBy); err != nil {
			log.Warn("restore: could not remove the selection from app_meta", slog.Any("err", err))
		}
		if a.restoreBy == "cli" {
			a.auth.Audit(ctx, &auth.Principal{Username: "cli"}, "", "system.restore", "", map[string]any{"sections": a.restoreSections})
		}
	}
	if err := a.checkAdminPasswordFile(ctx); err != nil {
		return err
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
	// Release checks (docs/ARCHITECTURE.md 14.3) use the same outbound client as
	// list downloads: DNS through the upstreams, public destinations only.
	caps := a.storage.Capabilities()
	service := runsAsSystemdService(caps.Systemd)
	a.hostSampler = a.newHostSampler()
	a.sampleHost()
	// Package mode (the Debian package's marker; never in a container):
	// the check offers only releases with the .deb of this architecture.
	packaged := caps.Container != "docker" && caps.Container != "podman" && update.PackageInstalled()
	releases := &update.Client{HTTP: newFetchClient(lookup46, a.proxyFor(proxyUpdateCheck)), Package: packaged}
	// A changed outbound proxy applies to the next connection: idle
	// connections made with the previous setting are closed (list
	// downloads, the release check, notifications).
	a.set.Subscribe(func(o, n *settings.All) {
		if proxyChanged(o, n) {
			fetch.CloseIdleConnections()
			releases.HTTP.CloseIdleConnections()
			a.notify.CloseIdleConnections()
		}
	})
	a.updates = newUpdater(a.cfg.DataDir, version.Version, a.cdb, a.set, func() string {
		_, err := os.Stat(update.HelperMarker)
		return updateMode(caps.Container, packaged, service, err == nil)
	}, releases.Latest, log)
	a.updates.emit = a.emit
	a.updates.load(ctx)
	a.backups = newBackupScheduler(a.cfg.DataDir, a.instanceID, a.cdb, a.set, a.Backup, a.backupTarget, a.emit, log)
	a.backups.load(ctx)
	a.storage.OnStatusChange(func(string, storage.Status) { a.kickStore() })
	a.set.Subscribe(func(o, n *settings.All) {
		if o.Cache.ActiveStoreID != n.Cache.ActiveStoreID || o.Cache.MinFreeBytes != n.Cache.MinFreeBytes {
			a.kickStore()
		}
		if o.Cache.MaxSizeBytes != n.Cache.MaxSizeBytes || o.Cache.MaxAgeDays != n.Cache.MaxAgeDays {
			a.kickEvict() // apply a lowered limit now, not within the next minute
		}
		if o.Updates != n.Updates {
			a.updates.kickCheck() // e.g. pre-releases were allowed: look for them now
		}
	})

	if a.dns, err = dnsserver.New(ctx, dnsserver.Deps{
		DB: a.cdb, Settings: a.set, Upstream: a.up, Filter: a.filter, Clients: a.clients,
		Services: a.services, Parental: a.parental, Logs: a.logs, ACL: a.acl, DownloadCacheReady: a.downloadCacheReady,
		Container: a.storage.Capabilities().Container, Neighbours: a.clients.Neighbours, Leases: a.dhcp,
		Encrypted: a.encrypted, ValidatingForwarders: a.up.SetValidatingForwarders, Log: log,
	}); err != nil {
		return fmt.Errorf("dns: %w", err)
	}
	// Group changes reach the local records (a deleted group's record
	// links cascade: its records then serve nobody) and the upstream sets
	// of the groups.
	a.syncGroupUpstreams(ctx)
	a.clients.OnChange(func() {
		bg := context.Background()
		if err := a.dns.ReloadRecords(bg); err != nil {
			log.Warn("reload local records", slog.Any("err", err))
		}
		a.syncGroupUpstreams(bg)
	})
	if a.proxy, err = proxy.New(ctx, proxy.Deps{
		DB: a.cdb, Settings: a.set, Services: a.services, Lookup: lookup4, Store: a.proxyStore,
		StoreFull: a.storeFull.Load, Clients: a.clients, Logs: a.logs, ACL: a.acl,
		InstanceID: a.instanceID, Log: log,
	}); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	a.sni = sni.New(sni.Deps{Settings: a.set, Services: a.services, Lookup: lookup4, Clients: a.clients,
		Logs: a.logs, ACL: a.acl, Log: log})
	a.network = newNetChecker(a.netSources(), log)
	a.ntp = ntp.New(ntp.Deps{
		Settings: func() (bool, int, int, int) {
			s := a.set.Get()
			return s.NTP.Enabled, s.NTP.Stratum, s.DNS.RateLimitIPv4Prefix, s.DNS.RateLimitIPv6Prefix
		},
		Allowed: func(ip netip.Addr) bool { return a.acl.Get().Allowed(ip) },
		Refused: a.dns.CountRefused, Clock: ntp.ReadClock, Log: log.With(slog.String("component", "ntp")),
	})
	a.sync = newSyncer(a)
	a.sync.load(ctx)
	if !a.restoredAt.IsZero() || a.restoreSections != nil {
		a.sync.forget(ctx) // the next sync applies the primary's configuration again
	}
	a.set.Subscribe(a.sync.settingsChanged)
	a.api = api.New(api.Deps{
		Config: a.cfg, Settings: a.set, Auth: a.auth, DNS: a.dns, Upstream: a.up, Filter: a.filter,
		Clients: a.clients, Services: a.services, Proxy: a.proxy, SNI: a.sni, Storage: a.storage,
		Logs: a.logs, Runtime: a, Updates: a.updates, Notify: a.notify, Backups: a.backups,
		Parental: a.parental, Network: a.network, DHCP: a.dhcp, TLS: a.webTLS, WebAccess: a.web, AppLog: a.appLog, Diag: a,
		Encrypted: a, Listeners: a, Sync: a.sync,
		UI: webui.Handler(), Log: log,
	})
	a.storeState.Store(&api.StoreState{TargetID: a.set.Get().Cache.ActiveStoreID, Reason: "starting"})
	// Every component migrated: record the schema with this version, so
	// the next upgrade names its copy after the version that can open it.
	if err := recordSchema(ctx, a.cdb); err != nil {
		log.Warn("could not record the schema versions of the configuration database", slog.Any("err", err))
	}
	return nil
}

// checkAdminPasswordFile handles a PICACHE_ADMIN_PASSWORD_FILE that names a
// file that does not exist (config.Config.AdminPasswordFileMissing): while
// no account exists the admin cannot be provisioned, so the start fails;
// afterwards the file is not needed and a line left in picache.env (or a
// Docker secret removed without the variable) is only a warning.
func (a *App) checkAdminPasswordFile(ctx context.Context) error {
	f := a.cfg.AdminPasswordFileMissing
	if f == "" {
		return nil
	}
	required, err := a.auth.SetupRequired(ctx)
	if err != nil {
		return err
	}
	if required {
		return fmt.Errorf("PICACHE_ADMIN_PASSWORD_FILE: %s does not exist and no account exists yet: create the file, "+
			"or remove the variable and create the admin with the setup token", f)
	}
	a.log.Warn("PICACHE_ADMIN_PASSWORD_FILE names a file that does not exist; an account exists, so it is not needed: remove the variable",
		slog.String("file", f))
	return nil
}

// syncGroupUpstreams hands the upstreams of the enabled client groups to
// the resolver, which builds one set per distinct list (ARCHITECTURE 7.4).
func (a *App) syncGroupUpstreams(ctx context.Context) {
	cfg, err := a.clients.GroupUpstreamConfigs(ctx)
	if err != nil {
		a.log.Warn("read the upstreams of the groups", slog.Any("err", err))
		return
	}
	groups := make([]upstream.GroupUpstreams, 0, len(cfg))
	for _, c := range cfg {
		groups = append(groups, upstream.GroupUpstreams{ID: c.ID, Name: c.Name, Preset: c.Preset, Upstreams: c.Upstreams})
	}
	a.up.SetGroupUpstreams(groups)
}

// openLogs opens logs.db. A broken database is moved aside and recreated
// (only the newest broken copy is kept: each can be as large as
// logs.maxDbSizeMiB); if that fails too, logging is disabled (DNS must
// never depend on logs).
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
		st.OnDamage(a.kickHealth) // the check "logs" warns at once
		return d, st, nil
	}
	moveAside := func() {
		ts := time.Now().UTC().Format("20060102T150405")
		for _, sfx := range []string{"", "-wal", "-shm"} {
			older, _ := filepath.Glob(a.paths.LogsDB + sfx + ".broken-*")
			for _, f := range older {
				_ = os.Remove(f)
			}
			_ = os.Rename(a.paths.LogsDB+sfx, a.paths.LogsDB+sfx+".broken-"+ts)
		}
	}
	// A file PiCache may not write (owned by root after a copy without
	// chown, a read-only file system) is intact: keep it and say why, as
	// for picache.db, instead of moving it aside as broken (or opening it
	// read-only, so that every write fails). Only a file that cannot be
	// opened for other reasons (damaged, newer) is moved aside.
	if hint := writableHint(a.paths.LogsDB); hint != "" {
		a.log.Error("logging disabled: logs.db is kept as it is: " + hint)
		a.logs = logs.Discard("logs.db cannot be written: "+hint, a.log)
		return
	}
	// Damage found while PiCache ran: check the file now, move it aside
	// when the check fails.
	marker := logs.DamagedMarker(a.paths.LogsDB)
	if _, err := os.Stat(marker); err == nil {
		if res := quickCheck(ctx, a.paths.LogsDB); res != "ok" {
			a.log.Error("logs.db is damaged; moving it aside and starting a fresh one", slog.String("check", res))
			moveAside()
		} else {
			a.log.Info("logs.db was reported damaged, but its check passes; keeping it")
		}
		_ = os.Remove(marker)
	}
	d, st, err := open()
	if err != nil {
		a.log.Error("logs.db cannot be opened; moving it aside and starting a fresh one", slog.Any("err", err))
		moveAside()
		d, st, err = open()
	}
	if err != nil {
		a.log.Error("logging disabled: logs.db unusable", slog.Any("err", err))
		a.logs = logs.Discard(err.Error(), a.log)
		return
	}
	a.ldb, a.logs = d, st
}

// logsCheckTimeout bounds the check of a logs.db that was reported damaged.
var logsCheckTimeout = 5 * time.Minute

// quickCheck returns the first line of PRAGMA quick_check of the database
// at path ("ok" when it passes, else the problem or the error), read-only.
func quickCheck(ctx context.Context, path string) string {
	d, err := db.OpenReadOnly(path)
	if err != nil {
		return err.Error()
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(ctx, logsCheckTimeout)
	defer cancel()
	var res string
	if err := d.R.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&res); err != nil {
		return err.Error()
	}
	return res
}

// proxyStore returns the active store as the proxy interface (nil stays nil).
func (a *App) proxyStore() proxy.SliceStore {
	if s := a.store.Load(); s != nil {
		return s
	}
	return nil
}

// downloadCacheReady gates DNS overrides: the cache listener must be bound.
// (dnsserver additionally checks that a valid cache IP is known.)
func (a *App) downloadCacheReady() (bool, string) {
	if len(a.ln.cache) == 0 {
		if msg, ok := a.ln.failed["cache"]; ok {
			return false, "cache HTTP listener not bound: " + msg
		}
		return false, "cache HTTP listener is disabled (PICACHE_CACHE_LISTEN)"
	}
	return true, ""
}

// lookupClientName resolves the PTR name of a client address via the local
// PTR upstreams, else the router resolver while it answers.
func (a *App) lookupClientName(ctx context.Context, ip netip.Addr) (string, error) {
	servers := a.set.Get().DNS.LocalPTRUpstreams
	if len(servers) == 0 {
		if r := a.dns.Router(); r.Address != "" && r.Answers {
			// "[fe80::1%eth0]:53": a bare IPv6 address is no upstream string.
			if rip, err := netip.ParseAddr(r.Address); err == nil {
				servers = []string{netip.AddrPortFrom(rip, 53).String()}
			}
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
	// A fresh installation shows the getting-started checklist (Defaults
	// gives true for documents of earlier versions). PICACHE_INITIAL_CONFIG
	// runs later, so a document that sets the member wins.
	if _, err := a.set.Update(ctx, func(s *settings.All) error { s.Web.OnboardingDone = false; return nil }); err != nil {
		a.log.Warn("could not enable the getting-started checklist", slog.Any("err", err))
	}
	search := netutil.ResolvConfSearch()
	if len(search) == 0 {
		return
	}
	d := search[0]
	if _, err := a.set.Update(ctx, func(s *settings.All) error { s.DNS.LocalDomain = d; return nil }); err == nil {
		a.log.Info("detected local domain", slog.String("domain", d))
	}
}

func (a *App) serve(parent context.Context) error {
	return a.serveWith(parent, a.startServers)
}

// startFunc starts servers and background components in ctx, the serve
// context: goRun runs a server (its error, unless the context is done,
// shuts PiCache down), bg tracks the components. It returns the HTTP
// servers shutdown drains: the web and DoH servers and the download
// cache's server (nil: none).
type startFunc func(ctx context.Context, goRun func(name string, fn func() error), bg *sync.WaitGroup) (servers []*http.Server, cache *http.Server)

// serveWith runs start and blocks until parent is cancelled (SIGTERM,
// SIGINT), a server fails or a restart is requested, then shuts down. The
// servers and the background components run in their own context, which
// shutdown cancels only after the HTTP servers stopped: a cancelled parent
// starts the shutdown, it does not take DNS down at once.
func (a *App) serveWith(parent context.Context, start startFunc) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
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
	servers, cacheSrv := start(ctx, goRun, &bg)
	var runErr error
	select {
	case <-parent.Done():
	case runErr = <-errc:
		a.log.Error("fatal error, shutting down", slog.Any("err", runErr))
	case <-a.restart:
		a.log.Warn("restart requested via the API")
		runErr = ErrRestart
	}
	a.shutdown(servers, cacheSrv, cancel, &srv, &bg)
	a.log.Info("stopped")
	return runErr
}

// startServers starts the servers and the background components of
// PiCache (startFunc).
func (a *App) startServers(ctx context.Context, goRun func(string, func() error), bg *sync.WaitGroup) ([]*http.Server, *http.Server) {
	// The web certificate is loaded (or created) and a pending web access
	// reset applied before the web listeners serve.
	a.webTLS.start()
	a.applyWebAccessReset(ctx)
	for _, fn := range []func(context.Context){
		a.logs.Start, a.up.Start, a.clients.Start, a.filter.Start, a.services.Start, a.auth.Start,
		a.storage.Start, a.proxy.Start, a.storeLoop, a.evictLoop, a.healthLoop, a.updates.run,
		a.notify.Start, a.backups.Start, a.network.Start, a.dhcp.Start, a.sync.Start,
		func(ctx context.Context) { a.acl.Run(ctx.Done(), time.Minute) }, // follows prefix changes within a minute
		a.maintenanceLoop, // web ACL, web access reset, web certificate: every minute and on SIGHUP
	} {
		bg.Go(func() { fn(ctx) })
	}
	// Nothing answers DNS (plain, DoT, DoH) before the cached blocklists
	// are compiled, so they never fail open after a restart; the sockets
	// are bound, so queries wait in them.
	a.waitFilterReady(ctx)

	aclFn := a.acl.Get
	var dnsTCP []netListener
	for _, ln := range a.ln.dnsTCP {
		dnsTCP = append(dnsTCP, netutil.LimitDNSListener(ln, aclFn, 32, 1024))
	}
	// DoT and the dedicated DoH listeners (docs/ARCHITECTURE.md 19).
	a.refreshEncrypted()
	dot, dohServers := a.serveEncrypted(goRun)
	goRun("dns", func() error { return a.dns.Serve(ctx, a.ln.dnsUDP, dnsTCP, dot) })
	if len(a.ln.ntp) > 0 {
		goRun("ntp", func() error { return a.ntp.Serve(ctx, a.ln.ntp) })
	}
	for _, ln := range a.ln.sni {
		limited := netutil.LimitListener(ln, aclFn, 256, 4096)
		goRun("sni", func() error { return a.sni.Serve(ctx, limited) })
	}

	var servers []*http.Server // web and DoH, drained with httpShutdownGrace
	cacheSrv := &http.Server{
		Handler:           a.proxy.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
	}
	for _, ln := range a.ln.cache {
		limited := netutil.LimitListener(ln, aclFn, 256, 4096)
		goRun("cache", func() error { return cacheSrv.Serve(limited) })
	}

	servers = append(servers, dohServers...)
	servers = append(servers, a.serveWeb(goRun, a.api.Handler())...)
	a.log.Info("PiCache is running", slog.Any("listeners", a.Listeners()), slog.String("instance", a.instanceID))
	return servers, cacheSrv
}

// filterReadyWait bounds how long the listeners wait for the first
// compile of the cached blocklists (a very large set on a slow host).
var filterReadyWait = 30 * time.Second

// waitFilterReady waits until the filter compiled the cached copies of the
// enabled lists, at most filterReadyWait.
func (a *App) waitFilterReady(ctx context.Context) {
	start := time.Now()
	t := time.NewTimer(filterReadyWait)
	defer t.Stop()
	select {
	case <-a.filter.Ready():
		a.log.Debug("blocklists ready before the DNS listeners serve", slog.Duration("waited", time.Since(start).Round(time.Millisecond)))
	case <-t.C:
		a.log.Warn("the blocklists were not compiled in time; DNS answers without them until they are",
			slog.Duration("waited", filterReadyWait))
	case <-ctx.Done():
	}
}

// serveWeb starts the web servers with handler on the HTTP and HTTPS web
// listeners and returns them.
func (a *App) serveWeb(goRun func(string, func() error), handler http.Handler) []*http.Server {
	var servers []*http.Server
	newWeb := func() *http.Server {
		return &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       60 * time.Second,
			WriteTimeout:      120 * time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    32 << 10,
			// DoH on the web listeners is bounded per request; HTTP/2
			// streams per connection are bounded here.
			HTTP2:    &http.HTTP2Config{MaxConcurrentStreams: 32},
			ErrorLog: slog.NewLogLogger(a.log.Handler(), slog.LevelDebug),
		}
	}
	// The web listeners close connections from addresses outside the web
	// ACL right after accept (stage 1; the API checks every request too),
	// then cap the connections per client, per IPv6 /64 and in total,
	// shared by HTTP and HTTPS (webLimiter; trusted proxies only in total,
	// loopback in total and its reserve).
	limit := newWebLimiter(func(ip netip.Addr) bool { return a.web.Get().TrustedProxy(ip) }, webLimits, a.log)
	webSrv := newWeb()
	servers = append(servers, webSrv)
	for _, ln := range a.ln.web {
		guarded := limit.Listener(a.web.Listener(ln))
		goRun("web", func() error { return webSrv.Serve(guarded) })
	}
	if len(a.ln.webTLS) > 0 {
		// Never disabled for a certificate problem: the web certificate
		// falls back to the local CA or a self-signed certificate.
		webTLS := newWeb()
		webTLS.TLSConfig = a.webTLS.tlsConfig()
		servers = append(servers, webTLS)
		for _, ln := range a.ln.webTLS {
			guarded := limit.Listener(a.web.Listener(ln))
			goRun("web-tls", func() error { return webTLS.ServeTLS(guarded, "", "") })
		}
	}
	return servers
}

// Shutdown budgets. The web and DoH servers get httpShutdownGrace to finish
// their requests, the download cache's server cacheShutdownGrace (a
// download that is cut off is resumed by its client with a range request),
// then they are closed. The background components, cancelled after that,
// get their own componentStopWait, so a long request cannot use up the time
// they need to flush before the databases close. Together with closing the
// cache store (at most 10 s for running operations) this stays below
// Docker's stop_grace_period of 30 s; without a long request PiCache stops
// well within Docker's default of 10 s.
var (
	httpShutdownGrace  = 12 * time.Second
	cacheShutdownGrace = 3 * time.Second
	serverStopWait     = time.Second
	componentStopWait  = 5 * time.Second
)

// shutdown stops PiCache in the order that keeps DNS answering longest, so
// a restart or an update interrupts DNS only for the flush of the
// components and the next start:
//
//  1. The API's event streams end (http.Server.Shutdown would wait its whole
//     grace for them) and the HTTP servers (servers and cache, which may be
//     nil) stop accepting, finish their requests within their grace and are
//     then closed. DNS, DoT, NTP and the SNI pass-through keep answering
//     meanwhile: their serve context (stop) is still running.
//  2. stop cancels the serve context: DNS and the other servers of that
//     context stop, and so do the background components (bg).
//  3. The listeners are closed and the server goroutines (srv) and the
//     components are awaited, each within its own budget.
func (a *App) shutdown(servers []*http.Server, cache *http.Server, stop context.CancelFunc, srv, bg *sync.WaitGroup) {
	if a.api != nil {
		a.api.EndStreams()
	}
	drain := func(s *http.Server, grace time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			_ = s.Close() // grace period over: drop the remaining connections
		}
	}
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() { drain(s, httpShutdownGrace) })
	}
	if cache != nil {
		wg.Go(func() { drain(cache, cacheShutdownGrace) })
	}
	wg.Wait()
	stop()
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

// newFetchClient is the HTTP client for list and cache-domains downloads
// and the release check: resolution via the bypass resolver, SSRF guard
// (private destinations only with netutil.WithAllowPrivate), no redirects
// (callers follow them manually), no environment proxy; through the
// outbound proxy while proxy returns a tunnel (network.proxyFor), which is
// asked for a tunnel to the checked address.
func newFetchClient(lookup netutil.Resolver, proxy func(context.Context) *netutil.Tunnel) *http.Client {
	d := &netutil.SafeDialer{Resolve: lookup, Timeout: 15 * time.Second, Proxy: proxy}
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

// newWhoisClient is the HTTP client of the RDAP lookups (clients package):
// resolution via the bypass resolver, public destinations only (no private
// exception), TLS 1.2+ against the system roots, never a proxy; the
// clients package bounds the redirects (https only) and the time.
func newWhoisClient(lookup netutil.Resolver) *http.Client {
	d := &netutil.SafeDialer{Resolve: lookup, Timeout: 10 * time.Second,
		AllowPrivate: func(context.Context) bool { return false }}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         d.DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			ForceAttemptHTTP2:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     60 * time.Second,
		},
	}
}

// refreshRoutes reads the route table (the snapshot of iface: identifiers
// and GET /network/interfaces) at the start, every maintenance tick and on
// SIGHUP; a change drops the cached identities.
func (a *App) refreshRoutes() {
	changed, truncated := netutil.RefreshRoutes()
	if truncated && !a.routesTruncWarned {
		a.routesTruncWarned = true
		a.log.Warn("the route table has more than 4096 routes: iface: identifiers use the first 4096")
	}
	if changed && a.clients != nil {
		a.clients.InterfacesChanged()
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
