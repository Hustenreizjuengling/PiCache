package services

import (
	"context"
	"fmt"
	"maps"
	"slices"
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
// built-in product label (Blizzard, Riot, …), else the rule default.
// Never blocks on the network.
func (r *Registry) Label(groupKey string) string {
	if l, ok := (*r.labels.Load())[groupKey]; ok {
		return l
	}
	return defaultLabel(groupKey, r.snap.Load().name)
}

// Labels resolves several keys at once.
func (r *Registry) Labels(keys []string) map[string]string {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = r.Label(k)
	}
	return out
}

// SearchLabels returns the group keys whose user label or built-in product
// label contains q (case-insensitive), for Library search by name. The
// result is sorted and bounded.
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
