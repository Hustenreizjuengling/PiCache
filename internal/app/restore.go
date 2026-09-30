package app

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
	"github.com/hustenreizjuengling/picache/internal/storage"
)

// Partial restores (docs/ARCHITECTURE.md 15.3): a backup
// restores selected sections only. The selection travels inside the staged
// file (app_meta restore.sections; restore.by = "cli" for `picache
// restore`) and applies at the next start like a full restore. Every
// picache.db table belongs to exactly one section (restoreSectionMap) or
// is never restored (neverRestoredTables); a test fails for a table of
// MigrateConfigDB missing from both.

// restoreSection is one section of the restore: its tables in insert
// order (parents before children) and the tables whose rows are deleted
// before (their links cascade).
type restoreSection struct {
	name    string
	tables  []string
	parents []string
}

// restoreSectionMap is the section map of the restore (settings tables are
// merged by member, see mergeSettings).
var restoreSectionMap = []restoreSection{
	{settings.SectionSettings, []string{"settings", "settings_secrets"}, nil},
	{settings.SectionClientsGroups, []string{"client_clients", "client_groups", "client_identifiers", "client_memberships"},
		[]string{"client_groups", "client_clients"}},
	{settings.SectionListsRules, []string{"filter_lists", "filter_list_groups", "filter_rules", "filter_rule_groups", "filter_ip_rules",
		"filter_ip_rule_groups"}, []string{"filter_lists", "filter_rules", "filter_ip_rules"}},
	{settings.SectionLocalDNS, []string{"dns_records", "dns_record_groups", "dns_forwarders", "dns_forwarder_domains"},
		[]string{"dns_records", "dns_forwarders"}},
	{settings.SectionParental, []string{"parental_groups"}, []string{"parental_groups"}},
	{settings.SectionDHCP, []string{"dhcp_static", "dhcp_leases"}, []string{"dhcp_static", "dhcp_leases"}},
	{settings.SectionDownloadCache, []string{"services_custom", "services_extra_domains", "services_labels"},
		[]string{"services_custom", "services_extra_domains", "services_labels"}},
	{settings.SectionNotifications, []string{"notify_channels"}, []string{"notify_channels"}},
	{settings.SectionStorage, []string{"storage_targets"}, []string{"storage_targets"}},
}

// neverRestoredTables are kept live by every restore (accounts, sessions,
// tokens, the audit log, the process's own state).
var neverRestoredTables = []string{"auth_users", "auth_sessions", "auth_tokens", "auth_audit", "app_meta", "schema_migrations",
	"proxy_noslice_hosts"}

// groupLinkTables are the links of the group-linked sections to client
// groups (the only references between sections): table → group column.
var groupLinkTables = map[string]string{"filter_list_groups": "group_id", "filter_rule_groups": "group_id",
	"filter_ip_rule_groups": "group_id", "dns_record_groups": "group_id", "parental_groups": "group_id"}

// app_meta keys of a staged restore.
const (
	metaRestoreSections = "restore.sections"
	metaRestoreBy       = "restore.by"
)

// sectionOf returns the restore section of a table.
func sectionOf(table string) (restoreSection, bool) {
	for _, s := range restoreSectionMap {
		if slices.Contains(s.tables, table) {
			return s, true
		}
	}
	return restoreSection{}, false
}

// writeRestoreMeta records the selection of a staged file: restore.sections
// for a partial restore (deleted for a full one) and restore.by (only
// "cli"); an upload's own keys are overwritten. A file without app_meta
// gets the table only when there is something to record.
func writeRestoreMeta(ctx context.Context, path string, sections []string, by string) error {
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(DELETE)&_pragma=trusted_schema(0)")
	if err != nil {
		return err
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	var tables int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'app_meta'`).Scan(&tables); err != nil {
		return err
	}
	if tables == 0 && sections == nil && by == "" {
		return d.Close()
	}
	if _, err := d.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS app_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, `DELETE FROM app_meta WHERE key IN (?, ?)`, metaRestoreSections, metaRestoreBy); err != nil {
		return err
	}
	if sections != nil {
		b, _ := json.Marshal(sections)
		if _, err := d.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)`, metaRestoreSections, string(b)); err != nil {
			return err
		}
	}
	if by != "" {
		if _, err := d.ExecContext(ctx, `INSERT INTO app_meta (key, value) VALUES (?, ?)`, metaRestoreBy, by); err != nil {
			return err
		}
	}
	return d.Close()
}

