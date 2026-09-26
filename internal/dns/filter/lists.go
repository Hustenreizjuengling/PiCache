package filter

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
)

const (
	maxLists          = 100
	maxURLLen         = 2048
	maxNameLen        = 100
	maxCommentLen     = 500
	maxGroupsPerEntry = 64
)

// Lists returns all lists.
func (e *Engine) Lists(ctx context.Context) ([]List, error) {
	if err := e.reloadListGroups(ctx); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]List, 0, len(e.lists))
	for _, rt := range sortedLists(e.lists) {
		out = append(out, rt.copy())
	}
	return out, nil
}

// list returns one list.
func (e *Engine) list(id int64) (List, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rt, ok := e.lists[id]
	if !ok {
		return List{}, apperr.NotFound("list", id)
	}
	return rt.copy(), nil
}

func (rt *listRT) copy() List {
	l := rt.List
	l.GroupIDs = slices.Clone(nonNil(rt.GroupIDs))
	l.TLDBlocksIgnored = rt.tldBlocksIgnored()
	l.IPBlocksIgnored = rt.ipBlocksIgnored()
	return l
}

// tldBlocksIgnored returns the entries of the loaded copy that the TLD
// guard ignores (0 while none is loaded in the current format).
func (rt *listRT) tldBlocksIgnored() int {
	if rt.parsed == nil || rt.parsed.format != rt.format() || rt.parsed.format.ips {
		return 0
	}
	return rt.parsed.broad
}

// ipBlocksIgnored returns the blocks of the loaded copy of a list of format
// ips that the IP guard ignores (0 while none is loaded in the current
// format).
func (rt *listRT) ipBlocksIgnored() int {
	if rt.parsed == nil || rt.parsed.format != rt.format() || !rt.parsed.format.ips {
		return 0
	}
	return rt.parsed.broad
}

// format returns the parse format of the list's configuration.
func (rt *listRT) format() listFormat {
	return formatOf(rt.Kind, rt.PlainDomains, rt.Category, rt.Format)
}

