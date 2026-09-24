package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/listing"
	"github.com/hustenreizjuengling/picache/internal/logs"
)

// GroupView is a content group as shown in the Library: the store
// aggregate plus its display label (UserLabel: set by the user, not
// built-in) and the number of clients that downloaded it.
type GroupView struct {
	cachestore.GroupUsage `json:",inline"`
	Label                 string `json:"label"`
	UserLabel             bool   `json:"userLabel"`
	Clients               int    `json:"clients"`
}

// GroupDetail is the response of GET /cache/groups/detail.
type GroupDetail struct {
	Group   GroupView                       `json:"group"`
	Clients []logs.GroupClient              `json:"clients"`
	Objects listing.Page[cachestore.Object] `json:"objects"`
}

// errNoCacheStore is returned by cache endpoints while no store is online.
var errNoCacheStore = apperr.Unavailable("no cache store is online")

// registerCacheRoutes registers the cache endpoints (docs/API.md).
func (s *Server) registerCacheRoutes() {
	s.route("GET /api/v1/cache/state", permRead, s.cacheState)
	s.route("GET /api/v1/cache/services", permRead, s.cacheServices)
	s.route("GET /api/v1/cache/groups", permRead, s.cacheGroups)
	s.route("GET /api/v1/cache/groups/detail", permRead, s.cacheGroupDetail)
	s.route("GET /api/v1/cache/objects", permRead, s.cacheObjects)
	s.route("DELETE /api/v1/cache/objects/{id}", permAdmin, s.cacheDeleteObject)
	s.route("POST /api/v1/cache/objects/{id}/pin", permAdmin, s.cachePinObject)
	s.route("POST /api/v1/cache/groups/delete", permAdmin, s.cacheDeleteGroup)
	s.route("POST /api/v1/cache/groups/pin", permAdmin, s.cachePinGroup)
	s.route("POST /api/v1/cache/services/{service}/purge", permAdmin, s.cachePurgeService)
	s.route("POST /api/v1/cache/evict", permAdmin, s.cacheEvict)
	s.route("POST /api/v1/cache/verify", permAdmin, s.cacheStartVerify)
	s.route("GET /api/v1/cache/verify", permRead, s.cacheVerifyState)
}

// cacheStore returns the online store or 503.
func (s *Server) cacheStore() (*cachestore.Store, error) {
	if st := s.d.Runtime.ActiveStore(); st != nil {
		return st, nil
	}
	return nil, errNoCacheStore
}

// cacheErr maps a store that was closed underneath a request (store switch
// or offline) to 503.
func cacheErr(err error) error {
	if errors.Is(err, cachestore.ErrClosed) {
		return errNoCacheStore
	}
	return err
}

// cacheRetention is the inactive retention used for ExpiresAt.
func (s *Server) cacheRetention() time.Duration {
	if s.d.Settings == nil {
		return 0
	}
	return time.Duration(s.d.Settings.Get().Cache.MaxAgeDays) * 24 * time.Hour
}

func (s *Server) cacheState(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Runtime.StoreState())
}

func (s *Server) cacheServices(w http.ResponseWriter, r *http.Request) error {
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	out, err := st.Services(r.Context())
	if err != nil {
		return cacheErr(err)
	}
	return ok(w, out)
}

// cachePaging reads sort, desc, limit and offset.
func cachePaging(r *http.Request) (sort string, desc bool, limit, offset int, err error) {
	if limit, err = qInt(r, "limit", 0); err != nil {
		return
	}
	if offset, err = qInt(r, "offset", 0); err != nil {
		return
	}
	return qString(r, "sort"), qBool(r, "desc"), limit, offset, nil
}

func (s *Server) cacheGroups(w http.ResponseWriter, r *http.Request) error {
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	sort, desc, limit, offset, err := cachePaging(r)
	if err != nil {
		return err
	}
	q := cachestore.GroupQuery{Service: qString(r, "service"), Search: qString(r, "search"), Sort: sort, Desc: desc,
		Limit: limit, Offset: offset, Retention: s.cacheRetention()}
	if q.Search != "" && s.d.Services != nil {
		q.SearchKeys = s.d.Services.SearchLabels(q.Search)
	}
	page, err := st.Groups(r.Context(), q)
	if err != nil {
		return cacheErr(err)
	}
	return ok(w, listing.Page[GroupView]{Items: s.groupViews(r.Context(), page.Items), Total: page.Total})
}

// groupViews adds labels and client counts. Client counts come from the
// logs database; if that is unavailable they are reported as 0.
func (s *Server) groupViews(ctx context.Context, groups []cachestore.GroupUsage) []GroupView {
	out := make([]GroupView, len(groups))
	refs := make([]logs.GroupRef, len(groups))
	for i, g := range groups {
		out[i] = GroupView{GroupUsage: g, Label: g.GroupKey}
		refs[i] = logs.GroupRef{Service: g.Service, GroupKey: g.GroupKey}
	}
	if len(groups) == 0 {
		return out
	}
	if s.d.Services != nil {
		for i := range out {
			if l, ok := s.d.Services.UserLabel(out[i].GroupKey); ok {
				out[i].Label, out[i].UserLabel = l, true
			} else if l := s.d.Services.Label(out[i].GroupKey); l != "" {
				out[i].Label = l
			}
		}
	}
	if s.d.Logs != nil {
		counts, err := s.d.Logs.GroupClientCounts(ctx, refs)
		if err != nil {
			s.log.Debug("group client counts unavailable", slog.Any("err", err))
		}
		for i := range out {
			out[i].Clients = counts[refs[i]]
		}
	}
	return out
}

func (s *Server) cacheGroupDetail(w http.ResponseWriter, r *http.Request) error {
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	service, key := qString(r, "service"), qString(r, "key")
	if service == "" || key == "" {
		return apperr.Invalid("key", "service and key are required")
	}
	sort, desc, limit, offset, err := cachePaging(r)
	if err != nil {
		return err
	}
	groups, err := st.Groups(r.Context(), cachestore.GroupQuery{Service: service, GroupKey: key, Limit: 1,
		Retention: s.cacheRetention()})
	if err != nil {
		return cacheErr(err)
	}
	if len(groups.Items) == 0 {
		return apperr.NotFound("group", key)
	}
	objects, err := st.Objects(r.Context(), cachestore.ObjectQuery{Service: service, GroupKey: key, Sort: sort, Desc: desc,
		Limit: limit, Offset: offset, Retention: s.cacheRetention()})
	if err != nil {
		return cacheErr(err)
	}
	clients := []logs.GroupClient{}
	if s.d.Logs != nil {
		if c, err := s.d.Logs.GroupClients(r.Context(), service, key); err != nil {
			s.log.Debug("group clients unavailable", slog.Any("err", err))
		} else if c != nil {
			clients = c
		}
	}
	return ok(w, GroupDetail{Group: s.groupViews(r.Context(), groups.Items)[0], Clients: clients, Objects: objects})
}

func (s *Server) cacheObjects(w http.ResponseWriter, r *http.Request) error {
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	sort, desc, limit, offset, err := cachePaging(r)
	if err != nil {
		return err
	}
	page, err := st.Objects(r.Context(), cachestore.ObjectQuery{Service: qString(r, "service"),
		GroupKey: qString(r, "group"), Search: qString(r, "search"), Sort: sort, Desc: desc, Limit: limit,
		Offset: offset, Retention: s.cacheRetention()})
	if err != nil {
		return cacheErr(err)
	}
	return ok(w, page)
}

// cacheObjectID validates the {id} path value.
func cacheObjectID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !cachestore.ValidObjectID(id) {
		return "", apperr.Invalid("id", "malformed object id")
	}
	return id, nil
}

func (s *Server) cacheDeleteObject(w http.ResponseWriter, r *http.Request) error {
	id, err := cacheObjectID(r)
	if err != nil {
		return err
	}
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	if err := st.DeleteObject(r.Context(), id); err != nil {
		return cacheErr(err)
	}
	s.audit(r, "cache.object.delete", id, nil)
	return noContent(w)
}

func (s *Server) cachePinObject(w http.ResponseWriter, r *http.Request) error {
	id, err := cacheObjectID(r)
	if err != nil {
		return err
	}
	var in struct {
		Pinned *bool `json:"pinned"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Pinned == nil {
		return apperr.Invalid("pinned", "required")
	}
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	if err := st.SetPinned(r.Context(), id, *in.Pinned); err != nil {
		return cacheErr(err)
	}
	s.audit(r, "cache.object.pin", id, map[string]bool{"pinned": *in.Pinned})
	return noContent(w)
}

func (s *Server) cacheDeleteGroup(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Service string `json:"service"`
		Key     string `json:"key"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	extendDeadlines(w, 10*time.Minute) // removes many files on a slow NAS
	freed, err := st.DeleteGroup(r.Context(), in.Service, in.Key)
	if err != nil {
		return cacheErr(err)
	}
	s.audit(r, "cache.group.delete", in.Service+" "+in.Key, map[string]int64{"bytesFreed": freed})
	return ok(w, map[string]int64{"bytesFreed": freed})
}

func (s *Server) cachePinGroup(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Service string `json:"service"`
		Key     string `json:"key"`
		Pinned  *bool  `json:"pinned"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if in.Pinned == nil {
		return apperr.Invalid("pinned", "required")
	}
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	if err := st.SetGroupPinned(r.Context(), in.Service, in.Key, *in.Pinned); err != nil {
		return cacheErr(err)
	}
	s.audit(r, "cache.group.pin", in.Service+" "+in.Key, map[string]bool{"pinned": *in.Pinned})
	return noContent(w)
}

func (s *Server) cachePurgeService(w http.ResponseWriter, r *http.Request) error {
	service := r.PathValue("service")
	st, err := s.cacheStore()
	if err != nil {
		return err
	}
	extendDeadlines(w, 10*time.Minute)
	freed, err := st.DeleteService(r.Context(), service)
	if err != nil {
		return cacheErr(err)
	}
	s.audit(r, "cache.service.purge", service, map[string]int64{"bytesFreed": freed})
	return ok(w, map[string]int64{"bytesFreed": freed})
}

func (s *Server) cacheEvict(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.cacheStore(); err != nil {
		return err
	}
	extendDeadlines(w, 10*time.Minute)
	res, err := s.d.Runtime.EvictNow(r.Context())
	if err != nil {
		return cacheErr(err)
	}
	s.audit(r, "cache.evict", "", res)
	return ok(w, res)
}

func (s *Server) cacheStartVerify(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Repair bool `json:"repair"`
	}
	if err := decode(w, r, &in); err != nil {
		return err
	}
	if _, err := s.cacheStore(); err != nil {
		return err
	}
	if err := s.d.Runtime.StartVerify(in.Repair); err != nil {
		return cacheErr(err)
	}
	s.audit(r, "cache.verify", "", map[string]bool{"repair": in.Repair})
	return writeJSON(w, http.StatusAccepted, s.d.Runtime.VerifyState())
}

func (s *Server) cacheVerifyState(w http.ResponseWriter, r *http.Request) error {
	if _, err := s.cacheStore(); err != nil {
		return err
	}
	return ok(w, s.d.Runtime.VerifyState())
}
