package filter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
)

// migrations of component "filter" (append-only). Version 1 also creates the
// default list (HaGeZi Multi NORMAL) and links it to the Default group.
// Version 2 adds the category and catalogue key of a list; New fills them
// for existing lists (backfillCategories).
var migrations = []string{
	`CREATE TABLE filter_lists (
		id            INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused: the matcher refers to list IDs
		name          TEXT    NOT NULL,
		url           TEXT    NOT NULL UNIQUE,
		kind          TEXT    NOT NULL DEFAULT 'block' CHECK (kind IN ('block', 'allow')),
		plain_domains TEXT    NOT NULL DEFAULT 'exact' CHECK (plain_domains IN ('exact', 'subtree')),
		enabled       INTEGER NOT NULL DEFAULT 1,
		comment       TEXT    NOT NULL DEFAULT '',
		status        TEXT    NOT NULL DEFAULT 'pending',
		last_error    TEXT    NOT NULL DEFAULT '',
		last_updated  INTEGER NOT NULL DEFAULT 0,
		last_checked  INTEGER NOT NULL DEFAULT 0,
		last_success  INTEGER NOT NULL DEFAULT 0,
		entries       INTEGER NOT NULL DEFAULT 0,
		invalid       INTEGER NOT NULL DEFAULT 0,
		unsupported   INTEGER NOT NULL DEFAULT 0,
		size_bytes    INTEGER NOT NULL DEFAULT 0,
		etag          TEXT    NOT NULL DEFAULT '',
		last_modified TEXT    NOT NULL DEFAULT '',
		content_hash  TEXT    NOT NULL DEFAULT '',
		created_at    INTEGER NOT NULL
	);
	CREATE TABLE filter_list_groups (
		list_id  INTEGER NOT NULL REFERENCES filter_lists(id) ON DELETE CASCADE,
		group_id INTEGER NOT NULL REFERENCES client_groups(id) ON DELETE CASCADE,
		PRIMARY KEY (list_id, group_id)
	) WITHOUT ROWID;
	CREATE INDEX filter_list_groups_group ON filter_list_groups(group_id);
	CREATE TABLE filter_rules (
		id         INTEGER PRIMARY KEY,
		action     TEXT    NOT NULL CHECK (action IN ('allow', 'block')),
		type       TEXT    NOT NULL CHECK (type IN ('exact', 'subtree', 'regex')),
		pattern    TEXT    NOT NULL,
		enabled    INTEGER NOT NULL DEFAULT 1,
		comment    TEXT    NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		UNIQUE (action, type, pattern)
	);
	CREATE TABLE filter_rule_groups (
		rule_id  INTEGER NOT NULL REFERENCES filter_rules(id) ON DELETE CASCADE,
		group_id INTEGER NOT NULL REFERENCES client_groups(id) ON DELETE CASCADE,
		PRIMARY KEY (rule_id, group_id)
	) WITHOUT ROWID;
	CREATE INDEX filter_rule_groups_group ON filter_rule_groups(group_id);
	INSERT INTO filter_lists (id, name, url, kind, plain_domains, enabled, comment, created_at)
	VALUES (1, 'HaGeZi Multi NORMAL', 'https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/multi.txt',
		'block', 'exact', 1, 'Default list', CAST(strftime('%s', 'now') AS INTEGER) * 1000);
	INSERT INTO filter_list_groups (list_id, group_id) SELECT 1, id FROM client_groups WHERE id = 1;`,
	`ALTER TABLE filter_lists ADD COLUMN category TEXT NOT NULL DEFAULT '';
	ALTER TABLE filter_lists ADD COLUMN catalog_key TEXT NOT NULL DEFAULT '';`,
}

