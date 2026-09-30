package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/applog"
	"github.com/hustenreizjuengling/picache/internal/dhcp"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/upstream"
	"github.com/hustenreizjuengling/picache/internal/ntp"
	"github.com/hustenreizjuengling/picache/internal/settings"
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
			a.emit(m)
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

// blocklistsHealth evaluates the check "blocklists" (first match): nothing
// loaded while lists fail fails; failed or stale lists warn; more compiled
// entries than budget (the entry budget of this host's memory,
// filter.BudgetFor) warn; own
// lists with entries that the TLD guard ignores warn (a list that blocks
// whole TLDs needs the category abused-tlds).
func blocklistsHealth(blockingEnabled bool, fs filter.Stats, budget int) (status, msg, hint string) {
	switch {
	case blockingEnabled && fs.FailedLists > 0 && fs.Entries == 0:
		return "fail", "no blocklist could be loaded; nothing is blocked", "check the list URLs and the internet connection"
	case fs.FailedLists > 0:
		return "warn", fmt.Sprintf("%d list(s) failed to update", fs.FailedLists), "see Filtering → Blocklists"
	case fs.StaleLists > 0:
		return "warn", fmt.Sprintf("%d list(s) not updated for a long time", fs.StaleLists), "see Filtering → Blocklists"
	case fs.Entries > budget:
		return "warn", fmt.Sprintf("the blocklists hold %d entries; a small host may run short of memory", fs.Entries),
			fmt.Sprintf("use fewer or smaller lists (at most about %d entries in total on this host, Filtering → Blocklists)", budget)
	case fs.TLDGuardLists > 0:
		return "warn", fmt.Sprintf("%d own list(s) contain entries that would block a whole top-level domain; they are ignored", fs.TLDGuardLists),
			"if a list is meant to block whole TLDs, give it the category abused-tlds (Filtering → Blocklists)"
	case fs.IPGuardLists > 0:
		return "warn", fmt.Sprintf("%d list(s) of answer addresses contain blocks of broad or private networks; they are ignored", fs.IPGuardLists),
			"a list of answer addresses blocks public networks of at least /16 (IPv4) or /32 (IPv6); write an IPv6 block of an embedded IPv4 address as the IPv4 address (Filtering → Blocklists)"
	}
	return "ok", "", ""
}

// While the default upstreams are healthy, the health check says that
// fallback DNS is in use once the fallbacks answered fallbackWarnAnswers
// fetches within fallbackRecent: a single fallback answer (a connection
// the provider cut, a lost packet) is no reason to warn.
const (
	fallbackRecent      = 5 * time.Minute
	fallbackWarnAnswers = 3
)

// upstreamHealth evaluates the health check "upstreams" (first match): the
// clock guard warns; no default upstream healthy and no healthy fallback
// (or none configured) fails; no default upstream healthy warns; the
// fallbacks having answered at least fallbackWarnAnswers fetches within
// the last 5 minutes (recentFallbacks) warns; a group set that could not
// be built or has no healthy upstream warns (its clients get SERVFAIL:
// group sets have no fallbacks); ok otherwise.
func upstreamHealth(clockGuard bool, stats, fallbacks []upstream.UpstreamStat, recentFallbacks int,
	groups []upstream.GroupUpstreamStat) (status, msg, hint string) {
	healthy := func(list []upstream.UpstreamStat) int {
		n := 0
		for _, s := range list {
			if s.Healthy {
				n++
			}
		}
		return n
	}
	primaryDown := len(stats) > 0 && healthy(stats) == 0
	switch {
	case clockGuard:
		return "warn", "system clock is not set; using unencrypted DNS to the bootstrap servers", "enable NTP (e.g. systemd-timesyncd) on the host"
	case primaryDown && healthy(fallbacks) == 0:
		return "fail", "no upstream DNS server is answering", "check the internet connection and the upstream settings"
	case primaryDown:
		return "warn", "fallback DNS in use: the upstream DNS servers are not answering", "check the internet connection and the upstream settings"
	case recentFallbacks >= fallbackWarnAnswers:
		return "warn", "fallback DNS in use: the upstream DNS servers left several queries of the last 5 minutes unanswered",
			"check the internet connection and the upstream settings"
	}
	for _, g := range groups {
		if g.Error != "" || (len(g.Upstreams) > 0 && healthy(g.Upstreams) == 0) {
			return "warn", fmt.Sprintf("the upstreams of group %s are not answering: their clients get SERVFAIL", strings.Join(g.Names, ", ")),
				"check the group's resolver (Clients & groups) and the internet connection"
		}
	}
	return "ok", "", ""
}