func sortedLists(m map[int64]*listRT) []*listRT {
	out := make([]*listRT, 0, len(m))
	for _, rt := range m {
		out = append(out, rt)
	}
	slices.SortFunc(out, func(a, b *listRT) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// CreateList adds a list and triggers its first download (background).
func (e *Engine) CreateList(ctx context.Context, in ListInput) (List, error) {
	in, nameGiven, err := e.validateList(in)
	if err != nil {
		return List{}, err
	}
	entry, isCatalog := catalogByURL(in.URL)
	if isCatalog && in.Kind != entry.Kind {
		if entry.Kind == "allow" {
			return List{}, apperr.Invalid("kind", "this catalogue list is an allowlist")
		}
		return List{}, apperr.Invalid("kind", "this catalogue list is a blocklist")
	}
	format := FormatDomains
	if in.Format != nil {
		format = *in.Format
	}
	if format == FormatIPs && isCatalog {
		return List{}, apperr.Invalid("format", "this catalogue list holds domain names")
	}
	in.Format = &format
	fallback := CategoryOther
	if isCatalog {
		fallback = entry.Category
	}
	explicit := in.Category
	if in.Category, err = resolveCategory(in.Category, fallback, in.Kind); err != nil {
		return List{}, err
	}
	if in.Category, err = ipListCategory(format, in.Category, explicit != ""); err != nil {
		return List{}, err
	}
	if in.GroupIDs == nil {
		in.GroupIDs = []int64{defaultGroupID}
	}
	return e.createList(ctx, in, entry.Key, !nameGiven)
}

// ipListCategory checks the category of a list of answer addresses: a
// blocklist has the category security or other, an allowlist allow; such a
// list is never a protection list. A category the request named (explicit)
// that does not fit is refused, a stored or derived one becomes other.
func ipListCategory(format, category string, explicit bool) (string, error) {
	if format != FormatIPs || category == CategorySecurity || category == CategoryOther || category == CategoryAllow {
		return category, nil
	}
	if explicit {
		return "", apperr.Invalid("category", "lists of answer addresses have the category security or other")
	}
	return CategoryOther, nil
}

// createList inserts a validated list (catalogKey: the catalogue key of its
// URL, "" if none; nameAuto: the name was derived from the URL and the
// first download's title replaces it).
func (e *Engine) createList(ctx context.Context, in ListInput, catalogKey string, nameAuto bool) (List, error) {
	e.listMu.Lock()
	defer e.listMu.Unlock()
	now := e.now()
	var id int64
	err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM filter_lists`).Scan(&n); err != nil {
			return err
		}
		if n >= maxLists {
			return apperr.Conflict("at most %d lists are supported", maxLists)
		}
		if err := checkGroups(ctx, tx, in.GroupIDs); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO filter_lists (name, url, kind, plain_domains, category, catalog_key, enabled, comment,
				created_at, format, name_auto)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, in.Name, in.URL, in.Kind, in.PlainDomains, in.Category, catalogKey, in.Enabled,
			in.Comment, db.Ms(now), *in.Format, nameAuto)
		if isUniqueViolation(err) {
			return apperr.Conflict("a list with this URL already exists")
		}
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return setGroups(ctx, tx, "filter_list_groups", "list_id", id, in.GroupIDs)
	})
	if err != nil {
		return List{}, err
	}
	rt := &listRT{
		List: List{
			ID: id, Name: in.Name, NameAuto: nameAuto, URL: in.URL, Kind: in.Kind, Format: *in.Format, PlainDomains: in.PlainDomains,
			Category: in.Category, CatalogKey: catalogKey,
			Enabled: in.Enabled, GroupIDs: in.GroupIDs, Comment: in.Comment, Status: statusPending,
			CreatedAt: db.Time(db.Ms(now)),
		},
		jitter:       newJitter(),
		wantDownload: in.Enabled,
	}
	e.mu.Lock()
	e.lists[id] = rt
	l := rt.copy()
	e.mu.Unlock()
	e.signal()
	e.log.Info("list added", slog.Int64("id", id), slog.String("name", in.Name), slog.String("url", redactURL(in.URL)))
	return l, nil
}

// UpdateList updates a list.
func (e *Engine) UpdateList(ctx context.Context, id int64, in ListInput) (List, error) {
	in, nameGiven, err := e.validateList(in)
	if err != nil {
		return List{}, err
	}
	e.listMu.Lock()
	defer e.listMu.Unlock()
	e.mu.Lock()
	rt, ok := e.lists[id]
	var old List
	if ok {
		old = rt.copy()
	}
	e.mu.Unlock()
	if !ok {
		return List{}, apperr.NotFound("list", id)
	}
	if in.GroupIDs == nil {
		in.GroupIDs = old.GroupIDs
	}
	format, formatNamed := old.Format, in.Format != nil
	if formatNamed {
		format = *in.Format
	}
	if _, isCatalog := catalogByURL(in.URL); isCatalog && format == FormatIPs {
		if formatNamed {
			return List{}, apperr.Invalid("format", "this catalogue list holds domain names")
		}
		format = FormatDomains
	}
	in.Format = &format
	explicit := in.Category
	if in.Category, err = resolveCategory(in.Category, old.Category, in.Kind); err != nil {
		return List{}, err
	}
	if in.Category, err = ipListCategory(format, in.Category, explicit != ""); err != nil {
		return List{}, err
	}
	// The name: a new one given by the request is the user's; an empty one
	// makes it automatic again (the host name now, the title after the next
	// successful download); the stored name sent back keeps its state.
	nameAuto := old.NameAuto
	switch {
	case !nameGiven:
		nameAuto = true
	case in.Name != old.Name:
		nameAuto = false
	}
	urlChanged := in.URL != old.URL
	catalogKey := old.CatalogKey
	if urlChanged {
		entry, _ := catalogByURL(in.URL)
		catalogKey = entry.Key
	}
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkGroups(ctx, tx, in.GroupIDs); err != nil {
			return err
		}
		query := `UPDATE filter_lists SET name = ?, url = ?, kind = ?, plain_domains = ?, category = ?, catalog_key = ?,
			enabled = ?, comment = ?, format = ?, name_auto = ?`
		if urlChanged {
			query += `, status = 'pending', last_error = '', last_updated = 0, last_checked = 0, last_success = 0,
				entries = 0, invalid = 0, unsupported = 0, size_bytes = 0, etag = '', last_modified = '', content_hash = ''`
		}
		res, err := tx.ExecContext(ctx, query+` WHERE id = ?`, in.Name, in.URL, in.Kind, in.PlainDomains, in.Category, catalogKey,
			in.Enabled, in.Comment, format, nameAuto, id)
		if isUniqueViolation(err) {
			return apperr.Conflict("a list with this URL already exists")
		}
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return cmp.Or(err, apperr.NotFound("list", id))
		}
		return setGroups(ctx, tx, "filter_list_groups", "list_id", id, in.GroupIDs)
	})
	if err != nil {
		return List{}, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	rt, ok = e.lists[id]
	if !ok {
		return List{}, apperr.NotFound("list", id)
	}
	oldFormat := rt.format()
	rt.Name, rt.NameAuto, rt.URL, rt.Kind, rt.PlainDomains = in.Name, nameAuto, in.URL, in.Kind, in.PlainDomains
	rt.Format, rt.Category, rt.CatalogKey = format, in.Category, catalogKey
	rt.Comment, rt.GroupIDs = in.Comment, in.GroupIDs
	reparse := rt.format() != oldFormat // kind, format, plainDomains, or a category change into or out of abused-tlds
	wasEnabled := rt.Enabled
	rt.Enabled = in.Enabled
	if urlChanged {
		rt.List = List{
			ID: rt.ID, Name: rt.Name, NameAuto: rt.NameAuto, URL: rt.URL, Kind: rt.Kind, Format: rt.Format,
			PlainDomains: rt.PlainDomains, Category: rt.Category, CatalogKey: rt.CatalogKey,
			Enabled: rt.Enabled, GroupIDs: rt.GroupIDs, Comment: rt.Comment, Status: statusPending,
			CreatedAt: rt.CreatedAt,
		}
		rt.etag, rt.lastModified, rt.hash = "", "", ""
		e.removeFiles(id)
		if rt.parsed != nil {
			rt.parsed = nil
			e.parseGen++
		}
		rt.wantDownload = rt.Enabled
	}
	switch {
	case !rt.Enabled:
		rt.wantDownload, rt.wantReparse = false, false
		if rt.parsed != nil {
			rt.parsed = nil // free the memory; re-read from the cached copy when enabled again
			e.parseGen++
		}
	case urlChanged:
	case !wasEnabled || reparse:
		if e.hasCache(id) {
			rt.wantReparse = true
		} else {
			rt.wantDownload = true
		}
	}
	e.publishLocked(nil, nil, nil)
	e.requestCompile()
	e.signal()
	return rt.copy(), nil
}

// DeleteList removes a list and its cached copy.
func (e *Engine) DeleteList(ctx context.Context, id int64) error {
	e.listMu.Lock()
	defer e.listMu.Unlock()
	res, err := e.db.W.ExecContext(ctx, `DELETE FROM filter_lists WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return cmp.Or(err, apperr.NotFound("list", id))
	}
	e.mu.Lock()
	if _, ok := e.lists[id]; ok {
		delete(e.lists, id)
		e.parseGen++
	}
	e.removeFiles(id)
	e.publishLocked(nil, nil, nil)
	e.mu.Unlock()
	e.requestCompile()
	return nil
}

// validateList normalises and validates a list input; nameGiven reports
// whether the request named the list (an empty name is derived from the
// URL: its host name, or the file name of a local list).
func (e *Engine) validateList(in ListInput) (_ ListInput, nameGiven bool, _ error) {
	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" {
		return in, false, apperr.Invalid("url", "is required")
	}
	if len(in.URL) > maxURLLen {
		return in, false, apperr.Invalid("url", "must be at most %d characters", maxURLLen)
	}
	u, err := e.checkListURL(in.URL)
	if err != nil {
		return in, false, err
	}
	in.URL = u.String()
	in.Name = strings.TrimSpace(in.Name)
	nameGiven = in.Name != ""
	if !nameGiven {
		in.Name = u.Hostname()
		if u.Scheme == "file" || in.Name == "" {
			in.Name = path.Base(u.Path)
		}
	}
	if err := checkText("name", in.Name, maxNameLen); err != nil {
		return in, false, err
	}
	in.Comment = strings.TrimSpace(in.Comment)
	if err := checkText("comment", in.Comment, maxCommentLen); err != nil {
		return in, false, err
	}
	if in.Format != nil {
		f := strings.ToLower(strings.TrimSpace(*in.Format))
		if f != FormatDomains && f != FormatIPs {
			return in, false, apperr.Invalid("format", "must be %s or %s", FormatDomains, FormatIPs)
		}
		in.Format = &f
	}
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	if in.Kind == "" {
		in.Kind = "block"
		if c, ok := catalogByURL(in.URL); ok {
			in.Kind = c.Kind // a catalogue URL takes the entry's kind
		}
	}
	if in.Kind != "block" && in.Kind != "allow" {
		return in, false, apperr.Invalid("kind", "must be block or allow")
	}
	in.Category = strings.ToLower(strings.TrimSpace(in.Category))
	if in.Category != "" && !validCategory(in.Category) {
		return in, false, apperr.Invalid("category", "must be one of %s or %s", strings.Join(Categories, ", "), CategoryOther)
	}
	in.PlainDomains = cmp.Or(strings.ToLower(strings.TrimSpace(in.PlainDomains)), "exact")
	if in.PlainDomains != "exact" && in.PlainDomains != "subtree" {
		return in, false, apperr.Invalid("plainDomains", "must be exact or subtree")
	}
	if in.GroupIDs, err = normalizeGroups(in.GroupIDs); err != nil {
		return in, false, err
	}
	return in, nameGiven, nil
}