// readRestoreMeta returns the selection of a staged file (nil sections: a
// full restore).
func readRestoreMeta(ctx context.Context, path string) (sections []string, by string, err error) {
	d, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)&_pragma=trusted_schema(0)")
	if err != nil {
		return nil, "", err
	}
	defer d.Close()
	var raw string
	err = d.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, metaRestoreSections).Scan(&raw)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err != nil && strings.Contains(err.Error(), "no such table")):
	case err != nil:
		return nil, "", err
	default:
		var names []string
		if err := json.Unmarshal([]byte(raw), &names); err != nil {
			return nil, "", fmt.Errorf("restore.sections: %w", err)
		}
		if sections, err = settings.CheckSections("sections", names, settings.RestoreSections, "restore"); err != nil {
			return nil, "", err
		}
	}
	_ = d.QueryRowContext(ctx, `SELECT value FROM app_meta WHERE key = ?`, metaRestoreBy).Scan(&by)
	return sections, by, nil
}

// copyFile copies src to a new file dst (O_EXCL, 0600).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// removeDB removes a database file with its journal files.
func removeDB(path string) {
	for _, sfx := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(path + sfx)
	}
}

// migratedCopy copies a staged backup and runs every component migration
// on the copy, so an older backup has the live schema. The caller removes
// it.
func migratedCopy(ctx context.Context, staged string) (string, error) {
	dst := fmt.Sprintf("%s.migrated-%d", staged, time.Now().UnixNano())
	if err := copyFile(staged, dst); err != nil {
		return "", err
	}
	d, err := db.Open(dst, 1)
	if err != nil {
		removeDB(dst)
		return "", err
	}
	err = MigrateConfigDB(ctx, d)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		removeDB(dst)
		return "", fmt.Errorf("migrate the backup: %w", err)
	}
	return dst, nil
}

// liveCopy writes a consistent copy of the live database (VACUUM INTO,
// read-only for the CLI).
func liveCopy(ctx context.Context, livePath, dst string) error {
	d, err := sql.Open("sqlite", "file:"+livePath+"?mode=ro&_pragma=busy_timeout(5000)&_pragma=trusted_schema(0)")
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = d.ExecContext(ctx, `VACUUM INTO ?`, dst)
	return err
}

// mergeRestore merges the sections of the backup src into the database at
// work (a copy of the live database): in one transaction with foreign keys
// on, each selected section's parent rows are deleted (their links
// cascade), the backup's rows inserted by column name (children after
// parents), group links of sections restored without clients-and-groups
// mapped by group name onto the live groups, the section's settings
// members merged; then PRAGMA foreign_key_check and the storage targets
// that cache.activeStoreId and backups.destination name. commit false
// rolls back (the dry run of the upload). Section errors are
// apperr.Invalid with the field "sections".
func mergeRestore(ctx context.Context, work, src string, sections []string, commit bool) error {
	d, err := sql.Open("sqlite", "file:"+work+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=trusted_schema(0)"+
		"&_pragma=journal_mode(DELETE)")
	if err != nil {
		return err
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	conn, err := d.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS src`, "file:"+src+"?mode=ro"); err != nil {
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := mergeSections(ctx, tx, sections); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA main.foreign_key_check`)
	if err != nil {
		return err
	}
	bad := ""
	if rows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		_ = rows.Scan(&table, &rowid, &parent, &fkid)
		bad = table + " → " + parent
	}
	rows.Close()
	if bad != "" {
		return apperr.Invalid("sections", "the restored rows refer to rows that do not exist (%s)", bad)
	}
	if err := checkStorageRefs(ctx, tx); err != nil {
		return err
	}
	if !commit {
		return nil
	}
	return tx.Commit()
}

