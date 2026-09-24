package api

import (
	"context"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/lancache/services"
)

// lanCacheListDomains is how many domains per service the list view returns.
const lanCacheListDomains = 50

// lanCacheRefreshWait bounds POST /lancache/source/refresh.
const lanCacheRefreshWait = 5 * time.Minute

// registerLanCacheRoutes registers the lancache endpoints (docs/API.md).
func (s *Server) registerLanCacheRoutes() {
	s.route("GET /api/v1/lancache/services", permRead, s.lanCacheServices)
	s.route("GET /api/v1/lancache/services/{id}", permRead, s.lanCacheService)
	s.route("PUT /api/v1/lancache/services/{id}/enabled", permAdmin, s.lanCacheSetEnabled)
	s.route("PUT /api/v1/lancache/services/{id}/domains", permAdmin, s.lanCacheSetDomains)
	s.route("POST /api/v1/lancache/services", permAdmin, s.lanCacheCreate)
	s.route("PUT /api/v1/lancache/services/{id}", permAdmin, s.lanCacheUpdate)
	s.route("DELETE /api/v1/lancache/services/{id}", permAdmin, s.lanCacheDelete)
	s.route("GET /api/v1/lancache/source", permRead, s.lanCacheSource)
	s.route("POST /api/v1/lancache/source/refresh", permAdmin, s.lanCacheRefresh)
	s.route("PUT /api/v1/lancache/labels", permAdmin, s.lanCacheSetLabel)
	s.route("GET /api/v1/lancache/sni", permRead, s.lanCacheSNI)
}

// lanCacheID validates the {id} path value.
func lanCacheID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !services.ValidServiceID(id) {
		return "", apperr.Invalid("id", "invalid service id")
	}
	return id, nil
}

func (s *Server) lanCacheServices(w http.ResponseWriter, r *http.Request) error {
	list, err := s.d.Services.Services(r.Context())
	if err != nil {
		return err
	}
	for i := range list {
		if len(list[i].Domains) > lanCacheListDomains {
			list[i].Domains = list[i].Domains[:lanCacheListDomains:lanCacheListDomains]
		}
	}
	return ok(w, list)
}

func (s *Server) lanCacheService(w http.ResponseWriter, r *http.Request) error {
	id, err := lanCacheID(r)
	if err != nil {
		return err
	}
	sv, err := s.d.Services.Service(r.Context(), id)
	if err != nil {
		return err
	}
	return ok(w, sv)
}

func (s *Server) lanCacheSetEnabled(w http.ResponseWriter, r *http.Request) error {
	id, err := lanCacheID(r)
	if err != nil {
		return err
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
	if err := s.d.Services.SetEnabled(r.Context(), id, *in.Enabled); err != nil {
		return err
	}
	s.audit(r, "lancache.service.enable", id, map[string]any{"enabled": *in.Enabled})
	return s.lanCacheService(w, r)
}

func (s *Server) lanCacheSetDomains(w http.ResponseWriter, r *http.Request) error {
	id, err := lanCacheID(r)
	if err != nil {
		return err
	}
	var in struct {
		ExtraDomains []string `json:"extraDomains"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.d.Services.SetExtraDomains(r.Context(), id, in.ExtraDomains); err != nil {
		return err
	}
	s.audit(r, "lancache.service.domains", id, map[string]any{"extraDomains": in.ExtraDomains})
	return s.lanCacheService(w, r)
}

func (s *Server) lanCacheCreate(w http.ResponseWriter, r *http.Request) error {
	var in services.ServiceInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	sv, err := s.d.Services.CreateCustom(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "lancache.service.create", sv.ID, in)
	return created(w, sv)
}

func (s *Server) lanCacheUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := lanCacheID(r)
	if err != nil {
		return err
	}
	var in services.ServiceInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	sv, err := s.d.Services.UpdateCustom(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "lancache.service.update", id, in)
	return ok(w, sv)
}

func (s *Server) lanCacheDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := lanCacheID(r)
	if err != nil {
		return err
	}
	if err := s.d.Services.DeleteCustom(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "lancache.service.delete", id, nil)
	return noContent(w)
}

func (s *Server) lanCacheSource(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Services.Status())
}

func (s *Server) lanCacheRefresh(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, lanCacheRefreshWait+30*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), lanCacheRefreshWait)
	defer cancel()
	if err := s.d.Services.Refresh(ctx); err != nil {
		return err
	}
	s.audit(r, "lancache.source.refresh", "", nil)
	return ok(w, s.d.Services.Status())
}

func (s *Server) lanCacheSetLabel(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		GroupKey string `json:"groupKey"`
		Label    string `json:"label"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if err := s.d.Services.SetLabel(r.Context(), in.GroupKey, in.Label); err != nil {
		return err
	}
	s.audit(r, "lancache.label.set", in.GroupKey, map[string]any{"label": in.Label})
	return noContent(w)
}

func (s *Server) lanCacheSNI(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.SNI.Stats())
}
