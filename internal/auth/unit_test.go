package auth

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/hustenreizjuengling/picache/internal/apperr"
)

func TestPasswordHash(t *testing.T) {
	h := hashPassword("correct horse battery")
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("PHC string %q", h)
	}
	if h == hashPassword("correct horse battery") {
		t.Fatal("salt must be random")
	}
	ok, rehash, err := verifyPassword(h, "correct horse battery")
	if !ok || rehash || err != nil {
		t.Fatalf("verify = %v %v %v", ok, rehash, err)
	}
	if ok, _, _ := verifyPassword(h, "correct horse batterY"); ok {
		t.Fatal("wrong password accepted")
	}

	// A hash with weaker parameters verifies and asks for a rehash.
	salt := []byte("0123456789abcdef")
	old := fmt.Sprintf("$argon2id$v=19$m=8192,t=1,p=1$%s$%s", phcB64.EncodeToString(salt),
		phcB64.EncodeToString(argon2.IDKey([]byte("pw"), salt, 1, 8192, 1, 32)))
	if ok, rehash, err := verifyPassword(old, "pw"); !ok || !rehash || err != nil {
		t.Fatalf("old params: %v %v %v", ok, rehash, err)
	}
}

func TestParsePHCRejects(t *testing.T) {
	salt, key := phcB64.EncodeToString(make([]byte, 16)), phcB64.EncodeToString(make([]byte, 32))
	for _, s := range []string{
		"",
		"$argon2i$v=19$m=19456,t=2,p=1$" + salt + "$" + key,
		"$argon2id$v=16$m=19456,t=2,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=19456,t=2$" + salt + "$" + key,
		"$argon2id$v=19$m=19456,t=2,p=1,x=1$" + salt + "$" + key,
		"$argon2id$v=19$m=4194304,t=2,p=1$" + salt + "$" + key, // 4 GiB
		"$argon2id$v=19$m=19456,t=1000,p=1$" + salt + "$" + key,
		"$argon2id$v=19$m=19456,t=2,p=0$" + salt + "$" + key,
		"$argon2id$v=19$m=19456,t=2,p=1$" + salt + "$" + phcB64.EncodeToString(make([]byte, 4)),
		"$argon2id$v=19$m=19456,t=2,p=1$!!$" + key,
	} {
		if _, _, _, err := parsePHC(s); err == nil {
			t.Errorf("parsePHC(%q) accepted", s)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		user string
		ok   bool
	}{
		{"admin", true}, {"a.b_c@d-e", true}, {"9lives", true},
		{"", false}, {".admin", false}, {"ad min", false}, {"ädmin", false}, {strings.Repeat("a", 65), false},
	} {
		if err := validateUsername(tc.user); (err == nil) != tc.ok {
			t.Errorf("validateUsername(%q) = %v", tc.user, err)
		}
	}
	for _, tc := range []struct {
		pw string
		ok bool
	}{
		{"0123456789", true}, {"ääääääääää", true}, {"012345678", false},
		{strings.Repeat("x", maxPasswordBytes+1), false}, {"0123456789\x00", false}, {"\xff\xfe01234567", false},
	} {
		if err := validatePassword("password", tc.pw); (err == nil) != tc.ok {
			t.Errorf("validatePassword(%q) = %v", tc.pw, err)
		}
	}
}

// RFC 6238 appendix B (SHA-1), truncated to 6 digits.
func TestTOTPVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, tc := range []struct {
		unix int64
		code string
	}{
		{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"},
		{1234567890, "005924"}, {2000000000, "279037"}, {20000000000, "353130"},
	} {
		if got := totpCode(secret, tc.unix/totpPeriod); got != tc.code {
			t.Errorf("t=%d: code %s, want %s", tc.unix, got, tc.code)
		}
	}
	now := time.Unix(1111111111, 0)
	step := now.Unix() / totpPeriod
	for _, tc := range []struct {
		step int64
		want int64
	}{{step - 1, step - 1}, {step, step}, {step + 1, step + 1}, {step - 2, -1}, {step + 2, -1}} {
		if got := totpMatch(secret, totpCode(secret, tc.step), now); got != tc.want {
			t.Errorf("match step %d = %d, want %d", tc.step, got, tc.want)
		}
	}
	for in, want := range map[string]bool{"123456": true, " 123 456 ": true, "12345": false, "12345a": false, "1234567": false} {
		if _, ok := normalizeTOTP(in); ok != want {
			t.Errorf("normalizeTOTP(%q) = %v", in, ok)
		}
	}
}

func TestThrottle(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c1, c2, u := clientThrottleKey("192.168.1.2"), clientThrottleKey("192.168.1.3"), userThrottleKey("Admin")

	for _, tc := range []struct {
		name    string
		failGap time.Duration // between failures
		fails   int
		check   time.Duration // after the last failure
		keys    []string
		locked  bool
	}{
		{"four failures", time.Second, 4, 0, []string{c1, u}, false},
		{"five failures lock client", time.Second, 5, 0, []string{c1}, true},
		{"client lockout lasts", time.Second, 5, lockoutDuration - time.Second, []string{c1}, true},
		{"five failures delay user from another client", time.Second, 5, 0, []string{c2, u}, true},
		{"user delay is short, never a lockout", time.Second, 5, userDelayBase, []string{c2, u}, false},
		{"other client and user unaffected", time.Second, 5, 0, []string{c2, userThrottleKey("bob")}, false},
		{"lockout expires", time.Second, 5, lockoutDuration, []string{c1, u}, false},
		{"client failures outside the window do not add up", 4 * time.Minute, 5, 0, []string{c1}, false},
		{"user failures a window apart do not add up", failureWindow, 5, 0, []string{c2, u}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := newThrottle()
			now := t0
			for i := range tc.fails {
				if i > 0 {
					now = now.Add(tc.failGap)
				}
				th.fail(now, c1, u)
			}
			err := th.allow(now.Add(tc.check), tc.keys...)
			if locked := apperr.KindOf(err) == apperr.KindTooMany; locked != tc.locked {
				t.Fatalf("locked = %v (err %v), want %v", locked, err, tc.locked)
			}
		})
	}

	t.Run("user delay doubles and is capped at 30 s", func(t *testing.T) {
		for n, want := range map[int]time.Duration{
			1: 0, 4: 0, 5: time.Second, 6: 2 * time.Second, 7: 4 * time.Second, 8: 8 * time.Second,
			9: 16 * time.Second, 10: 30 * time.Second, 11: 30 * time.Second, 1000: 30 * time.Second,
		} {
			if got := userDelay(n); got != want {
				t.Errorf("userDelay(%d) = %v, want %v", n, got, want)
			}
		}
		th := newThrottle()
		for range 100 {
			th.fail(t0, u)
		}
		if err := th.allow(t0.Add(userDelayMax-time.Second), u); apperr.KindOf(err) != apperr.KindTooMany {
			t.Fatalf("within the delay: %v", err)
		}
		if err := th.allow(t0.Add(userDelayMax), u); err != nil {
			t.Fatalf("after 30 s: %v", err)
		}
	})
	t.Run("success resets", func(t *testing.T) {
		th := newThrottle()
		for range 4 {
			th.fail(t0, c1, u)
		}
		th.succeed(c1, u)
		th.fail(t0, c1, u)
		if err := th.allow(t0, c1, u); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("global rate", func(t *testing.T) {
		th := newThrottle()
		for i := range globalAttempts {
			if err := th.allow(t0, clientThrottleKey(fmt.Sprintf("10.0.0.%d", i))); err != nil {
				t.Fatalf("attempt %d: %v", i, err)
			}
		}
		if err := th.allow(t0, c1); apperr.KindOf(err) != apperr.KindTooMany {
			t.Fatalf("global limit not enforced: %v", err)
		}
		if err := th.allow(t0.Add(time.Second), c1); err != nil {
			t.Fatalf("limit must refill: %v", err)
		}
	})
	t.Run("sweep", func(t *testing.T) {
		th := newThrottle()
		th.fail(t0, c1)
		th.sweep(t0.Add(failureWindow + time.Second))
		if len(th.fails) != 0 {
			t.Fatalf("%d records left", len(th.fails))
		}
	})
	if got := clientThrottleKey("2001:db8::1"); got != clientThrottleKey("2001:db8::ffff") || got != "c:2001:db8::/64" {
		t.Fatalf("IPv6 client key = %q", got)
	}
}

func TestAuditDetails(t *testing.T) {
	type input struct {
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"plain", map[string]any{"url": "https://x"}, `{"url":"https://x"}`},
		{"struct", input{Name: "nas", Password: "hunter2"}, `{"name":"nas","password":"[redacted]"}`},
		{"names normalised", map[string]any{"New_Password": "x", "setupToken": "y", "TOTP": 1, "api-key": "z"},
			`{"New_Password":"[redacted]","TOTP":"[redacted]","api-key":"[redacted]","setupToken":"[redacted]"}`},
		{"nested objects and arrays", map[string]any{"a": []any{map[string]any{"secret": map[string]any{"k": 1}}, "secret"}},
			`{"a":[{"secret":"[redacted]"},"secret"]}`},
		{"values named like secrets stay", map[string]any{"field": "password"}, `{"field":"password"}`},
		{"PEM members", map[string]any{"keyPem": "-----BEGIN PRIVATE KEY-----", "cert_pem": "x", "subject": "CN=a"},
			`{"cert_pem":"[redacted]","keyPem":"[redacted]","subject":"CN=a"}`},
		{"top-level string", "token", `"token"`},
		{"unmarshalable", map[string]any{"f": func() {}}, `{"error":"details could not be recorded"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := auditDetails(tc.in); got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
	long := auditDetails(map[string]string{"v": strings.Repeat("ä", 5000)})
	if len(long) > auditMaxDetails || !strings.HasSuffix(long, "…") {
		t.Fatalf("truncation: len %d", len(long))
	}
}
