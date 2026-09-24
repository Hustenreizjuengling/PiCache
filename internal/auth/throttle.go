package auth

import (
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Login throttling (docs/ARCHITECTURE.md 6.1).
const (
	maxFailures     = 5
	failureWindow   = 15 * time.Minute
	lockoutDuration = 15 * time.Minute
	globalAttempts  = 10 // per second, all clients together
	// maxThrottleKeys bounds the failure table. The global limit admits at
	// most ~9 000 attempts per failure window, each touching two keys, so
	// the bound is not reached in practice; beyond it failures are not
	// recorded (the global limit still applies).
	maxThrottleKeys = 32768
)

type failRecord struct {
	count       int       // failures since first
	first       time.Time // start of the current window
	lockedUntil time.Time
}

// throttle tracks failed attempts per client key ("c:<prefix>") and per
// username ("u:<name>") and enforces the global attempt rate.
type throttle struct {
	mu     sync.Mutex
	fails  map[string]*failRecord
	global *rate.Limiter
}

func newThrottle() *throttle {
	return &throttle{fails: map[string]*failRecord{}, global: rate.NewLimiter(globalAttempts, globalAttempts)}
}

// clientThrottleKey returns the throttle key of a client IP (/32 or /64).
func clientThrottleKey(ip string) string {
	addr := netutil.AddrFromRemote(ip)
	if !addr.IsValid() {
		return "c:unknown"
	}
	return "c:" + netutil.ClientKey(addr).String()
}

// userThrottleKey returns the throttle key of a (possibly unknown) username.
func userThrottleKey(username string) string {
	u := strings.ToLower(username)
	if len(u) > maxUsernameLen {
		u = u[:maxUsernameLen]
	}
	return "u:" + u
}

// allow reports an apperr.TooMany error if any key is locked out or the
// global attempt rate is exceeded. Locked-out callers do not consume global
// tokens, so a locked attacker cannot starve other users.
func (t *throttle) allow(now time.Time, keys ...string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var until time.Time
	for _, k := range keys {
		if r := t.fails[k]; r != nil && r.lockedUntil.After(until) {
			until = r.lockedUntil
		}
	}
	if until.After(now) {
		mins := int(math.Ceil(until.Sub(now).Minutes()))
		return apperr.TooMany("too many failed attempts; try again in %d minute(s)", mins)
	}
	if !t.global.AllowN(now, 1) {
		return apperr.TooMany("too many login attempts; try again in a few seconds")
	}
	return nil
}

// fail records a failed attempt for every key; the fifth failure within the
// window locks the key for lockoutDuration. It returns the keys that became
// locked by this failure.
func (t *throttle) fail(now time.Time, keys ...string) (locked []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		r := t.fails[k]
		if r == nil {
			if len(t.fails) >= maxThrottleKeys {
				t.sweepLocked(now)
				if len(t.fails) >= maxThrottleKeys {
					continue
				}
			}
			r = &failRecord{first: now}
			t.fails[k] = r
		}
		if now.Sub(r.first) > failureWindow {
			r.count, r.first = 0, now
		}
		r.count++
		if r.count >= maxFailures {
			r.lockedUntil = now.Add(lockoutDuration)
			r.count, r.first = 0, now
			locked = append(locked, k)
		}
	}
	return locked
}

// succeed forgets the failures of keys after a successful attempt.
func (t *throttle) succeed(keys ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, k := range keys {
		delete(t.fails, k)
	}
}

// sweep removes records whose window and lockout have passed.
func (t *throttle) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweepLocked(now)
}

func (t *throttle) sweepLocked(now time.Time) {
	for k, r := range t.fails {
		if !r.lockedUntil.After(now) && now.Sub(r.first) > failureWindow {
			delete(t.fails, k)
		}
	}
}

// recordFailure counts a failed attempt and logs new lockouts. Usernames are
// never logged: users sometimes type their password into that field.
func (a *Service) recordFailure(keys ...string) {
	for _, k := range a.throttle.fail(a.now(), keys...) {
		if client, ok := strings.CutPrefix(k, "c:"); ok {
			a.log.Warn("client locked out after repeated failed sign-in attempts",
				slog.String("client", client), slog.Duration("for", lockoutDuration))
		} else {
			a.log.Warn("a username was locked out after repeated failed sign-in attempts",
				slog.Duration("for", lockoutDuration))
		}
	}
}
