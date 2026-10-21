// Package epic is the Epic Games Store part of the prefill service
// (docs/ARCHITECTURE.md 20.9): a device-code sign-in, the account's games,
// the manifest of their current Windows build and its chunk list. It is
// written for PiCache from the published behaviour of Epic's launcher and
// of Legendary, Heroic and EpicPrefill, without their code.
package epic

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Sign-in without a password typed into PiCache: Epic's device-code flow,
// which only console clients may start (the published client of Fortnite
// on the Switch). The user opens the verification URL (a QR code), signs in
// on Epic's own page and confirms the code; the device code then yields a
// session of that client, which is exchanged for a session of the
// launcher's client (the one the game list and the download addresses
// need). Only the launcher session is kept; it is renewed with its refresh
// token (about 23 days, a new one with every refresh).

// ClientCredentials is a published OAuth client of Epic's.
type ClientCredentials struct {
	ID     string
	Secret string
}

func (c ClientCredentials) basic() string {
	return "basic " + base64.StdEncoding.EncodeToString([]byte(c.ID+":"+c.Secret))
}

// The clients as Epic's apps use them (configurable through Options).
var (
	// DeviceCodeClient starts device-code sign-ins (fortniteSwitchGameClient).
	DeviceCodeClient = ClientCredentials{ID: "98f7e42c2e3a4f86a74eb43fbb41ed39", Secret: "0a2449a2-001a-451e-afec-3e812901c4d7"}
	// LauncherClient is the launcher's client (launcherAppClient2).
	LauncherClient = ClientCredentials{ID: "34a02cf8f4414e29b15921876da36f9a", Secret: "daafbccc737745039dffe53d94fc76cf"}
)

// UserAgent is sent to Epic's services and the CDN, like the launcher's.
const UserAgent = "EpicGamesLauncher/19.2.2-51542141+++UE5+Release-Distro-5.5 Windows/10.0.26200.1.256.64bit"

const (
	defaultAccountURL  = "https://account-public-service-prod.ol.epicgames.com"
	defaultLauncherURL = "https://launcher-public-service-prod06.ol.epicgames.com"
	defaultCatalogURL  = "https://catalog-public-service-prod06.ol.epicgames.com"
	defaultTicketURL   = "https://launcher-public-service-prod.ak.epicgames.com"
	defaultDeliveryURL = "https://artifact-delivery-service-public-prod.ol.epicgames.com"

	maxAnswer   = 16 << 20
	callTimeout = 60 * time.Second
)

// Options configure a Client.
type Options struct {
	// The services' base URLs (tests: a fake).
	AccountURL, LauncherURL, CatalogURL, TicketURL, DeliveryURL string
	DeviceClient, Launcher                                      ClientCredentials
	// DialContext, if set, opens every connection (tests, pinned addresses).
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Used is told about every host contacted and why.
	Used func(host, protocol, purpose string)
	Log  *slog.Logger
	// PollInterval, if set, replaces Epic's sign-in poll interval (tests).
	PollInterval time.Duration
}

// Client talks to Epic's account, launcher and catalog services.
type Client struct {
	o  Options
	hc *http.Client
}

