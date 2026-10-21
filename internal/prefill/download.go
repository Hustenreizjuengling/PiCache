package prefill

import (
	"context"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"
)

// downloadAll fetches paths from host through the cache with the job's
// connections and rate limit, each up to chunkAttempts times. The first
// path that keeps failing ends the download.
func (d *Daemon) downloadAll(ctx context.Context, r *jobRun, host, userAgent, service, id string, paths []string) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(max(r.conns, 1))
	for _, p := range paths {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			var last error
			for attempt := range chunkAttempts {
				if attempt > 0 {
					if err := sleepCtx(gctx, time.Duration(attempt*attempt)*2*time.Second); err != nil {
						return err
					}
				}
				res, _, err := d.cache.get(gctx, host, p, userAgent, r.lim, false, 0)
				if res.bytes > 0 {
					d.addBytes(r, service, id, res.bytes, res.hit)
				}
				if err == nil {
					return nil
				}
				last = err
				if errors.Is(err, errCacheRefused) || gctx.Err() != nil {
					break
				}
			}
			return fmt.Errorf("%s: %w", p, last)
		})
	}
	err := g.Wait()
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return err
}
