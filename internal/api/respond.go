package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// perm is the permission a route requires (docs/API.md: P, R, U, A, S).
type perm int

const (
	permPublic  perm = iota // P: no authentication
	permRead                // R: any authenticated principal (read or admin scope)
	permAdmin               // A: admin scope (an admin's browser session or an admin API token)
	permSession             // S: interactive browser session of an admin (never API tokens)
	permSelf                // U: interactive browser session of any role (never API tokens): the own account
)

// lockClass says whether PICACHE_CONFIG_LOCKED refuses a route for
// sessions (docs/ARCHITECTURE.md 6.1).
type lockClass int

const (
	lockNone   lockClass = iota // GET/HEAD and every P, R and U route
	lockLocked                  // the default of every other A and S route: refused for sessions while locked
	lockExempt                  // no stored configuration changes, nothing deleted (routeExempt)
	lockPause                   // POST /dns/blocking: the handler locks a permanent disable (routePause)
)

// routeOpt classifies a route for the configuration lock and the
// destructive switch.
type routeOpt int

const (
	routeExempt      routeOpt = iota + 1 // not locked by PICACHE_CONFIG_LOCKED
	routePause                           // lock class pause (only POST /dns/blocking)
	routeDestructive                     // refused while PICACHE_DESTRUCTIVE_API is off
)

// routeInfo is one registered route (the tests iterate the registry).
type routeInfo struct {
	Pattern     string
	Perm        perm
	Lock        lockClass
	Destructive bool
}

// handlerFunc is an API handler that returns an error instead of writing it.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

type ctxKey int

const (
	principalKey ctxKey = iota + 1
	clientKey
)

var errNotFoundRoute = apperr.NotFound("route", "")

// errConfigLocked is the answer to a locked route called by a session while
// PICACHE_CONFIG_LOCKED is on.
func errConfigLocked() error {
	return apperr.Locked("the configuration is locked on this host (PICACHE_CONFIG_LOCKED); change it with an admin API token or unset the variable")
}

// classify returns the registry entry of a route.
func classify(pattern string, p perm, opts []routeOpt) routeInfo {
	info := routeInfo{Pattern: pattern, Perm: p, Lock: lockLocked}
	method, _, _ := strings.Cut(pattern, " ")
	for _, o := range opts {
		switch o {
		case routeExempt:
			info.Lock = lockExempt
		case routePause:
			info.Lock = lockPause
		case routeDestructive:
			info.Destructive = true
		}
	}
	if method == http.MethodGet || method == http.MethodHead || p == permPublic || p == permRead || p == permSelf {
		info.Lock = lockNone
	}
	return info
}

// route registers pattern ("METHOD /api/v1/…") with a permission and its
// classification (routeExempt, routePause, routeDestructive; every other
// non-GET A or S route is locked by PICACHE_CONFIG_LOCKED). After
// authentication and the permission check, and before the handler reads
// the body, it refuses a locked route for sessions while the configuration
// is locked, then a destructive route while PICACHE_DESTRUCTIVE_API is off.
func (s *Server) route(pattern string, p perm, h handlerFunc, opts ...routeOpt) {
	info := classify(pattern, p, opts)
	s.routes = append(s.routes, info)
	s.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if p != permPublic {
			pr, err := s.d.Auth.Authenticate(r)
			if err != nil {
				writeError(w, r, s.log, err)
				return
			}
			if (p == permAdmin || p == permSession) && pr.Scope != auth.ScopeAdmin {
				writeError(w, r, s.log, apperr.Forbidden("this action requires admin rights"))
				return
			}
			if (p == permSession || p == permSelf) && pr.TokenID != 0 {
				writeError(w, r, s.log, apperr.Forbidden("this action requires an interactive login"))
				return
			}
			if info.Lock == lockLocked && pr.TokenID == 0 && s.configLocked() {
				writeError(w, r, s.log, errConfigLocked())
				return
			}
			if info.Destructive && !s.destructiveAllowed() {
				writeError(w, r, s.log, apperr.Forbidden("this action is disabled on this host (PICACHE_DESTRUCTIVE_API=false)"))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), principalKey, pr))
		}
		w.Header().Set("Cache-Control", "no-store")
		if err := h(w, r); err != nil {
			writeError(w, r, s.log, err)
		}
	})
}