// listenersHealth evaluates the check "listeners": a role of the saved
// listeners that could not be bound and fell back to its environment or
// default value fails (until a start binds everything); another listener
// that could not be bound warns; a failed DoT, DoH or NTP listener counts
// only while the protocol is switched on.
func listenersHealth(li api.ListenerInfo, e settings.EncryptedDNS, ntpOn bool, saved []string) (status, msg, hint string) {
	if len(saved) > 0 {
		return "fail", strings.Join(saved, "; "),
			"change the listeners under System → Network (applied at the next start) or run picache listeners --reset on the host"
	}
	var roles []string
	for r, err := range li.Failed {
		if (r == "dot" && !e.DoT) || (r == "doh" && !e.DoH) || (r == "ntp" && !ntpOn) {
			continue
		}
		roles = append(roles, r+": "+err)
	}
	if len(roles) == 0 {
		return "ok", "", ""
	}
	sort.Strings(roles)
	return "warn", fmt.Sprint(roles), "free the port or change the PICACHE_*_LISTEN setting, then restart"
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
	st, msg, hint := listenersHealth(li, set.DNS.Encrypted, set.NTP.Enabled, a.ln.savedFailures())
	add("listeners", st, msg, hint)

	// Upstreams (+ clock guard, fallbacks)
	st, msg, hint = upstreamHealth(a.up.ClockGuard(), a.up.Stats(), a.up.FallbackStats(), a.up.FallbacksSince(time.Now().Add(-fallbackRecent)), a.up.GroupStats())
	add("upstreams", st, msg, hint)

	// DNSSEC (validate mode only)
	if st, msg, hint, show := a.dnssecCheck(ctx, set); show {
		add("dnssec", st, msg, hint)
	}

	// Filtering
	st, msg, hint = blocklistsHealth(set.Filter.Enabled, a.filter.Stats(), a.filter.EntryBudget())
	add("blocklists", st, msg, hint)

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

	// The certificate of the TLS listeners (only while one is bound)
	if a.webTLS != nil {
		if st, msg, hint, show := a.webTLS.health(time.Now()); show {
			add("tls", st, msg, hint)
		}
	}

	// Encrypted DNS (while DoT or DoH is on or plain DNS is off)
	if st, msg, hint, show := a.encryptedHealth(); show {
		add("encrypted-dns", st, msg, hint)
	}

	// DHCP server (only when it is enabled or available)
	if a.dhcp != nil {
		if st, msg, hint, show := a.dhcp.Health(); show {
			add("dhcp", st, msg, hint)
		}
	}

	// Host resources (only when a value could be read)
	if st, msg, hint, show := a.hostHealth(); show {
		add("host", st, msg, hint)
	}

	// Network check (the cached check of DNS → Network check)
	if a.network != nil {
		st, msg, hint := networkHealth(a.network.Check(ctx, netip.Addr{}))
		add("network", st, msg, hint)
	}

	// Log file and syslog (while one is configured)
	if a.appLog != nil {
		if st, msg, hint, show := loggingHealth(a.appLog.Sinks(), &a.logDrops, time.Now()); show {
			add("logging", st, msg, hint)
		}
	}

	// NTP server (while ntp.enabled)
	if set.NTP.Enabled && a.ntp != nil {
		st, msg, hint := ntpHealth(a.ntp.ClockState(), a.deployment() == dhcp.DeploymentDocker)
		add("ntp", st, msg, hint)
	}

	// Follower sync (while the mode is follower)
	if a.sync != nil {
		if st, msg, hint, show := a.sync.health(); show {
			add("sync", st, msg, hint)
		}
	}
	return h
}