// New returns a Client.
func New(o Options) *Client {
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	def(&o.AccountURL, defaultAccountURL)
	def(&o.LauncherURL, defaultLauncherURL)
	def(&o.CatalogURL, defaultCatalogURL)
	def(&o.TicketURL, defaultTicketURL)
	def(&o.DeliveryURL, defaultDeliveryURL)
	if o.DeviceClient.ID == "" {
		o.DeviceClient = DeviceCodeClient
	}
	if o.Launcher.ID == "" {
		o.Launcher = LauncherClient
	}
	tr := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: callTimeout,
		MaxIdleConns: 4, IdleConnTimeout: 60 * time.Second}
	if o.DialContext != nil {
		tr.DialContext = o.DialContext
	} else {
		tr.DialContext = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	}
	return &Client{o: o, hc: &http.Client{Transport: tr, Timeout: 2 * callTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Error is an error answer of an Epic service.
type Error struct {
	Status  int
	Code    string // errorCode, e.g. errors.com.epicgames.account.oauth.authorization_pending
	Message string
	// ContinuationURL is where the user must act (accept a policy).
	ContinuationURL string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("epic: HTTP %d", e.Status)
	}
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	if e.ContinuationURL != "" {
		msg += " (" + e.ContinuationURL + ")"
	}
	return "epic: " + msg
}

const (
	codePending          = "errors.com.epicgames.account.oauth.authorization_pending"
	codeSlowDown         = "errors.com.epicgames.account.oauth.slow_down"
	codeExpiredCode      = "errors.com.epicgames.account.oauth.expired_device_code"
	codeNotFound         = "errors.com.epicgames.not_found"
	codeInvalidRefresh   = "errors.com.epicgames.account.auth_token.invalid_refresh_token"
	codeCorrectiveAction = "errors.com.epicgames.oauth.corrective_action_required"
)

// Rejected reports whether err means that the stored sign-in is no longer
// valid (sign in again), not a passing failure.
func Rejected(err error) bool {
	var e *Error
	if !errors.As(err, &e) {
		return false
	}
	return e.Code == codeInvalidRefresh || e.Code == codeCorrectiveAction ||
		(e.Status >= 400 && e.Status < 500 && strings.HasPrefix(e.Code, "errors.com.epicgames.account"))
}

// do sends a request and decodes a 2xx JSON answer into out.
func (c *Client) do(ctx context.Context, method, rawURL, auth, purpose string, form url.Values, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var rd io.Reader
	ctype := ""
	switch {
	case form != nil:
		rd, ctype = strings.NewReader(form.Encode()), "application/x-www-form-urlencoded"
	case body != nil:
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd, ctype = strings.NewReader(string(b)), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	if c.o.Used != nil {
		c.o.Used(req.URL.Hostname(), req.URL.Scheme, purpose)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		// The URL may carry a token (sessions/kill): never in an error.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("epic %s: %w", req.URL.Hostname(), err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer+1))
	if err != nil {
		return fmt.Errorf("epic %s: %w", req.URL.Hostname(), err)
	}
	if len(b) > maxAnswer {
		return fmt.Errorf("epic %s: answer too large", req.URL.Hostname())
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		e := &Error{Status: resp.StatusCode}
		var eb struct {
			ErrorCode       string `json:"errorCode"`
			ErrorMessage    string `json:"errorMessage"`
			ContinuationURL string `json:"continuationUrl"`
			Continuation    string `json:"continuation"`
		}
		if json.Unmarshal(b, &eb) == nil {
			e.Code, e.Message = eb.ErrorCode, eb.ErrorMessage
			e.ContinuationURL = eb.ContinuationURL
			if e.ContinuationURL == "" {
				e.ContinuationURL = eb.Continuation
			}
		}
		return e
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("epic %s: %w", req.URL.Hostname(), err)
	}
	return nil
}

// Session is a signed-in session of the launcher's client.
type Session struct {
	AccessToken      string
	ExpiresAt        time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	AccountID        string
	DisplayName      string
}

type tokenAnswer struct {
	AccessToken      string    `json:"access_token"`
	ExpiresAt        time.Time `json:"expires_at"`
	ExpiresIn        int64     `json:"expires_in"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	RefreshExpires   int64     `json:"refresh_expires"`
	AccountID        string    `json:"account_id"`
	DisplayName      string    `json:"displayName"`
}

func (t tokenAnswer) session(now time.Time) *Session {
	s := &Session{AccessToken: t.AccessToken, ExpiresAt: t.ExpiresAt, RefreshToken: t.RefreshToken,
		RefreshExpiresAt: t.RefreshExpiresAt, AccountID: t.AccountID, DisplayName: t.DisplayName}
	if s.ExpiresAt.IsZero() && t.ExpiresIn > 0 {
		s.ExpiresAt = now.Add(time.Duration(t.ExpiresIn) * time.Second)
	}
	if s.RefreshExpiresAt.IsZero() && t.RefreshExpires > 0 {
		s.RefreshExpiresAt = now.Add(time.Duration(t.RefreshExpires) * time.Second)
	}
	return s
}

func (c *Client) token(ctx context.Context, client ClientCredentials, form url.Values) (tokenAnswer, error) {
	var t tokenAnswer
	err := c.do(ctx, http.MethodPost, c.o.AccountURL+"/account/api/oauth/token", client.basic(), "epic-account", form, nil, &t)
	if err == nil && t.AccessToken == "" {
		err = errors.New("epic: the token answer has no access token")
	}
	return t, err
}

// DeviceLogin is a pending device-code sign-in.
type DeviceLogin struct {
	c          *Client
	mu         sync.Mutex
	deviceCode string
	// UserCode is what the user confirms; VerificationURL opens Epic's
	// page with it (the QR code).
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
	interval        time.Duration
}

// Interval is how often Poll should be called.
func (l *DeviceLogin) Interval() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.interval
}

// StartDeviceLogin starts a device-code sign-in.
func (c *Client) StartDeviceLogin(ctx context.Context) (*DeviceLogin, error) {
	ct, err := c.token(ctx, c.o.DeviceClient, url.Values{"grant_type": {"client_credentials"}})
	if err != nil {
		return nil, err
	}
	var a struct {
		UserCode                string `json:"user_code"`
		DeviceCode              string `json:"device_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"`
		Interval                int64  `json:"interval"`
	}
	err = c.do(ctx, http.MethodPost, c.o.AccountURL+"/account/api/oauth/deviceAuthorization", "bearer "+ct.AccessToken,
		"epic-account", url.Values{"prompt": {"login"}}, nil, &a)
	if err != nil {
		return nil, err
	}
	if a.DeviceCode == "" || a.UserCode == "" {
		return nil, errors.New("epic: incomplete device authorization")
	}
	v := a.VerificationURIComplete
	if v == "" {
		v = a.VerificationURI + "?userCode=" + url.QueryEscape(a.UserCode)
	}
	if u, err := url.Parse(v); err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("epic: unexpected verification URL %q", v)
	}
	l := &DeviceLogin{c: c, deviceCode: a.DeviceCode, UserCode: a.UserCode, VerificationURL: v,
		ExpiresAt: time.Now().Add(time.Duration(max(a.ExpiresIn, 60)) * time.Second), interval: 10 * time.Second}
	if a.Interval > 10 && a.Interval <= 60 {
		l.interval = time.Duration(a.Interval) * time.Second
	}
	if c.o.PollInterval > 0 {
		l.interval = c.o.PollInterval
	}
	return l, nil
}

// ErrExpired: the device code expired before the user confirmed it.
var ErrExpired = errors.New("epic: the code expired")

// Poll asks whether the user confirmed the code: nil, nil while not; the
// launcher session once confirmed.
func (l *DeviceLogin) Poll(ctx context.Context) (*Session, error) {
	if time.Now().After(l.ExpiresAt) {
		return nil, ErrExpired
	}
	c := l.c
	t, err := c.token(ctx, c.o.DeviceClient, url.Values{"grant_type": {"device_code"}, "device_code": {l.deviceCode}})
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			switch e.Code {
			case codePending:
				return nil, nil
			case codeSlowDown:
				l.mu.Lock()
				l.interval = min(l.interval+5*time.Second, time.Minute)
				l.mu.Unlock()
				return nil, nil
			case codeExpiredCode, codeNotFound:
				return nil, ErrExpired
			}
			if e.Status >= 500 {
				return nil, nil
			}
		} else {
			return nil, nil // a network error: try again until the code expires
		}
		return nil, err
	}
	// Device client session → exchange code → launcher session; the device
	// client's session is ended right away.
	defer c.kill(context.WithoutCancel(ctx), t.AccessToken)
	return c.exchangeToLauncher(ctx, t.AccessToken)
}

func (c *Client) exchangeToLauncher(ctx context.Context, accessToken string) (*Session, error) {
	var ex struct {
		Code string `json:"code"`
	}
	err := c.do(ctx, http.MethodGet, c.o.AccountURL+"/account/api/oauth/exchange?consumingClientId="+url.QueryEscape(c.o.Launcher.ID),
		"bearer "+accessToken, "epic-account", nil, nil, &ex)
	if err != nil {
		// Without the binding to the launcher's client.
		if err = c.do(ctx, http.MethodGet, c.o.AccountURL+"/account/api/oauth/exchange", "bearer "+accessToken, "epic-account", nil, nil, &ex); err != nil {
			return nil, err
		}
	}
	if ex.Code == "" {
		return nil, errors.New("epic: no exchange code")
	}
	t, err := c.token(ctx, c.o.Launcher, url.Values{"grant_type": {"exchange_code"}, "exchange_code": {ex.Code}, "token_type": {"eg1"}})
	if err != nil {
		return nil, err
	}
	return t.session(time.Now()), nil
}

// Refresh renews a launcher session with its refresh token (the answer
// carries a new refresh token: store it).
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	t, err := c.token(ctx, c.o.Launcher, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "token_type": {"eg1"}})
	if err != nil {
		return nil, err
	}
	return t.session(time.Now()), nil
}

// Kill ends a session at Epic (sign-out; best effort).
func (c *Client) Kill(ctx context.Context, accessToken string) error {
	return c.kill(ctx, accessToken)
}

func (c *Client) kill(ctx context.Context, accessToken string) error {
	if accessToken == "" {
		return nil
	}
	return c.do(ctx, http.MethodDelete, c.o.AccountURL+"/account/api/oauth/sessions/kill/"+url.PathEscape(accessToken),
		"bearer "+accessToken, "epic-account", nil, nil, nil)
}
