package api

import (
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// registerFilterRoutes registers the filter endpoints (docs/API.md).
func (s *Server) registerFilterRoutes() {
	s.route("GET /api/v1/filter/lists", permRead, s.filterLists)
	s.route("POST /api/v1/filter/lists", permAdmin, s.filterCreateList)
	s.route("POST /api/v1/filter/lists/batch", permAdmin, s.filterBatchLists)
	s.route("PUT /api/v1/filter/lists/{id}", permAdmin, s.filterUpdateList)
	s.route("DELETE /api/v1/filter/lists/{id}", permAdmin, s.filterDeleteList)
	s.route("POST /api/v1/filter/lists/{id}/refresh", permAdmin, s.filterRefreshList, routeExempt)
	s.route("POST /api/v1/filter/lists/refresh", permAdmin, s.filterRefreshAll, routeExempt)
	s.route("GET /api/v1/filter/catalog", permRead, s.filterCatalog)
	s.route("GET /api/v1/filter/rules", permRead, s.filterRules)
	s.route("POST /api/v1/filter/rules", permAdmin, s.filterCreateRule)
	s.route("GET /api/v1/filter/rules/export", permRead, s.filterExportRules)
	s.route("POST /api/v1/filter/rules/import", permAdmin, s.filterImportRules)
	s.route("POST /api/v1/filter/rules/batch", permAdmin, s.filterBatchRules)
	s.route("POST /api/v1/filter/rules/device", permAdmin, s.filterDeviceRule)
	s.route("PUT /api/v1/filter/rules/{id}", permAdmin, s.filterUpdateRule)
	s.route("DELETE /api/v1/filter/rules/{id}", permAdmin, s.filterDeleteRule)
	s.route("GET /api/v1/filter/ip-rules", permRead, s.filterIPRules)
	s.route("POST /api/v1/filter/ip-rules", permAdmin, s.filterCreateIPRule)
	s.route("POST /api/v1/filter/ip-rules/batch", permAdmin, s.filterBatchIPRules)
	s.route("PUT /api/v1/filter/ip-rules/{id}", permAdmin, s.filterUpdateIPRule)
	s.route("DELETE /api/v1/filter/ip-rules/{id}", permAdmin, s.filterDeleteIPRule)
	s.route("GET /api/v1/filter/stats", permRead, s.filterStats)
	s.route("POST /api/v1/filter/explain", permRead, s.filterExplain)
	s.route("GET /api/v1/filter/search", permRead, s.filterSearch)
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

// filterBatchLists deletes, enables or disables lists; enabling beyond the
// entry budget needs force (409 on field force otherwise).
func (s *Server) filterBatchLists(w http.ResponseWriter, r *http.Request) error {
	in, err := decodeBatch(w, r, true)
	if err != nil {
		return err
	}
	n, err := s.d.Filter.BatchLists(r.Context(), in.Action, in.IDs, in.Force != nil && *in.Force)
	if err != nil {
		return err
	}
	s.auditBatch(r, "filter.list.batch", in)
	return ok(w, batchResult{Changed: n})
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

// ruleQuery reads the filters of GET /filter/rules (and its export).
func ruleQuery(r *http.Request) filter.RuleQuery {
	return filter.RuleQuery{Action: qString(r, "action"), Type: qString(r, "type"), Search: qString(r, "search")}
}

func (s *Server) filterRules(w http.ResponseWriter, r *http.Request) error {
	rules, err := s.d.Filter.Rules(r.Context(), ruleQuery(r))
	if err != nil {
		return err
	}
	return ok(w, rules)
}

// filterExportRules downloads the rules (the filters of GET /filter/rules)
// as text that the import reads back.
func (s *Server) filterExportRules(w http.ResponseWriter, r *http.Request) error {
	rules, err := s.d.Filter.Rules(r.Context(), ruleQuery(r))
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="picache-rules.txt"`)
	if err := filter.WriteRuleExport(w, rules, time.Now()); err != nil {
		return errAlreadyWritten{err}
	}
	return nil
}

// filterImportRules imports rules from text (all or nothing; dry runs
// validate only); 200 with the result unless the request itself is
// invalid. Only an applied import is audited.
func (s *Server) filterImportRules(w http.ResponseWriter, r *http.Request) error {
	var in filter.RuleImport
	if err := decode(w, r, &in); err != nil {
		return err
	}
	res, err := s.d.Filter.ImportRules(r.Context(), in)
	if err != nil {
		return err
	}
	if res.Applied {
		s.audit(r, "filter.rule.import", "", map[string]int{"added": res.Added})
	}
	return ok(w, res)
}

func (s *Server) filterBatchRules(w http.ResponseWriter, r *http.Request) error {
	in, err := decodeBatch(w, r, false)
	if err != nil {
		return err
	}
	n, err := s.d.Filter.BatchRules(r.Context(), in.Action, in.IDs)
	if err != nil {
		return err
	}
	s.auditBatch(r, "filter.rule.batch", in)
	return ok(w, batchResult{Changed: n})
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

// --- IP rules ---

func (s *Server) filterIPRules(w http.ResponseWriter, r *http.Request) error {
	rules, err := s.d.Filter.IPRules(r.Context(), filter.IPRuleQuery{Action: qString(r, "action"), Search: qString(r, "search")})
	if err != nil {
		return err
	}
	return ok(w, rules)
}

func (s *Server) filterCreateIPRule(w http.ResponseWriter, r *http.Request) error {
	var in filter.IPRuleInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	rule, err := s.d.Filter.CreateIPRule(r.Context(), in)
	if err != nil {
		return err
	}
	s.audit(r, "filter.ip_rule.create", strconv.FormatInt(rule.ID, 10), rule)
	return created(w, rule)
}

func (s *Server) filterUpdateIPRule(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	var in filter.IPRuleInput
	if err := decode(w, r, &in); err != nil {
		return err
	}
	rule, err := s.d.Filter.UpdateIPRule(r.Context(), id, in)
	if err != nil {
		return err
	}
	s.audit(r, "filter.ip_rule.update", strconv.FormatInt(id, 10), rule)
	return ok(w, rule)
}

func (s *Server) filterDeleteIPRule(w http.ResponseWriter, r *http.Request) error {
	id, err := pathID(r, "id")
	if err != nil {
		return err
	}
	if err := s.d.Filter.DeleteIPRule(r.Context(), id); err != nil {
		return err
	}
	s.audit(r, "filter.ip_rule.delete", strconv.FormatInt(id, 10), nil)
	return noContent(w)
}

func (s *Server) filterBatchIPRules(w http.ResponseWriter, r *http.Request) error {
	in, err := decodeBatch(w, r, false)
	if err != nil {
		return err
	}
	n, err := s.d.Filter.BatchIPRules(r.Context(), in.Action, in.IDs)
	if err != nil {
		return err
	}
	s.auditBatch(r, "filter.ip_rule.batch", in)
	return ok(w, batchResult{Changed: n})
}

func (s *Server) filterStats(w http.ResponseWriter, r *http.Request) error {
	return ok(w, s.d.Filter.Stats())
}

// explainRequest is the body of POST /filter/explain.
type explainRequest struct {
	Domain   string `json:"domain"`
	ClientIP string `json:"clientIp"` // evaluate as this client (default: the caller)
	QType    string `json:"qtype"`    // default A
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
	QType    string          `json:"qtype"`
	GroupIDs []int64         `json:"groupIds"`
	Matches  []filter.Match  `json:"matches"`
	Decision explainDecision `json:"decision"`
}

// clientGroups returns the enabled groups of the client ip (none: []).
func (s *Server) clientGroups(ip netip.Addr) []int64 {
	groups := []int64{}
	if id := s.d.Clients.Identify(ip); id != nil && id.GroupIDs != nil {
		groups = id.GroupIDs
	}
	return groups
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
	qtype := uint16(1) // A
	if v := strings.TrimSpace(in.QType); v != "" {
		t, err := settings.ParseQType(v)
		if err != nil {
			return apperr.Invalid("qtype", "unknown record type %q", v)
		}
		qtype = t
	}
	ip := netutil.AddrFromRemote(r.RemoteAddr)
	if in.ClientIP = strings.TrimSpace(in.ClientIP); in.ClientIP != "" {
		addr, err := netip.ParseAddr(in.ClientIP)
		if err != nil {
			return apperr.Invalid("clientIp", "must be an IP address")
		}
		ip = netutil.Canon(addr)
	}
	groups := s.clientGroups(ip)
	matches, err := s.d.Filter.Explain(r.Context(), domain, qtype, groups)
	if err != nil {
		return err
	}
	if matches == nil {
		matches = []filter.Match{}
	}
	d := s.d.Filter.Check(domain, qtype, groups)
	return ok(w, explainResponse{
		Domain: domain, QType: settings.QTypeName(qtype), GroupIDs: groups, Matches: matches,
		Decision: explainDecision{Action: d.Action.String(), Name: d.Name, Source: d.Source, Kind: d.Kind},
	})
}

// filterSearch searches the rules and the local copies of the enabled
// lists (GET /filter/search?q&limit&clientIp).
func (s *Server) filterSearch(w http.ResponseWriter, r *http.Request) error {
	extendDeadlines(w, 30*time.Second)
	limit, err := qInt(r, "limit", 200)
	if err != nil {
		return err
	}
	var groups []int64
	client := false
	if v := qString(r, "clientIp"); v != "" {
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return apperr.Invalid("clientIp", "must be an IP address")
		}
		groups, client = s.clientGroups(netutil.Canon(ip)), true
	}
	res, err := s.d.Filter.Search(r.Context(), qString(r, "q"), limit, groups, client)
	if err != nil {
		return err
	}
	return ok(w, res)
}

// --- Only for this device ---

// deviceRuleRequest is the body of POST /filter/rules/device.
type deviceRuleRequest struct {
	ClientIP string           `json:"clientIp"`
	Rule     filter.RuleInput `json:"rule"`
}

// deviceRuleResponse says what POST /filter/rules/device did.
type deviceRuleResponse struct {
	Client        clients.Client `json:"client"`
	Group         clients.Group  `json:"group"`
	Rule          filter.Rule    `json:"rule"`
	CreatedClient bool           `json:"createdClient"`
	CreatedGroup  bool           `json:"createdGroup"`
	CreatedRule   bool           `json:"createdRule"`
}

// filterDeviceRule makes a rule apply to one device only ("Only for this
// device", the groups-based form of a client modifier): the device gets a
// client of its own (unless one describes it), a group that holds only
// that client, and the rule is added for that group. Refusals come first
// (nothing changed); when the rule cannot be saved, the clients change is
// undone. Requests are serialised (deviceMu), so an undo never removes a
// client or group another request reused.
func (s *Server) filterDeviceRule(w http.ResponseWriter, r *http.Request) error {
	var in deviceRuleRequest
	if err := decode(w, r, &in); err != nil {
		return err
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(in.ClientIP))
	if err != nil || ip.Zone() != "" {
		return apperr.Invalid("clientIp", "must be an IP address")
	}
	ip = netutil.Canon(ip)
	mac, protectedMAC, err := s.deviceRefusal(ip)
	if err != nil {
		return err
	}
	if s.d.Settings.Get().Logs.AnonymizeClientIPs {
		return apperr.Conflict("client addresses are anonymised; configure the device on Clients & groups")
	}
	if in.Rule.GroupIDs != nil {
		return apperr.Invalid("rule.groupIds", "the device group is chosen by the server")
	}
	if err := filter.ValidateRule(in.Rule); err != nil {
		if ae, ok := apperr.As(err); ok && ae.Kind == apperr.KindInvalid {
			return apperr.Invalid("rule."+ae.Field, "%s", ae.Message)
		}
		return err
	}
	req := clients.DeviceRequest{IP: ip, Name: s.d.Clients.DeviceName(ip, mac)}
	if !protectedMAC {
		req.MAC = mac
	}
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	dev, err := s.d.Clients.EnsureDeviceGroup(r.Context(), req)
	if err != nil {
		return err
	}
	rule, createdRule, err := s.d.Filter.AddRuleForGroup(r.Context(), in.Rule, dev.Group.ID)
	if err != nil {
		if rerr := s.d.Clients.RevertDeviceGroup(r.Context(), dev); rerr != nil {
			s.log.Error("undo the device group of a refused rule", slog.Any("err", rerr))
		}
		return err
	}
	s.audit(r, "filter.rule.device", strconv.FormatInt(rule.ID, 10), map[string]any{
		"clientId": dev.Client.ID, "groupId": dev.Group.ID, "createdClient": dev.CreatedClient,
		"createdGroup": dev.CreatedGroup, "createdRule": createdRule})
	return ok(w, deviceRuleResponse{Client: dev.Client, Group: dev.Group, Rule: rule,
		CreatedClient: dev.CreatedClient, CreatedGroup: dev.CreatedGroup, CreatedRule: createdRule})
}

// deviceRefusal refuses an address that is not a device of its own:
// loopback, unspecified, this machine, the router (any of its addresses)
// or a trusted EDNS forwarder (400 clientIp). It returns the address's
// neighbour-table MAC and whether that MAC is protected (the router's,
// this machine's or a trusted forwarder's: it never becomes an identifier).
func (s *Server) deviceRefusal(ip netip.Addr) (mac string, protected bool, err error) {
	switch {
	case ip.IsLoopback():
		return "", false, apperr.Invalid("clientIp", "%s is a loopback address; configure it on Clients & groups", ip)
	case ip.IsUnspecified() || ip.IsMulticast():
		return "", false, apperr.Invalid("clientIp", "%s is not a device address", ip)
	}
	mac, _ = s.neighbourMAC(ip)
	if s.d.DNS == nil {
		return mac, false, nil
	}
	for _, c := range s.d.DNS.ProtectedClients(s.d.Settings.Get().DNS.EDNSClientTrusted, s.neighbourMAC) {
		switch {
		case c.Addr.IsValid() && netutil.Canon(c.Addr) == ip:
			return "", false, apperr.Invalid("clientIp", "%s is %s; configure it on Clients & groups", ip, c.What)
		case c.MAC != "" && mac != "" && strings.EqualFold(c.MAC, mac):
			protected = true
		}
	}
	return mac, protected, nil
}

// listAudit returns the audit details of a list; the URL is recorded without
// its query string (it may carry an access token).
func listAudit(l filter.List) map[string]any {
	u := l.URL
	if p, err := url.Parse(u); err == nil {
		p.RawQuery, p.ForceQuery, p.User = "", false, nil
		u = p.String()
	}
	return map[string]any{"name": l.Name, "url": u, "kind": l.Kind, "enabled": l.Enabled, "groupIds": l.GroupIDs, "format": l.Format}
}
