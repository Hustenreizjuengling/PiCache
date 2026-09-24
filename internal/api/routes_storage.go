package api

import (
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

// storageLongOp bounds the handlers that wait for the storage (test, init,
// activate): a hanging NAS is given up on by the storage package earlier.
const storageLongOp = 2 * time.Minute

// registerStorageRoutes registers the storage endpoints (docs/API.md).
func (s *Server) registerStorageRoutes() {
	s.route("GET /api/v1/storage/capabilities", permRead, s.storageCapabilities)
	s.route("GET /api/v1/storage/targets", permRead, s.storageTargets)
	s.route("GET /api/v1/storage/targets/{id}", permRead, s.storageTarget)
	s.route("POST /api/v1/storage/targets", permAdmin, s.storageCreate)
	s.route("PUT /api/v1/storage/targets/{id}", permAdmin, s.storageUpdate)
	s.route("DELETE /api/v1/storage/targets/{id}", permAdmin, s.storageDelete)
	s.route("POST /api/v1/storage/targets/{id}/test", permAdmin, s.storageTest)
	s.route("POST /api/v1/storage/targets/{id}/apply", permAdmin, s.storageApply)
	s.route("POST /api/v1/storage/targets/{id}/init", permAdmin, s.storageInit)
	s.route("POST /api/v1/storage/targets/{id}/activate", permAdmin, s.storageActivate)
	s.route("GET /api/v1/storage/targets/{id}/snippets", permAdmin, s.storageSnippets)
}

// storageTargetID validates the {id} path value ("local" or 32 hex chars).
func storageTargetID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !storage.ValidTargetID(id) {
		return "", apperr.Invalid("id", "invalid storage target id")
	}
	return id, nil
}

// storageActiveID is the target currently used by the cache.
func (s *Server) storageActiveID() string { return s.d.Settings.Get().Cache.ActiveStoreID }

func (s *Server) storageCapabilities(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Storage.Capabilities())
}

func (s *Server) storageTargets(w http.ResponseWriter, r *http.Request) error {
	list, err := s.d.Storage.Targets(r.Context(), s.storageActiveID())
	if err != nil {
		return err
	}
	if list == nil {
		list = []storage.TargetWithStatus{}
	}
	return ok(w, list)
}

func (s *Server) storageTarget(w http.ResponseWriter, r *http.Request) error {
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	t, err := s.d.Storage.Target(r.Context(), id)
	if err != nil {
		return err
	}
	return ok(w, storage.TargetWithStatus{Target: t, Status: s.d.Storage.Status(id), Active: id == s.storageActiveID()})
}

func (s *Server) storageCreate(w http.ResponseWriter, r *http.Request) error {
	var in storage.TargetInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	t, err := s.d.Storage.Create(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "storage.create", t.ID, t) // the Target never contains the password
	return created(w, t)
}

func (s *Server) storageUpdate(w http.ResponseWriter, r *http.Request) error {
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	var in storage.TargetInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	t, err := s.d.Storage.Update(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "storage.update", t.ID, t)
	return ok(w, t)
}

func (s *Server) storageDelete(w http.ResponseWriter, r *http.Request) error {
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	if err := s.d.Storage.Delete(r.Context(), id, s.storageActiveID()); err != nil {
		return err
	}
	s.audit(r, "storage.delete", id, nil)
	return noContent(w)
}

func (s *Server) storageTest(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, storageLongOp)
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	if _, err := s.d.Storage.Target(r.Context(), id); err != nil {
		return err
	}
	return ok(w, s.d.Storage.Test(r.Context(), id))
}

func (s *Server) storageApply(w http.ResponseWriter, r *http.Request) error {
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	st, err := s.d.Storage.RequestApply(r.Context(), id)
	if err != nil {
		return err
	}
	s.audit(r, "storage.apply", id, nil)
	return ok(w, st)
}

func (s *Server) storageInit(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, storageLongOp)
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	var in struct {
		Adopt bool `json:"adopt"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	res, err := s.d.Storage.InitStore(r.Context(), id, in.Adopt)
	if err != nil {
		return err
	}
	s.audit(r, "storage.init", id, res)
	return ok(w, res)
}

func (s *Server) storageActivate(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, storageLongOp)
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	if _, err := s.d.Storage.Target(r.Context(), id); err != nil {
		return err
	}
	if err := s.d.Runtime.ActivateStore(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "storage.activate", id, nil)
	return ok(w, s.d.Runtime.StoreState())
}

func (s *Server) storageSnippets(w http.ResponseWriter, r *http.Request) error {
	id, err := storageTargetID(r)
	if err != nil {
		return err
	}
	sn, err := s.d.Storage.Snippets(r.Context(), id)
	if err != nil {
		return err
	}
	return ok(w, sn)
}
