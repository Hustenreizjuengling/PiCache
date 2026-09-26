// Package parental implements parental controls per client group
// (docs/ARCHITECTURE.md 16): services from an embedded catalogue that are
// always blocked, weekly schedules that block all internet or selected
// services, one manual override per group that blocks all internet
// ("block") or lifts the group's restrictions ("allow") until a time, safe
// search per group and a timed pause of the group's filtering. The DNS
// server asks Check (7.1 step 7a) and SafeSearch (step 7c) for every query
// and FilterGroups once per query.
//
// Table (picache.db, component "parental"): parental_groups(group_id
// REFERENCES client_groups(id) ON DELETE CASCADE, config (JSON
// {blockedServices, schedules, safeSearch}), override_mode,
// override_until, updated_at, pause_until). A group without a row has no
// restrictions; deleting a group deletes its row. The category switches
// of the API are list assignments of the filter package and never stored
// here.
//
// Rules:
//   - Only enabled groups apply (the client identity lists enabled groups
//     only). A client in several groups gets the union of their
//     restrictions; an "allow" override lifts only its own group's.
//   - Parental controls do not depend on the global blocking switch: a
//     pause of the lists and rules does not lift a bedtime.
//   - Schedules follow the host's wall clock (time.Local, like scheduled
//     backups): a window starts on each listed day at start and ends at end
//     the same day, or the next day when end is before start. When the
//     clocks go forward, a window that starts in the skipped hour starts at
//     the first valid instant (03:00); when they go back, the repeated hour
//     counts as the clock shows it.
//   - An override lasts at most 7 days. An expired one is ignored and
//     removed from the table with the next write.
//   - Safe search and the pause are independent of the override: an allow
//     override lifts neither safe search nor the protection lists, and a
//     pause ends only the group's filtering (its lists and rules), never
//     parental controls or safe search. A pause lasts at most 7 days; an
//     elapsed one is ignored and removed with the next write.
//
// Hot path: Check reads an immutable snapshot (atomic.Pointer) that is
// rebuilt on every write and on group changes (Reload). When none of the
// client's groups has a restriction it returns at once, without reading
// the time zone or allocating; otherwise it costs O(groups × schedules)
// plus one suffix walk of the name through the service domains.
package parental

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/clients"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// Block modes of a schedule.
const (
	BlockAll      = "all"      // all internet
	BlockServices = "services" // the schedule's services
)

// Override modes.
const (
	OverrideBlock = "block" // block all internet until the time
	OverrideAllow = "allow" // lift the group's services and schedules until the time
)

// Decision kinds.
const (
	KindSchedule = "schedule" // a block-all schedule is active
	KindOverride = "override" // a block override is active
	KindService  = "service"  // the name belongs to a blocked service
)

// Limits.
const (
	maxSchedules   = 10
	maxServices    = 256 // per list (blocked services, services of a schedule)
	maxNameLen     = 40  // schedule names, characters
	maxOverride    = 7 * 24 * time.Hour
	maxOverrideMin = int(maxOverride / time.Minute)
	maxPause       = maxOverride // a pause of the group's filtering
)

// Schedule blocks all internet or selected services on the listed days
// from Start to End (End before Start: until End the next day).
type Schedule struct {
	ID       string   `json:"id"` // 8 hex characters; assigned by the server
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Days     []int    `json:"days"`  // 0 = Sunday … 6 = Saturday, ascending
	Start    string   `json:"start"` // HH:MM
	End      string   `json:"end"`   // HH:MM
	Block    string   `json:"block"` // all | services
	Services []string `json:"services"`
}

// Config is a group's stored configuration.
type Config struct {
	BlockedServices []string   `json:"blockedServices"`
	Schedules       []Schedule `json:"schedules"`
	SafeSearch      SafeSearch `json:"safeSearch"`
}

// YouTube restricted mode levels (SafeSearch.YouTube).
const (
	YouTubeOff      = "off"
	YouTubeModerate = "moderate"
	YouTubeStrict   = "strict"
)

// SafeSearch is a group's safe search configuration: per search engine
// whether its restricted mode is enforced (step 7c). A stored
// configuration without it has everything off.
type SafeSearch struct {
	Google     bool   `json:"google"`
	YouTube    string `json:"youtube"` // off | moderate | strict
	Bing       bool   `json:"bing"`
	DuckDuckGo bool   `json:"duckduckgo"`
	Ecosia     bool   `json:"ecosia"`
	Yandex     bool   `json:"yandex"`
	Pixabay    bool   `json:"pixabay"`
}