// configLocked reports whether PICACHE_CONFIG_LOCKED is on.
func (s *Server) configLocked() bool { return s.d.Config != nil && s.d.Config.ConfigLocked }

// destructiveAllowed reports whether PICACHE_DESTRUCTIVE_API is on (the
// default; a process without a configuration allows them).
func (s *Server) destructiveAllowed() bool { return s.d.Config == nil || s.d.Config.DestructiveAPI }

// requireUnlocked refuses a configuration change of a session while the
// configuration is locked (for routes of lock class pause, which lock only
// some requests).
func (s *Server) requireUnlocked(r *http.Request) error {
	if p := principal(r); p != nil && p.TokenID == 0 && s.configLocked() {
		return errConfigLocked()
	}
	return nil
}

// principal returns the authenticated principal (nil on public routes).
func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

// audit records a state-changing admin action.
func (s *Server) audit(r *http.Request, action, target string, details any) {
	s.d.Auth.Audit(r.Context(), principal(r), clientIP(r), action, target, details)
}

// clientIP returns the effective client address (the peer, or the address
// a trusted reverse proxy forwarded; the access middleware has rewritten
// r.RemoteAddr to it).
func clientIP(r *http.Request) string {
	if ip := netutil.AddrFromRemote(r.RemoteAddr); ip.IsValid() {
		return ip.String()
	}
	return ""
}

// writeJSON writes v with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return nil
	}
	if err := json.MarshalWrite(w, v); err != nil {
		return errAlreadyWritten{err}
	}
	return nil
}

// ok writes 200 with v.
func ok(w http.ResponseWriter, v any) error { return writeJSON(w, http.StatusOK, v) }

// created writes 201 with v.
func created(w http.ResponseWriter, v any) error { return writeJSON(w, http.StatusCreated, v) }

// noContent writes 204.
func noContent(w http.ResponseWriter) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// errAlreadyWritten marks an error after the response started.
type errAlreadyWritten struct{ error }

// decode reads a JSON body (max 1 MiB, unknown members rejected).
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	return decodeLimit(w, r, dst, 1<<20)
}

// decodeLimit reads a JSON body of at most limit bytes (unknown members
// rejected).
func decodeLimit(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct != "application/json" {
		return apperr.Invalid("body", "Content-Type must be application/json")
	}
	body := http.MaxBytesReader(w, r.Body, limit)
	if err := json.UnmarshalRead(body, dst, json.RejectUnknownMembers(true)); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return apperr.Invalid("body", "request body too large")
		}
		if errors.Is(err, io.EOF) {
			return apperr.Invalid("body", "request body is empty")
		}
		return apperr.Invalid("body", "invalid JSON: %s", sanitizeJSONError(err))
	}
	return nil
}

func sanitizeJSONError(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "json: ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Field   string `json:"field,omitempty"`
	} `json:"error"`
}

// writeError maps err to an HTTP response. Internal errors are logged and
// reported generically.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var aw errAlreadyWritten
	if errors.As(err, &aw) {
		log.Debug("write response", slog.String("path", r.URL.Path), slog.Any("err", aw.error))
		return
	}
	status, code := http.StatusInternalServerError, "internal"
	var body errorBody
	if ae, ok := apperr.As(err); ok {
		switch ae.Kind {
		case apperr.KindInvalid:
			status, code = http.StatusBadRequest, "invalid"
		case apperr.KindNotFound:
			status, code = http.StatusNotFound, "not_found"
		case apperr.KindConflict:
			status, code = http.StatusConflict, "conflict"
		case apperr.KindForbidden:
			status, code = http.StatusForbidden, "forbidden"
		case apperr.KindUnavailable:
			status, code = http.StatusServiceUnavailable, "unavailable"
		case apperr.KindUnauthorized:
			status, code = http.StatusUnauthorized, "unauthorized"
		case apperr.KindTooMany:
			status, code = http.StatusTooManyRequests, "too_many_requests"
		case apperr.KindLocked:
			status, code = http.StatusForbidden, "config_locked"
		}
		if status != http.StatusInternalServerError {
			body.Error.Message = ae.Message
			body.Error.Field = ae.Field
		}
		if ae.Err != nil {
			log.Info("request error", slog.String("path", r.URL.Path), slog.String("kind", code), slog.Any("cause", ae.Err))
		}
	}
	if status == http.StatusInternalServerError {
		log.Error("internal error", slog.String("method", r.Method), slog.String("path", r.URL.Path), slog.Any("err", err))
		body.Error.Message = "internal error (see server log)"
	}
	body.Error.Code = code
	w.Header().Set("Cache-Control", "no-store")
	_ = writeJSON(w, status, body)
}

