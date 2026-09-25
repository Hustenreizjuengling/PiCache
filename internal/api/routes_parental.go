package api

import (
	"net/http"
	"strconv"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
)

// registerParentalRoutes registers the parental controls endpoints
// (docs/API.md). Read tokens see the catalogue, the groups' controls and
// their state; changes need admin rights and are audited.
func (s *Server) registerParentalRoutes() {
	s.route("GET /api/v1/parental/services", permRead, s.parentalServices)
	s.route("GET /api/v1/parental/groups", permRead, s.parentalGroups)
	s.route("GET /api/v1/parental/groups/{id}", permRead, s.parentalGroup)
	s.route("PUT /api/v1/parental/groups/{id}", permAdmin, s.parentalUpdate)
	s.route("PUT /api/v1/parental/groups/{id}/override", permAdmin, s.parentalOverride)
	s.route("DELETE /api/v1/parental/groups/{id}/override", permAdmin, s.parentalOverrideClear)
}

var errNoParental = apperr.Unavailable("parental controls are not available")

func (s *Server) parentalServices(w http.ResponseWriter, r *http.Request) error {
	return ok(w, parental.Services())
}

func (s *Server) parentalGroups(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	gs, err := s.d.Parental.List(r.Context())
	if err != nil {
		return err
	}
	return ok(w, gs)
}

func (s *Server) parentalGroup(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	gc, err := s.d.Parental.Get(r.Context(), id)
	if err != nil {
		return err
	}
	return ok(w, gc)
}

func (s *Server) parentalUpdate(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in parental.Config
	if err := decode(w, r, &in); err != nil {
		return err
	}
	gc, err := s.d.Parental.Update(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "parental.update", strconv.FormatInt(id, 10),
		parental.Config{BlockedServices: gc.BlockedServices, Schedules: gc.Schedules})
	return ok(w, gc)
}

func (s *Server) parentalOverride(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in parental.OverrideInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	gc, err := s.d.Parental.SetOverride(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "parental.override", strconv.FormatInt(id, 10), gc.Override)
	return ok(w, gc)
}

func (s *Server) parentalOverrideClear(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	gc, err := s.d.Parental.ClearOverride(r.Context(), id)
	if err != nil {
		return err
	}
	s.audit(r, "parental.override_clear", strconv.FormatInt(id, 10), nil)
	return ok(w, gc)
}
