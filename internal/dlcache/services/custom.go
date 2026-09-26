package services

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Bounds for user-managed data.
const (
	customPrefix      = "custom-"
	maxCustomServices = 64
	maxExtraDomains   = 1000 // per service
	maxNameLen        = 64   // runes
	maxDescLen        = 512  // runes
)

var migrations = []string{
	`CREATE TABLE services_custom (
		id          TEXT    PRIMARY KEY,
		name        TEXT    NOT NULL,
		description TEXT    NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	);
	CREATE TABLE services_extra_domains (
		service_id TEXT    NOT NULL,
		pattern    TEXT    NOT NULL,
		created_at INTEGER NOT NULL,
		PRIMARY KEY (service_id, pattern)
	) WITHOUT ROWID;
	CREATE TABLE services_labels (
		group_key  TEXT    PRIMARY KEY,
		label      TEXT    NOT NULL,
		updated_at INTEGER NOT NULL
	) WITHOUT ROWID;`,
}

// customService is a row of services_custom; its host patterns are its
// extra domains.
type customService struct {
	ID          string
	Name        string
	Description string
}

// loadDB reads custom services, extra domains and labels.
func (r *Registry) loadDB(ctx context.Context) error {
	rows, err := r.db.R.QueryContext(ctx, `SELECT id, name, description FROM services_custom ORDER BY id LIMIT ?`, maxCustomServices)
	if err != nil {
		return fmt.Errorf("services: load custom services: %w", err)
	}
	var custom []customService
	for rows.Next() {
		var c customService
		if err := rows.Scan(&c.ID, &c.Name, &c.Description); err != nil {
			rows.Close()
			return err
		}
		custom = append(custom, c)
	}
	if err := rows.Close(); err != nil {
		return err
	}

	rows, err = r.db.R.QueryContext(ctx, `SELECT service_id, pattern FROM services_extra_domains ORDER BY service_id, created_at, pattern`)
	if err != nil {
		return fmt.Errorf("services: load extra domains: %w", err)
	}
	extras := map[string][]string{}
	for rows.Next() {
		var id, p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return err
		}
		if len(extras[id]) < maxExtraDomains && ValidatePattern(p) == nil {
			extras[id] = append(extras[id], p)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}

	labels, err := r.loadLabels(ctx)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.custom, r.extras = custom, extras
	r.mu.Unlock()
	r.labels.Store(&labels)
	return nil
}

// SetEnabled enables or disables a service (persisted in settings.DownloadCache.DisabledServices).
func (r *Registry) SetEnabled(ctx context.Context, id string, enabled bool) error {
	if _, ok := r.snap.Load().index[id]; !ok {
		return apperr.NotFound("service", id)
	}
	return r.setDisabled(ctx, id, !enabled)
}

// setDisabled adds id to or removes it from downloadCache.disabledServices; the
// settings listener rebuilds the matchers. r.mu must not be held.
func (r *Registry) setDisabled(ctx context.Context, id string, disabled bool) error {
	_, err := r.set.Update(ctx, func(a *settings.All) error {
		list := slices.DeleteFunc(slices.Clone(a.DownloadCache.DisabledServices), func(s string) bool { return s == id })
		if disabled {
			list = append(list, id)
		}
		a.DownloadCache.DisabledServices = list
		return nil
	})
	return err
}

// SetExtraDomains replaces the user-added hosts of a service (ValidatePattern each).
func (r *Registry) SetExtraDomains(ctx context.Context, id string, domains []string) error {
	patterns, err := normalizePatterns("extraDomains", domains, 0)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.snap.Load().index[id]; !ok {
		return apperr.NotFound("service", id)
	}
	if err := r.db.Tx(ctx, func(tx *sql.Tx) error { return replaceDomains(ctx, tx, id, patterns) }); err != nil {
		return err
	}
	r.setExtrasLocked(id, patterns)
	r.rebuildLocked(r.set.Get())
	return nil
}

// CreateCustom adds a custom service.
func (r *Registry) CreateCustom(ctx context.Context, in ServiceInput) (Service, error) {
	name, desc, patterns, err := validateInput(in)
	if err != nil {
		return Service{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.custom) >= maxCustomServices {
		return Service{}, apperr.Conflict("at most %d custom services are supported", maxCustomServices)
	}
	for _, c := range r.custom {
		if strings.EqualFold(c.Name, name) {
			return Service{}, apperr.Conflict("a custom service named %q already exists", name)
		}
	}
	id := r.newCustomIDLocked(name)
	now := db.NowMs()
	err = r.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO services_custom (id, name, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			id, name, desc, now, now); err != nil {
			return err
		}
		return replaceDomains(ctx, tx, id, patterns)
	})
	if err != nil {
		return Service{}, err
	}
	r.custom = append(r.custom, customService{ID: id, Name: name, Description: desc})
	slices.SortFunc(r.custom, func(a, b customService) int { return strings.Compare(a.ID, b.ID) })
	r.setExtrasLocked(id, patterns)
	r.rebuildLocked(r.set.Get())
	return r.serviceLocked(id), nil
}