// resolveCategory returns the category to store: explicit (validated by
// validateList) or, when empty, fallback (the catalogue entry's on create,
// the stored one on update), adjusted to the kind: an allowlist always has
// "allow" and a blocklist never.
func resolveCategory(explicit, fallback, kind string) (string, error) {
	switch {
	case explicit == "":
		return categoryForKind(kind, fallback), nil
	case kind == "allow" && explicit != CategoryAllow:
		return "", apperr.Invalid("category", "an allowlist always has the category %s", CategoryAllow)
	case kind == "block" && explicit == CategoryAllow:
		return "", apperr.Invalid("category", "the category %s is only for allowlists", CategoryAllow)
	}
	return explicit, nil
}

// checkText validates a free-text field.
func checkText(field, s string, max int) error {
	if utf8.RuneCountInString(s) > max {
		return apperr.Invalid(field, "must be at most %d characters", max)
	}
	if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return apperr.Invalid(field, "must not contain control characters")
	}
	return nil
}

// checkListURL validates a list URL: https; http only for private IP
// literal hosts; file:// only below <lists>/local/.
func (e *Engine) checkListURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return nil, apperr.Invalid("url", "must be an absolute URL")
	}
	if u.User != nil {
		return nil, apperr.Invalid("url", "must not contain credentials")
	}
	u.Fragment, u.RawFragment = "", ""
	switch u.Scheme {
	case "https":
		if u.Hostname() == "" {
			return nil, apperr.Invalid("url", "must contain a host")
		}
		if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !netutil.IsPublicUnicast(ip) && !isPrivateLiteral(u.Hostname()) {
			return nil, apperr.Invalid("url", "the address %s is not allowed", ip)
		}
	case "http":
		if !isPrivateLiteral(u.Hostname()) {
			return nil, apperr.Invalid("url", "must use https (plain http is allowed only for private IP addresses)")
		}
	case "file":
		if _, err := e.localPath(u); err != nil {
			return nil, err
		}
	default:
		return nil, apperr.Invalid("url", "must be an https URL")
	}
	return u, nil
}

// localPath returns the path of a file:// list relative to e.localDir.
func (e *Engine) localPath(u *url.URL) (string, error) {
	errOutside := apperr.Invalid("url", "local lists must be files in %s", e.localDir)
	if u.Host != "" && u.Host != "localhost" || u.RawQuery != "" || u.Path == "" {
		return "", errOutside
	}
	p := u.Path
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:] // file:///C:/…
	}
	rel, err := filepath.Rel(e.localDir, filepath.Clean(filepath.FromSlash(p)))
	if err != nil || !filepath.IsLocal(rel) {
		return "", errOutside
	}
	return rel, nil
}

// isPrivateLiteral reports whether host is an IP literal in a private,
// loopback or CGNAT range (never link-local).
func isPrivateLiteral(host string) bool {
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = netutil.Canon(ip)
	return netutil.IsPrivateLAN(ip) && !ip.IsLinkLocalUnicast()
}

// redactURL removes credentials, query and fragment for logs and errors.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

func (e *Engine) cachePath(id int64) string {
	return filepath.Join(e.dir, strconv.FormatInt(id, 10)+".txt")
}

func (e *Engine) tmpPath(id int64) string {
	return filepath.Join(e.dir, strconv.FormatInt(id, 10)+".tmp")
}

func (e *Engine) hasCache(id int64) bool {
	fi, err := os.Stat(e.cachePath(id))
	return err == nil && fi.Mode().IsRegular()
}

// removeFiles deletes the cached copy of a list.
func (e *Engine) removeFiles(id int64) {
	for _, p := range []string{e.cachePath(id), e.tmpPath(id)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			e.log.Warn("remove list file", slog.String("path", p), slog.Any("err", err))
		}
	}
}

// reloadListGroups re-reads list group memberships (group deletions cascade
// in the database) and republishes them if they changed.
func (e *Engine) reloadListGroups(ctx context.Context) error {
	groups, err := loadGroupMap(ctx, e.db.R, `SELECT list_id, group_id FROM filter_list_groups ORDER BY list_id, group_id`)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	for id, rt := range e.lists {
		if g := nonNil(groups[id]); !slices.Equal(g, rt.GroupIDs) {
			rt.GroupIDs = g
			changed = true
		}
	}
	if changed {
		e.publishLocked(nil, nil, nil)
	}
	return nil
}

// ReloadGroups re-reads the group memberships of lists and rules. Deleting a
// client group removes its memberships in the database (ON DELETE CASCADE);
// call this after group changes so a reused group ID never inherits stale
// memberships. The engine also reconciles once a minute.
func (e *Engine) ReloadGroups(ctx context.Context) error {
	if err := e.reloadListGroups(ctx); err != nil {
		return err
	}
	return e.reloadRuleGroups(ctx)
}
