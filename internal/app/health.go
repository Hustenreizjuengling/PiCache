package app

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
)

// healthLoop evaluates health every 60 s, logs every status change once and
// raises the health.* notifications (debounced, events.go).
func (a *App) healthLoop(ctx context.Context) {
	prev := map[string]string{}
	var watch healthWatch
	eval := func() {
		h := a.evalHealth(ctx)
		a.health.Store(&h)
		for _, m := range watch.observe(h.Checks) {
			a.notify.Emit(m)
		}
		for _, c := range h.Checks {
			old, seen := prev[c.Name]
			if seen && old == c.Status {
				continue
			}
			prev[c.Name] = c.Status
			switch {
			case c.Status != "ok":
				a.log.Warn("health check", slog.String("check", c.Name), slog.String("status", c.Status),
					slog.String("message", c.Message), slog.String("hint", c.Hint))
			case seen:
				a.log.Info("health check recovered", slog.String("check", c.Name))
			}
		}
	}
	// First evaluation after components had a moment to start.
	select {
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
	}
	eval()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			eval()
		}
	}
}

// Health returns the last evaluated health (evaluating now if none yet).
func (a *App) Health(ctx context.Context) api.Health {
	if h := a.health.Load(); h != nil {
		return *h
	}
	h := a.evalHealth(ctx)
	return h
}

func (a *App) evalHealth(ctx context.Context) api.Health {
	h := api.Health{OK: true, CheckedAt: time.Now().UTC()}
	add := func(name, status, msg, hint string) {
		if status == "fail" {
			h.OK = false
		}
		h.Checks = append(h.Checks, api.HealthCheck{Name: name, Status: status, Message: msg, Hint: hint})
	}
	set := a.set.Get()

	// Listeners
	li := a.Listeners()
	if len(li.Failed) > 0 {
		roles := make([]string, 0, len(li.Failed))
		for r := range li.Failed {
			roles = append(roles, r+": "+li.Failed[r])
		}
		sort.Strings(roles)
		add("listeners", "warn", fmt.Sprint(roles), "free the port or change the PICACHE_*_LISTEN setting, then restart")
	} else {
		add("listeners", "ok", "", "")
	}

	// Upstreams (+ clock guard)
	stats := a.up.Stats()
	healthy := 0
	for _, s := range stats {
		if s.Healthy {
			healthy++
		}
	}
	switch {
	case a.up.ClockGuard():
		add("upstreams", "warn", "system clock is not set; using unencrypted DNS to the bootstrap servers", "enable NTP (e.g. systemd-timesyncd) on the host")
	case len(stats) > 0 && healthy == 0:
		add("upstreams", "fail", "no upstream DNS server is answering", "check the internet connection and the upstream settings")
	default:
		add("upstreams", "ok", "", "")
	}

	// Filtering
	fs := a.filter.Stats()
	switch {
	case set.Filter.Enabled && fs.FailedLists > 0 && fs.Entries == 0:
		add("blocklists", "fail", "no blocklist could be loaded; nothing is blocked", "check the list URLs and the internet connection")
	case fs.FailedLists > 0:
		add("blocklists", "warn", fmt.Sprintf("%d list(s) failed to update", fs.FailedLists), "see Filtering → Blocklists")
	case fs.StaleLists > 0:
		add("blocklists", "warn", fmt.Sprintf("%d list(s) not updated for a long time", fs.StaleLists), "see Filtering → Blocklists")
	default:
		add("blocklists", "ok", "", "")
	}

	// DNS rate limiting
	if top := a.dns.Stats().TopRateLimited; len(top) > 0 {
		add("dns-rate-limit", "warn", fmt.Sprintf("%d client(s) were rate limited in the last hour (e.g. %s)", len(top), top[0].Client),
			"if it is a router forwarding DNS, add it to the rate-limit exemptions")
	}

	// Download cache
	if set.DownloadCache.Enabled {
		st := a.services.Status()
		switch {
		case !st.Ready:
			add("cache-domains", "fail", "no cache-domains list loaded: "+st.Error, "check the internet connection; overrides are inactive until the list is loaded")
		case !st.LastFetched.IsZero() && time.Since(st.LastFetched) > 7*24*time.Hour:
			add("cache-domains", "warn", "cache-domains list not updated for more than 7 days", "")
		default:
			add("cache-domains", "ok", "", "")
		}
		ci := a.dns.CacheIPs()
		if !ci.Ready {
			add("download_cache", "fail", "download cache DNS answers are inactive: "+ci.Reason, "set the cache IP in the download cache settings")
		} else if ci.Reason != "" {
			add("download_cache", "warn", ci.Reason, "")
		} else {
			add("download_cache", "ok", "", "")
		}
		if len(li.Bound["sni"]) == 0 {
			add("sni", "warn", "HTTPS pass-through (:443) is not running; HTTPS to overridden hosts (Windows Update metadata, Battle.net, Epic) fails",
				"enable PICACHE_SNI_LISTEN or free port 443")
		}
		ss := a.StoreState()
		switch {
		case !ss.Online:
			add("cache-store", "fail", "cache storage offline: "+ss.Reason, ss.Hint)
		case ss.Full:
			add("cache-store", "warn", "cache storage is full (only pinned content left); new downloads are not cached", "unpin content or add space")
		case ss.LowSpace:
			add("cache-store", "warn", "cache storage is low on space; evicting old content", "")
		case ss.SDCard:
			add("cache-store", "warn", "the cache is on an SD card (slow, wears out)", "use a USB SSD or a NAS share for the cache")
		default:
			add("cache-store", "ok", "", "")
		}
	}

	// Logs / data disk
	m := a.logs.Metrics()
	switch {
	case m.Disabled != "":
		add("logs", "warn", "logging disabled: "+m.Disabled, "check the data directory; logs.db was moved aside")
	case m.RawPaused:
		add("logs", "warn", "raw log inserts are paused: the data disk has less than 1 GiB free", "free space on the data disk")
	case m.Dropped > 0:
		add("logs", "ok", fmt.Sprintf("%d events dropped under load", m.Dropped), "")
	default:
		add("logs", "ok", "", "")
	}
	if free, ok := diskFree(a.cfg.DataDir); ok && free < 1<<30 {
		add("data-disk", "fail", fmt.Sprintf("only %d MiB free in %s", free>>20, a.cfg.DataDir), "free space on the data disk")
	}
	return h
}