// mergeSections applies the selected sections in tx (main = the live
// copy, src = the backup).
func mergeSections(ctx context.Context, tx *sql.Tx, sections []string) error {
	withGroups := slices.Contains(sections, settings.SectionClientsGroups)
	var groupMap map[int64]int64
	if !withGroups {
		var err error
		if groupMap, err = restoreGroupMap(ctx, tx); err != nil {
			return err
		}
	}
	// The built-in cache target keeps the live store id: its store lives
	// in this machine's cache directory, not in the backup
	// (keepLocalStore).
	var localStore sql.NullString
	if slices.Contains(sections, settings.SectionStorage) {
		err := tx.QueryRowContext(ctx, `SELECT store_id FROM main.storage_targets WHERE id = ?`, storage.LocalTargetID).Scan(&localStore)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	// Delete the parents of every selected section first (clients and
	// groups last: their deletion cascades into the links of the other
	// sections, which are restored too by the dependency rule).
	for _, name := range []string{settings.SectionListsRules, settings.SectionLocalDNS, settings.SectionParental, settings.SectionDHCP,
		settings.SectionDownloadCache, settings.SectionNotifications, settings.SectionStorage, settings.SectionClientsGroups} {
		if !slices.Contains(sections, name) {
			continue
		}
		sec, _ := sectionByName(name)
		for _, t := range sec.parents {
			if _, err := tx.ExecContext(ctx, `DELETE FROM main.`+t); err != nil {
				return fmt.Errorf("restore %s: %w", t, err)
			}
		}
	}
	for _, name := range []string{settings.SectionClientsGroups, settings.SectionListsRules, settings.SectionLocalDNS,
		settings.SectionParental, settings.SectionDHCP, settings.SectionDownloadCache, settings.SectionNotifications, settings.SectionStorage} {
		if !slices.Contains(sections, name) {
			continue
		}
		sec, _ := sectionByName(name)
		for _, t := range sec.tables {
			if col, linked := groupLinkTables[t]; linked && !withGroups {
				if err := copyRemapped(ctx, tx, name, t, col, groupMap); err != nil {
					return err
				}
				continue
			}
			if err := copyTable(ctx, tx, t); err != nil {
				return err
			}
		}
	}
	if slices.Contains(sections, settings.SectionStorage) {
		if _, err := tx.ExecContext(ctx, `UPDATE main.storage_targets SET store_id = ? WHERE id = ?`, localStore.String,
			storage.LocalTargetID); err != nil {
			return fmt.Errorf("restore storage_targets: %w", err)
		}
	}
	return mergeSettings(ctx, tx, sections)
}

func sectionByName(name string) (restoreSection, bool) {
	for _, s := range restoreSectionMap {
		if s.name == name {
			return s, true
		}
	}
	return restoreSection{}, false
}

// tableColumns returns the columns of a table of the live copy.
func tableColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, 'main')`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, `"`+c+`"`)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("restore: no table %s", table)
	}
	return out, rows.Err()
}

// copyTable inserts every row of src.table into main.table by column name.
func copyTable(ctx context.Context, tx *sql.Tx, table string) error {
	cols, err := tableColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	list := strings.Join(cols, ", ")
	if _, err := tx.ExecContext(ctx, `INSERT INTO main.`+table+` (`+list+`) SELECT `+list+` FROM src.`+table); err != nil {
		return fmt.Errorf("restore %s: %w", table, err)
	}
	return nil
}

// restoreGroupMap maps the backup's groups to the live groups by name
// (case-insensitive; group 1 Default always to 1).
func restoreGroupMap(ctx context.Context, tx *sql.Tx) (map[int64]int64, error) {
	live := map[string]int64{}
	rows, err := tx.QueryContext(ctx, `SELECT id, name FROM main.client_groups`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		live[strings.ToLower(name)] = id
	}
	rows.Close()
	out := map[int64]int64{1: 1}
	rows, err = tx.QueryContext(ctx, `SELECT id, name FROM src.client_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		if id == 1 {
			continue
		}
		if l, ok := live[strings.ToLower(name)]; ok {
			out[id] = l
		}
	}
	return out, rows.Err()
}

