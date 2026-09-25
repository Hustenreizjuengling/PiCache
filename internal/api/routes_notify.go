package api

import (
	"net/http"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/notify"
)

// registerNotifyRoutes registers the notification endpoints (docs/API.md).
// Channel URLs can carry access tokens (webhook ids, ?auth=), so reading
// channels and the delivery log needs admin rights; the event list does not.
func (s *Server) registerNotifyRoutes() {
	s.route("GET /api/v1/notifications/channels", permAdmin, s.notifyChannels)
	s.route("POST /api/v1/notifications/channels", permAdmin, s.notifyCreate)
	s.route("PUT /api/v1/notifications/channels/{id}", permAdmin, s.notifyUpdate)
	s.route("DELETE /api/v1/notifications/channels/{id}", permAdmin, s.notifyDelete)
	s.route("POST /api/v1/notifications/channels/{id}/test", permAdmin, s.notifyTest)
	s.route("GET /api/v1/notifications/events", permRead, s.notifyEvents)
	s.route("GET /api/v1/notifications/log", permAdmin, s.notifyLog)
}

var errNoNotify = apperr.Unavailable("notifications are not available")

// notifyChannelID validates the {id} path value (32 hex characters).
func notifyChannelID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !notify.ValidChannelID(id) {
		return "", apperr.Invalid("id", "invalid channel id")
	}
	return id, nil
}

func (s *Server) notifyChannels(w http.ResponseWriter, r *http.Request) error {
	if s.d.Notify == nil {
		return errNoNotify
	}
	return ok(w, s.d.Notify.Channels())
}

func (s *Server) notifyCreate(w http.ResponseWriter, r *http.Request) error {
	if s.d.Notify == nil {
		return errNoNotify
	}
	var in notify.ChannelInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	c, err := s.d.Notify.Create(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "notifications.channel.create", c.ID, c.Redacted()) // never the secret, no query string
	return created(w, c)
}

func (s *Server) notifyUpdate(w http.ResponseWriter, r *http.Request) error {
	if s.d.Notify == nil {
		return errNoNotify
	}
	id, err := notifyChannelID(r)
	if err != nil {
		return err
	}
	var in notify.ChannelInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	c, err := s.d.Notify.Update(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "notifications.channel.update", c.ID, map[string]any{"channel": c.Redacted(), "secretChanged": in.Secret != nil})
	return ok(w, c)
}

func (s *Server) notifyDelete(w http.ResponseWriter, r *http.Request) error {
	if s.d.Notify == nil {
		return errNoNotify
	}
	id, err := notifyChannelID(r)
	if err != nil {
		return err
	}
	if err := s.d.Notify.Delete(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "notifications.channel.delete", id, nil)
	return noContent(w)
}

// notifyTest sends a test message synchronously (at most 10 s).
func (s *Server) notifyTest(w http.ResponseWriter, r *http.Request) error {
	if s.d.Notify == nil {
		return errNoNotify
	}
	id, err := notifyChannelID(r)
	if err != nil {
		return err
	}
	res, err := s.d.Notify.Test(r.Context(), id)
	if err != nil {
		return err
	}
	s.audit(r, "notifications.channel.test", id, map[string]any{"ok": res.OK, "status": res.Status})
	return ok(w, res)
}

func (s *Server) notifyEvents(w http.ResponseWriter, r *http.Request) error {
	return ok(w, notify.Events())
}

func (s *Server) notifyLog(w http.ResponseWriter, r *http.Request) error {
	if s.d.Notify == nil {
		return errNoNotify
	}
	limit, err := qInt(r, "limit", 200)
	if err != nil {
		return err
	}
	if limit < 1 || limit > 200 {
		return apperr.Invalid("limit", "must be between 1 and 200")
	}
	return ok(w, s.d.Notify.Log(limit))
}
