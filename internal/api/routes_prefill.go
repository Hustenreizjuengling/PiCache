package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
	"github.com/hustenreizjuengling/picache/internal/update"
)

// The prefill endpoints pass the admin's requests on to the prefill
// service (docs/ARCHITECTURE.md 20) over its Unix socket. PiCache holds no
// store sign-in: the service answers with account names and expiry dates
// only, never with a token.

// Prefill is the prefill service's socket client (prefillapi.Client).
type Prefill interface {
	Do(ctx context.Context, method, path string, body any) (prefillapi.Response, error)
	Installed() bool
	Socket() string
}

// PrefillHost switches the prefill service on or off through the root
// helper (app.prefillHost).
type PrefillHost interface {
	// Mode: helper (switched from the web UI), package, docker, manual.
	Mode(ctx context.Context) string
	// Status is the last request's state (nil: none).
	Status() *update.PrefillStatus
	// Request queues enable or disable (apperr.Conflict without the
	// helper or while a request runs).
	Request(ctx context.Context, enable bool, by string) (*update.PrefillStatus, error)
}

// PrefillState is GET /prefill: whether the service runs, and its status.
type PrefillState struct {
	// Installed: the socket exists. Running: the service answered.
	Installed bool               `json:"installed"`
	Running   bool               `json:"running"`
	Socket    string             `json:"socket"`
	Error     string             `json:"error"`
	Status    *prefillapi.Status `json:"status"`
	// Mode says how the service is switched on: helper (the button of the
	// web UI), package, docker or manual (a command on the host).
	Mode string `json:"mode"`
	// Setup is the last switch request through the helper.
	Setup *update.PrefillStatus `json:"setup"`
}

// prefillWait bounds the calls that talk to Steam (sign-in, game list).
const prefillWait = 6 * time.Minute

func (s *Server) registerPrefillRoutes() {
	s.route("GET /api/v1/prefill", permAdmin, s.prefillState)
	s.route("GET /api/v1/prefill/settings", permAdmin, s.prefillGetSettings)
	s.route("PUT /api/v1/prefill/settings", permAdmin, s.prefillPutSettings)
	s.route("POST /api/v1/prefill/steam/login", permAdmin, s.prefillSteamLogin, routeExempt)
	s.route("DELETE /api/v1/prefill/steam/login", permAdmin, s.prefillSteamCancelLogin, routeExempt)
	s.route("POST /api/v1/prefill/steam/logout", permAdmin, s.prefillSteamLogout, routeExempt)
	s.route("GET /api/v1/prefill/steam/apps", permAdmin, s.prefillSteamApps)
	s.route("PUT /api/v1/prefill/steam/selection", permAdmin, s.prefillSteamSelection)
	s.route("POST /api/v1/prefill/jobs", permAdmin, s.prefillStartJob, routeExempt)
	s.route("DELETE /api/v1/prefill/jobs/current", permAdmin, s.prefillCancelJob, routeExempt)
	s.route("GET /api/v1/prefill/history", permAdmin, s.prefillHistory)
	s.route("PUT /api/v1/prefill/service", permAdmin, s.prefillSetService)
	s.route("GET /api/v1/prefill/battlenet/products", permAdmin, s.prefillBattleNetProducts)
	s.route("PUT /api/v1/prefill/battlenet/selection", permAdmin, s.prefillBattleNetSelection)
	s.route("POST /api/v1/prefill/epic/login", permAdmin, s.prefillEpicLogin, routeExempt)
	s.route("DELETE /api/v1/prefill/epic/login", permAdmin, s.prefillEpicCancelLogin, routeExempt)
	s.route("POST /api/v1/prefill/epic/logout", permAdmin, s.prefillEpicLogout, routeExempt)
	s.route("GET /api/v1/prefill/epic/apps", permAdmin, s.prefillEpicApps)
	s.route("PUT /api/v1/prefill/epic/selection", permAdmin, s.prefillEpicSelection)
}

func (s *Server) prefillBattleNetProducts(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), prefillWait)
	defer cancel()
	path := "/v1/battlenet/products"
	if r.URL.Query().Get("refresh") == "true" {
		path += "?refresh=true"
	}
	var out prefillapi.BattleNetProducts
	if err := s.prefillCall(ctx, http.MethodGet, path, nil, &out); err != nil {
		return err
	}
	return ok(w, out)
}

func (s *Server) prefillBattleNetSelection(w http.ResponseWriter, r *http.Request) error {
	var in prefillapi.BattleNetSelection
	if err := decode(w, r, &in); err != nil {
		return err
	}
	var out prefillapi.BattleNetSelection
	if err := s.prefillCall(r.Context(), http.MethodPut, "/v1/battlenet/selection", in, &out); err != nil {
		return err
	}
	s.audit(r, "prefill.battlenet.selection", "", map[string]int{"products": len(out.Products)})
	return ok(w, out)
}