// copyRemapped inserts the link rows of a group-linked table with their
// groups mapped by name; a link to a group that does not exist live
// refuses the restore.
func copyRemapped(ctx context.Context, tx *sql.Tx, section, table, col string, groupMap map[int64]int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT t.`+col+`, COALESCE(g.name, '#' || t.`+col+`) FROM src.`+table+` t
		LEFT JOIN src.client_groups g ON g.id = t.`+col)
	if err != nil {
		return err
	}
	type miss struct {
		id   int64
		name string
	}
	var missing []miss
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if _, ok := groupMap[id]; !ok {
			missing = append(missing, miss{id, name})
		}
	}
	rows.Close()
	if len(missing) > 0 {
		return apperr.Invalid("sections", "%s of the backup refers to the group %q, which does not exist here: restore clients-and-groups too or create the group first",
			section, missing[0].name)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS restore_group_map (src INTEGER PRIMARY KEY, live INTEGER NOT NULL)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM temp.restore_group_map`); err != nil {
		return err
	}
	for s, l := range groupMap {
		if _, err := tx.ExecContext(ctx, `INSERT INTO temp.restore_group_map (src, live) VALUES (?, ?)`, s, l); err != nil {
			return err
		}
	}
	cols, err := tableColumns(ctx, tx, table)
	if err != nil {
		return err
	}
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = "t." + c
		if c == `"`+col+`"` {
			sel[i] = "m.live"
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO main.`+table+` (`+strings.Join(cols, ", ")+`) SELECT `+strings.Join(sel, ", ")+
		` FROM src.`+table+` t JOIN temp.restore_group_map m ON m.src = t.`+col); err != nil {
		return fmt.Errorf("restore %s: %w", table, err)
	}
	return nil
}

// mergeSettings merges the settings members of the selected sections
// into the live document: settings = every member but dhcp,
// cache.activeStoreId and backups.destination (plus settings_secrets);
// dhcp = the dhcp member; storage = cache.activeStoreId and
// backups.destination. With settings the result is the backup's document
// apart from those members: a member the backup lacks (an older version's
// backup) is left out and decodes from the defaults, as after a full
// restore, so a live member never outlives its secret (settings_secrets is
// replaced as a whole: a live follower or proxy without its token or
// password).
func mergeSettings(ctx context.Context, tx *sql.Tx, sections []string) error {
	withSettings, withDHCP, withStorage := slices.Contains(sections, settings.SectionSettings),
		slices.Contains(sections, settings.SectionDHCP), slices.Contains(sections, settings.SectionStorage)
	if !withSettings && !withDHCP && !withStorage {
		return nil
	}
	var liveDoc, srcDoc string
	if err := tx.QueryRowContext(ctx, `SELECT doc FROM main.settings WHERE id = 1`).Scan(&liveDoc); err != nil {
		return fmt.Errorf("restore settings: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT doc FROM src.settings WHERE id = 1`).Scan(&srcDoc); err != nil {
		return fmt.Errorf("restore settings: %w", err)
	}
	var live, src map[string]jsontext.Value
	if err := json.Unmarshal([]byte(liveDoc), &live); err != nil {
		return apperr.Invalid("sections", "the live settings cannot be read")
	}
	if err := json.Unmarshal([]byte(srcDoc), &src); err != nil {
		return apperr.Invalid("sections", "the settings of the backup cannot be read")
	}
	member := func(doc map[string]jsontext.Value, section, key string) jsontext.Value {
		var m map[string]jsontext.Value
		_ = json.Unmarshal(doc[section], &m)
		return m[key]
	}
	setMember := func(doc map[string]jsontext.Value, section, key string, v jsontext.Value) {
		var m map[string]jsontext.Value
		_ = json.Unmarshal(doc[section], &m)
		if m == nil {
			m = map[string]jsontext.Value{}
		}
		if v == nil {
			delete(m, key)
		} else {
			m[key] = v
		}
		b, _ := json.Marshal(m)
		doc[section] = b
	}
	keepStore, keepDest := member(live, "cache", "activeStoreId"), member(live, "backups", "destination")
	if withSettings {
		merged := map[string]jsontext.Value{}
		for k, v := range src {
			if k != "dhcp" {
				merged[k] = v
			}
		}
		if v, ok := live["dhcp"]; ok {
			merged["dhcp"] = v
		}
		live = merged
		if !withStorage {
			setMember(live, "cache", "activeStoreId", keepStore)
			setMember(live, "backups", "destination", keepDest)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM main.settings_secrets`); err != nil {
			return err
		}
		if err := copyTable(ctx, tx, "settings_secrets"); err != nil {
			return err
		}
	}
	if withDHCP {
		if v, ok := src["dhcp"]; ok {
			live["dhcp"] = v
		} else {
			delete(live, "dhcp")
		}
	}
	if withStorage {
		setMember(live, "cache", "activeStoreId", member(src, "cache", "activeStoreId"))
		setMember(live, "backups", "destination", member(src, "backups", "destination"))
	}
	b, err := json.Marshal(live, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE main.settings SET doc = ?, updated_at = ? WHERE id = 1`, string(b), db.NowMs())
	return err
}