// SafeSearchInput changes safe search member by member: a nil member keeps
// the stored value.
type SafeSearchInput struct {
	Google     *bool   `json:"google"`
	YouTube    *string `json:"youtube"`
	Bing       *bool   `json:"bing"`
	DuckDuckGo *bool   `json:"duckduckgo"`
	Ecosia     *bool   `json:"ecosia"`
	Yandex     *bool   `json:"yandex"`
	Pixabay    *bool   `json:"pixabay"`
}

// CategoriesInput switches the category switches member by member (nil
// keeps the current list assignment). The API applies it through the
// filter lists (filter.Engine.SetPresets); parental never stores it.
type CategoriesInput struct {
	Adult    *bool `json:"adult"`
	Gambling *bool `json:"gambling"`
	Dating   *bool `json:"dating"`
	Piracy   *bool `json:"piracy"`
	Bypass   *bool `json:"bypass"`
}

// Want returns the switches that are set (switch name → on).
func (c *CategoriesInput) Want() map[string]bool {
	out := map[string]bool{}
	if c == nil {
		return out
	}
	for name, v := range map[string]*bool{"adult": c.Adult, "gambling": c.Gambling, "dating": c.Dating,
		"piracy": c.Piracy, "bypass": c.Bypass} {
		if v != nil {
			out[name] = *v
		}
	}
	return out
}

// UpdateInput is the body of PUT /parental/groups/{id}: blockedServices
// and schedules replace the stored ones; safeSearch and categories are
// optional, and so are their members (absent or null keeps the stored
// value or the current list assignment), so a body of an older client
// never turns safe search off or removes a group from its lists.
type UpdateInput struct {
	BlockedServices []string         `json:"blockedServices"`
	Schedules       []Schedule       `json:"schedules"`
	SafeSearch      *SafeSearchInput `json:"safeSearch"`
	Categories      *CategoriesInput `json:"categories"`
}

// CategorySwitch is the state of a category switch for a group.
type CategorySwitch struct {
	On    bool   `json:"on"`    // every bound list is enabled and assigned to the group
	State string `json:"state"` // off | active | pending | failed
}

// Categories are the category switches of a group (whole categories
// through downloaded lists, filled in by the API from the filter lists).
type Categories struct {
	Adult    CategorySwitch `json:"adult"`
	Gambling CategorySwitch `json:"gambling"`
	Dating   CategorySwitch `json:"dating"`
	Piracy   CategorySwitch `json:"piracy"`
	Bypass   CategorySwitch `json:"bypass"`
}

// CategorySwitchNames are the names of the category switches (the members
// of Categories and CategoriesInput).
var CategorySwitchNames = []string{"adult", "gambling", "dating", "piracy", "bypass"}

// Switch returns a pointer to the switch named name (nil if unknown).
func (c *Categories) Switch(name string) *CategorySwitch {
	switch name {
	case "adult":
		return &c.Adult
	case "gambling":
		return &c.Gambling
	case "dating":
		return &c.Dating
	case "piracy":
		return &c.Piracy
	case "bypass":
		return &c.Bypass
	}
	return nil
}

func offCategories() Categories {
	off := CategorySwitch{State: "off"}
	return Categories{Adult: off, Gambling: off, Dating: off, Piracy: off, Bypass: off}
}

// Override is a manual override of a group.
type Override struct {
	Mode  string    `json:"mode"` // block | allow
	Until time.Time `json:"until"`
}

// OverrideInput sets the override of a group: exactly one of Minutes
// (1–10080) and Until (in the future, at most 7 days ahead).
type OverrideInput struct {
	Mode    string     `json:"mode"`
	Minutes *int       `json:"minutes,omitempty"`
	Until   *time.Time `json:"until,omitempty"`
}

// PauseInput pauses a group's filtering: exactly one of Minutes (1–10080)
// and Until (in the future, at most 7 days ahead).
type PauseInput struct {
	Minutes *int       `json:"minutes,omitempty"`
	Until   *time.Time `json:"until,omitempty"`
}

// GroupControls is the parental configuration and state of one group.
// SafeSearch is in effect whenever the group is enabled; Categories are
// filled in by the API (all "off" as returned by the engine).
type GroupControls struct {
	GroupID         int64      `json:"groupId"`
	GroupName       string     `json:"groupName"`
	GroupEnabled    bool       `json:"groupEnabled"`
	ClientCount     int        `json:"clientCount"`
	BlockedServices []string   `json:"blockedServices"`
	Schedules       []Schedule `json:"schedules"`
	SafeSearch      SafeSearch `json:"safeSearch"`
	Categories      Categories `json:"categories"`
	Override        *Override  `json:"override,omitempty"` // active override only
	State           GroupState `json:"state"`
	UpdatedAt       time.Time  `json:"updatedAt,omitzero"`
}

