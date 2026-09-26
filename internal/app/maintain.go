package app

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// maintenanceInterval is the tick of the web ACL rebuild, the web access
// reset marker and the certificate checks (docs/ARCHITECTURE.md 2).
const maintenanceInterval = time.Minute

// WebAccessResetMarker is the file `picache web-access --reset` creates in
// the data directory; the service applies and deletes it.
const WebAccessResetMarker = "web-access.reset"

// removeMarker deletes the reset marker (tests replace it).
var removeMarker = os.Remove

// maintenanceLoop runs the tick every 60 s and at once on SIGHUP. SIGHUP
// never stops PiCache (cmd/picache subscribes to it before Run).
func (a *App) maintenanceLoop(ctx context.Context) {
	t := time.NewTicker(maintenanceInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.hup:
			a.log.Info("SIGHUP: checking the web access and the web certificate now")
		}
		a.maintain(ctx)
	}
}

// maintain is one tick: the web ACL follows interface changes, a pending
// web access reset is applied and the web certificate is checked.
func (a *App) maintain(ctx context.Context) {
	if a.web != nil {
		a.web.Refresh()
	}
	a.applyWebAccessReset(ctx)
	if a.webTLS != nil {
		a.webTLS.tick()
	}
}

// applyWebAccessReset applies `picache web-access --reset` (the marker
// <data>/web-access.reset, a regular file; anything else is logged and
// left alone): the web UI is open to every address again
// (web.restrictToNetworks off), no proxy is trusted, TLS 1.2 is accepted,
// and an uploaded certificate is deleted (the fallback is served). The
// allowed networks and everything else stay. It is an internal recovery
// update (settings.Store.Recover): PICACHE_CONFIG_LOCKED does not apply,
// and stored settings that are invalid in another member do not refuse it.
// The marker is deleted afterwards; one that cannot be deleted is applied
// once per process start (or again once it was removed by hand).
func (a *App) applyWebAccessReset(ctx context.Context) {
	path := filepath.Join(a.cfg.DataDir, WebAccessResetMarker)
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A marker that could not be deleted was removed by hand: the next
		// one is applied again.
		a.resetMarkerWarned, a.resetApplied = false, false
		return
	case err == nil && !fi.Mode().IsRegular():
		err = errors.New("it is not a regular file; ignored")
	}
	if err != nil {
		if !a.resetMarkerWarned {
			a.resetMarkerWarned = true
			a.log.Warn("web access reset marker", slog.String("file", path), slog.Any("err", err))
		}
		return
	}
	if a.resetApplied {
		return
	}
	var cleared int
	if _, err := a.set.Recover(ctx, func(s *settings.All) error {
		cleared = len(s.Web.TrustedProxies)
		s.Web.RestrictToNetworks = false
		s.Web.TrustedProxies = []string{}
		s.Web.TLSMinVersion = settings.TLSVersion12
		return nil
	}); err != nil {
		a.log.Error("could not apply the web access reset; retried every minute", slog.Any("err", err))
		return
	}
	removed := false
	if a.webTLS != nil {
		if removed, err = a.webTLS.removeUploadForReset(); err != nil {
			a.log.Error("web access reset: could not delete the uploaded certificate", slog.Any("err", err))
		}
	}
	a.auth.Audit(ctx, &auth.Principal{Username: "cli"}, "", "web.access_reset", "", map[string]any{
		"restrictToNetworks": false, "trustedProxiesCleared": cleared, "tlsMinVersion": settings.TLSVersion12,
		"uploadedCertificateRemoved": removed,
	})
	a.log.Warn("web access was reset from the host", slog.Int("trustedProxiesCleared", cleared),
		slog.Bool("uploadedCertificateRemoved", removed))
	if err := removeMarker(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.resetApplied = true
		a.log.Error("could not delete the web access reset marker; it is applied again at the next start", slog.String("file", path), slog.Any("err", err))
	}
}
