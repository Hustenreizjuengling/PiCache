package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/applog"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/hostinfo"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// Diagnostics (docs/API.md "System"): the application log, the runtime
// debug level, host resources, the warning history, database sizes and
// the support bundle.

// HostInfo is GET /system/host: the last sample of the host's resources
// (refreshed with every health evaluation).
type HostInfo = hostinfo.Info

// LogRecord is a record of the application log.
type LogRecord = applog.Record

// SystemLog is GET /system/log.
type SystemLog struct {
	Records    []LogRecord      `json:"records"` // newest first
	BaseLevel  string           `json:"baseLevel"`
	Override   *applog.Override `json:"override,omitempty"`
	Components []string         `json:"components"`
	Capacity   int              `json:"capacity"`
	Dropped    uint64           `json:"dropped"` // records not delivered to slow streams
}

// DatabaseInfo is GET /system/databases: file sizes of the databases.
type DatabaseInfo struct {
	PiCache      DatabaseFile   `json:"picache"`
	Logs         LogsDatabase   `json:"logs"`
	CacheIndexes []CacheIndexDB `json:"cacheIndexes"`
}

// DatabaseFile is the size of a database and its WAL.
type DatabaseFile struct {
	Bytes    int64 `json:"bytes"`
	WALBytes int64 `json:"walBytes"`
}

// LogsDatabase is logs.db with its size cap (logs.maxDbSizeMiB).
type LogsDatabase struct {
	Bytes       int64   `json:"bytes"`
	WALBytes    int64   `json:"walBytes"`
	CapBytes    int64   `json:"capBytes"`    // 0 = no cap
	FillPercent float64 `json:"fillPercent"` // 0 without a cap
	Disabled    string  `json:"disabled,omitempty"`
	RawPaused   bool    `json:"rawPaused"`
}

// CacheIndexDB is the index database of a cache store.
type CacheIndexDB struct {
	StoreID  string `json:"storeId"`
	Bytes    int64  `json:"bytes"`
	WALBytes int64  `json:"walBytes"`
}

// Diagnostics is implemented by internal/app.
type Diagnostics interface {
	HostInfo() HostInfo
	Databases() DatabaseInfo
	// SupportBundle builds the support bundle (a zip of at most 16 MiB)
	// from cached data; client names are kept only with
	// includeClientNames.
	SupportBundle(ctx context.Context, includeClientNames bool) ([]byte, error)
}

// Limits of the diagnostics endpoints.
const (
	systemLogDefault   = 500
	eventPageMax       = 200
	supportBundleBuild = 30 * time.Second
	supportBundleWrite = 60 * time.Second
)

var (
	errNoAppLog = apperr.Unavailable("the application log is not available")
	errNoDiag   = apperr.Unavailable("diagnostics are not available")
)

// registerDiagRoutes registers the diagnostics endpoints (docs/API.md).
func (s *Server) registerDiagRoutes() {
	s.route("GET /api/v1/system/log", permAdmin, s.systemLog)
	s.route("GET /api/v1/stream/system-log", permAdmin, s.streamSystemLog)
	s.route("PUT /api/v1/system/log/level", permAdmin, s.systemLogLevelSet, routeExempt)
	s.route("DELETE /api/v1/system/log/level", permAdmin, s.systemLogLevelClear, routeExempt)
	s.route("GET /api/v1/system/host", permRead, s.systemHost)
	s.route("GET /api/v1/system/databases", permRead, s.systemDatabases)
	s.route("GET /api/v1/system/events", permRead, s.systemEvents)
	s.route("POST /api/v1/system/events/{id}/ack", permAdmin, s.systemEventAck, routeExempt)
	s.route("POST /api/v1/system/events/ack-all", permAdmin, s.systemEventAckAll, routeExempt)
	// Interactive admin sessions only, with the password asked again.
	s.route("POST /api/v1/system/support-bundle", permSession, s.systemSupportBundle, routeExempt)
}

// logFilter reads ?level (minimum level, default debug) and ?component
// ("" = all).
func logFilter(r *http.Request) (slog.Level, string, error) {
	lv := slog.LevelDebug
	if v := qString(r, "level"); v != "" {
		var ok bool
		if lv, ok = applog.ParseLevel(v); !ok {
			return 0, "", apperr.Invalid("level", "must be debug, info, warn or error")
		}
	}
	c := qString(r, "component")
	if c != "" && !applog.ValidComponent(c) {
		return 0, "", apperr.Invalid("component", "unknown component")
	}
	return lv, c, nil
}

func (s *Server) systemLog(w http.ResponseWriter, r *http.Request) error {
	if s.d.AppLog == nil {
		return errNoAppLog
	}
	lv, component, err := logFilter(r)
	if err != nil {
		return err
	}
	limit, err := qInt(r, "limit", systemLogDefault)
	if err != nil {
		return err
	}
	if limit < 1 || limit > applog.Capacity {
		return apperr.Invalid("limit", "must be between 1 and %d", applog.Capacity)
	}
	l := s.d.AppLog
	return ok(w, SystemLog{Records: l.Records(lv, component, limit), BaseLevel: l.BaseLevel(), Override: l.CurrentOverride(),
		Components: applog.Components, Capacity: applog.Capacity, Dropped: l.Dropped()})
}

func (s *Server) streamSystemLog(w http.ResponseWriter, r *http.Request) error {
	if s.d.AppLog == nil {
		return errNoAppLog
	}
	lv, component, err := logFilter(r)
	if err != nil {
		return err
	}
	ch, cancel, err := s.d.AppLog.Subscribe(lv, component)
	if errors.Is(err, applog.ErrTooMany) {
		return apperr.TooMany("too many application log streams (at most %d)", applog.MaxSubs)
	}
	if err != nil {
		return err
	}
	defer cancel()
	return sse(w, r, "record", ch, s.logsAlive(r))
}