// checkStorageRefs refuses settings whose cache.activeStoreId or
// backups.destination name a storage target the result does not have.
func checkStorageRefs(ctx context.Context, tx *sql.Tx) error {
	var store, dest sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT json_extract(doc, '$.cache.activeStoreId'), json_extract(doc, '$.backups.destination')
		FROM main.settings WHERE id = 1`).Scan(&store, &dest); err != nil {
		return err
	}
	for _, id := range []string{store.String, dest.String} {
		if id == "" || id == "local" {
			continue
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM main.storage_targets WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return apperr.Invalid("sections", "the settings refer to the storage target %s, which the result does not have: restore storage too", id)
		}
	}
	return nil
}

// dryRunRestore runs the merge of a partial restore on temporary copies:
// the upload's section errors come at upload time (the start checks
// again, the live data may have changed meanwhile).
func dryRunRestore(ctx context.Context, livePath, staged string, sections []string) error {
	src, err := migratedCopy(ctx, staged)
	if err != nil {
		return err
	}
	defer removeDB(src)
	work := fmt.Sprintf("%s.dryrun-%d", livePath, time.Now().UnixNano())
	defer removeDB(work)
	if err := liveCopy(ctx, livePath, work); err != nil {
		return fmt.Errorf("copy the live database: %w", err)
	}
	return mergeRestore(ctx, work, src, sections, false)
}

// applyPartialRestore builds the restored database from a copy of the live
// one and the (migrated copy of the) staged backup, and swaps it in like a
// full restore: picache.db.before-restore kept, WAL and SHM removed.
func (a *App) applyPartialRestore(ctx context.Context, staged string, sections []string) error {
	live := a.paths.ConfigDB
	liveNames, err := liveSchemaNames(ctx, live)
	if err != nil {
		return err
	}
	if err := validateBackup(ctx, staged, liveNames); err != nil {
		return err
	}
	src, err := migratedCopy(ctx, staged)
	if err != nil {
		return err
	}
	defer removeDB(src)
	work := live + ".restore-work"
	removeDB(work)
	if err := liveCopy(ctx, live, work); err != nil {
		return fmt.Errorf("copy the live database: %w", err)
	}
	if err := mergeRestore(ctx, work, src, sections, true); err != nil {
		removeDB(work)
		return err
	}
	backup := live + ".before-restore"
	_ = os.Remove(backup)
	if err := os.Rename(live, backup); err != nil {
		removeDB(work)
		return fmt.Errorf("restore: keep old database: %w", err)
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Remove(live + sfx)
	}
	if err := os.Rename(work, live); err != nil {
		_ = os.Rename(backup, live)
		return fmt.Errorf("restore: %w", err)
	}
	return os.Remove(staged)
}

// StageRestoreFile stages a backup file for the next start (`picache
// restore`): the checks of an upload (validateBackup against the live
// database opened read-only, the schema versions, the section rules and
// the dry-run merge), then picache.db.restore through a temporary file
// (O_CREAT|O_EXCL|O_NOFOLLOW) and a rename, with restore.sections and
// restore.by = "cli". A staged restore that exists already is refused
// unless force. It never swaps databases (the service may hold them).
func StageRestoreFile(ctx context.Context, dataDir, file string, sections []string, force bool) error {
	live := filepath.Join(dataDir, "picache.db")
	staged := live + ".restore"
	if sections != nil {
		var err error
		if sections, err = settings.CheckSections("sections", sections, settings.RestoreSections, "restore"); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(staged); err == nil && !force {
		return errors.New("a restore is staged already (" + staged + "); restart PiCache to apply it, or use --force to replace it")
	}
	fi, err := os.Lstat(file)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", file)
	}
	if fi.Size() > maxRestoreBytes {
		return errors.New("the backup is larger than 512 MiB")
	}
	in, err := os.OpenFile(file, os.O_RDONLY|oNoFollow, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(dataDir, ".picache.db.restore-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".tmp")
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	defer removeDB(tmp)
	n, err := io.Copy(out, io.LimitReader(in, maxRestoreBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxRestoreBytes {
		return errors.New("the backup is larger than 512 MiB")
	}
	liveNames, err := liveSchemaNames(ctx, live)
	if err != nil {
		return err
	}
	if err := validateBackup(ctx, tmp, liveNames); err != nil {
		return fmt.Errorf("not a usable PiCache backup: %w", err)
	}
	if sections != nil {
		if err := dryRunRestore(ctx, live, tmp, sections); err != nil {
			return err
		}
	}
	if err := writeRestoreMeta(ctx, tmp, sections, "cli"); err != nil {
		return err
	}
	return os.Rename(tmp, staged)
}
