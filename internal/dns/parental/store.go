package parental

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// migrations of the "parental" component (append-only). Timestamps are
// unix milliseconds like everywhere in picache.db. Version 2 adds the
// pause of the group's filtering.
var migrations = []string{
	`CREATE TABLE parental_groups (
		group_id       INTEGER PRIMARY KEY REFERENCES client_groups(id) ON DELETE CASCADE,
		config         TEXT    NOT NULL DEFAULT '{}',
		override_mode  TEXT    NOT NULL DEFAULT '',
		override_until INTEGER NULL,
		updated_at     INTEGER NOT NULL
	)`,
	`ALTER TABLE parental_groups ADD COLUMN pause_until INTEGER NULL`,
}

// row is one group's stored configuration.
type row struct {
	groupID    int64
	cfg        Config
	override   Override // Mode "" = none
	pauseUntil time.Time
	updated    time.Time
}

// load reads the rows of all groups (id == 0) or of one group. A stored
// configuration is sanitised, not rejected: services that left the
// catalogue and malformed schedules (an edited database) are dropped, so a
// release that removes a service never makes a group's controls
// unreadable.
func (e *Engine) load(ctx context.Context, id int64) ([]row, error) {
	where, args := "", []any{}
	if id != 0 {
		where, args = " WHERE group_id = ?", []any{id}
	}
	rows, err := e.db.R.QueryContext(ctx, `SELECT group_id, config, override_mode, COALESCE(override_until, 0), updated_at,
		COALESCE(pause_until, 0) FROM parental_groups`+where+` ORDER BY group_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("parental: load: %w", err)
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		var r row
		var cfg, mode string
		var until, updated, pause int64
		if err := rows.Scan(&r.groupID, &cfg, &mode, &until, &updated, &pause); err != nil {
			return nil, fmt.Errorf("parental: load: %w", err)
		}
		r.cfg = e.decodeConfig(r.groupID, cfg)
		if (mode == OverrideBlock || mode == OverrideAllow) && until != 0 {
			r.override = Override{Mode: mode, Until: db.Time(until)}
		}
		if pause != 0 {
			r.pauseUntil = db.Time(pause)
		}
		r.updated = db.Time(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// decodeConfig reads a stored configuration; an unreadable one counts as
// empty (logged), a readable one is sanitised.
func (e *Engine) decodeConfig(group int64, stored string) Config {
	var cfg Config
	if err := json.Unmarshal([]byte(stored), &cfg); err != nil {
		e.log.Warn("unreadable parental controls ignored", slog.Int64("group", group), slog.Any("err", err))
		cfg = Config{}
	}
	return sanitizeConfig(cfg)
}

// writer is a write transaction on one group's row.
type writer struct {
	e   *Engine
	ctx context.Context
	tx  *sql.Tx
	id  int64
	now time.Time
}

// write runs fn for group id in a transaction (apperr.NotFound for an
// unknown group), removes expired overrides and pauses of all groups and
// reloads the snapshot.
func (e *Engine) write(ctx context.Context, id int64, fn func(writer) error) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	now := e.now()
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM client_groups WHERE id = ?`, id).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return errUnknownGroup(id)
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE parental_groups SET override_mode = '', override_until = NULL
			WHERE override_mode != '' AND (override_until IS NULL OR override_until <= ?)`, db.Ms(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE parental_groups SET pause_until = NULL
			WHERE pause_until IS NOT NULL AND pause_until <= ?`, db.Ms(now)); err != nil {
			return err
		}
		return fn(writer{e: e, ctx: ctx, tx: tx, id: id, now: now})
	})
	if err != nil {
		return err
	}
	// The change is committed: the snapshot must follow it even when the
	// request goes away now (a cancelled reload would keep, e.g., a cleared
	// pause in force until the next write).
	if err := e.reload(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("parental: reload: %w", err)
	}
	return nil
}

// loadConfig reads the group's stored configuration (sanitised; empty for
// a group without a row).
func (w writer) loadConfig() (Config, error) {
	var stored string
	err := w.tx.QueryRowContext(w.ctx, `SELECT config FROM parental_groups WHERE group_id = ?`, w.id).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return sanitizeConfig(Config{}), nil
	case err != nil:
		return Config{}, err
	}
	return w.e.decodeConfig(w.id, stored), nil
}

func (w writer) saveConfig(cfg Config) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = w.tx.ExecContext(w.ctx, `INSERT INTO parental_groups (group_id, config, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(group_id) DO UPDATE SET config = excluded.config, updated_at = excluded.updated_at`,
		w.id, string(b), db.Ms(w.now))
	return err
}

// saveOverride stores o (Mode "" removes the override).
func (w writer) saveOverride(o Override) error {
	var until any
	if o.Mode != "" {
		until = db.Ms(o.Until)
	}
	_, err := w.tx.ExecContext(w.ctx, `INSERT INTO parental_groups (group_id, override_mode, override_until, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(group_id) DO UPDATE SET override_mode = excluded.override_mode,
			override_until = excluded.override_until, updated_at = excluded.updated_at`,
		w.id, o.Mode, until, db.Ms(w.now))
	return err
}

