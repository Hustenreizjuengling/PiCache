package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/dlcache/proxy"
	"github.com/hustenreizjuengling/picache/internal/dlcache/services"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/hostinfo"
	"github.com/hustenreizjuengling/picache/internal/notify"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

// Diagnostics of the app (api.Diagnostics): host resources, database
// sizes and the support bundle (supportbundle.go).

// maxIndexFiles bounds the cache index databases listed.
const maxIndexFiles = 64

// sampleHost reads the host's resources and keeps the sample for
// GET /system/host (at start and with every health evaluation).
func (a *App) sampleHost() hostinfo.Info {
	s := a.hostSampler
	if s == nil { // parts of the App built by tests
		s = &hostinfo.Sampler{}
	}
	in := s.Sample(time.Now())
	a.host.Store(&in)
	return in
}

// newHostSampler returns the sampler of the host's resources: the data and
// cache directories' disks, container or not.
func (a *App) newHostSampler() *hostinfo.Sampler {
	s := &hostinfo.Sampler{Disks: []hostinfo.DiskPath{{Path: a.cfg.DataDir, Role: "data"}, {Path: a.cfg.CacheDir, Role: "cache"}}}
	if a.storage != nil {
		s.Container = a.storage.Capabilities().Container != ""
	}
	return s
}

// HostInfo returns the last sample of the host's resources.
func (a *App) HostInfo() api.HostInfo {
	if in := a.host.Load(); in != nil {
		return *in
	}
	return a.sampleHost()
}

// hostHealth evaluates the health check "host" on a new sample (show is
// false when no value could be read).
func (a *App) hostHealth() (status, msg, hint string, show bool) {
	in := a.sampleHost()
	h := a.set.Get().Health
	status, msg, show = in.Check(hostinfo.Thresholds{MemoryAvailableMinPercent: h.MemoryAvailableMinPercent,
		LoadPerCPUMax: h.LoadPerCPUMax, TemperatureMaxCelsius: h.TemperatureMaxCelsius})
	if status == "warn" {
		hint = "see System → Health & about → Host resources; the thresholds are set there"
	}
	return status, msg, hint, show
}

// fileSize returns the size of a regular file (0 if it is missing or no
// regular file).
func fileSize(path string) int64 {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return 0
	}
	return fi.Size()
}

// Databases returns the sizes of picache.db, logs.db and the cache index
// databases (file sizes; at most 64 index files).
func (a *App) Databases() api.DatabaseInfo {
	out := api.DatabaseInfo{
		PiCache:      api.DatabaseFile{Bytes: fileSize(a.paths.ConfigDB), WALBytes: fileSize(a.paths.ConfigDB + "-wal")},
		Logs:         api.LogsDatabase{Bytes: fileSize(a.paths.LogsDB), WALBytes: fileSize(a.paths.LogsDB + "-wal")},
		CacheIndexes: []api.CacheIndexDB{},
	}
	if a.set != nil {
		out.Logs.CapBytes = int64(a.set.Get().Logs.MaxDBSizeMiB) << 20
	}
	if a.logs != nil {
		m := a.logs.Metrics()
		out.Logs.Disabled, out.Logs.RawPaused = m.Disabled, m.RawPaused
	}
	if out.Logs.CapBytes > 0 {
		out.Logs.FillPercent = float64(out.Logs.Bytes) * 100 / float64(out.Logs.CapBytes)
	}
	entries, err := os.ReadDir(a.paths.CacheIndexDir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		name := e.Name()
		if len(out.CacheIndexes) == maxIndexFiles {
			break
		}
		if !strings.HasSuffix(name, ".db") || !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(a.paths.CacheIndexDir, name)
		out.CacheIndexes = append(out.CacheIndexes, api.CacheIndexDB{StoreID: strings.TrimSuffix(name, ".db"),
			Bytes: fileSize(p), WALBytes: fileSize(p + "-wal")})
	}
	return out
}

// configComponent is the name and the migrations of a picache.db component.
type configComponent struct {
	name  string
	steps func() []string
}

// configComponents are the components of picache.db in start order (build).
var configComponents = []configComponent{
	{"app", func() []string { return appMigrations }},
	{"settings", settings.Migrations},
	{"notify", notify.Migrations},
	{"clients", clients.Migrations},
	{"filter", filter.Migrations},
	{"parental", parental.Migrations},
	{"services", services.Migrations},
	{"dhcp", dhcp.Migrations},
	{"auth", auth.Migrations},
	{"storage", storage.Migrations},
	{"dns", dnsserver.Migrations},
	{"proxy", proxy.Migrations},
}

// MigrateConfigDB builds the schema of every picache.db component of this
// binary in d, in start order, starting nothing (`picache db salvage`).
func MigrateConfigDB(ctx context.Context, d *db.DB) error {
	for _, c := range configComponents {
		if err := d.Migrate(ctx, c.name, c.steps()); err != nil {
			return err
		}
	}
	return nil
}

// ConfigSchemaVersions returns the schema version of every picache.db
// component of this binary.
func ConfigSchemaVersions() map[string]int {
	out := make(map[string]int, len(configComponents))
	for _, c := range configComponents {
		out[c.name] = len(c.steps())
	}
	return out
}
