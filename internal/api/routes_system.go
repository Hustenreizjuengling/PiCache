package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"runtime/metrics"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/dlcache/proxy"
	"github.com/hustenreizjuengling/picache/internal/dlcache/sni"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/version"
)

const (
	maxRestoreBodyBytes = 512 << 20
	backupDeadline      = 15 * time.Minute
)

// registerSystemRoutes registers the system endpoints (docs/API.md).
func (s *Server) registerSystemRoutes() {
	s.route("GET /api/v1/system/info", permRead, s.systemInfo)
	s.route("GET /api/v1/system/health", permRead, s.systemHealth)
	s.route("GET /api/v1/system/overview", permRead, s.systemOverview)
	s.route("GET /api/v1/system/audit", permAdmin, s.systemAudit)
	s.route("GET /api/v1/system/backup", permAdmin, s.systemBackup)
	// A restore replaces the whole configuration at once: interactive
	// sessions only, and the password is asked again (restorePasswordHeader).
	s.route("POST /api/v1/system/restore", permSession, s.systemRestore, routeDestructive)
	s.route("POST /api/v1/system/restart", permAdmin, s.systemRestart, routeExempt)
	s.route("GET /api/v1/system/update", permRead, s.systemUpdate)
	s.route("POST /api/v1/system/update/check", permAdmin, s.systemUpdateCheck, routeExempt)
	// Installing replaces the binary: interactive sessions only, and the
	// password is asked again (like a restore).
	s.route("POST /api/v1/system/update/apply", permSession, s.systemUpdateApply)
}

type memoryInfo struct {
	AllocBytes uint64 `json:"allocBytes"` // live heap objects
	SysBytes   uint64 `json:"sysBytes"`   // memory obtained from the OS
	LimitBytes uint64 `json:"limitBytes"` // GOMEMLIMIT (0 = none)
	NumGC      uint64 `json:"numGC"`
}

type systemInfoResponse struct {
	Version         version.Info `json:"version"`
	StartedAt       time.Time    `json:"startedAt"`
	UptimeSec       int64        `json:"uptimeSec"`
	InstanceID      string       `json:"instanceId"`
	Listeners       ListenerInfo `json:"listeners"`
	DataDir         string       `json:"dataDir"`
	CacheDir        string       `json:"cacheDir"`
	MountRoot       string       `json:"mountRoot"`
	MasterKeySource string       `json:"masterKeySource"`
	Memory          memoryInfo   `json:"memory"`
	Goroutines      int          `json:"goroutines"`
	// WebRefused counts the connections and requests the web ACL refused
	// since the start; ClientAddress is this request's effective client,
	// PeerAddress its TCP peer (a trusted proxy when they differ).
	WebRefused    uint64 `json:"webRefused"`
	ClientAddress string `json:"clientAddress"`
	PeerAddress   string `json:"peerAddress"`
}

func (s *Server) systemInfo(w http.ResponseWriter, r *http.Request) error {
	rt := s.d.Runtime
	started := rt.StartedAt()
	ci := requestClient(r)
	addr := func(ip netip.Addr) string {
		if ip.IsValid() {
			return ip.String()
		}
		return ""
	}
	return ok(w, systemInfoResponse{
		WebRefused:      s.web.Refused(),
		ClientAddress:   addr(ci.client),
		PeerAddress:     addr(ci.peer),
		Version:         version.Get(),
		StartedAt:       started.UTC(),
		UptimeSec:       int64(time.Since(started).Seconds()),
		InstanceID:      rt.InstanceID(),
		Listeners:       rt.Listeners(),
		DataDir:         s.d.Config.DataDir,
		CacheDir:        s.d.Config.CacheDir,
		MountRoot:       s.d.Config.MountRoot,
		MasterKeySource: rt.MasterKeySource(),
		Memory:          readMemory(),
		Goroutines:      runtime.NumGoroutine(),
	})
}