func (s *Server) prefillEpicLogin(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), prefillWait)
	defer cancel()
	var out prefillapi.EpicLogin
	if err := s.prefillCall(ctx, http.MethodPost, "/v1/epic/login", nil, &out); err != nil {
		return err
	}
	s.audit(r, "prefill.epic.login", "", nil)
	return ok(w, out)
}

func (s *Server) prefillEpicCancelLogin(w http.ResponseWriter, r *http.Request) error {
	if err := s.prefillCall(r.Context(), http.MethodDelete, "/v1/epic/login", nil, nil); err != nil {
		return err
	}
	return noContent(w)
}

func (s *Server) prefillEpicLogout(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), prefillWait)
	defer cancel()
	if err := s.prefillCall(ctx, http.MethodPost, "/v1/epic/logout", nil, nil); err != nil {
		return err
	}
	s.audit(r, "prefill.epic.logout", "", nil)
	return noContent(w)
}

func (s *Server) prefillEpicApps(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), prefillWait)
	defer cancel()
	path := "/v1/epic/apps"
	if r.URL.Query().Get("refresh") == "true" {
		path += "?refresh=true"
	}
	var out prefillapi.EpicApps
	if err := s.prefillCall(ctx, http.MethodGet, path, nil, &out); err != nil {
		return err
	}
	return ok(w, out)
}

func (s *Server) prefillEpicSelection(w http.ResponseWriter, r *http.Request) error {
	var in prefillapi.EpicSelection
	if err := decode(w, r, &in); err != nil {
		return err
	}
	var out prefillapi.EpicSelection
	if err := s.prefillCall(r.Context(), http.MethodPut, "/v1/epic/selection", in, &out); err != nil {
		return err
	}
	s.audit(r, "prefill.epic.selection", "", map[string]int{"apps": len(out.Apps)})
	return ok(w, out)
}

// prefillSetService asks the root helper to switch the prefill service on
// or off (202 with the request's state).
func (s *Server) prefillSetService(w http.ResponseWriter, r *http.Request) error {
	if s.d.PrefillHost == nil {
		return apperr.Unavailable("the prefill service cannot be switched here")
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Enabled == nil {
		return apperr.Invalid("enabled", "required")
	}
	st, err := s.d.PrefillHost.Request(r.Context(), *in.Enabled, principal(r).Username)
	if err != nil {
		return err
	}
	action := "prefill.service.disable"
	if *in.Enabled {
		action = "prefill.service.enable"
	}
	s.audit(r, action, "", nil)
	return writeJSON(w, http.StatusAccepted, st)
}

var errPrefillNotRunning = apperr.Unavailable("the prefill service is not running: switch it on under Cache → Prefill " +
	"(or on the host: `sudo picache prefill enable`, docs/DEPLOYMENT.md, Prefill)")

// prefillCall sends one request and decodes a 2xx answer into out (nil:
// no body). An error answer becomes the apperr of its code.
func (s *Server) prefillCall(ctx context.Context, method, path string, body, out any) error {
	if s.d.Prefill == nil {
		return errPrefillNotRunning
	}
	resp, err := s.d.Prefill.Do(ctx, method, path, body)
	if errors.Is(err, prefillapi.ErrNotRunning) {
		return errPrefillNotRunning
	}
	if err != nil {
		return apperr.Wrap(apperr.KindUnavailable, err, "the prefill service did not answer")
	}
	if resp.Status >= 400 {
		var eb prefillapi.ErrorBody
		msg := "the prefill service refused the request"
		if json.Unmarshal(resp.Body, &eb) == nil && eb.Error.Message != "" {
			msg = eb.Error.Message
		}
		switch eb.Error.Code {
		case prefillapi.CodeInvalid:
			return apperr.Invalid("", "%s", msg)
		case prefillapi.CodeNotFound:
			return apperr.NotFound(msg, "")
		case prefillapi.CodeConflict:
			return apperr.Conflict("%s", msg)
		case prefillapi.CodeForbidden:
			return apperr.Forbidden("the prefill service refuses PiCache's user: check PICACHE_PREFILL_CLIENT_USER")
		default:
			return apperr.Unavailable("prefill service: %s", msg)
		}
	}
	if out == nil || len(resp.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return apperr.Wrap(apperr.KindUnavailable, err, "the prefill service sent an invalid answer")
	}
	return nil
}

func (s *Server) prefillState(w http.ResponseWriter, r *http.Request) error {
	st := PrefillState{Mode: update.ModeManual}
	if s.d.PrefillHost != nil {
		st.Mode = s.d.PrefillHost.Mode(r.Context())
		st.Setup = s.d.PrefillHost.Status()
	}
	if s.d.Prefill == nil {
		st.Error = "not configured"
		return ok(w, st)
	}
	st.Installed = s.d.Prefill.Installed()
	st.Socket = s.d.Prefill.Socket()
	if !st.Installed {
		return ok(w, st)
	}
	var status prefillapi.Status
	path := "/v1/status"
	if r.URL.Query().Get("checkCache") == "true" {
		path += "?checkCache=true"
	}
	if err := s.prefillCall(r.Context(), http.MethodGet, path, nil, &status); err != nil {
		st.Error = err.Error()
		return ok(w, st)
	}
	st.Running = true
	st.Status = &status
	return ok(w, st)
}

func (s *Server) prefillGetSettings(w http.ResponseWriter, r *http.Request) error {
	var out prefillapi.Settings
	if err := s.prefillCall(r.Context(), http.MethodGet, "/v1/settings", nil, &out); err != nil {
		return err
	}
	return ok(w, out)
}

func (s *Server) prefillPutSettings(w http.ResponseWriter, r *http.Request) error {
	var in prefillapi.Settings
	if err := decode(w, r, &in); err != nil {
		return err
	}
	var out prefillapi.Settings
	if err := s.prefillCall(r.Context(), http.MethodPut, "/v1/settings", in, &out); err != nil {
		return err
	}
	s.audit(r, "prefill.settings", "", map[string]any{"schedule": out.Schedule.Enabled, "maxMbps": out.MaxMbps,
		"connections": out.Connections})
	return ok(w, out)
}

func (s *Server) prefillSteamLogin(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), prefillWait)
	defer cancel()
	var out prefillapi.SteamLogin
	if err := s.prefillCall(ctx, http.MethodPost, "/v1/steam/login", nil, &out); err != nil {
		return err
	}
	s.audit(r, "prefill.steam.login", "", nil)
	return ok(w, out)
}

