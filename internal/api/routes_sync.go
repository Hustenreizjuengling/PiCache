package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/auth"
	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/dns/filter"
	"github.com/hustenreizjuengling/picache/internal/dns/parental"
	dnsserver "github.com/hustenreizjuengling/picache/internal/dns/server"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/version"
)

// Follower sync (docs/ARCHITECTURE.md 15.4): the configuration export of a
// primary (GET /system/export, permission X) and the state of this
// follower (GET /system/sync, POST /system/sync/run). While a section is
// synced, every route that writes it answers 409 (routeSyncSection).

// ExportFormat and ExportFormatVersion identify an export.
const (
	ExportFormat        = "picache-export"
	ExportFormatVersion = 1
	// MaxExportBytes bounds an export (503 beyond).
	MaxExportBytes = 64 << 20
)

// ConfigExport is GET /system/export: the exportable sections of the
// primary, only data its R routes return; group references are ids plus
// the primary's group name table. ContentSHA256 is the lower-case hex
// SHA-256 of the deterministic JSON of Sections.
type ConfigExport struct {
	Format        string                    `json:"format"`
	FormatVersion int                       `json:"formatVersion"`
	Version       string                    `json:"version"`
	Schema        map[string]int            `json:"schema"`
	ExportedAt    time.Time                 `json:"exportedAt"`
	InstanceID    string                    `json:"instanceId"`
	ContentSHA256 string                    `json:"contentSha256"`
	Groups        []ExportGroupName         `json:"groups"`
	Sections      map[string]jsontext.Value `json:"sections"`
}

// ExportGroupName is one group of the primary's name table.
type ExportGroupName struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// ExportClientsGroups is the section clients-and-groups.
type ExportClientsGroups struct {
	Groups  []clients.Group  `json:"groups"`
	Clients []clients.Client `json:"clients"`
}

// ExportListsRules is the section lists-and-rules. Its lists carry only
// their configuration (exportList): the download state is runtime data.
type ExportListsRules struct {
	Lists   []filter.List   `json:"lists"`
	Rules   []filter.Rule   `json:"rules"`
	IPRules []filter.IPRule `json:"ipRules"`
}

// ExportLocalDNS is the section local-dns.
type ExportLocalDNS struct {
	Records    []dnsserver.Record    `json:"records"`
	Forwarders []dnsserver.Forwarder `json:"forwarders"`
}

// ExportParental is the section parental: the configuration of the
// groups, never their overrides or pauses.
type ExportParental struct {
	Groups []ExportParentalGroup `json:"groups"`
}

// ExportParentalGroup is the parental configuration of one group.
type ExportParentalGroup struct {
	GroupID         int64               `json:"groupId"`
	BlockedServices []string            `json:"blockedServices"`
	Schedules       []parental.Schedule `json:"schedules"`
	SafeSearch      parental.SafeSearch `json:"safeSearch"`
	Categories      map[string]bool     `json:"categories"` // informational: the switches follow the lists
}

// SyncStatus is GET /system/sync.
type SyncStatus struct {
	Mode              string    `json:"mode"`             // off | follower
	Source            string    `json:"source,omitempty"` // origin only
	Sections          []string  `json:"sections"`
	IntervalMinutes   int       `json:"intervalMinutes"`
	Running           bool      `json:"running"`
	LastRun           time.Time `json:"lastRun,omitzero"`
	LastSuccess       time.Time `json:"lastSuccess,omitzero"`
	LastError         string    `json:"lastError,omitempty"`
	LastAppliedSHA256 string    `json:"lastAppliedSha256,omitempty"`
	PrimaryVersion    string    `json:"primaryVersion,omitempty"`
	NextRun           time.Time `json:"nextRun,omitzero"`
}

// SyncManager is implemented by internal/app: the follower's runs.
type SyncManager interface {
	Status() SyncStatus
	// Run starts a run now: apperr.Conflict "sync is off" or "a sync is
	// running", apperr.TooMany within 30 s of the previous start.
	Run() error
	// ConfigSchema returns the schema version of every picache.db
	// component (app.ConfigSchemaVersions).
	ConfigSchema() map[string]int
}

// exportState is the in-memory state of the export: one at a time, the
// last content hash audited per sync token.
type exportState struct {
	mu        sync.Mutex
	running   bool
	lastAudit map[int64]string // sync token id → contentSha256
}

// registerSyncRoutes registers the export and the follower's routes.
func (s *Server) registerSyncRoutes() {
	s.route("GET /api/v1/system/export", permExport, s.systemExport)
	s.route("GET /api/v1/system/sync", permRead, s.syncStatus)
	s.route("POST /api/v1/system/sync/run", permAdmin, s.syncRun, routeExempt)
}

