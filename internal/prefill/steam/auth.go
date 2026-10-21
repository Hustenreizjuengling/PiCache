package steam

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sign-in with a QR code (IAuthenticationService): BeginAuthSessionViaQR
// returns a challenge URL (https://s.team/q/…) that the Steam app scans;
// PollAuthSessionStatus says when the user confirmed it in the app and
// then carries the account name and a refresh token (a JWT). The client
// never sees a password and never asks for a Steam Guard code: the
// confirmation happens on the phone.

const (
	platformSteamClient = 1    // EAuthTokenPlatformType_SteamClient
	osTypeLinux         = -203 // EOSType_LinuxUnknown
	websiteClient       = "Client"

	// UserAgent is the Steam client's. The download cache recognises
	// Steam's downloads by it.
	UserAgent = "Valve/Steam HTTP Client 1.0"
)

// QR is a pending QR sign-in.
type QR struct {
	c         *Conn
	mu        sync.Mutex
	url       string
	clientID  uint64
	requestID []byte
	interval  time.Duration
	scanned   bool
}

// QRResult is a confirmed sign-in.
type QRResult struct {
	AccountName  string
	RefreshToken string
	// SteamID, Expires and TokenID come from the refresh token's claims.
	SteamID uint64
	Expires time.Time
	TokenID string
}

// BeginQR starts a QR sign-in. deviceName is how the sign-in appears in
// the account's device list.
func (c *Conn) BeginQR(ctx context.Context, deviceName string) (*QR, error) {
	var dev pb
	dev.string(1, deviceName)
	dev.uint(2, platformSteamClient)
	dev.int32(3, osTypeLinux)
	// Like Steam's client libraries: the device details and the website,
	// not the top-level name and platform.
	var req pb
	req.bytes(3, dev.b)
	req.string(4, websiteClient)
	b, err := c.service(ctx, "Authentication.BeginAuthSessionViaQR#1", req.b, false)
	if err != nil {
		return nil, err
	}
	q := &QR{c: c, interval: 5 * time.Second}
	for f, err := range fields(b) {
		if err != nil {
			return nil, err
		}
		switch f.num {
		case 1:
			q.clientID = f.n
		case 2:
			q.url = f.str()
		case 3:
			q.requestID = append([]byte(nil), f.b...)
		case 4:
			if v := f.float32(); v >= 1 && v <= 30 {
				q.interval = time.Duration(float64(v) * float64(time.Second))
			}
		}
	}
	if q.clientID == 0 || q.url == "" || len(q.requestID) == 0 {
		return nil, errors.New("steam: incomplete QR sign-in answer")
	}
	return q, nil
}

// URL is the current challenge URL (it changes while the sign-in waits).
func (q *QR) URL() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.url
}

// Scanned reports whether the code was scanned and waits for the
// confirmation in the app.
func (q *QR) Scanned() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.scanned
}

// Interval is how often Poll should be called.
func (q *QR) Interval() time.Duration { return q.interval }

// Poll asks whether the sign-in was confirmed. It returns nil while it
// waits; the challenge URL may have changed (URL). An error ends the
// sign-in (an Error with resultExpired when it timed out).
func (q *QR) Poll(ctx context.Context) (*QRResult, error) {
	q.mu.Lock()
	var req pb
	req.uint(1, q.clientID)
	req.bytes(2, q.requestID)
	q.mu.Unlock()
	b, err := q.c.service(ctx, "Authentication.PollAuthSessionStatus#1", req.b, false)
	if err != nil {
		return nil, err
	}
	var res QRResult
	for f, err := range fields(b) {
		if err != nil {
			return nil, err
		}
		switch f.num {
		case 1:
			if f.n != 0 {
				q.mu.Lock()
				q.clientID = f.n
				q.mu.Unlock()
			}
		case 2:
			if s := f.str(); s != "" {
				q.mu.Lock()
				q.url = s
				q.mu.Unlock()
			}
		case 3:
			res.RefreshToken = f.str()
		case 5:
			if f.bool() {
				q.mu.Lock()
				q.scanned = true
				q.mu.Unlock()
			}
		case 6:
			res.AccountName = f.str()
		}
	}
	if res.RefreshToken == "" {
		return nil, nil
	}
	claims, err := ParseToken(res.RefreshToken)
	if err != nil {
		return nil, err
	}
	res.SteamID, res.Expires, res.TokenID = claims.SteamID, claims.Expires, claims.TokenID
	return &res, nil
}

// TokenClaims are the claims of a Steam JWT that the client uses.
type TokenClaims struct {
	SteamID uint64
	Expires time.Time
	TokenID string
}

// ParseToken reads the claims of a refresh token. The signature is not
// checked: the token comes from Steam over TLS and goes back only to
// Steam, which checks it.
func ParseToken(tok string) (TokenClaims, error) {
	var c TokenClaims
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return c, errors.New("steam: the refresh token is not a JWT")
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c, errors.New("steam: the refresh token's claims are not base64")
	}
	var claims struct {
		Sub string `json:"sub"`
		Exp int64  `json:"exp"`
		Jti string `json:"jti"`
	}
	if err := json.Unmarshal(b, &claims); err != nil {
		return c, errors.New("steam: the refresh token's claims are not JSON")
	}
	id, err := strconv.ParseUint(claims.Sub, 10, 64)
	if err != nil || id == 0 {
		return c, errors.New("steam: the refresh token names no Steam ID")
	}
	c.SteamID = id
	if claims.Exp > 0 {
		c.Expires = time.Unix(claims.Exp, 0).UTC()
	}
	c.TokenID = claims.Jti
	return c, nil
}