func (s *Server) prefillSteamCancelLogin(w http.ResponseWriter, r *http.Request) error {
	if err := s.prefillCall(r.Context(), http.MethodDelete, "/v1/steam/login", nil, nil); err != nil {
		return err
	}
	return noContent(w)
}

func (s *Server) prefillSteamLogout(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), prefillWait)
	defer cancel()
	if err := s.prefillCall(ctx, http.MethodPost, "/v1/steam/logout", nil, nil); err != nil {
		return err
	}
	s.audit(r, "prefill.steam.logout", "", nil)
	return noContent(w)
}

func (s *Server) prefillSteamApps(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), prefillWait)
	defer cancel()
	path := "/v1/steam/apps"
	if r.URL.Query().Get("refresh") == "true" {
		path += "?refresh=true"
	}
	var out prefillapi.SteamApps
	if err := s.prefillCall(ctx, http.MethodGet, path, nil, &out); err != nil {
		return err
	}
	return ok(w, out)
}

func (s *Server) prefillSteamSelection(w http.ResponseWriter, r *http.Request) error {
	var in prefillapi.Selection
	if err := decode(w, r, &in); err != nil {
		return err
	}
	var out prefillapi.Selection
	if err := s.prefillCall(r.Context(), http.MethodPut, "/v1/steam/selection", in, &out); err != nil {
		return err
	}
	s.audit(r, "prefill.steam.selection", "", map[string]int{"apps": len(out.Apps)})
	return ok(w, out)
}

func (s *Server) prefillStartJob(w http.ResponseWriter, r *http.Request) error {
	var in prefillapi.StartJob
	if r.ContentLength != 0 {
		if err := decode(w, r, &in); err != nil {
			return err
		}
	}
	var out prefillapi.Job
	if err := s.prefillCall(r.Context(), http.MethodPost, "/v1/jobs", in, &out); err != nil {
		return err
	}
	s.audit(r, "prefill.job.start", "", map[string]any{"job": out.ID, "force": in.Force})
	return writeJSON(w, http.StatusAccepted, out)
}

func (s *Server) prefillCancelJob(w http.ResponseWriter, r *http.Request) error {
	if err := s.prefillCall(r.Context(), http.MethodDelete, "/v1/jobs/current", nil, nil); err != nil {
		return err
	}
	s.audit(r, "prefill.job.cancel", "", nil)
	return noContent(w)
}

func (s *Server) prefillHistory(w http.ResponseWriter, r *http.Request) error {
	var out prefillapi.History
	if err := s.prefillCall(r.Context(), http.MethodGet, "/v1/history", nil, &out); err != nil {
		return err
	}
	return ok(w, out)
}