// savePause stores the end of the group's pause (zero removes it).
func (w writer) savePause(until time.Time) error {
	var v any
	if !until.IsZero() {
		v = db.Ms(until)
	}
	_, err := w.tx.ExecContext(w.ctx, `INSERT INTO parental_groups (group_id, pause_until, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(group_id) DO UPDATE SET pause_until = excluded.pause_until, updated_at = excluded.updated_at`,
		w.id, v, db.Ms(w.now))
	return err
}

func errUnknownGroup(id int64) error { return apperr.NotFound("group", id) }

// --- validation ---

// validateUpdate checks and normalises the services, schedules and safe
// search members of a PUT body (Update merges the members into the stored
// safe search).
func validateUpdate(in UpdateInput) (Config, error) {
	cfg, err := validateConfig(Config{BlockedServices: in.BlockedServices, Schedules: in.Schedules})
	if err != nil {
		return Config{}, err
	}
	if err := in.SafeSearch.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate checks the members that are set.
func (in *SafeSearchInput) validate() error {
	if in != nil && in.YouTube != nil && !validYouTube(*in.YouTube) {
		return apperr.Invalid("safeSearch.youtube", `must be "off", "moderate" or "strict"`)
	}
	return nil
}

// apply returns s with the members of in that are set.
func (in *SafeSearchInput) apply(s SafeSearch) SafeSearch {
	if in == nil {
		return s
	}
	set := func(dst *bool, v *bool) {
		if v != nil {
			*dst = *v
		}
	}
	set(&s.Google, in.Google)
	set(&s.Bing, in.Bing)
	set(&s.DuckDuckGo, in.DuckDuckGo)
	set(&s.Ecosia, in.Ecosia)
	set(&s.Yandex, in.Yandex)
	set(&s.Pixabay, in.Pixabay)
	if in.YouTube != nil {
		s.YouTube = *in.YouTube
	}
	return s
}

func validYouTube(v string) bool {
	return v == YouTubeOff || v == YouTubeModerate || v == YouTubeStrict
}

// validateConfig checks and normalises a configuration: known, unique
// services (sorted), at most 10 schedules. Schedules keep their id when it
// is well-formed and unique; new ones (and malformed ids) get a fresh one.
func validateConfig(in Config) (Config, error) {
	out := Config{Schedules: []Schedule{}, SafeSearch: SafeSearch{YouTube: YouTubeOff}}
	var err error
	if out.BlockedServices, err = validServices("blockedServices", in.BlockedServices, 0); err != nil {
		return Config{}, err
	}
	if len(in.Schedules) > maxSchedules {
		return Config{}, apperr.Invalid("schedules", "at most %d schedules per group are allowed", maxSchedules)
	}
	used := map[string]bool{}
	for i, s := range in.Schedules {
		f := func(member string) string { return fmt.Sprintf("schedules[%d].%s", i, member) }
		v := Schedule{Enabled: s.Enabled, Block: s.Block}
		if v.Name, err = cleanName(f("name"), s.Name); err != nil {
			return Config{}, err
		}
		if v.Days, err = validDays(f("days"), s.Days); err != nil {
			return Config{}, err
		}
		if _, _, ok := settings.ParseClock(s.Start); !ok {
			return Config{}, apperr.Invalid(f("start"), "must be a time of day as HH:MM (00:00 to 23:59)")
		}
		if _, _, ok := settings.ParseClock(s.End); !ok {
			return Config{}, apperr.Invalid(f("end"), "must be a time of day as HH:MM (00:00 to 23:59)")
		}
		if s.End == s.Start {
			return Config{}, apperr.Invalid(f("end"), "must differ from the start")
		}
		v.Start, v.End = s.Start, s.End
		switch s.Block {
		case BlockAll:
			if len(s.Services) > 0 {
				return Config{}, apperr.Invalid(f("services"), "must be empty when the schedule blocks all internet")
			}
			v.Services = []string{}
		case BlockServices:
			if v.Services, err = validServices(f("services"), s.Services, 1); err != nil {
				return Config{}, err
			}
		default:
			return Config{}, apperr.Invalid(f("block"), `must be "all" or "services"`)
		}
		v.ID = s.ID
		if !validScheduleID(v.ID) || used[v.ID] {
			v.ID = newScheduleID(used)
		}
		used[v.ID] = true
		out.Schedules = append(out.Schedules, v)
	}
	return out, nil
}

// validServices checks a list of service ids (at least min, at most 256,
// all known); duplicates are removed and the list is sorted.
func validServices(field string, in []string, min int) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, id := range in {
		if _, ok := serviceIndex[id]; !ok {
			return nil, apperr.Invalid(field, "unknown service %q", id)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) < min {
		return nil, apperr.Invalid(field, "select at least one service")
	}
	if len(out) > maxServices {
		return nil, apperr.Invalid(field, "at most %d services are allowed", maxServices)
	}
	slices.Sort(out)
	return out, nil
}

// validDays checks the days of a schedule (0 = Sunday … 6); duplicates are
// removed and the list is sorted.
func validDays(field string, in []int) ([]int, error) {
	out := make([]int, 0, len(in))
	for _, d := range in {
		if d < 0 || d > 6 {
			return nil, apperr.Invalid(field, "days must be between 0 (Sunday) and 6 (Saturday)")
		}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, apperr.Invalid(field, "select at least one day")
	}
	slices.Sort(out)
	return out, nil
}

// cleanName trims a schedule name and checks its length and characters.
func cleanName(field, s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", apperr.Invalid(field, "required")
	}
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > maxNameLen {
		return "", apperr.Invalid(field, "must be valid text of at most %d characters", maxNameLen)
	}
	if strings.ContainsFunc(s, unicode.IsControl) {
		return "", apperr.Invalid(field, "must not contain control characters")
	}
	return s, nil
}

