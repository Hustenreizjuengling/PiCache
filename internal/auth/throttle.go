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
//
// Client keys ("c:<prefix>"), device keys ("d:<id>", device.go) and session
// keys ("s:<id>", password confirmations) are locked out hard: the fifth
// failure within failureWindow blocks the key for lockoutDuration. Username
// keys ("u:<name>") are only slowed down: from the fifth failure on, the
// next attempt for the username has to wait userDelayBase·2^(failures−5), at
// most userDelayMax, and the count is forgotten after a successful sign-in
// or failureWindow without failures. Any LAN host can fail sign-ins for the
// admin's username, so a hard lockout per username would let it keep the
// owner out indefinitely; the delay only limits the guessing rate. A host
// with several addresses can still keep the delay running, so a browser
// that signed in before (device key) and a signed-in session (session key)
// are not subject to it.
const (
	maxFailures     = 5
	failureWindow   = 15 * time.Minute
	lockoutDuration = 15 * time.Minute
	userDelayBase   = time.Second
	userDelayMax    = 30 * time.Second
	globalAttempts  = 10 // per second, all clients together
	// maxThrottleKeys bounds the failure table. The global limit admits at
	// most ~9 000 attempts per failure window, each touching at most three
	// keys, so the bound is not reached in practice; beyond it failures are
	// not recorded (the global limit still applies).
	maxThrottleKeys = 32768
)

type failRecord struct {
	count       int       // failures in the current window
	first       time.Time // start of the current window (client keys)
	last        time.Time // latest failure (username keys: the window restarts with each)
	lockedUntil time.Time // client keys: lockout; username keys: delay
}

// throttle tracks failed attempts per client key and per username and
// enforces the global attempt rate.
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

// sessionThrottleKey returns the throttle key of a session's password
// confirmations.
func sessionThrottleKey(id string) string { return "s:" + id }

func isUserKey(k string) bool { return strings.HasPrefix(k, "u:") }

// userDelay is the wait after the n-th failure for a username (n ≥ maxFailures).
func userDelay(n int) time.Duration {
	shift := n - maxFailures
	if shift < 0 {
		return 0
	}
	if shift >= 6 { // 2^6 s > userDelayMax
		return userDelayMax
	}
	return min(userDelayMax, userDelayBase<<shift)
}

// allow reports an apperr.TooMany error if any key is locked out or delayed,
// or the global attempt rate is exceeded. Blocked callers do not consume
// global tokens, so a locked attacker cannot starve other users.
func (t *throttle) allow(now time.Time, keys ...string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	var until time.Time
	for _, k := range keys {
		if r := t.fails[k]; r != nil && r.lockedUntil.After(until) {
			until = r.lockedUntil
		}
	}
	if wait := until.Sub(now); wait > 0 {
		if wait < time.Minute {
			return apperr.TooMany("too many failed attempts; try again in %d second(s)", int(math.Ceil(wait.Seconds())))
		}
		return apperr.TooMany("too many failed attempts; try again in %d minute(s)", int(math.Ceil(wait.Minutes())))
	}
	if !t.global.AllowN(now, 1) {
		return apperr.TooMany("too many login attempts; try again in a few seconds")
	}
	return nil
}

// fail records a failed attempt for every key: the fifth failure of a client
// key within the window locks it for lockoutDuration; from the fifth failure
// of a username key on, the username is delayed (userDelay). It returns the
// keys that became locked or started to be delayed by this failure.
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
			r = &failRecord{first: now, last: now}
			t.fails[k] = r
		}
		user := isUserKey(k)
		switch {
		case user && now.Sub(r.last) >= failureWindow:
			r.count = 0
		case !user && now.Sub(r.first) > failureWindow:
			r.count, r.first = 0, now
		}
		r.count++
		r.last = now
		if r.count < maxFailures {
			continue
		}
		if user {
			r.lockedUntil = now.Add(userDelay(r.count))
			if r.count == maxFailures {
				locked = append(locked, k)
			}
			continue
		}
		r.lockedUntil = now.Add(lockoutDuration)
		r.count, r.first = 0, now
		locked = append(locked, k)
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
		if !r.lockedUntil.After(now) && now.Sub(r.last) > failureWindow {
			delete(t.fails, k)
		}
	}
}

// recordFailure counts a failed attempt and logs new lockouts and delays.
// Usernames are never logged: users sometimes type their password into that
// field.
func (a *Service) recordFailure(keys ...string) {
	for _, k := range a.throttle.fail(a.now(), keys...) {
		switch {
		case strings.HasPrefix(k, "c:"):
			a.log.Warn("client locked out after repeated failed sign-in attempts",
				slog.String("client", strings.TrimPrefix(k, "c:")), slog.Duration("for", lockoutDuration))
		case strings.HasPrefix(k, "d:"):
			a.log.Warn("a browser that signed in before is locked out after repeated failed sign-in attempts "+
				"(its device cookie may have been copied)", slog.Duration("for", lockoutDuration))
		case strings.HasPrefix(k, "s:"):
			a.log.Warn("password confirmations of a session are locked after repeated wrong passwords",
				slog.Duration("for", lockoutDuration))
		default:
			a.log.Warn("sign-in attempts for a username from browsers that have not signed in before are delayed "+
				"after repeated failures (up to 30 s per attempt, never a lockout)", slog.Duration("delay", userDelay(maxFailures)))
		}
	}
}
