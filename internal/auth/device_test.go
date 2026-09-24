package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

// longPassword fails without an argon2id computation (like an attacker's
// cheapest attempt), which keeps the simulated attacks fast.
var longPassword = strings.Repeat("x", maxPasswordBytes+1)

// SEC-05 (residual): a host with several LAN addresses that keeps failing
// sign-ins for the admin's username keeps the username delay running for as
// long as it goes on. A browser that signed in before (device cookie) and a
// signed-in session are throttled by their own keys, so the attack cannot
// keep the owner out of either.
func TestSustainedUsernameAttackDoesNotBlockKnownDevice(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	owner := ReqMeta{IP: "192.168.1.50"}
	first, err := e.a.Login(ctx, "admin", testPassword, "", owner)
	if err != nil {
		t.Fatal(err)
	}
	if first.Device == "" {
		t.Fatal("a sign-in must issue a device cookie")
	}
	device := first.Device
	session := &Principal{UserID: first.UserID, Username: "admin", SessionID: first.ID, Scope: ScopeAdmin, IP: owner.IP}

	const (
		addrs = 16
		step  = 100 * time.Millisecond
	)
	next, attackerFails := 0, 0
	attack := func() {
		// One attempt per tick, from the next address whose client key is
		// not locked, the moment the username delay allows one.
		for range addrs {
			ip := fmt.Sprintf("192.168.1.%d", 100+next%addrs)
			next++
			_, err := e.a.Login(ctx, "admin", longPassword, "", ReqMeta{IP: ip})
			if apperr.KindOf(err) == apperr.KindUnauthorized {
				attackerFails++
				return
			}
			if ae, ok := apperr.As(err); ok && !strings.Contains(ae.Message, "minute") {
				return // the username delay holds everyone without a device cookie back
			}
		}
	}
	var deviceTries, deviceOK, newTries, newOK, confirmTries, confirmOK int
	for tick := range int(30 * time.Minute / step) {
		attack()
		if tick > int(5*time.Minute/step) && tick%int(30*time.Second/step) == 0 {
			e.clock.Advance(step / 2) // right after the attacker's attempt
			// A browser without a device cookie is what the attack holds up
			// (tried first: the owner's success below resets the delay).
			newTries++
			if _, err := e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: "192.168.1.51"}); err == nil {
				newOK++
			}
			deviceTries++
			s, err := e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: owner.IP, Devices: []string{device}})
			if err == nil {
				deviceOK++
				device = s.Device // the browser stores the renewed cookie
			} else {
				t.Logf("known device refused at %v: %v", time.Duration(tick)*step, err)
			}
			confirmTries++
			if err := e.a.ConfirmPassword(ctx, session, testPassword); err == nil {
				confirmOK++
			} else {
				t.Logf("session confirmation refused at %v: %v", time.Duration(tick)*step, err)
			}
			e.clock.Advance(step / 2)
			continue
		}
		e.clock.Advance(step)
	}
	t.Logf("attacker: %d failures from %d addresses; known device %d/%d, new browser %d/%d, session confirmations %d/%d",
		attackerFails, addrs, deviceOK, deviceTries, newOK, newTries, confirmOK, confirmTries)
	if newOK*4 > newTries {
		t.Fatalf("the simulated attack is too weak: a browser without a device cookie got in %d of %d times", newOK, newTries)
	}
	if deviceOK != deviceTries {
		t.Fatalf("a browser that signed in before must not be held up: %d of %d sign-ins succeeded", deviceOK, deviceTries)
	}
	if confirmOK != confirmTries {
		t.Fatalf("a signed-in session must not be held up: %d of %d confirmations succeeded", confirmOK, confirmTries)
	}
}