// readMemory samples runtime/metrics (no stop-the-world, unlike ReadMemStats).
func readMemory() memoryInfo {
	samples := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/gomemlimit:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(samples)
	val := func(i int) uint64 {
		if samples[i].Value.Kind() == metrics.KindUint64 {
			return samples[i].Value.Uint64()
		}
		return 0
	}
	m := memoryInfo{AllocBytes: val(0), SysBytes: val(1), LimitBytes: val(2), NumGC: val(3)}
	if m.LimitBytes == math.MaxInt64 {
		m.LimitBytes = 0
	}
	return m
}

func (s *Server) systemHealth(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Runtime.Health(r.Context()))
}

type overviewHealth struct {
	OK       bool `json:"ok"`
	Warnings int  `json:"warnings"`
	Failures int  `json:"failures"`
}

type systemOverviewResponse struct {
	Blocking             dnsserver.BlockingStatus `json:"blocking"`
	DNS                  dnsserver.Stats          `json:"dns"`
	CacheIPs             dnsserver.CacheIPStatus  `json:"cacheIps"`
	Router               dnsserver.RouterStatus   `json:"router"`
	DownloadCacheEnabled bool                     `json:"downloadCacheEnabled"`
	ServicesReady        bool                     `json:"servicesReady"`
	Store                StoreState               `json:"store"`
	Proxy                proxy.Stats              `json:"proxy"`
	SNI                  sni.Stats                `json:"sni"`
	Filter               filter.Stats             `json:"filter"`
	Upstreams            []upstream.UpstreamStat  `json:"upstreams"`
	ClockGuard           bool                     `json:"clockGuard"`
	Health               overviewHealth           `json:"health"`
}

func (s *Server) systemOverview(w http.ResponseWriter, r *http.Request) error {
	d := s.d
	h := d.Runtime.Health(r.Context())
	sum := overviewHealth{OK: h.OK}
	for _, c := range h.Checks {
		switch c.Status {
		case "warn":
			sum.Warnings++
		case "fail":
			sum.Failures++
		}
	}
	ups := d.Upstream.Stats()
	if ups == nil {
		ups = []upstream.UpstreamStat{}
	}
	return ok(w, systemOverviewResponse{
		Blocking:             d.DNS.Blocking(),
		DNS:                  d.DNS.Stats(),
		CacheIPs:             d.DNS.CacheIPs(),
		Router:               d.DNS.Router(),
		DownloadCacheEnabled: d.Settings.Get().DownloadCache.Enabled,
		ServicesReady:        d.Services.Status().Ready,
		Store:                d.Runtime.StoreState(),
		Proxy:                d.Proxy.Stats(),
		SNI:                  d.SNI.Stats(),
		Filter:               d.Filter.Stats(),
		Upstreams:            ups,
		ClockGuard:           d.Upstream.ClockGuard(),
		Health:               sum,
	})
}

func (s *Server) systemAudit(w http.ResponseWriter, r *http.Request) error {
	limit, err := qInt(r, "limit", 50)
	if err != nil {
		return err
	}
	offset, err := qInt(r, "offset", 0)
	if err != nil {
		return err
	}
	items, total, err := s.d.Auth.AuditLog(r.Context(), auth.AuditQuery{Search: qString(r, "search"), Limit: limit, Offset: offset})
	if err != nil {
		return err
	}
	return ok(w, listing.Page[auth.AuditEntry]{Items: items, Total: total})
}

// backupDownload sets the attachment headers on the first write, so that an
// error before any byte was produced can still be answered with JSON.
type backupDownload struct {
	w        http.ResponseWriter
	filename string
	started  bool
}

func (d *backupDownload) Write(p []byte) (int, error) {
	if !d.started {
		d.start()
	}
	return d.w.Write(p)
}

func (d *backupDownload) start() {
	d.started = true
	h := d.w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": d.filename}))
	d.w.WriteHeader(http.StatusOK)
}

func (s *Server) systemBackup(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, backupDeadline)
	includeSecrets := qBool(r, "includeSecrets")
	dw := &backupDownload{w: w, filename: "picache-backup-" + time.Now().UTC().Format("2006-01-02") + ".db"}
	if err := s.d.Runtime.Backup(r.Context(), dw, includeSecrets); err != nil {
		if !dw.started {
			return err
		}
		// Abort the connection: a truncated file must not look like a
		// complete download.
		s.log.Error("backup failed during download", slog.Any("err", err))
		panic(http.ErrAbortHandler)
	}
	if !dw.started {
		dw.start()
	}
	s.audit(r, "system.backup", "", map[string]bool{"includeSecrets": includeSecrets})
	return nil
}

