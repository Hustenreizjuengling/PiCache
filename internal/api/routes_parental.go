package api

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strconv"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
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
	s.route("PUT /api/v1/parental/groups/{id}/pause", permAdmin, s.parentalPause)
	s.route("DELETE /api/v1/parental/groups/{id}/pause", permAdmin, s.parentalPauseClear)
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
	for i := range gs {
		s.fillCategories(&gs[i])
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
	s.fillCategories(&gc)
	return ok(w, gc)
}

// fillCategories sets the category switches of gc from the filter lists
// (parental never imports filter).
func (s *Server) fillCategories(gc *parental.GroupControls) {
	if s.d.Filter == nil {
		return
	}
	for name, st := range s.d.Filter.Presets(gc.GroupID) {
		if sw := gc.Categories.Switch(name); sw != nil {
			*sw = parental.CategorySwitch{On: st.On, State: st.State}
		}
	}
}

// parentalUpdateAudit are the details of the parental.update audit event.
type parentalUpdateAudit struct {
	BlockedServices []string            `json:"blockedServices"`
	Schedules       []parental.Schedule `json:"schedules"`
	SafeSearch      parental.SafeSearch `json:"safeSearch"`
	Categories      map[string]bool     `json:"categories"` // on after the change
}

// parentalUpdate stores a group's services, schedules and safe search and
// switches its category switches. The switches are list assignments: they
// are applied first (one filter transaction) and the lists they created or
// changed are audited at once; then the parental configuration is saved.
// If that fails, the list changes are undone exactly (created lists are
// deleted, changed ones restored) and the undo is audited too.
func (s *Server) parentalUpdate(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in parental.UpdateInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := parental.ValidateUpdate(in); err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := s.d.Parental.Get(ctx, id); err != nil {
		return err // 404 for an unknown group, before any list changes
	}
	want := in.Categories.Want()
	if len(want) > 0 && s.d.Filter == nil {
		return apperr.Unavailable("the category switches need the filter lists")
	}
	var change filter.PresetChange
	if len(want) > 0 {
		if change, err = s.d.Filter.SetPresets(ctx, id, want); err != nil {
			return err
		}
		for _, l := range change.Lists {
			action := "filter.list.update"
			if slices.Contains(change.Created, l.ID) {
				action = "filter.list.create"
			}
			s.audit(r, action, strconv.FormatInt(l.ID, 10), listAudit(l))
		}
	}
	gc, err := s.d.Parental.Update(ctx, id, in)
	if err != nil {
		if len(change.Lists) > 0 {
			s.revertPresets(r, id, change)
		}
		return err
	}
	s.fillCategories(&gc)
	details := parentalUpdateAudit{BlockedServices: gc.BlockedServices, Schedules: gc.Schedules, SafeSearch: gc.SafeSearch,
		Categories: map[string]bool{}}
	for _, name := range parental.CategorySwitchNames {
		details.Categories[name] = gc.Categories.Switch(name).On
	}
	s.audit(r, "parental.update", strconv.FormatInt(id, 10), details)
	return ok(w, gc)
}

// revertPresets undoes the list changes of a SetPresets call after the
// parental configuration could not be saved, and audits what it undid.
func (s *Server) revertPresets(r *http.Request, group int64, c filter.PresetChange) {
	deleted, restored, err := s.d.Filter.RevertPresets(context.WithoutCancel(r.Context()), c)
	if err != nil {
		s.log.Error("parental: could not undo the category switches", slog.Int64("group", group), slog.Any("err", err))
		return
	}
	for _, id := range deleted {
		s.audit(r, "filter.list.delete", strconv.FormatInt(id, 10), nil)
	}
	for _, l := range restored {
		s.audit(r, "filter.list.update", strconv.FormatInt(l.ID, 10), listAudit(l))
	}
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
	s.fillCategories(&gc)
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
	s.fillCategories(&gc)
	return ok(w, gc)
}

// parentalPause pauses the group's filtering (its lists and rules; parental
// controls, safe search and protection lists stay in force).
func (s *Server) parentalPause(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in parental.PauseInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	gc, err := s.d.Parental.SetPause(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "parental.pause", strconv.FormatInt(id, 10), map[string]any{"until": gc.State.PausedUntil})
	s.fillCategories(&gc)
	return ok(w, gc)
}

func (s *Server) parentalPauseClear(w http.ResponseWriter, r *http.Request) error {
	if s.d.Parental == nil {
		return errNoParental
	}
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	gc, err := s.d.Parental.ClearPause(r.Context(), id)
	if err != nil {
		return err
	}
	s.audit(r, "parental.pause_clear", strconv.FormatInt(id, 10), nil)
	s.fillCategories(&gc)
	return ok(w, gc)
}
