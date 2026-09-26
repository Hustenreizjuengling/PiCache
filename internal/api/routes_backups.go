package api

import (
	"context"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// ScheduledBackups is implemented by internal/app: scheduled backups of
// picache.db (docs/ARCHITECTURE.md 15.2). The settings are the settings
// section "backups" (PATCH /settings/backups).
type ScheduledBackups interface {
	// Overview returns the settings, the last run, the next run and the
	// backups in the current destination.
	Overview(ctx context.Context) ScheduledBackupsOverview
	// RunNow starts a run in the background, the same as a scheduled one
	// (apperr.Conflict while one runs).
	RunNow() error
	// Open opens a backup of the current destination for download; name
	// must be a scheduled backup of this installation.
	Open(ctx context.Context, name string) (io.ReadCloser, int64, error)
	// Delete removes a backup of the current destination.
	Delete(ctx context.Context, name string) error
}

// ScheduledBackupRun is the result of the last run.
type ScheduledBackupRun struct {
	Time        time.Time `json:"time"` // start of the run
	OK          bool      `json:"ok"`
	Error       string    `json:"error,omitempty"`
	File        string    `json:"file,omitempty"`
	SizeBytes   int64     `json:"sizeBytes,omitempty"`
	Destination string    `json:"destination"` // "local" or a storage target id
}

// ScheduledBackupFile is a backup in the destination.
type ScheduledBackupFile struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"sizeBytes"`
	Time      time.Time `json:"time"` // from the file name (UTC)
}

// ScheduledBackupsOverview is GET /system/backups/scheduled.
type ScheduledBackupsOverview struct {
	Settings settings.Backups    `json:"settings"`
	Last     *ScheduledBackupRun `json:"last,omitempty"`
	Next     time.Time           `json:"next,omitzero"` // next run while enabled
	Running  bool                `json:"running"`
	// TimeZone is the abbreviation of the host's time zone that
	// settings.time refers to ("CEST", "UTC"; containers default to UTC).
	TimeZone string `json:"timeZone"`
	// DestinationPath is the directory of the current destination, as seen
	// by PiCache ("" when the storage target is unknown).
	DestinationPath string `json:"destinationPath"`
	// FilesError says why Files is empty when the destination cannot be
	// read (e.g. the storage target is offline).
	FilesError string                `json:"filesError,omitempty"`
	Files      []ScheduledBackupFile `json:"files"` // newest first
}

// registerBackupRoutes registers the scheduled backup endpoints (docs/API.md).
// The overview (status and file names) is readable with read rights;
// running, downloading and deleting backups needs admin rights.
func (s *Server) registerBackupRoutes() {
	s.route("GET /api/v1/system/backups/scheduled", permRead, s.backupsScheduled)
	s.route("POST /api/v1/system/backups/scheduled/run", permAdmin, s.backupsRun, routeExempt)
	s.route("GET /api/v1/system/backups/scheduled/files/{name}", permAdmin, s.backupsDownload)
	s.route("DELETE /api/v1/system/backups/scheduled/files/{name}", permAdmin, s.backupsDelete, routeDestructive)
}

var errNoScheduledBackups = apperr.Unavailable("scheduled backups are not available")

// backupFileName checks the {name} path value roughly; the exact pattern
// (with this installation's id) is checked by ScheduledBackups.
func backupFileName(r *http.Request) (string, error) {
	name := r.PathValue("name")
	if len(name) == 0 || len(name) > 128 {
		return "", apperr.Invalid("name", "not a scheduled backup")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_') {
			return "", apperr.Invalid("name", "not a scheduled backup")
		}
	}
	return name, nil
}

func (s *Server) backupsScheduled(w http.ResponseWriter, r *http.Request) error {
	if s.d.Backups == nil {
		return errNoScheduledBackups
	}
	return ok(w, s.d.Backups.Overview(r.Context()))
}

func (s *Server) backupsRun(w http.ResponseWriter, r *http.Request) error {
	if s.d.Backups == nil {
		return errNoScheduledBackups
	}
	if err := s.d.Backups.RunNow(); err != nil {
		return err
	}
	s.audit(r, "system.backup_scheduled_run", "", nil)
	return writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) backupsDownload(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, backupDeadline)
	if s.d.Backups == nil {
		return errNoScheduledBackups
	}
	name, err := backupFileName(r)
	if err != nil {
		return err
	}
	f, size, err := s.d.Backups.Open(r.Context(), name)
	if err != nil {
		return err
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	s.audit(r, "system.backup_download", name, nil)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return nil
	}
	if n, err := io.Copy(w, io.LimitReader(f, size)); err != nil || n != size {
		// Abort the connection: a truncated file must not look complete.
		s.log.Warn("scheduled backup download aborted", slog.String("file", name), slog.Any("err", err))
		panic(http.ErrAbortHandler)
	}
	return nil
}

func (s *Server) backupsDelete(w http.ResponseWriter, r *http.Request) error {
	if s.d.Backups == nil {
		return errNoScheduledBackups
	}
	name, err := backupFileName(r)
	if err != nil {
		return err
	}
	if err := s.d.Backups.Delete(r.Context(), name); err != nil {
		return err
	}
	s.audit(r, "system.backup_delete", name, nil)
	return noContent(w)
}
