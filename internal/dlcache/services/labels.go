package services

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// Label bounds.
const (
	maxLabels        = 10_000
	maxLabelLen      = 128 // runes
	maxLabelKeyLen   = 320 // covers "<service>:<253-char host>"
	maxSearchResults = 500
)

func (r *Registry) loadLabels(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.R.QueryContext(ctx, `SELECT group_key, label FROM services_labels LIMIT ?`, maxLabels)
	if err != nil {
		return nil, fmt.Errorf("services: load labels: %w", err)
	}
	defer rows.Close()
	labels := map[string]string{}
	for rows.Next() {
		var k, l string
		if err := rows.Scan(&k, &l); err != nil {
			return nil, err
		}
		labels[k] = l
	}
	return labels, rows.Err()
}

// Label returns the display label for a group key: user override, else the
// built-in product label (Blizzard, Riot, the Steamworks redistributables,
// …), else the name of a Steam depot's app (while downloadCache.steamNames
// is on), the store name of an Xbox package, PlayStation title or Windows
// update (while downloadCache.xboxNames / psnNames / windowsNames is on) or
// the name in an Epic build's manifest, else the rule default. Never blocks
// on the network (an unknown group is only queued).
func (r *Registry) Label(groupKey string) string {
	if l, ok := (*r.labels.Load())[groupKey]; ok {
		return l
	}
	_, fixed := productLabels[groupKey]
	if depot, ok := steamDepotID(groupKey); ok {
		// Asked also for a product label: it queues the lookup of the icon.
		if l, _, ok := r.steam.label(depot); ok && !fixed {
			return l
		}
	} else if !fixed {
		if l, _, ok := r.catalog.label(groupKey); ok {
			return l
		}
	}
	return defaultLabel(groupKey, r.snap.Load().name)
}

// IconPath returns the API path of a group's icon ("" when it has none):
// the icon of a Steam depot's app or of an Xbox package or PlayStation
// title ("/cache/icons/<namespace>/<id>"; Windows updates and Epic builds
// have none).
func (r *Registry) IconPath(groupKey string) string {
	if depot, ok := steamDepotID(groupKey); ok {
		if _, app, ok := r.steam.label(depot); ok && app != 0 {
			return "/cache/icons/steam/" + strconv.FormatUint(uint64(app), 10)
		}
		return ""
	}
	if _, fixed := productLabels[groupKey]; fixed {
		return ""
	}
	if _, icon, ok := r.catalog.label(groupKey); ok && icon {
		ns, id, _ := strings.Cut(groupKey, ":")
		return "/cache/icons/" + ns + "/" + id
	}
	return ""
}

// CatalogIcon returns the stored icon (JPEG or PNG) of an Xbox package or
// PlayStation title group; ok=false while its source is off or there is
// none.
func (r *Registry) CatalogIcon(ctx context.Context, groupKey string) (icon []byte, ok bool, err error) {
	return r.catalog.icon(ctx, groupKey)
}

// SteamIcon returns the stored icon (JPEG) of a Steam app; ok=false while
// downloadCache.steamNames is off or there is none.
func (r *Registry) SteamIcon(ctx context.Context, app uint32) (icon []byte, ok bool, err error) {
	return r.steam.icon(ctx, app)
}

// UserLabel returns the user's label override for a group key; ok=false
// if the label is not overridden (Label then returns the built-in one).
func (r *Registry) UserLabel(groupKey string) (label string, ok bool) {
	label, ok = (*r.labels.Load())[groupKey]
	return label, ok
}

// Labels resolves several keys at once.
func (r *Registry) Labels(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = r.Label(k)
	}
	return out
}

// SearchLabels returns the group keys whose user label, built-in product
// label, Steam app or store name (where no product label is built in)
// contains q (case-insensitive), for Library search by name. The result is
// sorted and bounded.
func (r *Registry) SearchLabels(q string) []string {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return nil
	}
	user := *r.labels.Load()
	var out []string
	for k, l := range user {
		if strings.Contains(strings.ToLower(l), q) {
			out = append(out, k)
		}
	}
	for k, l := range productLabels {
		if _, overridden := user[k]; !overridden && strings.Contains(strings.ToLower(l), q) {
			out = append(out, k)
		}
	}
	for _, depot := range r.steam.search(q) {
		k := "steam:depot:" + strconv.FormatUint(uint64(depot), 10)
		if _, fixed := productLabels[k]; user[k] == "" && !fixed {
			out = append(out, k)
		}
	}
	for _, k := range r.catalog.search(q) {
		if _, fixed := productLabels[k]; user[k] == "" && !fixed {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	if len(out) > maxSearchResults {
		out = out[:maxSearchResults]
	}
	return out
}

// SetLabel stores a user label override for a group key ("" removes it).
func (r *Registry) SetLabel(ctx context.Context, groupKey, label string) error {
	label = strings.TrimSpace(label)
	switch {
	case groupKey == "" || len(groupKey) > maxLabelKeyLen || !strings.Contains(groupKey, ":") ||
		!utf8.ValidString(groupKey) || cleanText(groupKey, len(groupKey)) != groupKey:
		return apperr.Invalid("groupKey", "invalid group key")
	case utf8.RuneCountInString(label) > maxLabelLen || !utf8.ValidString(label) || cleanText(label, len(label)) != label:
		return apperr.Invalid("label", "must be at most %d printable characters", maxLabelLen)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cur := *r.labels.Load()
	if _, exists := cur[groupKey]; label != "" && !exists && len(cur) >= maxLabels {
		return apperr.Conflict("at most %d labels are supported", maxLabels)
	}
	var err error
	if label == "" {
		_, err = r.db.W.ExecContext(ctx, `DELETE FROM services_labels WHERE group_key = ?`, groupKey)
	} else {
		_, err = r.db.W.ExecContext(ctx,
			`INSERT INTO services_labels (group_key, label, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(group_key) DO UPDATE SET label = excluded.label, updated_at = excluded.updated_at`,
			groupKey, label, db.NowMs())
	}
	if err != nil {
		return err
	}
	next := maps.Clone(cur)
	if label == "" {
		delete(next, groupKey)
	} else {
		next[groupKey] = label
	}
	r.labels.Store(&next)
	return nil
}
