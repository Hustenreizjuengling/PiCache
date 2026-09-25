package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// Priorities per severity (info, warning, error).
var (
	ntfyPriority   = map[Severity]string{SeverityInfo: "3", SeverityWarning: "4", SeverityError: "5"}
	gotifyPriority = map[Severity]int{SeverityInfo: 4, SeverityWarning: 6, SeverityError: 8}
)

// webhookPayload is the JSON body of a webhook (member order as documented).
type webhookPayload struct {
	Event    string   `json:"event"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`
	Message  string   `json:"message"`
	Time     string   `json:"time"` // RFC 3339, UTC
	Instance string   `json:"instance"`
	Hostname string   `json:"hostname"`
	Version  string   `json:"version"`
}

type gotifyPayload struct {
	Title    string `json:"title"`
	Message  string `json:"message"`
	Priority int    `json:"priority"`
}

var errForbiddenAddr = errors.New("link-local, multicast and unspecified addresses are not allowed")

// newClient is the outbound HTTP client of all channels. Private and
// loopback destinations are allowed: the channels are configured by the
// admin, and the usual receivers (Home Assistant, a self-hosted ntfy or
// Gotify) run on the LAN or on this host. Link-local addresses (cloud
// metadata), multicast and unspecified addresses are refused after name
// resolution, also when a NAT64/6to4 address embeds one. Names are resolved
// by the host's resolver, so LAN names work. No redirects are followed, no
// proxy is used and TLS certificates are verified.
func newClient() *http.Client {
	d := &net.Dialer{Timeout: requestTimeout, KeepAlive: 30 * time.Second, Control: dialControl}
	return &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           d.DialContext,
			ForceAttemptHTTP2:     true,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   requestTimeout,
			ResponseHeaderTimeout: requestTimeout,
			MaxIdleConnsPerHost:   1,
			IdleConnTimeout:       90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dialControl refuses forbidden destinations right before connecting.
func dialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if forbiddenAddr(ap.Addr()) {
		return errForbiddenAddr
	}
	return nil
}

// forbiddenAddr reports link-local, multicast, broadcast and unspecified
// addresses (0.0.0.0/8 included), also embedded in NAT64/6to4 addresses.
func forbiddenAddr(ip netip.Addr) bool {
	ip = netutil.Canon(ip)
	if ip.IsLoopback() {
		return false // ::1 is not an IPv4-compatible address
	}
	if v4, ok := netutil.EmbeddedIPv4(ip); ok && !ip.IsUnspecified() {
		ip = v4
	}
	return !ip.IsValid() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsMulticast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		(ip.Is4() && (ip.As4()[0] == 0 || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255})))
}

// send delivers m to c once. It returns the HTTP status (0 if there was no
// answer) and an error without the URL (its query may carry a token).
func (s *Service) send(ctx context.Context, c Channel, sealed string, m Message) (int, error) {
	req, err := s.buildRequest(ctx, c, sealed, m)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, requestError(ctx, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	switch code := resp.StatusCode; {
	case code >= 200 && code <= 299:
		return code, nil
	case code >= 300 && code <= 399:
		return code, fmt.Errorf("HTTP %d %s: redirects are not followed; use the final URL", code, http.StatusText(code))
	default:
		return code, fmt.Errorf("HTTP %d %s", code, http.StatusText(code))
	}
}

// requestError returns the cause of a failed request without the URL.
func requestError(ctx context.Context, err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	switch {
	case errors.Is(err, errForbiddenAddr):
		return &permanentError{"the host resolves to a link-local, multicast or unspecified address, which is not allowed"}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errors.New("no answer within 10 seconds")
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled (PiCache is shutting down)")
	}
	msg := err.Error()
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return errors.New(msg)
}

// buildRequest renders m in the format of the channel's kind.
func (s *Service) buildRequest(ctx context.Context, c Channel, sealed string, m Message) (*http.Request, error) {
	u, err := parseURL(c.URL)
	if err != nil {
		return nil, &permanentError{"invalid URL; edit the channel"}
	}
	secret := ""
	if sealed != "" {
		if s.box == nil {
			return nil, &permanentError{"no master key to open the stored secret"}
		}
		b, err := s.box.Open(sealed, secretAAD(c.ID))
		if err != nil {
			return nil, &permanentError{"the stored secret cannot be decrypted (was the master key replaced?); enter it again"}
		}
		secret = string(b)
	}
	h := http.Header{}
	h.Set("User-Agent", "PiCache/"+s.opt.Version)
	var body []byte
	switch c.Kind {
	case KindWebhook:
		body, err = json.Marshal(webhookPayload{Event: m.Event, Severity: m.Severity, Title: m.Title, Message: m.Message,
			Time: m.Time.UTC().Format(time.RFC3339), Instance: s.opt.InstanceID, Hostname: s.opt.Hostname, Version: s.opt.Version})
		h.Set("Content-Type", "application/json")
		if secret != "" {
			h.Set("Authorization", secret)
		}
	case KindNtfy:
		body = []byte(m.Message)
		h.Set("Content-Type", "text/plain; charset=utf-8")
		// Header values are ASCII; ntfy decodes RFC 2047 encoded words.
		h.Set("Title", mime.QEncoding.Encode("utf-8", m.Title))
		h.Set("Priority", ntfyPriority[m.Severity])
		h.Set("Tags", m.Event+","+string(m.Severity))
		if secret != "" {
			h.Set("Authorization", "Bearer "+secret)
		}
	case KindGotify:
		if secret == "" {
			return nil, &permanentError{"no Gotify application token is stored; enter it again"}
		}
		u = u.JoinPath("message")
		body, err = json.Marshal(gotifyPayload{Title: m.Title, Message: m.Message, Priority: gotifyPriority[m.Severity]})
		h.Set("Content-Type", "application/json")
		h.Set("X-Gotify-Key", secret)
	default:
		return nil, &permanentError{"unknown channel kind"}
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, &permanentError{"invalid URL; edit the channel"}
	}
	for k, v := range h {
		req.Header[k] = v
	}
	return req, nil
}

// Test sends a test notification to channel id right away (no queue, no
// filter, also while the channel is disabled) and records it in the log.
// At most maxConcurrent tests run at the same time.
func (s *Service) Test(ctx context.Context, id string) (TestResult, error) {
	w := s.worker(id)
	if w == nil {
		return TestResult{}, apperr.NotFound("notification channel", id)
	}
	select {
	case s.tests <- struct{}{}:
		defer func() { <-s.tests }()
	default:
		return TestResult{}, apperr.TooMany("other test notifications are being sent; try again in a moment")
	}
	c, sealed := w.snapshot()
	host := s.opt.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	m := Message{Event: EventTest, Severity: SeverityInfo, Time: s.now().UTC(), Title: "PiCache test notification",
		Message: cleanText(fmt.Sprintf("This is a test notification from PiCache on %s for the channel %q. "+
			"If you can read it, notifications to this channel work.", host, c.Name), maxMessageLen, true)}
	rctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	start := time.Now()
	status, err := s.send(rctx, c, sealed, m)
	res := TestResult{OK: err == nil, Status: status, DurationMs: time.Since(start).Milliseconds()}
	if err != nil {
		res.Error = err.Error()
	}
	s.record(c, m, 1, err)
	return res, nil
}