// logLevelResponse is the answer of PUT /system/log/level.
type logLevelResponse struct {
	BaseLevel string           `json:"baseLevel"`
	Override  *applog.Override `json:"override"`
}

func (s *Server) systemLogLevelSet(w http.ResponseWriter, r *http.Request) error {
	if s.d.AppLog == nil {
		return errNoAppLog
	}
	var in struct {
		Level     string `json:"level"`
		Component string `json:"component"`
		Minutes   int    `json:"minutes"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	o, err := s.d.AppLog.SetOverride(in.Level, in.Component, time.Duration(in.Minutes)*time.Minute)
	switch {
	case errors.Is(err, applog.ErrLevel):
		return apperr.Invalid("level", "must be debug or info and more verbose than the level PiCache was started with (%s)", s.d.AppLog.BaseLevel())
	case errors.Is(err, applog.ErrComponent):
		return apperr.Invalid("component", "unknown component")
	case errors.Is(err, applog.ErrMinutes):
		return apperr.Invalid("minutes", "must be between 1 and %d", applog.MaxOverride)
	case err != nil:
		return err
	}
	s.audit(r, "system.log_level", "", map[string]any{"level": in.Level, "component": in.Component, "minutes": in.Minutes})
	return ok(w, logLevelResponse{BaseLevel: s.d.AppLog.BaseLevel(), Override: &o})
}

func (s *Server) systemLogLevelClear(w http.ResponseWriter, r *http.Request) error {
	if s.d.AppLog == nil {
		return errNoAppLog
	}
	s.d.AppLog.ClearOverride()
	s.audit(r, "system.log_level_clear", "", nil)
	return noContent(w)
}

func (s *Server) systemHost(w http.ResponseWriter, r *http.Request) error {
	if s.d.Diag == nil {
		return errNoDiag
	}
	return ok(w, s.d.Diag.HostInfo())
}

func (s *Server) systemDatabases(w http.ResponseWriter, r *http.Request) error {
	if s.d.Diag == nil {
		return errNoDiag
	}
	return ok(w, s.d.Diag.Databases())
}

// isAdmin reports whether the principal has admin scope.
func isAdmin(r *http.Request) bool {
	p := principal(r)
	return p != nil && p.Scope == auth.ScopeAdmin
}

func (s *Server) systemEvents(w http.ResponseWriter, r *http.Request) error {
	q := logs.EventQuery{Security: isAdmin(r), Cursor: qString(r, "cursor")}
	switch v := qString(r, "unacknowledged"); v {
	case "":
	case "true", "false":
		b := v == "true"
		q.Unacknowledged = &b
	default:
		return apperr.Invalid("unacknowledged", "must be true or false")
	}
	limit, err := qInt(r, "limit", 50)
	if err != nil {
		return err
	}
	if limit < 1 || limit > eventPageMax {
		return apperr.Invalid("limit", "must be between 1 and %d", eventPageMax)
	}
	q.Limit = limit
	page, err := s.d.Logs.Events(r.Context(), q)
	if err != nil {
		return err
	}
	if !q.Security {
		for i := range page.Items {
			page.Items[i].AcknowledgedBy = ""
		}
	}
	return ok(w, page)
}

// actor names the principal in the history (like the audit log).
func actor(r *http.Request) string {
	p := principal(r)
	if p == nil {
		return ""
	}
	if p.TokenID != 0 {
		return p.Username + " (API token #" + strconv.FormatInt(p.TokenID, 10) + ")"
	}
	return p.Username
}

func (s *Server) systemEventAck(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	ev, err := s.d.Logs.AckEvent(r.Context(), id, actor(r))
	if err != nil {
		return err
	}
	s.audit(r, "system.event_ack", strconv.FormatInt(id, 10), nil)
	return ok(w, ev)
}

func (s *Server) systemEventAckAll(w http.ResponseWriter, r *http.Request) error {
	n, err := s.d.Logs.AckAllEvents(r.Context(), actor(r))
	if err != nil {
		return err
	}
	s.audit(r, "system.event_ack_all", "", map[string]int64{"acknowledged": n})
	return ok(w, map[string]int64{"acknowledged": n})
}

// systemSupportBundle builds and sends the support bundle. The password is
// read from the JSON body only (never from the URL) and checked like a
// restore (throttled, a failure is audited).
func (s *Server) systemSupportBundle(w http.ResponseWriter, r *http.Request) error {
	if s.d.Diag == nil {
		return errNoDiag
	}
	extendDeadlines(w, supportBundleWrite)
	var in struct {
		CurrentPassword    string `json:"currentPassword"`
		IncludeClientNames bool   `json:"includeClientNames"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.confirmCurrentPassword(r.Context(), principal(r), in.CurrentPassword); err != nil {
		return err
	}
	if !s.bundling.CompareAndSwap(false, true) {
		return apperr.TooMany("a support bundle is being built; try again when it is done")
	}
	defer s.bundling.Store(false)
	ctx, cancel := context.WithTimeout(r.Context(), supportBundleBuild)
	defer cancel()
	b, err := s.d.Diag.SupportBundle(ctx, in.IncludeClientNames)
	if err != nil {
		return err
	}
	s.audit(r, "system.support_bundle", "", map[string]bool{"includeClientNames": in.IncludeClientNames})
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	h.Set("Content-Disposition", `attachment; filename="picache-support-`+time.Now().UTC().Format("20060102T150405Z")+`.zip"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
	return nil
}