// restorePasswordHeader carries the current password for POST
// /system/restore, percent-encoded as UTF-8 (JavaScript encodeURIComponent;
// header values cannot carry arbitrary Unicode). ASCII passwords without
// "%" can be sent as they are.
const restorePasswordHeader = "X-PiCache-Password"

func (s *Server) systemRestore(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, backupDeadline)
	if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/octet-stream" {
		return apperr.Invalid("body", "Content-Type must be application/octet-stream")
	}
	body := http.MaxBytesReader(w, r.Body, maxRestoreBodyBytes)
	pw, err := url.PathUnescape(r.Header.Get(restorePasswordHeader))
	if err != nil {
		pw = "\x00" // malformed encoding: never a valid password, counted as a wrong one
	}
	if err := s.d.Auth.ConfirmPassword(r.Context(), principal(r), pw); err != nil {
		// Read the rest of the upload first: a browser that is still
		// sending the file may otherwise see a reset connection instead of
		// this answer.
		_, _ = io.Copy(io.Discard, body)
		return err
	}
	staged, err := s.d.Runtime.StageRestore(r.Context(), body)
	if err != nil {
		if _, tooBig := errors.AsType[*http.MaxBytesError](err); tooBig {
			return apperr.Invalid("body", "the backup is larger than 512 MiB")
		}
		return err
	}
	s.audit(r, "system.restore", "", nil)
	out := map[string]any{"staged": true, "message": "Restart PiCache to apply"}
	// Nothing is refused, but a requester the restored settings would lock
	// out is told how to get back in.
	if warn := restoreWarning(r, staged); warn != "" {
		out["webAccessWarning"] = warn
	}
	return writeJSON(w, http.StatusAccepted, out)
}

func (s *Server) systemRestart(w http.ResponseWriter, r *http.Request) error {
	s.audit(r, "system.restart", "", nil)
	if err := writeJSON(w, http.StatusAccepted, map[string]bool{"restarting": true}); err != nil {
		return err
	}
	_ = http.NewResponseController(w).Flush()
	s.d.Runtime.Restart()
	return nil
}

var errNoUpdater = apperr.Unavailable("update checks are not available")

func (s *Server) systemUpdate(w http.ResponseWriter, r *http.Request) error {
	if s.d.Updates == nil {
		return errNoUpdater
	}
	return ok(w, s.d.Updates.UpdateOverview(r.Context()))
}

func (s *Server) systemUpdateCheck(w http.ResponseWriter, r *http.Request) error {
	if s.d.Updates == nil {
		return errNoUpdater
	}
	return ok(w, s.d.Updates.CheckUpdate(r.Context()))
}

// systemUpdateApply queues the update found by the last check for the root
// helper. It needs the current password, checked before anything else.
func (s *Server) systemUpdateApply(w http.ResponseWriter, r *http.Request) error {
	if s.d.Updates == nil {
		return errNoUpdater
	}
	var in struct {
		Version         string `json:"version"`
		CurrentPassword string `json:"currentPassword"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	p := principal(r)
	if err := s.confirmCurrentPassword(r.Context(), p, in.CurrentPassword); err != nil {
		return err
	}
	if err := s.d.Updates.QueueUpdate(r.Context(), in.Version, p.Username); err != nil {
		return err
	}
	s.audit(r, "system.update_queued", in.Version, map[string]string{"from": version.Version})
	return writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// confirmCurrentPassword re-checks the password like a restore does
// (throttled, a failure is audited) but reports a missing or wrong password
// as invalid input of "currentPassword", like the account endpoints.
func (s *Server) confirmCurrentPassword(ctx context.Context, p *auth.Principal, pw string) error {
	err := s.d.Auth.ConfirmPassword(ctx, p, pw)
	if e, ok := apperr.As(err); ok && e.Kind == apperr.KindUnauthorized && e.Field == "password" {
		return apperr.Invalid("currentPassword", "%s", e.Message)
	}
	return err
}