// syncedSections returns the sections this follower syncs ([] unless the
// mode is follower) and the primary's origin.
func (s *Server) syncedSections() ([]string, string) {
	sy := s.d.Settings.Get().Sync
	if sy.Mode != settings.SyncFollower {
		return []string{}, ""
	}
	return slices.Clone(sy.Sections), sy.Source
}

// syncedSection refuses a write of a synced section: 409 "this is synced
// from <origin>: change it on the primary" (field: the settings member for
// the settings routes, else none).
func (s *Server) syncedSection(section, field string) error {
	secs, origin := s.syncedSections()
	if !slices.Contains(secs, section) {
		return nil
	}
	return &apperr.Error{Kind: apperr.KindConflict, Field: field, Message: "this is synced from " + origin + ": change it on the primary"}
}

var (
	errExportRunning = apperr.TooMany("another export is running")
	errExportTooBig  = apperr.Unavailable("the export is larger than 64 MiB")
	errNoSync        = apperr.Unavailable("the follower sync is not available")
)

func (s *Server) systemExport(w http.ResponseWriter, r *http.Request) error {
	raw := r.URL.Query()["sections"]
	var names []string
	for _, v := range raw {
		names = append(names, strings.Split(v, ",")...)
	}
	sections, err := settings.CheckSections("sections", names, settings.ExportSections, "export")
	if err != nil {
		return err
	}
	s.export.mu.Lock()
	if s.export.running {
		s.export.mu.Unlock()
		return errExportRunning
	}
	s.export.running = true
	s.export.mu.Unlock()
	defer func() {
		s.export.mu.Lock()
		s.export.running = false
		s.export.mu.Unlock()
	}()
	extendDeadlines(w, 2*time.Minute)
	out, err := s.buildExport(r.Context(), sections)
	if err != nil {
		return err
	}
	b, err := json.Marshal(out, json.Deterministic(true))
	if err != nil {
		return err
	}
	if len(b) > MaxExportBytes {
		return errExportTooBig
	}
	s.auditExport(r, sections, out.ContentSHA256)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, err = w.Write(b)
	return err
}

// auditExport records system.export; for a sync token only when the
// content changed since the last export audited for that token, so a
// follower polling an unchanged configuration does not fill the audit log.
func (s *Server) auditExport(r *http.Request, sections []string, sha string) {
	if p := principal(r); p != nil && p.Scope == auth.ScopeSync {
		s.export.mu.Lock()
		if s.export.lastAudit == nil {
			s.export.lastAudit = map[int64]string{}
		}
		same := s.export.lastAudit[p.TokenID] == sha
		s.export.lastAudit[p.TokenID] = sha
		if len(s.export.lastAudit) > 128 { // bounded: at most 100 tokens exist
			clear(s.export.lastAudit)
			s.export.lastAudit[p.TokenID] = sha
		}
		s.export.mu.Unlock()
		if same {
			return
		}
	}
	s.audit(r, "system.export", "", map[string]any{"sections": sections, "contentSha256": sha})
}