func validScheduleID(id string) bool {
	if len(id) != 8 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && strings.ToLower(id) == id
}

// newScheduleID returns a random id that is not in used.
func newScheduleID(used map[string]bool) string {
	for {
		b := make([]byte, 4)
		rand.Read(b)
		if id := hex.EncodeToString(b); !used[id] {
			return id
		}
	}
}

// validateOverride turns the input into an override ending at most 7 days
// after now (a minute of leeway for the browser's clock).
func validateOverride(in OverrideInput, now time.Time) (Override, error) {
	if in.Mode != OverrideBlock && in.Mode != OverrideAllow {
		return Override{}, apperr.Invalid("override.mode", `must be "block" or "allow"`)
	}
	var until time.Time
	switch {
	case in.Minutes != nil && in.Until != nil:
		return Override{}, apperr.Invalid("minutes", "give either minutes or until, not both")
	case in.Minutes != nil:
		if *in.Minutes < 1 || *in.Minutes > maxOverrideMin {
			return Override{}, apperr.Invalid("minutes", "must be between 1 and %d", maxOverrideMin)
		}
		until = now.Add(time.Duration(*in.Minutes) * time.Minute)
	case in.Until != nil:
		until = *in.Until
		if !until.After(now) {
			return Override{}, apperr.Invalid("override.until", "must be in the future")
		}
		if until.After(now.Add(maxOverride + time.Minute)) {
			return Override{}, apperr.Invalid("override.until", "must be at most 7 days ahead")
		}
	default:
		return Override{}, apperr.Invalid("minutes", "give the duration in minutes or an end time (until)")
	}
	return Override{Mode: in.Mode, Until: until.Truncate(time.Second).UTC()}, nil
}

// validatePause returns the end of a pause of at most 7 days after now (a
// minute of leeway for the browser's clock).
func validatePause(in PauseInput, now time.Time) (time.Time, error) {
	var until time.Time
	switch {
	case in.Minutes != nil && in.Until != nil:
		return time.Time{}, apperr.Invalid("minutes", "give either minutes or until, not both")
	case in.Minutes != nil:
		if *in.Minutes < 1 || *in.Minutes > maxOverrideMin {
			return time.Time{}, apperr.Invalid("minutes", "must be between 1 and %d", maxOverrideMin)
		}
		until = now.Add(time.Duration(*in.Minutes) * time.Minute)
	case in.Until != nil:
		until = *in.Until
		if !until.After(now) {
			return time.Time{}, apperr.Invalid("pause.until", "must be in the future")
		}
		if until.After(now.Add(maxPause + time.Minute)) {
			return time.Time{}, apperr.Invalid("pause.until", "must be at most 7 days ahead")
		}
	default:
		return time.Time{}, apperr.Invalid("minutes", "give the duration in minutes or an end time (until)")
	}
	return until.Truncate(time.Second).UTC(), nil
}

// sanitizeConfig keeps the valid parts of a stored configuration (an
// unknown YouTube level is off).
func sanitizeConfig(c Config) Config {
	out := Config{BlockedServices: knownServices(c.BlockedServices), Schedules: []Schedule{}, SafeSearch: c.SafeSearch}
	if !validYouTube(out.SafeSearch.YouTube) {
		out.SafeSearch.YouTube = YouTubeOff
	}
	for _, s := range c.Schedules {
		if len(out.Schedules) == maxSchedules {
			break
		}
		s.Services = knownServices(s.Services)
		days := make([]int, 0, len(s.Days))
		for _, d := range s.Days {
			if d >= 0 && d <= 6 && !slices.Contains(days, d) {
				days = append(days, d)
			}
		}
		slices.Sort(days)
		s.Days = days
		_, _, okStart := settings.ParseClock(s.Start)
		_, _, okEnd := settings.ParseClock(s.End)
		switch {
		case !validScheduleID(s.ID), len(days) == 0, !okStart, !okEnd, s.Start == s.End:
			continue
		case s.Block == BlockAll:
			s.Services = []string{}
		case s.Block == BlockServices && len(s.Services) > 0:
		default:
			continue
		}
		out.Schedules = append(out.Schedules, s)
	}
	return out
}

// knownServices keeps the catalogue ids of a stored list (sorted, unique,
// never nil).
func knownServices(in []string) []string {
	out := make([]string, 0, len(in))
	for _, id := range in {
		if _, ok := serviceIndex[id]; ok && !slices.Contains(out, id) && len(out) < maxServices {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
