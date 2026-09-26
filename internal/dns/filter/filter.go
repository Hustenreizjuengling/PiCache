// Package filter implements blocklists and custom rules: list subscriptions
// (fetch, parse, compile), the list catalogue with its categories and the
// category switches of the parental controls (SetPresets), user allow/deny
// rules, group scoping and the hot-path matcher (docs/ARCHITECTURE.md 7.2).
// An enabled list of a protection category (IsProtection) is also checked
// on its own by CheckProtection, which the DNS server enforces like
// parental controls.
//
// Tables (picache.db, component "filter"): filter_lists, filter_rules,
// filter_list_groups(list_id, group_id), filter_rule_groups(rule_id,
// group_id); group_id references client_groups(id) ON DELETE CASCADE. At
// first start the default list is created and linked to group 1.
//
// Compilation: list files are parsed line by line (bufio, never io.ReadAll)
// only when their downloaded content changed; the per-list parse result is
// kept, and the matcher snapshot is rebuilt from those results. Editing a
// list's name or groups swaps only the small source→groups table; the (small)
// user-rule matcher is rebuilt synchronously on every rule change. List
// recompile calls are coalesced (one running + one pending). Compiled
// patterns (regex + wildcard) are capped at 20 000 and at an estimated
// program size of 1 Mi instructions in total; a single pattern may expand to
// at most ~4 096 instructions (regexCost; measured on the parse tree, before
// anything is compiled), so a hostile list cannot turn short "{999}"
// repetitions into gigabytes of compiled programs. Excess within a list is
// counted as unsupported, excess across lists is reported as
// Stats.PatternsDropped. Explain rescans list files one at a time with a
// 10 s timeout.
//
// A changed download that has no entries (empty, blank or comment-only)
// while the cached copy has some is rejected as failed-cached: the last good
// copy stays in effect and the download is retried.
//
// Downloads: at most 256 MiB (io.LimitReader), streamed to <lists>/<id>.tmp;
// the fetch client does not follow redirects itself: this package follows up
// to 5 manually, re-checking each hop; netutil.WithAllowPrivate is set only
// when the configured URL host is a private IP literal, never after a
// redirect to another host. http:// URLs are accepted only for private IP
// literal hosts.
//
// The parser counts a subtree, wildcard or pattern block of a single label
// or an ICANN public suffix as invalid (every category but abused-tlds), so
// a broken or hostile list cannot block a whole TLD.
//
// $badfilter cancels matching rules of the same list. User rules are
// bounded (20 000, of which at most 1 000 regular expressions); a list
// subscription is the right tool for larger sets.
package filter

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

// Action is a filtering verdict.
type Action uint8

const (
	ActionNone  Action = iota // no rule matched
	ActionAllow               // explicitly allowed
	ActionBlock               // blocked
)

// String returns "allow", "block" or "none".
func (a Action) String() string {
	switch a {
	case ActionAllow:
		return "allow"
	case ActionBlock:
		return "block"
	}
	return "none"
}

// Decision is the result of Check.
type Decision struct {
	Action    Action
	Source    string // "list" | "rule"
	Kind      string // "exact" | "subtree" | "regex"
	ListID    int64
	RuleID    int64
	Name      string // list name or rule pattern, for logs ("blocked by …")
	Category  string // list decisions: the list's category ("" for rules)
	Important bool
}

// Blocked reports whether the decision blocks.
func (d Decision) Blocked() bool { return d.Action == ActionBlock }