// UpdateCustom updates a custom service.
func (r *Registry) UpdateCustom(ctx context.Context, id string, in ServiceInput) (Service, error) {
	name, desc, patterns, err := validateInput(in)
	if err != nil {
		return Service{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i, err := r.customIndexLocked(id)
	if err != nil {
		return Service{}, err
	}
	for _, c := range r.custom {
		if c.ID != id && strings.EqualFold(c.Name, name) {
			return Service{}, apperr.Conflict("a custom service named %q already exists", name)
		}
	}
	err = r.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE services_custom SET name = ?, description = ?, updated_at = ? WHERE id = ?`,
			name, desc, db.NowMs(), id); err != nil {
			return err
		}
		return replaceDomains(ctx, tx, id, patterns)
	})
	if err != nil {
		return Service{}, err
	}
	r.custom[i] = customService{ID: id, Name: name, Description: desc}
	r.setExtrasLocked(id, patterns)
	r.rebuildLocked(r.set.Get())
	return r.serviceLocked(id), nil
}

// DeleteCustom deletes a custom service.
func (r *Registry) DeleteCustom(ctx context.Context, id string) error {
	r.mu.Lock()
	i, err := r.customIndexLocked(id)
	if err == nil {
		err = r.db.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, `DELETE FROM services_extra_domains WHERE service_id = ?`, id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `DELETE FROM services_custom WHERE id = ?`, id)
			return err
		})
	}
	if err == nil {
		r.custom = slices.Delete(r.custom, i, i+1)
		delete(r.extras, id)
		r.rebuildLocked(r.set.Get())
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// Forget its disabled state so a new service with the same ID starts
	// enabled (outside r.mu: settings listeners take it).
	if slices.Contains(r.set.Get().DownloadCache.DisabledServices, id) {
		return r.setDisabled(ctx, id, false)
	}
	return nil
}

func (r *Registry) customIndexLocked(id string) (int, error) {
	for i, c := range r.custom {
		if c.ID == id {
			return i, nil
		}
	}
	if _, ok := r.snap.Load().index[id]; ok {
		return 0, apperr.Forbidden("only custom services can be edited or deleted")
	}
	return 0, apperr.NotFound("service", id)
}

func (r *Registry) serviceLocked(id string) Service {
	s := r.snap.Load()
	return s.services[s.index[id]]
}

func (r *Registry) setExtrasLocked(id string, patterns []string) {
	if len(patterns) == 0 {
		delete(r.extras, id)
		return
	}
	r.extras[id] = patterns
}

// newCustomIDLocked derives a unique "custom-<slug>" ID from name.
func (r *Registry) newCustomIDLocked(name string) string {
	slug := slugify(name, 32-len(customPrefix))
	taken := func(id string) bool {
		if _, ok := r.snap.Load().index[id]; ok {
			return true
		}
		return slices.ContainsFunc(r.custom, func(c customService) bool { return c.ID == id })
	}
	id := customPrefix + slug
	for n := 2; taken(id); n++ {
		sfx := "-" + strconv.Itoa(n)
		id = customPrefix + strings.TrimRight(clipASCII(slug, 32-len(customPrefix)-len(sfx)), "-") + sfx
	}
	return id
}

// slugify lower-cases name and keeps [a-z0-9] runs joined by '-'.
func slugify(name string, max int) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(name) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(c)
			continue
		}
		dash = true
	}
	s := strings.TrimRight(clipASCII(b.String(), max), "-")
	if s == "" {
		return "service"
	}
	return s
}

func clipASCII(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// validateInput checks a custom service definition.
func validateInput(in ServiceInput) (name, desc string, patterns []string, err error) {
	name = strings.TrimSpace(in.Name)
	desc = strings.TrimSpace(in.Description)
	switch {
	case name == "":
		return "", "", nil, apperr.Invalid("name", "required")
	case utf8.RuneCountInString(name) > maxNameLen || !utf8.ValidString(name) || cleanText(name, len(name)) != name:
		return "", "", nil, apperr.Invalid("name", "must be at most %d printable characters", maxNameLen)
	case utf8.RuneCountInString(desc) > maxDescLen || !utf8.ValidString(desc) || cleanText(desc, len(desc)) != desc:
		return "", "", nil, apperr.Invalid("description", "must be at most %d printable characters", maxDescLen)
	}
	patterns, err = normalizePatterns("domains", in.Domains, 1)
	return name, desc, patterns, err
}

// normalizePatterns normalises, validates and de-duplicates user patterns.
func normalizePatterns(field string, in []string, min int) ([]string, error) {
	if len(in) > maxExtraDomains {
		return nil, apperr.Invalid(field, "at most %d domains are allowed", maxExtraDomains)
	}
	out := make([]string, 0, len(in))
	for i, raw := range in {
		p := NormalizePattern(raw)
		if err := ValidatePattern(p); err != nil {
			return nil, apperr.Invalid(fmt.Sprintf("%s[%d]", field, i), "%q: %v", clip(raw, 80), err)
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if len(out) < min {
		return nil, apperr.Invalid(field, "at least %d domain is required", min)
	}
	return out, nil
}

func replaceDomains(ctx context.Context, tx *sql.Tx, id string, patterns []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM services_extra_domains WHERE service_id = ?`, id); err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	for i, p := range patterns {
		// created_at keeps the input order when loading.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO services_extra_domains (service_id, pattern, created_at) VALUES (?, ?, ?)`,
			id, p, now+int64(i)); err != nil {
			return err
		}
	}
	return nil
}

// Migrations returns the schema steps of component "services" in picache.db
// (`picache db salvage` builds a fresh schema with them).
func Migrations() []string { return slices.Clone(migrations) }
