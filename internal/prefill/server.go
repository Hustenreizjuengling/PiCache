package prefill

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// The socket API (docs/ARCHITECTURE.md 20.3). Every request comes from
// PiCache (which checked the admin's session) or from root on the host.

const maxBody = 64 << 10

// apiError is an error with an answer status.
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func invalid(msg string) error { return &apiError{http.StatusBadRequest, prefillapi.CodeInvalid, msg} }
func conflict(msg string) error {
	return &apiError{http.StatusConflict, prefillapi.CodeConflict, msg}
}
func unavailable(msg string) error {
	return &apiError{http.StatusServiceUnavailable, prefillapi.CodeUnavailable, msg}
}
func notFound(msg string) error { return &apiError{http.StatusNotFound, prefillapi.CodeNotFound, msg} }

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

func (d *Daemon) handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, f handlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if err := f(w, r); err != nil {
				d.writeError(w, err)
			}
		})
	}
	h("GET /v1/status", func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Query().Get("checkCache") == "true" {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			_ = d.checkCache(ctx)
			cancel()
		}
		return reply(w, d.status())
	})
	h("GET /v1/settings", func(w http.ResponseWriter, r *http.Request) error {
		return reply(w, d.currentSettings())
	})
	h("PUT /v1/settings", func(w http.ResponseWriter, r *http.Request) error {
		var s prefillapi.Settings
		if err := decode(r, &s); err != nil {
			return err
		}
		s, err := d.setSettings(s)
		if err != nil {
			return err
		}
		return reply(w, s)
	})
	h("PUT /v1/steam/selection", func(w http.ResponseWriter, r *http.Request) error {
		var sel prefillapi.Selection
		if err := decode(r, &sel); err != nil {
			return err
		}
		s := d.currentSettings()
		s.Steam.Apps = sel.Apps
		s, err := d.setSettings(s)
		if err != nil {
			return err
		}
		return reply(w, prefillapi.Selection{Apps: s.Steam.Apps})
	})
	h("POST /v1/steam/login", func(w http.ResponseWriter, r *http.Request) error {
		l, err := d.steam.beginLogin(r.Context())
		if err != nil {
			return err
		}
		return reply(w, l)
	})
	h("DELETE /v1/steam/login", func(w http.ResponseWriter, r *http.Request) error {
		d.steam.cancelLogin()
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	h("POST /v1/steam/logout", func(w http.ResponseWriter, r *http.Request) error {
		if err := d.steam.logout(r.Context()); err != nil {
			return err
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	h("GET /v1/steam/apps", func(w http.ResponseWriter, r *http.Request) error {
		apps, err := d.steam.gameList(r.Context(), r.URL.Query().Get("refresh") == "true")
		if err != nil {
			return err
		}
		return reply(w, apps)
	})
	h("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) error {
		var in prefillapi.StartJob
		if r.ContentLength != 0 {
			if err := decode(r, &in); err != nil {
				return err
			}
		}
		j, err := d.startJob("manual", in.Force, time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		return json.MarshalWrite(w, j)
	})
	h("DELETE /v1/jobs/current", func(w http.ResponseWriter, r *http.Request) error {
		if !d.cancelJob() {
			return notFound("no job is running")
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	h("GET /v1/history", func(w http.ResponseWriter, r *http.Request) error {
		return reply(w, d.jobHistory())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.peerAllowed(r.Context()) {
			d.writeError(w, &apiError{http.StatusForbidden, prefillapi.CodeForbidden, "this user may not use the prefill service"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json")
	return json.MarshalWrite(w, v)
}

// decode reads a JSON body of at most maxBody bytes; unknown members are
// refused.
func decode(r *http.Request, v any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		return invalid("the body must be JSON")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return invalid("the body could not be read")
	}
	if len(b) > maxBody {
		return invalid("the body is too large")
	}
	if err := json.Unmarshal(b, v, json.RejectUnknownMembers(true)); err != nil {
		return invalid("invalid JSON: " + err.Error())
	}
	return nil
}

func (d *Daemon) writeError(w http.ResponseWriter, err error) {
	var body prefillapi.ErrorBody
	status := http.StatusInternalServerError
	body.Error.Code = prefillapi.CodeInternal
	body.Error.Message = "internal error"
	var ae *apiError
	if errors.As(err, &ae) {
		status, body.Error.Code, body.Error.Message = ae.status, ae.code, ae.msg
	} else {
		d.log.Warn("prefill request failed", "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, body)
}