// List is a subscribed block or allow list. Category is a catalogue
// category or "other" ("allow" exactly for kind allow); an enabled list of
// a protection category is enforced like parental controls
// (CheckProtection). CatalogKey is the key of the catalogue entry with
// exactly this URL ("" for the user's own lists), set on create and when
// the URL changes. TLDBlocksIgnored counts the entries of the loaded copy
// that would block a whole TLD and that the TLD guard ignores (part of
// Invalid; 0 while no copy is loaded, e.g. for a disabled list).
type List struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	Kind         string    `json:"kind"`         // block | allow
	PlainDomains string    `json:"plainDomains"` // exact | subtree
	Category     string    `json:"category"`
	CatalogKey   string    `json:"catalogKey"`
	Enabled      bool      `json:"enabled"`
	GroupIDs     []int64   `json:"groupIds"`
	Comment      string    `json:"comment"`
	Status       string    `json:"status"` // pending | ok | unchanged | failed-cached | failed-empty
	LastError    string    `json:"lastError,omitempty"`
	LastUpdated  time.Time `json:"lastUpdated,omitzero"` // content last changed
	LastChecked  time.Time `json:"lastChecked,omitzero"`
	LastSuccess  time.Time `json:"lastSuccess,omitzero"`
	Entries      int       `json:"entries"`
	Invalid      int       `json:"invalid"`
	Unsupported  int       `json:"unsupported"`
	SizeBytes    int64     `json:"sizeBytes"`
	CreatedAt    time.Time `json:"createdAt"`

	// Not stored: counted from the loaded copy (listRT.copy).
	TLDBlocksIgnored int `json:"tldBlocksIgnored"`
}

// ListInput creates or updates a list. An empty Kind takes the kind of
// the catalogue entry with the same URL, else "block"; an empty
// PlainDomains defaults to "exact"; an empty Name is derived from the URL.
// An empty Category takes the catalogue entry's on create (else "other",
// "allow" for allowlists) and keeps the stored one on update. GroupIDs nil
// means the Default group on create and "unchanged" on update; an empty
// non-nil slice means no group (the list applies to nobody).
type ListInput struct {
	Name         string  `json:"name"`
	URL          string  `json:"url"`
	Kind         string  `json:"kind"`
	PlainDomains string  `json:"plainDomains"`
	Category     string  `json:"category"`
	Enabled      bool    `json:"enabled"`
	GroupIDs     []int64 `json:"groupIds"`
	Comment      string  `json:"comment"`
}

