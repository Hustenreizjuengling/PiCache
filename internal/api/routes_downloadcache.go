package api

import (
	"context"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dlcache/services"
)

// downloadCacheListDomains is how many domains per service the list view returns.
const downloadCacheListDomains = 50

// downloadCacheRefreshWait bounds POST /download-cache/source/refresh.
const downloadCacheRefreshWait = 5 * time.Minute

// registerDownloadCacheRoutes registers the download cache endpoints (docs/API.md).
func (s *Server) registerDownloadCacheRoutes() {
	s.route("GET /api/v1/download-cache/services", permRead, s.downloadCacheServices)
	s.route("GET /api/v1/download-cache/services/{id}", permRead, s.downloadCacheService)
	s.route("PUT /api/v1/download-cache/services/{id}/enabled", permAdmin, s.downloadCacheSetEnabled)
	s.route("PUT /api/v1/download-cache/services/{id}/domains", permAdmin, s.downloadCacheSetDomains)
	s.route("POST /api/v1/download-cache/services", permAdmin, s.downloadCacheCreate)
	s.route("PUT /api/v1/download-cache/services/{id}", permAdmin, s.downloadCacheUpdate)
	s.route("DELETE /api/v1/download-cache/services/{id}", permAdmin, s.downloadCacheDelete)
	s.route("GET /api/v1/download-cache/source", permRead, s.downloadCacheSource)
	s.route("POST /api/v1/download-cache/source/refresh", permAdmin, s.downloadCacheRefresh, routeExempt)
	s.route("PUT /api/v1/download-cache/labels", permAdmin, s.downloadCacheSetLabel)
	s.route("GET /api/v1/download-cache/sni", permRead, s.downloadCacheSNI)
}

// downloadCacheID validates the {id} path value.
func downloadCacheID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !services.ValidServiceID(id) {
		return "", apperr.Invalid("id", "invalid service id")
	}
	return id, nil
}

func (s *Server) downloadCacheServices(w http.ResponseWriter, r *http.Request) error {
	list, err := s.d.Services.Services(r.Context())
	if err != nil {
		return err
	}
	for i := range list {
		if len(list[i].Domains) > downloadCacheListDomains {
			list[i].Domains = list[i].Domains[:downloadCacheListDomains:downloadCacheListDomains]
		}
	}
	return ok(w, list)
}

func (s *Server) downloadCacheService(w http.ResponseWriter, r *http.Request) error {
	id, err := downloadCacheID(r)
	if err != nil {
		return err
	}
	sv, err := s.d.Services.Service(r.Context(), id)
	if err != nil {
		return err
	}
	return ok(w, sv)
}

func (s *Server) downloadCacheSetEnabled(w http.ResponseWriter, r *http.Request) error {
	id, err := downloadCacheID(r)
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
	s.audit(r, "download_cache.service.enable", id, map[string]any{"enabled": *in.Enabled})
	return s.downloadCacheService(w, r)
}

func (s *Server) downloadCacheSetDomains(w http.ResponseWriter, r *http.Request) error {
	id, err := downloadCacheID(r)
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
	s.audit(r, "download_cache.service.domains", id, map[string]any{"extraDomains": in.ExtraDomains})
	return s.downloadCacheService(w, r)
}

func (s *Server) downloadCacheCreate(w http.ResponseWriter, r *http.Request) error {
	var in services.ServiceInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	sv, err := s.d.Services.CreateCustom(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "download_cache.service.create", sv.ID, in)
	return created(w, sv)
}

func (s *Server) downloadCacheUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := downloadCacheID(r)
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
	s.audit(r, "download_cache.service.update", id, in)
	return ok(w, sv)
}

func (s *Server) downloadCacheDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := downloadCacheID(r)
	if err != nil {
		return err
	}
	if err := s.d.Services.DeleteCustom(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "download_cache.service.delete", id, nil)
	return noContent(w)
}

func (s *Server) downloadCacheSource(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Services.Status())
}

func (s *Server) downloadCacheRefresh(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, downloadCacheRefreshWait+30*time.Second)
	ctx, cancel := context.WithTimeout(r.Context(), downloadCacheRefreshWait)
	defer cancel()
	if err := s.d.Services.Refresh(ctx); err != nil {
		return err
	}
	s.audit(r, "download_cache.source.refresh", "", nil)
	return ok(w, s.d.Services.Status())
}

func (s *Server) downloadCacheSetLabel(w http.ResponseWriter, r *http.Request) error {
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
	s.audit(r, "download_cache.label.set", in.GroupKey, map[string]any{"label": in.Label})
	return noContent(w)
}

func (s *Server) downloadCacheSNI(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.SNI.Stats())
}
