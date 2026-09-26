package notify

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Kind of channel.
type Kind string

const (
	KindWebhook Kind = "webhook" // POST JSON; the secret is the Authorization header value
	KindNtfy    Kind = "ntfy"    // POST text to the topic URL; the secret is an access token
	KindGotify  Kind = "gotify"  // POST JSON to <url>/message; the secret is the application token (required)
)

// Channel is a notification destination. The secret is never returned
// (HasSecret only).
type Channel struct {
	ID          string    `json:"id"` // 32 hex characters
	Name        string    `json:"name"`
	Kind        Kind      `json:"kind"`
	URL         string    `json:"url"`
	HasSecret   bool      `json:"hasSecret"`
	Enabled     bool      `json:"enabled"`
	MinSeverity Severity  `json:"minSeverity"`
	Events      []string  `json:"events"` // event keys; empty = all events
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// ChannelInput creates or updates a channel. Secret: nil (absent or null)
// keeps the stored secret, "" removes it, a value replaces it.
type ChannelInput struct {
	Name        string   `json:"name"`
	Kind        Kind     `json:"kind"`
	URL         string   `json:"url"`
	Secret      *string  `json:"secret,omitempty"`
	Enabled     bool     `json:"enabled"`
	MinSeverity Severity `json:"minSeverity"` // "" = warning
	Events      []string `json:"events"`
}

// Redacted returns c with the URL as DisplayURL shows it (for the audit log).
func (c Channel) Redacted() Channel {
	c.URL = DisplayURL(c.URL)
	c.Events = slices.Clone(c.Events)
	return c
}

// accepts reports whether the channel's filter lets m through.
func (c *Channel) accepts(m Message) bool {
	return c.Enabled && m.Severity.rank() >= c.MinSeverity.rank() &&
		(len(c.Events) == 0 || slices.Contains(c.Events, m.Event))
}

// DisplayURL returns a URL without its query string and fragment (they may
// carry an access token), or "" if it cannot be parsed.
func DisplayURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment, u.User = "", false, "", "", nil
	return u.String()
}

// migrations of the "notify" component (append-only).
var migrations = []string{
	`CREATE TABLE notify_channels (
		id            TEXT    PRIMARY KEY,
		name          TEXT    NOT NULL,
		kind          TEXT    NOT NULL,
		url           TEXT    NOT NULL,
		secret_sealed TEXT    NULL,
		enabled       INTEGER NOT NULL DEFAULT 1,
		min_severity  TEXT    NOT NULL DEFAULT 'warning',
		events        TEXT    NOT NULL DEFAULT '[]',
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL
	)`,
}

// Validation limits.
const (
	maxName   = 64   // characters
	maxURL    = 2048 // bytes
	maxSecret = 1024 // bytes
)

var channelIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ValidChannelID reports whether id has the form of a channel id.
func ValidChannelID(id string) bool { return channelIDRE.MatchString(id) }

// secretAAD binds a sealed secret to its channel.
func secretAAD(id string) string { return "picache/notify/" + id + "/secret" }

// storedChannel is a row with its sealed secret.
type storedChannel struct {
	Channel
	sealed string
}

func loadChannels(ctx context.Context, q *sql.DB) ([]storedChannel, error) {
	// More rows than maxChannels can only come from an edited database;
	// the surplus is ignored.
	rows, err := q.QueryContext(ctx, `SELECT id, name, kind, url, COALESCE(secret_sealed, ''), enabled,
		min_severity, events, created_at, updated_at FROM notify_channels ORDER BY created_at, id LIMIT ?`, 2*maxChannels)
	if err != nil {
		return nil, fmt.Errorf("notify: load channels: %w", err)
	}
	defer rows.Close()
	var out []storedChannel
	for rows.Next() {
		var c storedChannel
		var kind, sev, evs string
		var created, updated int64
		if err := rows.Scan(&c.ID, &c.Name, &kind, &c.URL, &c.sealed, &c.Enabled, &sev, &evs, &created, &updated); err != nil {
			return nil, fmt.Errorf("notify: load channels: %w", err)
		}
		c.Kind, c.MinSeverity = Kind(kind), Severity(sev)
		c.HasSecret = c.sealed != ""
		c.CreatedAt, c.UpdatedAt = db.Time(created), db.Time(updated)
		// An unreadable list must not widen the filter to all events.
		if err := json.Unmarshal([]byte(evs), &c.Events); err != nil || c.MinSeverity.rank() < 0 {
			c.Enabled = false
		}
		c.Events = knownEvents(c.Events)
		out = append(out, c)
	}
	return out, rows.Err()
}