// GroupState is what the group's restrictions do right now.
type GroupState struct {
	BlockAll        bool      `json:"blockAll"`
	Reason          string    `json:"reason,omitempty"`   // override | schedule (while BlockAll)
	Schedule        string    `json:"schedule,omitempty"` // the active block-all schedule
	Until           time.Time `json:"until,omitzero"`     // when blocking all internet ends
	BlockedServices []string  `json:"blockedServices"`    // blocked now: always blocked + active service schedules (none while lifted)
	Lifted          bool      `json:"lifted"`             // an allow override is active
	LiftedUntil     time.Time `json:"liftedUntil,omitzero"`
	Next            *Change   `json:"next,omitempty"` // next schedule start or end within 7 days
	// Paused: the group's filtering (its lists and rules, protection
	// lists excepted) is paused until PausedUntil; independent of BlockAll
	// and Lifted.
	Paused      bool      `json:"paused"`
	PausedUntil time.Time `json:"pausedUntil,omitzero"`
	// TimeZone and UTCOffsetMinutes describe the host's local time that
	// schedules use ("CEST", 120), so the UI can show the plan on the
	// host's clock when the browser is in another zone.
	TimeZone         string `json:"timeZone"`
	UTCOffsetMinutes int    `json:"utcOffsetMinutes"`
}

// Change is the next start or end of a schedule window.
type Change struct {
	Time       time.Time `json:"time"`
	ScheduleID string    `json:"scheduleId"`
	Name       string    `json:"name"`
	Starts     bool      `json:"starts"` // false: the window ends
}

// Decision is the result of Check.
type Decision struct {
	Blocked  bool
	Kind     string // KindSchedule | KindOverride | KindService
	GroupID  int64
	Group    string    // group name
	Name     string    // schedule name (KindSchedule), service name (KindService), "" (KindOverride)
	Schedule string    // KindService: the schedule that blocks the service ("" = always blocked)
	Until    time.Time // end of the override or schedule window (zero for always blocked services)
}

// Reason is the text of the query log and of EDE 15, e.g. "Kids: Bedtime",
// "Kids: blocked by hand", "Kids: YouTube" or "Kids: YouTube (Homework time)".
func (d Decision) Reason() string {
	switch d.Kind {
	case KindOverride:
		return d.Group + ": blocked by hand"
	case KindService:
		if d.Schedule != "" {
			return d.Group + ": " + d.Name + " (" + d.Schedule + ")"
		}
	}
	return d.Group + ": " + d.Name
}

// Groups is the part of *clients.Registry the engine uses.
type Groups interface {
	Groups(ctx context.Context) ([]clients.Group, error)
}

// Engine stores the parental controls and answers Check.
type Engine struct {
	db     *db.DB
	groups Groups
	log    *slog.Logger
	now    func() time.Time // clock of the API (tests replace it)
	loc    *time.Location   // time zone of the schedules (tests replace it)

	writeMu sync.Mutex // serialises writes and reloads with their snapshot
	snap    atomic.Pointer[snapshot]
}