// backfillCategories gives every list with an empty category the category
// and key of the catalogue entry with exactly the same URL. Without one it
// gets "allow" for allowlists (an allowlist always has "allow", a blocklist
// never), "abused-tlds" for a blocklist whose cached copy in dir blocks
// mostly whole TLDs (it did so before the TLD guard, which would now drop
// those entries), and "other" otherwise. Idempotent: only rows with an
// empty category are touched.
func backfillCategories(ctx context.Context, d *db.DB, dir string) error {
	type fill struct {
		id                   int64
		category, key, plain string
	}
	var fills []fill
	rows, err := d.R.QueryContext(ctx, `SELECT id, url, kind, plain_domains FROM filter_lists WHERE category = ''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var url, kind string
		f := fill{category: CategoryOther}
		if err := rows.Scan(&f.id, &url, &kind, &f.plain); err != nil {
			rows.Close()
			return err
		}
		if c, ok := catalogByURL(url); ok {
			f.category, f.key = c.Category, c.Key
		}
		f.category = categoryForKind(kind, f.category)
		fills = append(fills, f)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(fills) == 0 {
		return nil
	}
	for i, f := range fills {
		if f.category == CategoryOther && mostlyTLDs(ctx, filepath.Join(dir, strconv.FormatInt(f.id, 10)+".txt"), f.plain) {
			fills[i].category = CategoryAbusedTLDs
		}
	}
	return d.Tx(ctx, func(tx *sql.Tx) error {
		for _, f := range fills {
			if _, err := tx.ExecContext(ctx, `UPDATE filter_lists SET category = ?, catalog_key = ? WHERE id = ? AND category = ''`,
				f.category, f.key, f.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// mostlyTLDs reports whether the list file at path (a blocklist with the
// given plainDomains mode) blocks mostly whole TLDs: the TLD guard would
// refuse at least half of its entries.
func mostlyTLDs(ctx context.Context, path, plain string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	lp := newLineParser(formatOf("block", plain, CategoryOther))
	broad, total := 0, 0
	err = scanLines(ctx, io.LimitReader(f, maxListBytes), func(line []byte, long bool) {
		if long {
			return
		}
		entries, st := lp.parse(string(line))
		switch st {
		case lineBroad:
			broad++
			total++
		case lineOK:
			total += len(entries)
		}
	})
	return err == nil && broad > 0 && 2*broad >= total
}

// categoryForKind returns category adjusted to the list kind: allowlists
// always have "allow", a blocklist with "allow" gets "other".
func categoryForKind(kind, category string) string {
	switch {
	case kind == "allow":
		return CategoryAllow
	case category == CategoryAllow || category == "":
		return CategoryOther
	}
	return category
}

// querier is implemented by *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const listColumns = `id, name, url, kind, plain_domains, category, catalog_key, enabled, comment, status, last_error,
	last_updated, last_checked, last_success, entries, invalid, unsupported, size_bytes,
	etag, last_modified, content_hash, created_at`

// loadLists reads all lists with their groups (q: the read pool, or the
// transaction that is about to change them).
func loadLists(ctx context.Context, q querier) ([]*listRT, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+listColumns+` FROM filter_lists ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("filter: load lists: %w", err)
	}
	defer rows.Close()
	var out []*listRT
	for rows.Next() {
		rt := &listRT{}
		var updated, checked, success, created int64
		if err := rows.Scan(&rt.ID, &rt.Name, &rt.URL, &rt.Kind, &rt.PlainDomains, &rt.Category, &rt.CatalogKey, &rt.Enabled, &rt.Comment,
			&rt.Status, &rt.LastError, &updated, &checked, &success, &rt.Entries, &rt.Invalid,
			&rt.Unsupported, &rt.SizeBytes, &rt.etag, &rt.lastModified, &rt.hash, &created); err != nil {
			return nil, fmt.Errorf("filter: load lists: %w", err)
		}
		rt.LastUpdated, rt.LastChecked, rt.LastSuccess, rt.CreatedAt = db.Time(updated), db.Time(checked), db.Time(success), db.Time(created)
		out = append(out, rt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter: load lists: %w", err)
	}
	groups, err := loadGroupMap(ctx, q, `SELECT list_id, group_id FROM filter_list_groups ORDER BY list_id, group_id`)
	if err != nil {
		return nil, err
	}
	for _, rt := range out {
		rt.GroupIDs = nonNil(groups[rt.ID])
	}
	return out, nil
}

// loadGroupMap reads (owner id, group id) pairs.
func loadGroupMap(ctx context.Context, q querier, query string, args ...any) (map[int64][]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("filter: load groups: %w", err)
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var owner, group int64
		if err := rows.Scan(&owner, &group); err != nil {
			return nil, fmt.Errorf("filter: load groups: %w", err)
		}
		out[owner] = append(out[owner], group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter: load groups: %w", err)
	}
	return out, nil
}

// checkGroups verifies that every group ID exists.
func checkGroups(ctx context.Context, q querier, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	var n int
	query := `SELECT COUNT(*) FROM client_groups WHERE id IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
	if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return fmt.Errorf("filter: check groups: %w", err)
	}
	if n != len(ids) {
		return apperr.Invalid("groupIds", "unknown group")
	}
	return nil
}

// setGroups replaces the groups of one list or rule. table is a constant.
func setGroups(ctx context.Context, tx *sql.Tx, table, ownerCol string, owner int64, ids []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE `+ownerCol+` = ?`, owner); err != nil {
		return err
	}
	for _, g := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+` (`+ownerCol+`, group_id) VALUES (?, ?)`, owner, g); err != nil {
			return err
		}
	}
	return nil
}

// normalizeGroups validates group IDs and returns them sorted and unique.
// nil stays nil (the caller decides the default).
func normalizeGroups(ids []int64) ([]int64, error) {
	if ids == nil {
		return nil, nil
	}
	if len(ids) > maxGroupsPerEntry {
		return nil, apperr.Invalid("groupIds", "at most %d groups", maxGroupsPerEntry)
	}
	out := slices.Clone(ids)
	for _, id := range out {
		if id <= 0 {
			return nil, apperr.Invalid("groupIds", "invalid group id %d", id)
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// isUniqueViolation reports whether err is an SQLite UNIQUE constraint error.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func nonNil(ids []int64) []int64 {
	if ids == nil {
		return []int64{}
	}
	return ids
}

// Migrations returns the schema steps of component "filter" in picache.db
// (`picache db salvage` builds a fresh schema with them).
func Migrations() []string { return slices.Clone(migrations) }
