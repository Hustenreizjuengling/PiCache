package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Known devices ("device cookies", OWASP). The username delay (throttle.go)
// cannot tell the owner from someone who fails sign-ins for the same
// username from many LAN addresses, so such a host could keep the delay
// running and every sign-in waiting. Therefore each successful sign-in (and
// the setup) gives the browser a device cookie: the account's id and name, a
// random device id and the issue time, sealed with the master key
// (secrets.Box, AAD deviceAAD), so it cannot be forged or altered. A sign-in
// that presents a valid device cookie issued for the username it tries is
// throttled by the device ("d:<id>", locked like a client: 5 failures →
// 15 min) instead of by the username delay. Failures from other hosts then
// cannot keep that browser out, and a stolen device cookie gives no more
// guesses than a single client. The cookie grants nothing else: the password
// (and TOTP) are still required. It is kept on sign-out and renewed with a
// new id at every sign-in.
const (
	DeviceCookie       = "picache_device"
	SecureDeviceCookie = "__Host-picache_device" // requests over TLS (see cookies.go)

	deviceTTL        = 180 * 24 * time.Hour
	deviceAAD        = "picache/auth/device"
	deviceVersion    = "1"
	deviceIDBytes    = 16
	maxDeviceCookie  = 512 // bytes of one cookie value that are looked at
	maxDeviceCookies = 4   // cookie values of one request that are looked at
)

// device is the content of a valid device cookie.
type device struct {
	id       string // hex, deviceIDBytes random bytes
	userID   int64
	username string
	issued   time.Time
}

func deviceThrottleKey(id string) string { return "d:" + id }

// issueDevice returns a new device cookie value for the account ("" if no
// master key is available).
func (a *Service) issueDevice(userID int64, username string) string {
	if a.box == nil {
		return ""
	}
	raw := make([]byte, deviceIDBytes)
	rand.Read(raw)
	payload := strings.Join([]string{deviceVersion, hex.EncodeToString(raw), strconv.FormatInt(userID, 10),
		strconv.FormatInt(a.now().Unix(), 10), username}, "\n")
	v, err := a.box.Seal([]byte(payload), deviceAAD)
	if err != nil {
		return ""
	}
	return v
}

// openDevice checks a device cookie value: sealed with this instance's
// master key and not older than deviceTTL.
func (a *Service) openDevice(v string) (device, bool) {
	if a.box == nil || v == "" || len(v) > maxDeviceCookie {
		return device{}, false
	}
	pt, err := a.box.Open(v, deviceAAD)
	if err != nil {
		return device{}, false
	}
	f := strings.SplitN(string(pt), "\n", 5)
	if len(f) != 5 || f[0] != deviceVersion || len(f[1]) != 2*deviceIDBytes {
		return device{}, false
	}
	uid, err1 := strconv.ParseInt(f[2], 10, 64)
	issued, err2 := strconv.ParseInt(f[3], 10, 64)
	if err1 != nil || err2 != nil {
		return device{}, false
	}
	d := device{id: f[1], userID: uid, username: f[4], issued: time.Unix(issued, 0)}
	if a.now().Sub(d.issued) >= deviceTTL {
		return device{}, false
	}
	return d, true
}

// knownDevice returns the first valid device cookie among values that was
// issued for username (compared like the username throttle key).
func (a *Service) knownDevice(values []string, username string) (device, bool) {
	ukey := userThrottleKey(username)
	for _, v := range values {
		if d, ok := a.openDevice(v); ok && userThrottleKey(d.username) == ukey {
			return d, true
		}
	}
	return device{}, false
}

// DeviceCookieValues returns the device cookie values of a request, the
// __Host- one first (at most maxDeviceCookies).
func DeviceCookieValues(r *http.Request) []string {
	var out []string
	for _, n := range []string{SecureDeviceCookie, DeviceCookie} {
		for _, c := range r.CookiesNamed(n) {
			if c.Value != "" && len(out) < maxDeviceCookies {
				out = append(out, c.Value)
			}
		}
	}
	return out
}

// DeviceCookieFor builds the device cookie of a new session (nil if none
// was issued). tls and secure as for Cookie.
func (a *Service) DeviceCookieFor(s *Session, tls, secure bool) *http.Cookie {
	if s == nil || s.Device == "" {
		return nil
	}
	name := DeviceCookie
	if tls {
		name = SecureDeviceCookie
	}
	return &http.Cookie{
		Name:     name,
		Value:    s.Device,
		Path:     "/",
		Expires:  a.now().Add(deviceTTL),
		MaxAge:   int(deviceTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure || tls,
		SameSite: http.SameSiteStrictMode,
	}
}