// New migrates the table and loads the snapshot. groups provides the group
// names (for the log reasons) and the group list of the API.
func New(ctx context.Context, d *db.DB, groups Groups, log *slog.Logger) (*Engine, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	e := &Engine{db: d, groups: groups, log: log.With(slog.String("component", "parental")), now: time.Now, loc: time.Local}
	e.snap.Store(&snapshot{})
	if err := d.Migrate(ctx, "parental", migrations); err != nil {
		return nil, err
	}
	if err := e.reload(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

// Reload rebuilds the snapshot from the table and the group names. The app
// calls it after group changes (clients.Registry.OnChange).
func (e *Engine) Reload(ctx context.Context) error {
	e.writeMu.Lock() // a reload must not overtake a write's newer snapshot
	defer e.writeMu.Unlock()
	return e.reload(ctx)
}

func (e *Engine) reload(ctx context.Context) error {
	rows, err := e.load(ctx, 0)
	if err != nil {
		return err
	}
	gs, err := e.groups.Groups(ctx)
	if err != nil {
		return err
	}
	names := make(map[int64]string, len(gs))
	for _, g := range gs {
		names[g.ID] = g.Name
	}
	e.snap.Store(newSnapshot(rows, names))
	return nil
}

// List returns the controls of all groups (id order) with their state now.
func (e *Engine) List(ctx context.Context) ([]GroupControls, error) {
	return e.controls(ctx, 0)
}

// Get returns the controls of one group (apperr.NotFound for an unknown group).
func (e *Engine) Get(ctx context.Context, id int64) (GroupControls, error) {
	out, err := e.controls(ctx, id)
	if err != nil {
		return GroupControls{}, err
	}
	if len(out) == 0 {
		return GroupControls{}, errUnknownGroup(id)
	}
	return out[0], nil
}

// controls assembles the controls of all groups (id == 0) or of one group.
func (e *Engine) controls(ctx context.Context, id int64) ([]GroupControls, error) {
	gs, err := e.groups.Groups(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := e.load(ctx, id)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*row, len(rows))
	for i := range rows {
		byID[rows[i].groupID] = &rows[i]
	}
	now := e.now()
	out := []GroupControls{}
	for _, g := range gs {
		if id != 0 && g.ID != id {
			continue
		}
		gc := GroupControls{GroupID: g.ID, GroupName: g.Name, GroupEnabled: g.Enabled, ClientCount: g.ClientCount,
			Categories: offCategories()}
		r := byID[g.ID]
		if r == nil {
			r = &row{groupID: g.ID, cfg: sanitizeConfig(Config{})}
		}
		gc.BlockedServices = r.cfg.BlockedServices
		gc.Schedules = r.cfg.Schedules
		gc.SafeSearch = r.cfg.SafeSearch
		gc.UpdatedAt = r.updated
		if r.override.Mode != "" && now.Before(r.override.Until) {
			o := r.override
			o.Until = o.Until.UTC()
			gc.Override = &o
		}
		gc.State = compile(r, g.Name).state(now, e.loc)
		if now.Before(r.pauseUntil) {
			gc.State.Paused, gc.State.PausedUntil = true, r.pauseUntil.UTC()
		}
		out = append(out, gc)
	}
	return out, nil
}

// ValidateUpdate checks the parental part of a PUT body without storing
// anything: services, schedules and the safe search members.
func ValidateUpdate(in UpdateInput) error {
	_, err := validateUpdate(in)
	return err
}

// Update replaces a group's blocked services and schedules and changes the
// safe search members that are set (the override and the pause are kept;
// in.Categories is applied by the API).
func (e *Engine) Update(ctx context.Context, id int64, in UpdateInput) (GroupControls, error) {
	cfg, err := validateUpdate(in)
	if err != nil {
		return GroupControls{}, err
	}
	err = e.write(ctx, id, func(w writer) error {
		stored, err := w.loadConfig()
		if err != nil {
			return err
		}
		cfg.SafeSearch = in.SafeSearch.apply(stored.SafeSearch)
		return w.saveConfig(cfg)
	})
	if err != nil {
		return GroupControls{}, err
	}
	// Saved: report it as saved even if the request goes away now (the API
	// undoes the category switches only when the save failed).
	return e.Get(context.WithoutCancel(ctx), id)
}

// SetOverride blocks all internet or lifts the group's restrictions until
// a time.
func (e *Engine) SetOverride(ctx context.Context, id int64, in OverrideInput) (GroupControls, error) {
	o, err := validateOverride(in, e.now())
	if err != nil {
		return GroupControls{}, err
	}
	if err := e.write(ctx, id, func(w writer) error { return w.saveOverride(o) }); err != nil {
		return GroupControls{}, err
	}
	return e.Get(ctx, id)
}

// ClearOverride ends the group's override (a no-op if it has none).
func (e *Engine) ClearOverride(ctx context.Context, id int64) (GroupControls, error) {
	if err := e.write(ctx, id, func(w writer) error { return w.saveOverride(Override{}) }); err != nil {
		return GroupControls{}, err
	}
	return e.Get(ctx, id)
}

// SetPause pauses the group's filtering until a time (replacing a pause it
// has). Parental controls, safe search and protection lists stay in force.
func (e *Engine) SetPause(ctx context.Context, id int64, in PauseInput) (GroupControls, error) {
	until, err := validatePause(in, e.now())
	if err != nil {
		return GroupControls{}, err
	}
	if err := e.write(ctx, id, func(w writer) error { return w.savePause(until) }); err != nil {
		return GroupControls{}, err
	}
	return e.Get(ctx, id)
}

// ClearPause resumes the group's filtering (a no-op if it is not paused).
func (e *Engine) ClearPause(ctx context.Context, id int64) (GroupControls, error) {
	if err := e.write(ctx, id, func(w writer) error { return w.savePause(time.Time{}) }); err != nil {
		return GroupControls{}, err
	}
	return e.Get(ctx, id)
}