// buildExport collects the sections (data the R routes return).
func (s *Server) buildExport(ctx context.Context, sections []string) (*ConfigExport, error) {
	out := &ConfigExport{Format: ExportFormat, FormatVersion: ExportFormatVersion, Version: version.Version,
		Schema: map[string]int{}, ExportedAt: time.Now().UTC(), Groups: []ExportGroupName{}, Sections: map[string]jsontext.Value{}}
	if s.d.Runtime != nil {
		out.InstanceID = s.d.Runtime.InstanceID()
	}
	if s.d.Sync != nil {
		out.Schema = s.d.Sync.ConfigSchema()
	}
	if s.d.Clients == nil {
		return nil, apperr.Unavailable("the configuration is not available")
	}
	groups, err := s.d.Clients.Groups(ctx)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		out.Groups = append(out.Groups, ExportGroupName{ID: g.ID, Name: g.Name})
	}
	add := func(name string, v any) error {
		b, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			return err
		}
		out.Sections[name] = jsontext.Value(b)
		return nil
	}
	for _, sec := range sections {
		var err error
		switch sec {
		case settings.SectionClientsGroups:
			var cl []clients.Client
			if cl, err = s.d.Clients.Clients(ctx); err == nil {
				err = add(sec, ExportClientsGroups{Groups: groups, Clients: cl})
			}
		case settings.SectionListsRules:
			if s.d.Filter == nil {
				return nil, apperr.Unavailable("the filter is not available")
			}
			var v ExportListsRules
			if v.Lists, err = s.d.Filter.Lists(ctx); err != nil {
				return nil, err
			}
			for i := range v.Lists {
				v.Lists[i] = exportList(v.Lists[i])
			}
			if v.Rules, err = s.d.Filter.Rules(ctx, filter.RuleQuery{}); err != nil {
				return nil, err
			}
			if v.IPRules, err = s.d.Filter.IPRules(ctx, filter.IPRuleQuery{}); err != nil {
				return nil, err
			}
			err = add(sec, v)
		case settings.SectionLocalDNS:
			if s.d.DNS == nil {
				return nil, apperr.Unavailable("the DNS server is not available")
			}
			var v ExportLocalDNS
			if v.Records, err = s.d.DNS.Records(ctx); err != nil {
				return nil, err
			}
			if v.Forwarders, err = s.d.DNS.Forwarders(ctx); err != nil {
				return nil, err
			}
			err = add(sec, v)
		case settings.SectionParental:
			if s.d.Parental == nil {
				return nil, apperr.Unavailable("parental controls are not available")
			}
			v := ExportParental{Groups: []ExportParentalGroup{}}
			gcs, err := s.d.Parental.List(ctx)
			if err != nil {
				return nil, err
			}
			for i := range gcs {
				gc := &gcs[i]
				s.fillCategories(gc)
				v.Groups = append(v.Groups, ExportParentalGroup{GroupID: gc.GroupID, BlockedServices: gc.BlockedServices,
					Schedules: gc.Schedules, SafeSearch: gc.SafeSearch, Categories: categoriesOn(gc.Categories)})
			}
			err = add(sec, v)
		case settings.SectionDNSSettings:
			var raw jsontext.Value
			if raw, err = settings.SyncableSettings(s.d.Settings.Get()); err == nil {
				out.Sections[sec] = raw
			}
		}
		if err != nil {
			return nil, err
		}
	}
	sha, err := ExportContentSHA256(out.Sections)
	if err != nil {
		return nil, err
	}
	out.ContentSHA256 = sha
	return out, nil
}

// exportList returns the configuration of a list: its input members, id
// and groupIds (what a follower applies), without the runtime members
// (status, lastError, the check and update times, the counts, sizeBytes,
// the ignored blocks). Every list check on the primary changes those, and
// the export's content hash must change with the configuration only, or
// each check would make every follower re-apply and recompile and add an
// audit row on both sides.
func exportList(l filter.List) filter.List {
	return filter.List{ID: l.ID, Name: l.Name, NameAuto: l.NameAuto, URL: l.URL, Kind: l.Kind, Format: l.Format,
		PlainDomains: l.PlainDomains, Category: l.Category, CatalogKey: l.CatalogKey, Enabled: l.Enabled,
		GroupIDs: l.GroupIDs, Comment: l.Comment, CreatedAt: l.CreatedAt}
}

// ExportContentSHA256 is the lower-case hex SHA-256 of the deterministic
// JSON of the sections (the follower compares it with the last applied).
func ExportContentSHA256(sections map[string]jsontext.Value) (string, error) {
	var buf bytes.Buffer
	if err := json.MarshalWrite(&buf, sections, json.Deterministic(true)); err != nil {
		return "", err
	}
	// The members are raw values: canonicalise them too.
	v := jsontext.Value(buf.Bytes())
	if err := v.Canonicalize(); err != nil {
		return "", err
	}
	h := sha256.Sum256(v)
	return hex.EncodeToString(h[:]), nil
}

// categoriesOn returns the on values of the category switches.
func categoriesOn(c parental.Categories) map[string]bool {
	b, err := json.Marshal(c)
	if err != nil {
		return map[string]bool{}
	}
	var m map[string]struct {
		On bool `json:"on"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]bool{}
	}
	out := map[string]bool{}
	for k, v := range m {
		out[k] = v.On
	}
	return out
}

func (s *Server) syncStatus(w http.ResponseWriter, r *http.Request) error {
	if s.d.Sync == nil {
		return errNoSync
	}
	return ok(w, s.d.Sync.Status())
}

func (s *Server) syncRun(w http.ResponseWriter, r *http.Request) error {
	if s.d.Sync == nil {
		return errNoSync
	}
	if err := s.d.Sync.Run(); err != nil {
		return err
	}
	s.audit(r, "sync.run", "", nil)
	return writeJSON(w, http.StatusAccepted, struct {
		Started bool `json:"started"`
	}{true})
}