// knownEvents keeps the known, selectable keys of a stored list (never nil).
func knownEvents(in []string) []string {
	out := make([]string, 0, len(in))
	for _, k := range in {
		if _, ok := eventInfo(k); ok && k != EventDropped && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out
}

// Channels lists the channels (oldest first).
func (s *Service) Channels() []Channel {
	s.mu.Lock()
	out := make([]Channel, 0, len(s.workers))
	for _, w := range s.workers {
		c, _ := w.snapshot()
		out = append(out, c)
	}
	s.mu.Unlock()
	slices.SortFunc(out, func(a, b Channel) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Channel returns one channel.
func (s *Service) Channel(id string) (Channel, error) {
	w := s.worker(id)
	if w == nil {
		return Channel{}, apperr.NotFound("notification channel", id)
	}
	c, _ := w.snapshot()
	return c, nil
}

func (s *Service) worker(id string) *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers[id]
}

// Create adds a channel.
func (s *Service) Create(ctx context.Context, in ChannelInput) (Channel, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	n := len(s.workers)
	s.mu.Unlock()
	if n >= maxChannels {
		return Channel{}, apperr.Conflict("at most %d notification channels are supported", maxChannels)
	}
	c, err := fromInput(newID(), in)
	if err != nil {
		return Channel{}, err
	}
	sealed, err := s.secretFor(c, nil, in.Secret)
	if err != nil {
		return Channel{}, err
	}
	c.HasSecret = sealed != ""
	now := db.Time(db.NowMs())
	c.CreatedAt, c.UpdatedAt = now, now
	evs, _ := json.Marshal(c.Events)
	if _, err := s.db.W.ExecContext(ctx, `INSERT INTO notify_channels (id, name, kind, url, secret_sealed, enabled,
		min_severity, events, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, string(c.Kind), c.URL, nullable(sealed), c.Enabled, string(c.MinSeverity), string(evs),
		db.Ms(c.CreatedAt), db.Ms(c.UpdatedAt)); err != nil {
		return Channel{}, fmt.Errorf("notify: insert channel: %w", err)
	}
	w := newWorker(c, sealed)
	s.mu.Lock()
	s.workers[c.ID] = w
	s.startWorkerLocked(w)
	s.mu.Unlock()
	s.log.Info("notification channel created", slog.String("channel", c.ID), slog.String("kind", string(c.Kind)))
	return c, nil
}

// Update changes a channel. The stored secret is kept (in.Secret nil) only
// while the kind and the URL's origin stay the same.
func (s *Service) Update(ctx context.Context, id string, in ChannelInput) (Channel, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	w := s.worker(id)
	if w == nil {
		return Channel{}, apperr.NotFound("notification channel", id)
	}
	cur, curSealed := w.snapshot()
	c, err := fromInput(id, in)
	if err != nil {
		return Channel{}, err
	}
	sealed, err := s.secretFor(c, &storedChannel{Channel: cur, sealed: curSealed}, in.Secret)
	if err != nil {
		return Channel{}, err
	}
	c.HasSecret = sealed != ""
	c.CreatedAt, c.UpdatedAt = cur.CreatedAt, db.Time(db.NowMs())
	evs, _ := json.Marshal(c.Events)
	if _, err := s.db.W.ExecContext(ctx, `UPDATE notify_channels SET name = ?, kind = ?, url = ?, secret_sealed = ?,
		enabled = ?, min_severity = ?, events = ?, updated_at = ? WHERE id = ?`,
		c.Name, string(c.Kind), c.URL, nullable(sealed), c.Enabled, string(c.MinSeverity), string(evs),
		db.Ms(c.UpdatedAt), id); err != nil {
		return Channel{}, fmt.Errorf("notify: update channel: %w", err)
	}
	w.update(c, sealed)
	s.log.Info("notification channel updated", slog.String("channel", id), slog.Bool("secretChanged", sealed != curSealed))
	return c, nil
}

// Delete removes a channel; its queued messages are dropped.
func (s *Service) Delete(ctx context.Context, id string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.worker(id) == nil {
		return apperr.NotFound("notification channel", id)
	}
	if _, err := s.db.W.ExecContext(ctx, `DELETE FROM notify_channels WHERE id = ?`, id); err != nil {
		return fmt.Errorf("notify: delete channel: %w", err)
	}
	s.mu.Lock()
	var cancel context.CancelFunc
	if w := s.workers[id]; w != nil {
		cancel = w.cancel
	}
	delete(s.workers, id)
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.log.Info("notification channel deleted", slog.String("channel", id))
	return nil
}

// fromInput normalises and validates everything but the secret.
func fromInput(id string, in ChannelInput) (Channel, error) {
	c := Channel{
		ID:          id,
		Name:        strings.TrimSpace(in.Name),
		Kind:        Kind(strings.ToLower(strings.TrimSpace(string(in.Kind)))),
		URL:         strings.TrimSpace(in.URL),
		Enabled:     in.Enabled,
		MinSeverity: Severity(strings.ToLower(strings.TrimSpace(string(in.MinSeverity)))),
		Events:      []string{},
	}
	if c.MinSeverity == "" {
		c.MinSeverity = SeverityWarning
	}
	n := utf8.RuneCountInString(c.Name)
	switch {
	case n == 0:
		return c, apperr.Invalid("name", "enter a name")
	case n > maxName:
		return c, apperr.Invalid("name", "at most %d characters", maxName)
	case strings.IndexFunc(c.Name, unicode.IsControl) >= 0 || !utf8.ValidString(c.Name):
		return c, apperr.Invalid("name", "must not contain control characters")
	}
	switch c.Kind {
	case KindWebhook, KindNtfy, KindGotify:
	default:
		return c, apperr.Invalid("kind", "must be webhook, ntfy or gotify")
	}
	if _, err := parseURL(c.URL); err != nil {
		return c, err
	}
	if c.MinSeverity.rank() < 0 {
		return c, apperr.Invalid("minSeverity", "must be info, warning or error")
	}
	for _, k := range in.Events {
		k = strings.TrimSpace(k)
		if _, ok := eventInfo(k); !ok || k == EventDropped {
			return c, apperr.Invalid("events", "unknown event %q", clip(k, 40))
		}
		if !slices.Contains(c.Events, k) {
			c.Events = append(c.Events, k)
		}
	}
	return c, nil
}

// parseURL checks a channel URL: http or https, a host name or IP address
// (not link-local, multicast or unspecified), no user name or password
// (the secret field carries credentials), no fragment, printable ASCII,
// at most maxURL bytes.
func parseURL(raw string) (*url.URL, error) {
	invalid := func(msg string) (*url.URL, error) { return nil, apperr.Invalid("url", "%s", msg) }
	if raw == "" {
		return invalid("enter the URL")
	}
	if len(raw) > maxURL {
		return invalid(fmt.Sprintf("at most %d characters", maxURL))
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] >= 0x7f {
			return invalid("must not contain spaces, control or non-ASCII characters (percent-encode them)")
		}
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return invalid("not a valid URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return invalid("must start with http:// or https://")
	case u.Opaque != "" || u.Host == "":
		return invalid("must contain a host name or IP address")
	case u.User != nil:
		return invalid("must not contain a user name or password; put credentials into the secret field")
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return invalid("must not contain a fragment (#…)")
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return invalid("invalid port")
		}
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" || forbiddenAddr(ip) {
			return invalid("link-local, multicast and unspecified addresses are not allowed")
		}
	} else if strings.Contains(host, ":") || !settings.ValidHostname(host) {
		return invalid("invalid host name")
	}
	return u, nil
}

// origin is the scheme, host and port a URL sends requests to.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return u.Scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// secretFor returns the sealed secret to store for c (cur: the stored
// channel on updates, nil on create).
func (s *Service) secretFor(c Channel, cur *storedChannel, in *string) (string, error) {
	var sealed string
	switch {
	case in == nil && cur != nil:
		sealed = cur.sealed
		if sealed != "" && (c.Kind != cur.Kind || origin(c.URL) != origin(cur.URL)) {
			return "", apperr.Invalid("secret", "enter the secret again: it is not kept when the kind or the server of the URL changes")
		}
	case in == nil, strings.TrimSpace(*in) == "":
	default:
		v := strings.TrimSpace(*in)
		if c.Kind == KindNtfy && len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			v = strings.TrimSpace(v[7:]) // the token alone; "Bearer " is added when sending
		}
		if len(v) > maxSecret {
			return "", apperr.Invalid("secret", "at most %d characters", maxSecret)
		}
		for i := 0; i < len(v); i++ {
			if v[i] < ' ' || v[i] >= 0x7f {
				return "", apperr.Invalid("secret", "must consist of printable ASCII characters")
			}
		}
		if s.box == nil {
			return "", errors.New("notify: no master key to seal the secret")
		}
		var err error
		if sealed, err = s.box.Seal([]byte(v), secretAAD(c.ID)); err != nil {
			return "", fmt.Errorf("notify: seal secret: %w", err)
		}
	}
	if c.Kind == KindGotify && sealed == "" {
		return "", apperr.Invalid("secret", "enter the application token of the Gotify app")
	}
	return sealed, nil
}

func nullable(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// newID returns 32 random lower-case hex characters.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// clip shortens untrusted input for error messages.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// Migrations returns the schema steps of component "notify" in picache.db
// (`picache db salvage` builds a fresh schema with them).
func Migrations() []string { return slices.Clone(migrations) }
