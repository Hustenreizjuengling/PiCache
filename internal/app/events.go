package app

import (
	"fmt"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/api"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/notify"
)

// Notification events of the app (docs/ARCHITECTURE.md 15.1). Health checks
// and the cache store are debounced here; the notifier only delivers.

// healthWatch turns health evaluations into health.* messages. A check is
// reported when it had the same problem status (warn or fail) in two
// consecutive evaluations and that status was not reported yet; a reported
// check that is ok again, or no longer evaluated (a check that only exists
// while there is a problem, or a feature that was switched off), is
// reported as recovered. It is used by the health loop only.
type healthWatch struct {
	checks map[string]*checkWatch
}

type checkWatch struct {
	status   string // status of the last evaluation
	count    int    // consecutive evaluations with that status
	notified string // "", "warn" or "fail": the problem reported last
}

func (h *healthWatch) observe(checks []api.HealthCheck) []notify.Message {
	if h.checks == nil {
		h.checks = map[string]*checkWatch{}
	}
	var out []notify.Message
	seen := map[string]bool{}
	for _, c := range checks {
		seen[c.Name] = true
		w := h.checks[c.Name]
		if w == nil {
			w = &checkWatch{}
			h.checks[c.Name] = w
		}
		if c.Status == w.status {
			w.count++
		} else {
			w.status, w.count = c.Status, 1
		}
		switch {
		case c.Status == "ok":
			if w.notified != "" {
				w.notified = ""
				out = append(out, healthRecovered(c.Name))
			}
		case (c.Status == "warn" || c.Status == "fail") && w.count >= 2 && w.notified != c.Status:
			w.notified = c.Status
			out = append(out, healthProblem(c))
		}
	}
	var gone []string
	for name, w := range h.checks {
		if !seen[name] {
			if w.notified != "" {
				gone = append(gone, name)
			}
			delete(h.checks, name)
		}
	}
	slices.Sort(gone)
	for _, name := range gone {
		out = append(out, healthRecovered(name))
	}
	return out
}

func healthProblem(c api.HealthCheck) notify.Message {
	m := notify.Message{Event: notify.EventHealthFailed, Title: "Health check failed: " + c.Name, Message: c.Message}
	if c.Status == "warn" {
		m.Event, m.Title = notify.EventHealthWarning, "Health check warning: "+c.Name
	}
	if m.Message == "" {
		m.Message = "The health check " + c.Name + " reports " + c.Status + "."
	}
	if c.Hint != "" {
		m.Message += "\nHint: " + c.Hint
	}
	return m
}

func healthRecovered(name string) notify.Message {
	return notify.Message{Event: notify.EventHealthRecovered, Title: "Health check recovered: " + name,
		Message: "The health check " + name + " is OK again."}
}

// storeOfflineDelay is how long the active cache store must be offline
// before storage.offline is sent.
const storeOfflineDelay = 2 * time.Minute

// storeWatch turns the store state into storage.offline (offline for
// storeOfflineDelay) and storage.online (back after a reported offline).
// It is used by the store loop only.
type storeWatch struct {
	offlineSince time.Time
	notified     bool
}

func (w *storeWatch) observe(online bool, name, reason string, now time.Time) (notify.Message, bool) {
	if online {
		w.offlineSince = time.Time{}
		if !w.notified {
			return notify.Message{}, false
		}
		w.notified = false
		return notify.Message{Event: notify.EventStorageOnline, Title: "Cache storage online: " + name,
			Message: fmt.Sprintf("The cache storage %q is online again; downloads are cached again.", name)}, true
	}
	if w.offlineSince.IsZero() {
		w.offlineSince = now
	}
	if w.notified || now.Sub(w.offlineSince) < storeOfflineDelay {
		return notify.Message{}, false
	}
	w.notified = true
	if reason == "" {
		reason = "unknown reason"
	}
	return notify.Message{Event: notify.EventStorageOffline, Title: "Cache storage offline: " + name,
		Message: fmt.Sprintf("The cache storage %q has been offline for %d minutes: %s. Downloads are passed through "+
			"uncached until it is back.", name, int(storeOfflineDelay/time.Minute), reason)}, true
}

// lockoutMessage describes a new sign-in lockout (security.lockout). It
// names the client, never the username or the session.
func lockoutMessage(l auth.Lockout) notify.Message {
	client := l.Client
	if client == "" {
		client = "an unknown address"
	}
	mins := int(l.For / time.Minute)
	m := notify.Message{Event: notify.EventSecurityLockout}
	switch l.Kind {
	case auth.LockoutClient:
		who := "The client " + l.Client
		m.Title = "Sign-in lockout: " + l.Client
		if l.Client == "" {
			who, m.Title = "A client with an unknown address", "Sign-in lockout"
		}
		m.Message = fmt.Sprintf("%s was locked out for %d minutes after 5 failed sign-in attempts or password confirmations.", who, mins)
	case auth.LockoutDevice:
		m.Title = "Sign-in lockout: a known browser"
		m.Message = fmt.Sprintf("A browser that signed in before was locked out for %d minutes after 5 failed sign-in attempts "+
			"(the last from %s). Its device cookie may have been copied.", mins, client)
	case auth.LockoutSession:
		m.Title = "Password confirmations locked"
		m.Message = fmt.Sprintf("Password confirmations of a signed-in session were locked for %d minutes after 5 wrong passwords "+
			"(the last from %s).", mins, client)
	default:
		m.Title = "Sign-ins for a user name delayed"
		m.Message = fmt.Sprintf("Sign-ins for a user name from browsers that have not signed in before are delayed after 5 failed "+
			"attempts (the last from %s). The delay grows to at most 30 seconds per attempt; it is never a lockout.", client)
	}
	return m
}
