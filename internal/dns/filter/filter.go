// Package filter implements blocklists and custom rules: list subscriptions
// (fetch, parse, compile), user allow/deny rules, group scoping and the
// hot-path matcher (docs/ARCHITECTURE.md 7.2).
package filter

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/settings"
)

var errNotImplemented = errors.New("filter: not implemented")

// Action is a filtering verdict.
type Action uint8

const (
	ActionNone  Action = iota // no rule matched
	ActionAllow               // explicitly allowed
	ActionBlock               // blocked
)

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
	Important bool
}

// Blocked reports whether the decision blocks.
func (d Decision) Blocked() bool { return d.Action == ActionBlock }

// List is a subscribed block or allow list.
type List struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	URL          string    `json:"url"`
	Kind         string    `json:"kind"`         // block | allow
	PlainDomains string    `json:"plainDomains"` // exact | subtree
	Enabled      bool      `json:"enabled"`
	GroupIDs     []int64   `json:"groupIds"`
	Comment      string    `json:"comment"`
	Status       string    `json:"status"` // pending | ok | unchanged | failed-cached | failed-empty
	LastError    string    `json:"lastError,omitempty"`
	LastUpdated  time.Time `json:"lastUpdated,omitzero"` // content last changed
	LastChecked  time.Time `json:"lastChecked,omitzero"`
	Entries      int       `json:"entries"`
	Invalid      int       `json:"invalid"`
	Unsupported  int       `json:"unsupported"`
	SizeBytes    int64     `json:"sizeBytes"`
	CreatedAt    time.Time `json:"createdAt"`
}

// ListInput creates or updates a list.
type ListInput struct {
	Name         string  `json:"name"`
	URL          string  `json:"url"`
	Kind         string  `json:"kind"`
	PlainDomains string  `json:"plainDomains"`
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
// "example.com".
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
}

// Stats describes the compiled matcher.
type Stats struct {
	Lists        int       `json:"lists"`
	Entries      int       `json:"entries"`
	Patterns     int       `json:"patterns"` // regex + wildcard patterns
	Rules        int       `json:"rules"`
	CompiledAt   time.Time `json:"compiledAt,omitzero"`
	CompileMs    int64     `json:"compileMs"`
	MemoryBytes  int64     `json:"memoryBytes"`
	Updating     bool      `json:"updating"`
}

// CatalogEntry is a curated list suggestion (embedded, no network needed).
type CatalogEntry struct {
	Key          string `json:"key"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	URL          string `json:"url"`
	Category     string `json:"category"` // general | security | privacy | other
	PlainDomains string `json:"plainDomains"`
	Recommended  bool   `json:"recommended"`
}

// Engine owns lists, rules and the compiled matcher.
type Engine struct {
	db  *db.DB
	set *settings.Store
	log *slog.Logger
}

// New creates the engine. fetch is the HTTP client for list downloads (uses
// the bypass resolver); listsDir stores downloaded copies. The clients
// package must be migrated first (list/rule group links reference groups).
func New(ctx context.Context, d *db.DB, set *settings.Store, fetch *http.Client, listsDir string, log *slog.Logger) (*Engine, error) {
	return &Engine{db: d, set: set, log: log}, nil
}

// Start loads cached list files, compiles and schedules updates until ctx ends.
func (e *Engine) Start(ctx context.Context) {}

// Check evaluates qname (lower-case, no trailing dot) for a client with the
// given enabled group IDs. Hot path: lock-free, allocation-free in the common case.
func (e *Engine) Check(qname string, groups []int64) Decision { return Decision{} }

// Explain lists every source matching qname and marks which applies to groups
// and which is decisive ("why is this blocked?").
func (e *Engine) Explain(ctx context.Context, qname string, groups []int64) ([]Match, error) {
	return nil, errNotImplemented
}

// Stats returns matcher statistics.
func (e *Engine) Stats() Stats { return Stats{} }

// Recompile rebuilds the matcher (e.g. after group changes). Asynchronous.
func (e *Engine) Recompile() {}

// Lists returns all lists.
func (e *Engine) Lists(ctx context.Context) ([]List, error) { return nil, errNotImplemented }

// CreateList adds a list and triggers its first download.
func (e *Engine) CreateList(ctx context.Context, in ListInput) (List, error) {
	return List{}, errNotImplemented
}

// UpdateList updates a list.
func (e *Engine) UpdateList(ctx context.Context, id int64, in ListInput) (List, error) {
	return List{}, errNotImplemented
}

// DeleteList removes a list and its cached copy.
func (e *Engine) DeleteList(ctx context.Context, id int64) error { return errNotImplemented }

// RefreshList re-downloads one list now and recompiles (waits for completion).
func (e *Engine) RefreshList(ctx context.Context, id int64) (List, error) {
	return List{}, errNotImplemented
}

// RefreshAll re-downloads all enabled lists and recompiles (waits).
func (e *Engine) RefreshAll(ctx context.Context) error { return errNotImplemented }

// Catalog returns the embedded list catalogue.
func (e *Engine) Catalog() []CatalogEntry { return nil }

// Rules returns user rules.
func (e *Engine) Rules(ctx context.Context, q RuleQuery) ([]Rule, error) { return nil, errNotImplemented }

// CreateRule adds a rule.
func (e *Engine) CreateRule(ctx context.Context, in RuleInput) (Rule, error) {
	return Rule{}, errNotImplemented
}

// UpdateRule updates a rule.
func (e *Engine) UpdateRule(ctx context.Context, id int64, in RuleInput) (Rule, error) {
	return Rule{}, errNotImplemented
}

// DeleteRule deletes a rule.
func (e *Engine) DeleteRule(ctx context.Context, id int64) error { return errNotImplemented }
