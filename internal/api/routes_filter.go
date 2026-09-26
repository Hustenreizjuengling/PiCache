package api

import (
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

// registerFilterRoutes registers the filter endpoints (docs/API.md).
func (s *Server) registerFilterRoutes() {
	s.route("GET /api/v1/filter/lists", permRead, s.filterLists)
	s.route("POST /api/v1/filter/lists", permAdmin, s.filterCreateList)
	s.route("PUT /api/v1/filter/lists/{id}", permAdmin, s.filterUpdateList)
	s.route("DELETE /api/v1/filter/lists/{id}", permAdmin, s.filterDeleteList)
	s.route("POST /api/v1/filter/lists/{id}/refresh", permAdmin, s.filterRefreshList, routeExempt)
	s.route("POST /api/v1/filter/lists/refresh", permAdmin, s.filterRefreshAll, routeExempt)
	s.route("GET /api/v1/filter/catalog", permRead, s.filterCatalog)
	s.route("GET /api/v1/filter/rules", permRead, s.filterRules)
	s.route("POST /api/v1/filter/rules", permAdmin, s.filterCreateRule)
	s.route("PUT /api/v1/filter/rules/{id}", permAdmin, s.filterUpdateRule)
	s.route("DELETE /api/v1/filter/rules/{id}", permAdmin, s.filterDeleteRule)
	s.route("GET /api/v1/filter/stats", permRead, s.filterStats)
	s.route("POST /api/v1/filter/explain", permRead, s.filterExplain)
}

// listRefreshTimeout bounds POST /filter/lists/{id}/refresh (the engine
// gives up after 5 min; the extra minute covers queueing behind a running
// download and the response).
const listRefreshTimeout = 6 * time.Minute

func (s *Server) filterLists(w http.ResponseWriter, r *http.Request) error {
	lists, err := s.d.Filter.Lists(r.Context())
	if err != nil {
		return err
	}
	return ok(w, lists)
}

func (s *Server) filterCreateList(w http.ResponseWriter, r *http.Request) error {
	var in filter.ListInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	l, err := s.d.Filter.CreateList(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "filter.list.create", strconv.FormatInt(l.ID, 10), listAudit(l))
	return created(w, l)
}

func (s *Server) filterUpdateList(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in filter.ListInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	l, err := s.d.Filter.UpdateList(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "filter.list.update", strconv.FormatInt(id, 10), listAudit(l))
	return ok(w, l)
}

func (s *Server) filterDeleteList(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.Filter.DeleteList(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "filter.list.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}

func (s *Server) filterRefreshList(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, listRefreshTimeout)
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	l, err := s.d.Filter.RefreshList(r.Context(), id)
	if err != nil {
		return err
	}
	s.audit(r, "filter.list.refresh", strconv.FormatInt(id, 10), map[string]string{"status": l.Status})
	return ok(w, l)
}

func (s *Server) filterRefreshAll(w http.ResponseWriter, r *http.Request) error {
	if err := s.d.Filter.RefreshAll(r.Context()); err != nil {
		return err
	}
	s.audit(r, "filter.lists.refresh", "", nil)
	return writeJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}

func (s *Server) filterCatalog(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Filter.Catalog())
}

func (s *Server) filterRules(w http.ResponseWriter, r *http.Request) error {
	rules, err := s.d.Filter.Rules(r.Context(), filter.RuleQuery{
		Action: qString(r, "action"), Type: qString(r, "type"), Search: qString(r, "search"),
	})
	if err != nil {
		return err
	}
	return ok(w, rules)
}

func (s *Server) filterCreateRule(w http.ResponseWriter, r *http.Request) error {
	var in filter.RuleInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	rule, err := s.d.Filter.CreateRule(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "filter.rule.create", strconv.FormatInt(rule.ID, 10), rule)
	return created(w, rule)
}

func (s *Server) filterUpdateRule(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in filter.RuleInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	rule, err := s.d.Filter.UpdateRule(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "filter.rule.update", strconv.FormatInt(id, 10), rule)
	return ok(w, rule)
}

func (s *Server) filterDeleteRule(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.Filter.DeleteRule(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "filter.rule.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}

func (s *Server) filterStats(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Filter.Stats())
}

// explainRequest is the body of POST /filter/explain.
type explainRequest struct {
	Domain   string `json:"domain"`
	ClientIP string `json:"clientIp"` // evaluate as this client (default: the caller)
}

// explainDecision is the effective filter decision for the explained name.
type explainDecision struct {
	Action string `json:"action"` // none | allow | block
	Name   string `json:"name"`   // list name or rule pattern
	Source string `json:"source"` // list | rule ("" for none)
	Kind   string `json:"kind"`   // exact | subtree | regex ("" for none)
}

// explainResponse answers "why is this (not) blocked?".
type explainResponse struct {
	Domain   string          `json:"domain"`
	GroupIDs []int64         `json:"groupIds"`
	Matches  []filter.Match  `json:"matches"`
	Decision explainDecision `json:"decision"`
}

func (s *Server) filterExplain(w http.ResponseWriter, r *http.Request) error {
	var in explainRequest
	if err := decode(w, r, &in); err != nil {
		return err
	}
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(in.Domain)), ".")
	if domain == "" {
		return apperr.Invalid("domain", "is required")
	}
	ip := netutil.AddrFromRemote(r.RemoteAddr)
	if in.ClientIP = strings.TrimSpace(in.ClientIP); in.ClientIP != "" {
		addr, err := netip.ParseAddr(in.ClientIP)
		if err != nil {
			return apperr.Invalid("clientIp", "must be an IP address")
		}
		ip = netutil.Canon(addr)
	}
	groups := []int64{}
	if id := s.d.Clients.Identify(ip); id != nil && id.GroupIDs != nil {
		groups = id.GroupIDs
	}
	matches, err := s.d.Filter.Explain(r.Context(), domain, groups)
	if err != nil {
		return err
	}
	if matches == nil {
		matches = []filter.Match{}
	}
	d := s.d.Filter.Check(domain, groups)
	return ok(w, explainResponse{
		Domain: domain, GroupIDs: groups, Matches: matches,
		Decision: explainDecision{Action: d.Action.String(), Name: d.Name, Source: d.Source, Kind: d.Kind},
	})
}

// listAudit returns the audit details of a list; the URL is recorded without
// its query string (it may carry an access token).
func listAudit(l filter.List) map[string]any {
	u := l.URL
	if p, err := url.Parse(u); err == nil {
		p.RawQuery, p.ForceQuery, p.User = "", false, nil
		u = p.String()
	}
	return map[string]any{"name": l.Name, "url": u, "kind": l.Kind, "enabled": l.Enabled, "groupIds": l.GroupIDs}
}
