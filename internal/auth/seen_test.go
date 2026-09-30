package auth

import (
	"context"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// failSessionWrites makes every update of auth_sessions fail as on a full
// data disk; the returned function lets them succeed again.
func failSessionWrites(t *testing.T, e *testEnv) func() {
	t.Helper()
	if _, err := e.d.W.Exec(`CREATE TRIGGER full_sessions BEFORE UPDATE ON auth_sessions BEGIN SELECT RAISE(FAIL, 'database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if _, err := e.d.W.Exec(`DROP TRIGGER full_sessions`); err != nil {
			t.Fatal(err)
		}
	}
}

func storedLastSeen(t *testing.T, e *testEnv, id string) time.Time {
	t.Helper()
	var ms int64
	if err := e.d.R.QueryRow(`SELECT last_seen FROM auth_sessions WHERE id = ?`, id).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	return db.Time(ms)
}

// R1: while last_seen cannot be written (a full data disk), a signed-in
// browser that keeps using its session stays signed in: the idle limit
// counts from the last use kept in memory, for requests and for the check
// of the event streams (Valid). The absolute limit still counts from the
// stored creation time, and a session left idle still expires. Once
// writes work again, the next request writes the last use back and the
// memory forgets the session. Before, the session expired after the idle
// limit however often it was used.
func TestSessionKeptWhileLastSeenCannotBeWritten(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	if _, err := e.set.Update(ctx, func(a *settings.All) error {
		a.Web.SessionIdleMinutes, a.Web.SessionMaxHours = 10, 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s := e.login(t)
	p := &Principal{UserID: s.UserID, Username: "admin", SessionID: s.ID, Scope: ScopeAdmin}
	created := e.clock.Now()
	allow := failSessionWrites(t, e)

	// A request every 4 minutes for 40 minutes: four idle limits.
	for range 10 {
		e.clock.Advance(4 * time.Minute)
		if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
			t.Fatalf("after %v: %v", e.clock.Now().Sub(created), err)
		}
		if !e.a.Valid(ctx, p) {
			t.Fatalf("after %v: the event stream check ended the session", e.clock.Now().Sub(created))
		}
	}
	if got := storedLastSeen(t, e, s.ID); !got.Equal(created) {
		t.Fatalf("test setup: last_seen was written (%v)", got)
	}
	if list, err := e.a.Sessions(ctx, p); err != nil || len(list) != 1 || !list[0].LastSeen.Equal(e.clock.Now()) {
		t.Fatalf("sessions %+v %v", list, err)
	}

	// Writes work again: the next request writes the last use back.
	allow()
	e.clock.Advance(4 * time.Minute)
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
		t.Fatal(err)
	}
	if got := storedLastSeen(t, e, s.ID); !got.Equal(e.clock.Now()) || e.a.seen.len() != 0 {
		t.Fatalf("last_seen %v, %d kept in memory", got, e.a.seen.len())
	}

	// The absolute limit (1 h) counts from the stored creation time.
	allow = failSessionWrites(t, e)
	for e.clock.Now().Before(created.Add(time.Hour - 4*time.Minute)) {
		e.clock.Advance(4 * time.Minute)
		if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
			t.Fatalf("after %v: %v", e.clock.Now().Sub(created), err)
		}
	}
	e.clock.Advance(4 * time.Minute)
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); apperr.KindOf(err) != apperr.KindUnauthorized {
		t.Fatalf("past the absolute limit: %v", err)
	}
	if e.a.Valid(ctx, p) || e.a.seen.len() != 0 {
		t.Fatalf("past the absolute limit: valid, or %d kept in memory", e.a.seen.len())
	}
	allow()

	// A session left idle expires, and the memory forgets it.
	s = e.login(t)
	p.SessionID = s.ID
	allow = failSessionWrites(t, e)
	defer allow()
	e.clock.Advance(4 * time.Minute)
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil || e.a.seen.len() != 1 {
		t.Fatalf("%v, %d kept in memory", err, e.a.seen.len())
	}
	e.clock.Advance(10 * time.Minute)
	if e.a.Valid(ctx, p) {
		t.Fatal("an idle session is valid")
	}
	if _, err := e.a.Authenticate(cookieRequest(s.Token)); apperr.KindOf(err) != apperr.KindUnauthorized || e.a.seen.len() != 0 {
		t.Fatalf("idle session: %v, %d kept in memory", err, e.a.seen.len())
	}
}

// The memory holds only sessions that are alive: signing out and revoking
// drop the entry, entries older than the idle limit are dropped when
// another is added, and it never holds more than maxSeenMemory entries.
func TestSeenMemoryBounded(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	allow := failSessionWrites(t, e)
	defer allow()
	s1, s2 := e.login(t), e.login(t)
	e.clock.Advance(2 * time.Minute)
	for _, s := range []*Session{s1, s2} {
		if _, err := e.a.Authenticate(cookieRequest(s.Token)); err != nil {
			t.Fatal(err)
		}
	}
	if n := e.a.seen.len(); n != 2 {
		t.Fatalf("%d kept, want 2", n)
	}
	if err := e.a.Logout(ctx, s1.Token); err != nil {
		t.Fatal(err)
	}
	p := &Principal{UserID: s2.UserID, SessionID: s2.ID}
	if err := e.a.RevokeSession(ctx, p, s2.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.a.seen.len(); n != 0 {
		t.Fatalf("%d kept after sign-out and revocation", n)
	}

	var m seenMemory
	start := time.Unix(0, 0)
	for i := range maxSeenMemory + 10 {
		m.set(string(rune('a'+i%26))+time.Duration(i).String(), start, time.Hour)
	}
	if n := m.len(); n != maxSeenMemory {
		t.Fatalf("%d kept, want at most %d", n, maxSeenMemory)
	}
	m.set("late", start.Add(time.Hour), time.Hour)
	if n := m.len(); n != 1 || m.get("late").IsZero() {
		t.Fatalf("%d kept after the idle limit, want only the new entry", n)
	}
}
