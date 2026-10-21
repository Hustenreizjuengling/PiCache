package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// prefillHost switches the prefill service on or off through the root
// helper (docs/ARCHITECTURE.md 20.1): PiCache only queues the action; the
// helper installs, enables, starts or stops the unit.
type prefillHost struct {
	dataDir string
	updates *updater
	log     *slog.Logger
	now     func() time.Time
	mu      sync.Mutex
}

// Mode is how the prefill service can be switched: helper (from the web
// UI), package, docker or manual (on the host).
func (p *prefillHost) Mode(ctx context.Context) string {
	if p.updates == nil {
		return update.ModeManual
	}
	return p.updates.UpdateOverview(ctx).Mode
}

func (p *prefillHost) Status() *update.PrefillStatus {
	return update.ReadPrefillStatus(p.dataDir, p.now())
}

func (p *prefillHost) Request(ctx context.Context, enable bool, by string) (*update.PrefillStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.Mode(ctx); m != update.ModeHelper {
		return nil, apperr.Conflict("the prefill service is switched on the host here (%s): sudo picache prefill enable", m)
	}
	if st := p.Status(); st != nil && st.State == update.StateRunning {
		return st, apperr.Conflict("the prefill service is being set up already")
	}
	action := update.PrefillDisable
	if enable {
		action = update.PrefillEnable
	}
	if err := update.QueuePrefill(p.dataDir, update.PrefillRequest{Action: action, RequestedAt: p.now().UTC(), RequestedBy: by}); err != nil {
		return nil, fmt.Errorf("queue the prefill request: %w", err)
	}
	p.log.Info("prefill service request queued for the helper", slog.String("action", action), slog.String("by", by))
	return p.Status(), nil
}