// loggingHealth evaluates the check "logging" (present while a log file or
// syslog is configured): a sink that cannot write warns, as do records
// dropped within the last dropWindow (drops; the cumulative count stays in
// GET /system/log).
func loggingHealth(sinks []applog.SinkState, drops *dropTracker, now time.Time) (status, msg, hint string, show bool) {
	if len(sinks) == 0 {
		return "", "", "", false
	}
	var msgs []string
	for _, s := range sinks {
		recent := drops.recent(s, now)
		switch {
		case !s.OK && s.Kind == applog.SinkFile:
			msgs = append(msgs, fmt.Sprintf("the log file %s cannot be written: %s", s.Target, s.Error))
		case !s.OK:
			msgs = append(msgs, fmt.Sprintf("syslog %s is not reachable: %s", s.Target, s.Error))
		case recent > 0:
			msgs = append(msgs, fmt.Sprintf("%d log records were dropped", recent))
		}
	}
	if len(msgs) == 0 {
		return "ok", "", "", true
	}
	return "warn", strings.Join(msgs, "; "),
		"check PICACHE_LOG_FILE and PICACHE_LOG_SYSLOG; the log on stderr (journal, docker logs) continues", true
}

// dropWindow is how long the check "logging" warns after a sink dropped
// records.
const dropWindow = 10 * time.Minute

// dropTracker turns the cumulative dropped counts of the log sinks into
// recent drops: one transient loss (a syslog server rebooting) must not
// keep the check in warn until PiCache restarts.
type dropTracker struct {
	mu   sync.Mutex
	seen map[string]dropMark // kind + target
}

// dropMark is the drop state of one sink: count the last dropped count
// seen, base the count before the current episode of drops, grew when
// count last grew (zero: never).
type dropMark struct {
	count, base int64
	grew        time.Time
}

// recent returns the records s dropped in the current episode: since its
// count started to grow after a quiet dropWindow, while the last growth is
// at most dropWindow old (0 otherwise).
func (d *dropTracker) recent(s applog.SinkState, now time.Time) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen == nil {
		d.seen = map[string]dropMark{}
	}
	key := s.Kind + " " + s.Target
	m := d.seen[key]
	if s.Dropped > m.count {
		if m.grew.IsZero() || now.Sub(m.grew) > dropWindow {
			m.base = m.count // a new episode
		}
		m.count, m.grew = s.Dropped, now
		d.seen[key] = m
	}
	if m.grew.IsZero() || now.Sub(m.grew) > dropWindow {
		return 0
	}
	return m.count - m.base
}

// ntpHealth evaluates the check "ntp" (present while ntp.enabled): the
// host clock is not synchronised, or its state cannot be read (a unit
// without SystemCallFilter=adjtimex, a container).
func ntpHealth(c ntp.ClockState, docker bool) (status, msg, hint string) {
	switch {
	case c.Err != nil && docker:
		return "warn", fmt.Sprintf("the clock state cannot be read (%v): NTP clients get unsynchronised answers", c.Err),
			"the container may not read the clock state; the NTP server of the host is the better choice here"
	case c.Err != nil:
		return "warn", fmt.Sprintf("the clock state cannot be read (%v): update the unit files (run the one-line installer once)", c.Err),
			"the units of 0.15 allow reading the clock state (SystemCallFilter=adjtimex)"
	case !c.Synced:
		return "warn", "the host clock is not synchronised: NTP clients get unsynchronised answers",
			"check the time synchronisation of the host (timedatectl)"
	}
	return "ok", "", ""
}