// --- query helpers ---

func qString(r *http.Request, name string) string { return strings.TrimSpace(r.URL.Query().Get(name)) }

func qInt(r *http.Request, name string, def int) (int, error) {
	v := qString(r, name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, apperr.Invalid(name, "must be an integer")
	}
	return n, nil
}

func qBool(r *http.Request, name string) bool {
	b, _ := strconv.ParseBool(qString(r, name))
	return b
}

// qStrings returns the non-empty values of a repeated parameter (not split
// at commas: names may contain them).
func qStrings(r *http.Request, name string) []string {
	var out []string
	for _, v := range r.URL.Query()[name] {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func qList(r *http.Request, name string) []string {
	var out []string
	for _, v := range r.URL.Query()[name] {
		for part := range strings.SplitSeq(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// qTime parses RFC 3339 or unix seconds.
func qTime(r *http.Request, name string) (time.Time, error) {
	v := qString(r, name)
	if v == "" {
		return time.Time{}, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, apperr.Invalid(name, "must be RFC 3339 or unix seconds")
	}
	return t, nil
}

// qRange reads from/to or range=15m|1h|24h|7d|30d (default def) and returns [from, to).
func qRange(r *http.Request, def time.Duration) (from, to time.Time, err error) {
	to, err = qTime(r, "to")
	if err != nil {
		return
	}
	if to.IsZero() {
		to = time.Now().UTC()
	}
	from, err = qTime(r, "from")
	if err != nil {
		return
	}
	if from.IsZero() {
		d := def
		if rs := qString(r, "range"); rs != "" {
			d, err = parseRange(rs)
			if err != nil {
				return
			}
		}
		from = to.Add(-d)
	}
	if !from.Before(to) {
		err = apperr.Invalid("from", "must be before to")
	}
	if to.Sub(from) > 400*24*time.Hour {
		err = apperr.Invalid("range", "must not exceed 400 days")
	}
	return
}

func parseRange(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || n < 1 || n > 400 {
			return 0, apperr.Invalid("range", "invalid range %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute {
		return 0, apperr.Invalid("range", "invalid range %q", s)
	}
	return d, nil
}

// pathID parses an int64 path value.
func pathID(r *http.Request, name string) (int64, error) {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		return 0, apperr.Invalid(name, "invalid id")
	}
	return n, nil
}

// extendDeadlines lifts the server read/write timeouts for this request:
// d > 0 sets both deadlines to now+d, d == 0 removes them (streams). Call it
// first in handlers that wait long (list/source refresh, storage test,
// backup/restore).
func extendDeadlines(w http.ResponseWriter, d time.Duration) {
	rc := http.NewResponseController(w)
	var t time.Time
	if d > 0 {
		t = time.Now().Add(d)
	}
	_ = rc.SetReadDeadline(t)
	_ = rc.SetWriteDeadline(t)
}

// sse streams events from ch as Server-Sent Events until the client
// disconnects, ch is closed, alive() returns false (checked every 15 s with
// the heartbeat: session revoked or expired) or the stream is 1 h old (the
// browser reconnects).
func sse[T any](w http.ResponseWriter, r *http.Request, event string, ch <-chan T, alive func() bool) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming unsupported")
	}
	extendDeadlines(w, 0)
	deadline := time.NewTimer(time.Hour)
	defer deadline.Stop()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-deadline.C:
			return nil
		case <-tick.C:
			if alive != nil && !alive() {
				return nil
			}
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return nil
			}
			fl.Flush()
		case v, open := <-ch:
			if !open {
				return nil
			}
			b, err := json.Marshal(v)
			if err != nil {
				return nil
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
				return nil
			}
			fl.Flush()
		}
	}
}