// Only a valid, unexpired device cookie issued for the username counts; a
// device is locked like a client after 5 failures, and its failures count
// for the username too.
func TestDeviceCookieValidation(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s, err := e.a.Login(ctx, "admin", testPassword, "", meta)
	if err != nil {
		t.Fatal(err)
	}
	dev := s.Device
	if d, ok := e.a.knownDevice([]string{dev}, "ADMIN"); !ok || d.userID != s.UserID {
		t.Fatalf("own device cookie not recognised (username case-insensitive): %+v %v", d, ok)
	}
	mid := len(dev) / 2 // a base64 character that carries 6 bits of the sealed data
	flip := byte('A')
	if dev[mid] == 'A' {
		flip = 'B'
	}
	tampered := dev[:mid] + string(flip) + dev[mid+1:]
	for name, v := range map[string]string{
		"tampered":     tampered,
		"garbage":      "v1:" + strings.Repeat("A", 100),
		"oversized":    dev + strings.Repeat("A", maxDeviceCookie),
		"session":      s.Token,
		"empty":        "",
		"other secret": mustSeal(t, e, "1\n"+strings.Repeat("ab", 16)+"\n1\n0\nadmin", "picache/auth/totp/1"),
	} {
		if _, ok := e.a.knownDevice([]string{v}, "admin"); ok {
			t.Errorf("%s device cookie accepted", name)
		}
	}
	if _, ok := e.a.knownDevice([]string{dev}, "someone"); ok {
		t.Error("a device cookie must only count for its own username")
	}
	if _, ok := e.a.knownDevice([]string{"junk", dev}, "admin"); !ok {
		t.Error("a valid device cookie after an invalid one must count")
	}

	// A copied device cookie gives no more than 5 guesses per 15 minutes,
	// whatever the number of addresses, and counts for the username.
	for i := range maxFailures {
		_, err := e.a.Login(ctx, "admin", "wrong password", "", ReqMeta{IP: fmt.Sprintf("10.1.0.%d", i+1), Devices: []string{dev}})
		wantKind(t, err, apperr.KindUnauthorized)
	}
	_, err = e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: "10.1.0.99", Devices: []string{dev}})
	if ae, ok := apperr.As(err); !ok || ae.Kind != apperr.KindTooMany || !strings.Contains(ae.Message, "minute") {
		t.Fatalf("locked device: %v", err)
	}
	_, err = e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: "10.1.0.98"})
	wantKind(t, err, apperr.KindTooMany) // the username delay started too
	e.clock.Advance(userDelayBase)
	s2, err := e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: "10.1.0.98"})
	if err != nil {
		t.Fatalf("another browser after the username delay: %v", err)
	}
	if s2.Device == "" || s2.Device == dev {
		t.Fatal("every sign-in must issue a new device cookie")
	}

	e.clock.Advance(lockoutDuration)
	if _, err := e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: "10.1.0.99", Devices: []string{dev}}); err != nil {
		t.Fatalf("device after its lockout: %v", err)
	}

	e.clock.Advance(deviceTTL)
	if _, ok := e.a.knownDevice([]string{dev}, "admin"); ok {
		t.Fatal("an expired device cookie must not count")
	}
}

func mustSeal(t *testing.T, e *testEnv, payload, aad string) string {
	t.Helper()
	v, err := e.a.box.Seal([]byte(payload), aad)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The device cookie: named like the session cookie, HttpOnly, SameSite=Strict,
// long-lived; read from both names.
func TestDeviceCookie(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	s := e.login(t)
	for _, tc := range []struct {
		tls, secure bool
		name        string
	}{{false, false, DeviceCookie}, {false, true, DeviceCookie}, {true, true, SecureDeviceCookie}} {
		c := e.a.DeviceCookieFor(s, tc.tls, tc.secure)
		if c.Name != tc.name || c.Value != s.Device || c.Secure != tc.secure || !c.HttpOnly ||
			c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || c.MaxAge < int((179*24*time.Hour).Seconds()) {
			t.Fatalf("tls=%v secure=%v: cookie %+v", tc.tls, tc.secure, c)
		}
	}
	if e.a.DeviceCookieFor(&Session{}, true, true) != nil {
		t.Fatal("no cookie without a device value")
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
	r.AddCookie(&http.Cookie{Name: DeviceCookie, Value: "plain"})
	r.AddCookie(&http.Cookie{Name: SecureDeviceCookie, Value: "host"})
	for range 6 {
		r.AddCookie(&http.Cookie{Name: DeviceCookie, Value: "more"})
	}
	if got := DeviceCookieValues(r); len(got) != maxDeviceCookies || got[0] != "host" || got[1] != "plain" {
		t.Fatalf("device cookie values = %v", got)
	}
}

// Password confirmations of a session are throttled by the session (and the
// client), not by the username: they neither wait for the username delay
// nor start it, and a session that guesses wrong is locked from any address.
func TestConfirmPasswordUsesSessionKey(t *testing.T) {
	e := newEnv(t)
	e.withAdmin(t)
	ctx := context.Background()
	s := e.login(t)
	p := &Principal{UserID: s.UserID, Username: "admin", SessionID: s.ID, Scope: ScopeAdmin}
	for i := range maxFailures {
		p.IP = fmt.Sprintf("10.2.0.%d", i+1)
		if err := e.a.ConfirmPassword(ctx, p, "wrong password"); apperr.KindOf(err) != apperr.KindUnauthorized {
			t.Fatalf("confirmation %d: %v", i, err)
		}
	}
	p.IP = "10.2.0.99"
	wantKind(t, e.a.ConfirmPassword(ctx, p, testPassword), apperr.KindTooMany)
	// Sign-ins for the username are not delayed by the session's failures.
	if _, err := e.a.Login(ctx, "admin", testPassword, "", ReqMeta{IP: "10.2.0.50"}); err != nil {
		t.Fatalf("sign-in after wrong confirmations of another session: %v", err)
	}
	// Another session is not affected either.
	s2 := e.login(t)
	p2 := &Principal{UserID: s2.UserID, Username: "admin", SessionID: s2.ID, Scope: ScopeAdmin, IP: "10.2.0.60"}
	if err := e.a.ConfirmPassword(ctx, p2, testPassword); err != nil {
		t.Fatalf("another session: %v", err)
	}
	e.clock.Advance(lockoutDuration)
	if err := e.a.ConfirmPassword(ctx, p, testPassword); err != nil {
		t.Fatalf("after the lockout: %v", err)
	}
}
