package clients

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Bounds of the upstreams per group (ARCHITECTURE 7.4, group sets).
const (
	maxGroupUpstreams  = 8  // upstreams of one group
	maxUpstreamLists   = 16 // distinct group upstream lists (a preset counts as its list)
	maxUpstreamEntries = 64 // distinct upstream entries across them
)

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// groupUpstreams are the upstream settings of one group.
type groupUpstreams struct {
	Upstreams []string
	Preset    string
}

func encodeUpstreams(list []string) string {
	if len(list) == 0 {
		return "[]"
	}
	b, err := json.Marshal(list)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// decodeUpstreams decodes a stored upstream list (never nil; a value that
// does not decode, e.g. from an edited database, is empty).
func decodeUpstreams(s string) []string {
	list, _ := decodeStoredUpstreams(s)
	return list
}

// decodeStoredUpstreams is decodeUpstreams that also returns the entries
// an earlier version stored with text after "#" that this version refuses
// (stored, used): they are read as that version did (the next save stores
// them so; the registry logs them when it loads the group).
func decodeStoredUpstreams(s string) (list []string, legacy [][2]string) {
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil || out == nil {
		return []string{}, nil
	}
	for _, u := range out {
		if used, ok := settings.LegacyUpstream(u); ok {
			legacy = append(legacy, [2]string{u, used})
		}
	}
	return settings.LegacyUpstreams(out), legacy
}

// logLegacyUpstreams logs each group upstream read with the meaning of a
// version before 1.0.0 (Group.legacy) once, naming the group: at the
// start, and when a restore or sync brings such an upstream (again).
// Called under writeMu or before the registry is shared.
func (r *Registry) logLegacyUpstreams(groups []Group) {
	logged := map[[2]string]bool{}
	for _, g := range groups {
		for _, c := range g.legacy {
			key := [2]string{g.Name, c[0]}
			if !r.legacyLogged[key] && !logged[key] {
				settings.LogLegacyUpstream(r.log, "Clients & groups", c[0], c[1], slog.String("group", g.Name))
			}
			logged[key] = true
		}
	}
	r.legacyLogged = logged
}

// canonicalUpstreams stores every valid host#port of list as host:port
// (settings.CanonicalUpstream), duplicates removed; never nil.
func canonicalUpstreams(list []string) []string {
	out := make([]string, 0, len(list))
	for _, u := range list {
		if u = settings.CanonicalUpstream(u); !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// groupUpstreamsOf reads the stored upstream settings of group id.
func groupUpstreamsOf(ctx context.Context, q queryer, id int64) (groupUpstreams, error) {
	var ups string
	var g groupUpstreams
	err := q.QueryRowContext(ctx, `SELECT upstreams, upstream_preset FROM client_groups WHERE id = ?`, id).Scan(&ups, &g.Preset)
	if errors.Is(err, sql.ErrNoRows) {
		return g, apperr.NotFound("group", id)
	}
	if err != nil {
		return g, err
	}
	g.Upstreams = decodeUpstreams(ups)
	return g, nil
}

// errDefaultUpstreams refuses upstreams for the Default group.
const errDefaultUpstreams = "the Default group uses the DNS upstreams (DNS settings)"

// resolveGroupUpstreams validates the upstream settings of group id (0 on
// create): the input merged with the stored settings old (nil members keep
// them). The syntax of every upstream is checked (settings.ParseUpstream);
// whether plain names are public and names have bootstrap servers depends
// on the settings and is checked by the API. A stored member that does not
// fit (a preset once upstreams are given) is reset unless the input names
// it.
func resolveGroupUpstreams(id int64, ups []string, preset *string, old groupUpstreams) (groupUpstreams, error) {
	// A kept stored list is saved in the stored form too.
	out := groupUpstreams{Upstreams: canonicalUpstreams(old.Upstreams), Preset: old.Preset}
	if ups != nil {
		if len(ups) > maxGroupUpstreams {
			return out, apperr.Invalid("upstreams", "at most %d upstreams", maxGroupUpstreams)
		}
		list := make([]string, 0, len(ups))
		for i, u := range ups {
			u = strings.TrimSpace(u)
			if _, err := settings.ParseUpstream(u); err != nil {
				return out, apperr.Invalid(fmt.Sprintf("upstreams[%d]", i), "%v", err)
			}
			// host#port is stored as host:port (versions before 1.0.0
			// ignore "#port").
			u = settings.CanonicalUpstream(u)
			if !slices.Contains(list, u) {
				list = append(list, u)
			}
		}
		out.Upstreams = list
	}
	if preset != nil {
		p := strings.ToLower(strings.TrimSpace(*preset))
		if _, ok := settings.UpstreamPresetByKey(p); p != "" && !ok {
			keys := []string{}
			for _, ps := range settings.UpstreamPresets() {
				keys = append(keys, ps.Key)
			}
			return out, apperr.Invalid("upstreamPreset", "must be empty or one of %s", strings.Join(keys, ", "))
		}
		out.Preset = p
	}
	if out.Upstreams == nil {
		out.Upstreams = []string{}
	}
	if id == DefaultGroupID {
		switch {
		case len(out.Upstreams) > 0:
			return out, apperr.Invalid("upstreams", errDefaultUpstreams)
		case out.Preset != "":
			return out, apperr.Invalid("upstreamPreset", errDefaultUpstreams)
		}
	}
	if len(out.Upstreams) > 0 && out.Preset != "" {
		switch {
		case preset != nil && ups != nil && len(ups) > 0:
			return out, apperr.Invalid("upstreamPreset", "choose a preset or your own upstreams, not both")
		case preset == nil:
			out.Preset = "" // the stored preset gives way to the new list
		default:
			out.Upstreams = []string{} // the stored list gives way to the new preset
		}
	}
	return out, nil
}

// checkUpstreamBounds refuses more than 16 distinct group upstream lists
// or 64 distinct upstream entries across them (the stored state of tx).
func checkUpstreamBounds(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `SELECT upstreams, upstream_preset FROM client_groups`)
	if err != nil {
		return err
	}
	defer rows.Close()
	lists, entries := map[string]bool{}, map[string]bool{}
	for rows.Next() {
		var ups, preset string
		if err := rows.Scan(&ups, &preset); err != nil {
			return err
		}
		list := decodeUpstreams(ups)
		if preset != "" {
			p, _ := settings.UpstreamPresetByKey(preset)
			list = p.Upstreams
		}
		if len(list) == 0 {
			continue
		}
		lists[strings.Join(list, "\n")] = true
		for _, u := range list {
			entries[u] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(lists) > maxUpstreamLists {
		return apperr.Conflict("at most %d different group upstream lists are supported", maxUpstreamLists)
	}
	if len(entries) > maxUpstreamEntries {
		return apperr.Conflict("at most %d different group upstreams are supported", maxUpstreamEntries)
	}
	return nil
}

// SetGroupUpstreams replaces only the upstream settings of group id (PUT
// /groups/{id}/upstreams): upstreams ([] = none) or preset ("" = none),
// not both; never for the Default group.
func (r *Registry) SetGroupUpstreams(ctx context.Context, id int64, upstreams []string, preset string) (Group, error) {
	if upstreams == nil {
		upstreams = []string{}
	}
	err := r.write(ctx, func(tx *sql.Tx) error {
		if _, err := groupUpstreamsOf(ctx, tx, id); err != nil {
			return err
		}
		ups, err := resolveGroupUpstreams(id, upstreams, &preset, groupUpstreams{})
		if err != nil {
			return err
		}
		if len(ups.Upstreams) > 0 && ups.Preset != "" {
			return apperr.Invalid("upstreamPreset", "choose a preset or your own upstreams, not both")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE client_groups SET upstreams = ?, upstream_preset = ? WHERE id = ?`,
			encodeUpstreams(ups.Upstreams), ups.Preset, id); err != nil {
			return err
		}
		return checkUpstreamBounds(ctx, tx)
	})
	if err != nil {
		return Group{}, err
	}
	return r.group(ctx, id)
}

// GroupUpstreamConfig is the upstream setting of one enabled group that
// has a preset or its own upstreams (the input of the resolver's group
// sets).
type GroupUpstreamConfig struct {
	ID        int64
	Name      string
	Preset    string
	Upstreams []string
}

// GroupUpstreamConfigs returns the enabled groups with a preset or their
// own upstreams, by id.
func (r *Registry) GroupUpstreamConfigs(ctx context.Context) ([]GroupUpstreamConfig, error) {
	groups, err := r.Groups(ctx)
	if err != nil {
		return nil, err
	}
	var out []GroupUpstreamConfig
	for _, g := range groups {
		if g.Enabled && g.ID != DefaultGroupID && (g.UpstreamPreset != "" || len(g.Upstreams) > 0) {
			out = append(out, GroupUpstreamConfig{ID: g.ID, Name: g.Name, Preset: g.UpstreamPreset, Upstreams: g.Upstreams})
		}
	}
	return out, nil
}