// Rule is a user allow/deny rule.
type Rule struct {
	ID        int64     `json:"id"`
	Action    string    `json:"action"` // allow | block
	Type      string    `json:"type"`   // exact | subtree | regex
	Pattern   string    `json:"pattern"`
	Enabled   bool      `json:"enabled"`
	GroupIDs  []int64   `json:"groupIds"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RuleInput creates or updates a rule. For type subtree, "*.example.com",
// "||example.com^" and "example.com" are all accepted and stored as
// "example.com". Regex patterns are Go RE2, ≤ 1024 characters (matched
// case-insensitively; enclosing slashes are removed). GroupIDs as in
// ListInput.
type RuleInput struct {
	Action   string  `json:"action"`
	Type     string  `json:"type"`
	Pattern  string  `json:"pattern"`
	Enabled  bool    `json:"enabled"`
	GroupIDs []int64 `json:"groupIds"`
	Comment  string  `json:"comment"`
}

// RuleQuery filters rules.
type RuleQuery struct {
	Action string
	Type   string
	Search string
}

// Match is one matching source for Explain.
type Match struct {
	Action    string  `json:"action"`
	Source    string  `json:"source"` // list | rule
	Kind      string  `json:"kind"`
	ListID    int64   `json:"listId,omitempty"`
	RuleID    int64   `json:"ruleId,omitempty"`
	Name      string  `json:"name"`    // list name or rule pattern
	Pattern   string  `json:"pattern"` // the matching entry (domain, ABP rule, regex)
	Important bool    `json:"important,omitempty"`
	GroupIDs  []int64 `json:"groupIds"`
	Applies   bool    `json:"applies"` // shares an enabled group with the client
	Decisive  bool    `json:"decisive"`
	// Category is the list's category ("" for rules): the query panel
	// shows list matches of category privacy as a known tracker (the
	// substitute for an external tracker database).
	Category string `json:"category"`
}

// Stats describes the compiled matcher.
type Stats struct {
	Lists           int       `json:"lists"`
	Entries         int       `json:"entries"`
	Patterns        int       `json:"patterns"`        // regex + wildcard patterns
	PatternsDropped int       `json:"patternsDropped"` // patterns beyond the total caps (20 000 patterns, 1 Mi estimated instructions)
	Rules           int       `json:"rules"`
	CompiledAt      time.Time `json:"compiledAt,omitzero"`
	CompileMs       int64     `json:"compileMs"`
	MemoryBytes     int64     `json:"memoryBytes"`
	Updating        bool      `json:"updating"`
	FailedLists     int       `json:"failedLists"`   // enabled lists in failed-* state
	StaleLists      int       `json:"staleLists"`    // last success older than 3× update interval
	TLDGuardLists   int       `json:"tldGuardLists"` // enabled own lists (no catalogue key) with entries the TLD guard ignores
}

// CatalogEntry is a curated list suggestion (embedded, no network needed).
// Entries is the entry count of the verification run at release time (a
// memory estimate: about EntryBytes per entry).
type CatalogEntry struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	DescriptionDe string `json:"descriptionDe"`
	URL           string `json:"url"`
	Kind          string `json:"kind"`     // block | allow
	Category      string `json:"category"` // Categories, never "other"
	PlainDomains  string `json:"plainDomains"`
	Recommended   bool   `json:"recommended"` // suggested for its category
	Entries       int    `json:"entries"`
	Maintainer    string `json:"maintainer"`
	License       string `json:"license"` // "" when the maintainer states none
	Homepage      string `json:"homepage"`
}

// List status values.
const (
	statusPending      = "pending"
	statusOK           = "ok"
	statusUnchanged    = "unchanged"
	statusFailedCached = "failed-cached"
	statusFailedEmpty  = "failed-empty"
)

// listRT is the in-memory state of a list: its row plus runtime data.
type listRT struct {
	List
	etag, lastModified, hash string
	parsed                   *parsed // nil when not loaded (disabled, never downloaded)
	jitter                   float64 // factor in [0.9, 1.1] for the next scheduled update
	wantDownload             bool
	wantReparse              bool
}

// Engine owns lists, rules and the compiled matcher.
type Engine struct {
	db       *db.DB
	set      *settings.Store
	log      *slog.Logger
	fetch    *http.Client
	dir      string // downloaded copies: <dir>/<id>.txt
	localDir string // file:// lists must live here
	maxBytes int64  // download size cap
	now      func() time.Time

	snap atomic.Pointer[snapshot] // read lock-free by Check

	mu          sync.Mutex // guards the fields below and snapshot publication
	lists       map[int64]*listRT
	parseGen    uint64 // incremented whenever the set of parse results changes
	compiledGen uint64 // parseGen the current list matcher was built from
	compiledAt  time.Time
	compileMs   int64
	stopped     bool

	compileMu sync.Mutex    // serialises list matcher builds
	ruleMu    sync.Mutex    // serialises rule writes and rule matcher rebuilds
	listMu    sync.Mutex    // serialises list writes (create, update, delete, SetPresets) with their in-memory update
	dlSem     chan struct{} // cap 1: serialises downloads and parses of list files
	explain   chan struct{} // bounds concurrent Explain rescans

	compileCh chan struct{} // cap 1: coalesced recompile request
	wake      chan struct{} // cap 1: list work is pending
	busy      atomic.Int32  // running downloads, parses and compiles
	inflight  sync.WaitGroup
	base      context.Context // cancelled when Start returns
	cancel    context.CancelFunc
}

// New creates the engine. fetch is the HTTP client for list downloads
// (SafeDialer over the bypass resolver, redirects not followed); listsDir
// stores downloaded copies. The clients package must be migrated first.
func New(ctx context.Context, d *db.DB, set *settings.Store, fetch *http.Client, listsDir string, log *slog.Logger) (*Engine, error) {
	if err := d.Migrate(ctx, "filter", migrations); err != nil {
		return nil, err
	}
	dir, err := filepath.Abs(listsDir)
	if err != nil {
		return nil, fmt.Errorf("filter: lists dir: %w", err)
	}
	if err := backfillCategories(ctx, d, dir); err != nil {
		return nil, fmt.Errorf("filter: list categories: %w", err)
	}
	localDir := filepath.Join(dir, "local")
	if err := os.MkdirAll(localDir, 0o750); err != nil {
		return nil, fmt.Errorf("filter: create %s: %w", localDir, err)
	}
	base, cancel := context.WithCancel(context.Background())
	e := &Engine{
		db: d, set: set, log: log.With(slog.String("component", "filter")),
		fetch: fetch, dir: dir, localDir: localDir, maxBytes: maxListBytes, now: utcNow,
		lists:     map[int64]*listRT{},
		compileCh: make(chan struct{}, 1), wake: make(chan struct{}, 1), dlSem: make(chan struct{}, 1),
		explain: make(chan struct{}, maxConcurrentExplains),
		base:    base, cancel: cancel,
	}
	e.snap.Store(emptySnapshot)
	rows, err := loadLists(ctx, d.R)
	if err != nil {
		cancel()
		return nil, err
	}
	for _, rt := range rows {
		rt.jitter = newJitter()
		e.lists[rt.ID] = rt
	}
	if err := e.rebuildRules(ctx); err != nil {
		cancel()
		return nil, err
	}
	return e, nil
}

func newJitter() float64 { return 0.9 + rand.Float64()*0.2 }

// Start loads cached list files, compiles and schedules updates (±10 %
// jitter). Blocks until ctx is done and its goroutines have exited.
func (e *Engine) Start(ctx context.Context) {
	defer e.stop()
	e.loadCached(ctx)
	e.requestCompile()
	var wg sync.WaitGroup
	wg.Go(func() { e.compileLoop(ctx) })
	wg.Go(func() { e.updateLoop(ctx) })
	wg.Wait()
}

// stop ends in-flight synchronous operations (RefreshList) and waits for them.
func (e *Engine) stop() {
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	e.cancel()
	e.inflight.Wait()
}

// beginOp registers a synchronous long-running operation; false after Start returned.
func (e *Engine) beginOp() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return false
	}
	e.inflight.Add(1)
	return true
}

// Check evaluates qname (lower-case, no trailing dot) for a client with the
// given enabled group IDs, with the precedence of ARCHITECTURE 7.2. Hot
// path: lock-free, allocation-free in the common case.
func (e *Engine) Check(qname string, groups []int64) Decision {
	return e.snap.Load().check(qname, groups, false)
}

// CheckRules evaluates only user rules (used before the download cache DNS
// answers: a user block rule for the client's groups wins over them).
func (e *Engine) CheckRules(qname string, groups []int64) Decision {
	return e.snap.Load().check(qname, groups, true)
}

// CheckProtection evaluates only the enabled protection lists (lists of
// the protection categories) that share a group with groups, with the
// list precedence of ARCHITECTURE 7.2 (steps 6–9 and 11): user rules and
// other lists are ignored, so an allowlist never lifts a protection-list
// block, but a protection list's own @@ entries apply. The DNS server
// enforces it like parental controls (step 7a). Hot path: lock-free and
// allocation-free; it returns at once when no protection list is enabled.
func (e *Engine) CheckProtection(qname string, groups []int64) Decision {
	return e.snap.Load().checkProtection(qname, groups)
}

// Stats returns matcher statistics.
func (e *Engine) Stats() Stats {
	s := e.snap.Load()
	interval, now := e.interval(), e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	st := Stats{
		Lists:           len(s.lists.ids),
		Entries:         s.lists.entries,
		Patterns:        s.lists.patterns,
		PatternsDropped: s.lists.dropped,
		Rules:           len(s.rules.rules),
		CompiledAt:      e.compiledAt,
		CompileMs:       e.compileMs,
		MemoryBytes:     s.lists.memory + s.rules.memory,
		Updating:        e.busy.Load() > 0,
	}
	for _, rt := range e.lists {
		if rt.parsed != nil {
			st.MemoryBytes += rt.parsed.memory()
		}
		if !rt.Enabled {
			continue
		}
		if rt.Status == statusFailedCached || rt.Status == statusFailedEmpty {
			st.FailedLists++
		}
		if rt.CatalogKey == "" && rt.tldBlocksIgnored() > 0 {
			st.TLDGuardLists++
		}
		last := rt.LastSuccess
		if last.IsZero() {
			last = rt.CreatedAt
		}
		if interval > 0 && now.Sub(last) > 3*interval {
			st.StaleLists++
		}
	}
	return st
}

// publishLocked stores a new snapshot. nil arguments keep the current
// matcher; list names and groups are always rebuilt from e.lists, so a list
// that was disabled or deleted stops applying immediately. e.mu must be held.
func (e *Engine) publishLocked(lists *listMatcher, rules *ruleMatcher) {
	cur := e.snap.Load()
	if lists == nil {
		lists = cur.lists
	}
	if rules == nil {
		rules = cur.rules
	}
	next := &snapshot{
		lists:      lists,
		rules:      rules,
		listNames:  make([]string, len(lists.ids)),
		listCats:   make([]string, len(lists.ids)),
		listGroups: make([][]int64, len(lists.ids)),
		protGroups: make([][]int64, len(lists.ids)),
	}
	for i, id := range lists.ids {
		if rt, ok := e.lists[id]; ok {
			next.listNames[i] = rt.Name
			next.listCats[i] = rt.Category
			if rt.Enabled {
				next.listGroups[i] = rt.GroupIDs
				if IsProtection(rt.Category) && len(rt.GroupIDs) > 0 {
					next.protGroups[i] = rt.GroupIDs
					next.hasProt = true
				}
			}
		}
	}
	e.snap.Store(next)
}

// requestCompile asks the compile loop for a rebuild (coalesced).
func (e *Engine) requestCompile() {
	select {
	case e.compileCh <- struct{}{}:
	default:
	}
}

// signal wakes the update loop.
func (e *Engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *Engine) compileLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.compileCh:
			e.compile()
		}
	}
}

// compile rebuilds the list matcher from the parse results of the enabled
// lists, unless nothing changed since the last build.
func (e *Engine) compile() {
	e.compileMu.Lock()
	defer e.compileMu.Unlock()
	e.mu.Lock()
	gen := e.parseGen
	if gen == e.compiledGen && !e.compiledAt.IsZero() {
		e.mu.Unlock()
		return
	}
	var ids []int64
	var results []*parsed
	for _, rt := range sortedLists(e.lists) {
		if rt.Enabled && rt.parsed != nil {
			ids = append(ids, rt.ID)
			results = append(results, rt.parsed)
		}
	}
	e.mu.Unlock()

	e.busy.Add(1)
	defer e.busy.Add(-1)
	start := time.Now()
	m := buildListMatcher(ids, results)
	took := time.Since(start)

	e.mu.Lock()
	e.compiledGen = gen
	e.compiledAt = e.now()
	e.compileMs = took.Milliseconds()
	e.publishLocked(m, nil)
	e.mu.Unlock()
	e.log.Info("blocklists compiled", slog.Int("lists", len(ids)), slog.Int("entries", m.entries),
		slog.Int("patterns", m.patterns), slog.Int("patternsDropped", m.dropped),
		slog.Duration("took", took.Round(time.Millisecond)))
}

// utcNow returns the current time in UTC with the millisecond precision of
// the database.
func utcNow() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }
