// Package storage manages cache storage targets (local path, SMB, NFS):
// capability detection, the mount guard, store initialisation, host-apply
// (a root helper writes systemd mount units; the service itself never
// mounts and never holds CAP_SYS_ADMIN) and configuration snippets
// (docs/ARCHITECTURE.md 10).
//
// Tables (picache.db, component "storage"): storage_targets
// (… password_sealed TEXT NULL …).
//
// Security rules:
//   - ValidateTarget is applied on Create/Update AND again by the root CLI on
//     every row it reads (the database is writable by the unprivileged service).
//   - Every non-built-in target path lives below cfg.MountRoot (default
//     /srv/picache), the only NAS location writable inside the sandbox. For
//     host-apply the mountpoint is always filepath.Join(MountRoot, id), never
//     taken from the database.
//   - The NAS password is sealed with secrets.Box (AAD
//     "picache/storage/<id>/password"), write-only in the API, never logged,
//     never put into snippets (placeholder "<your NAS password>"); only the
//     root CLI decrypts it to write /etc/picache/credentials/<id>.cred (0600).
//
// The mount guard checks every target every 30 s in single-flight goroutines
// with a timeout, so a hung NAS never blocks a caller: Status and StoreRoot
// only read the last result from memory.
package storage

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	cachestore "github.com/hustenreizjuengling/picache/internal/lancache/store"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

// ErrNotInitialised is wrapped by StoreRoot when the target has no store
// marker yet (activate with initialize=true or adopt=true).
var ErrNotInitialised = &apperr.Error{Kind: apperr.KindConflict, Message: "storage is not initialised as a PiCache cache store yet"}

// LocalTargetID is the built-in target backed by PICACHE_CACHE_DIR. It is
// initialised automatically when its directory is empty.
const LocalTargetID = "local"

// HostApplyFlagFile exists when the root helper is installed (created by
// install.sh together with the picache-storage.path unit).
const HostApplyFlagFile = "/etc/picache/host-apply.enabled"

// CredentialsDir holds the NAS credential files written by the root helper.
const CredentialsDir = "/etc/picache/credentials"

// Kind of storage.
type Kind string

const (
	KindLocal Kind = "local" // a local directory (another disk) below MountRoot, or the built-in target
	KindSMB   Kind = "smb"
	KindNFS   Kind = "nfs"
)

// Mode says who mounts a network target.
type Mode string

const (
	ModeExternal  Mode = "external"   // host fstab / Docker bind / Proxmox mp mounts it at Path
	ModeHostApply Mode = "host-apply" // PiCache's root helper writes a systemd .mount at MountRoot/<id>
)

// Target is a storage target. The password is never returned (HasPassword only).
type Target struct {
	ID                string    `json:"id"` // "local" or 32 hex chars
	Name              string    `json:"name"`
	Kind              Kind      `json:"kind"`
	Mode              Mode      `json:"mode"`
	Path              string    `json:"path"`   // mountpoint / directory as seen by PiCache (computed for host-apply)
	Server            string    `json:"server"` // IP literal (smb/nfs)
	Share             string    `json:"share"`  // smb share name
	Export            string    `json:"export"` // nfs export path
	Subdir            string    `json:"subdir"` // optional relative sub-directory used as store root
	Username          string    `json:"username"`
	Domain            string    `json:"domain"`
	HasPassword       bool      `json:"hasPassword"`
	SMBVersion        string    `json:"smbVersion"` // 3.1.1 (default) | 3.0 | 3
	SMBSeal           bool      `json:"smbSeal"`    // SMB3 encryption
	NFSVersion        string    `json:"nfsVersion"` // 4.2 (default) | 4.1 | 4 | 3
	NFSNConnect       int       `json:"nfsNconnect"`
	RequireMountpoint bool      `json:"requireMountpoint"` // always true for smb/nfs
	StoreID           string    `json:"storeId"`           // id from the store marker ("" until initialised)
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// TargetInput creates or updates a target. Password: nil = keep, "" = clear.
// Fields that do not apply to the kind are ignored.
type TargetInput struct {
	Name              string  `json:"name"`
	Kind              Kind    `json:"kind"`
	Mode              Mode    `json:"mode"`
	Path              string  `json:"path"` // required for external; ignored for host-apply
	Server            string  `json:"server"`
	Share             string  `json:"share"`
	Export            string  `json:"export"`
	Subdir            string  `json:"subdir"`
	Username          string  `json:"username"`
	Domain            string  `json:"domain"`
	Password          *string `json:"password,omitempty"`
	SMBVersion        string  `json:"smbVersion"`
	SMBSeal           bool    `json:"smbSeal"`
	NFSVersion        string  `json:"nfsVersion"`
	NFSNConnect       int     `json:"nfsNconnect"`
	RequireMountpoint bool    `json:"requireMountpoint"`
}

// Status is the live state of a target (mount guard result). Free space does
// not affect Online (low space only triggers eviction).
type Status struct {
	Online       bool      `json:"online"`
	Reason       string    `json:"reason,omitempty"` // why offline, user-facing
	Hint         string    `json:"hint,omitempty"`   // how to fix
	Mounted      bool      `json:"mounted"`
	FSType       string    `json:"fsType,omitempty"` // ext4, xfs, cifs, nfs, zfs, …
	Device       string    `json:"device,omitempty"` // backing device (e.g. mmcblk0p2) if local
	SDCard       bool      `json:"sdCard"`           // store root is on an SD/eMMC device (warn)
	SameFSAsData bool      `json:"sameFsAsData"`     // shares a filesystem with PICACHE_DATA_DIR
	TotalBytes   uint64    `json:"totalBytes"`
	FreeBytes    uint64    `json:"freeBytes"`
	LatencyMs    float64   `json:"latencyMs"` // last health-check write/rename latency
	StoreRoot    string    `json:"storeRoot"`
	StoreID      string    `json:"storeId,omitempty"` // marker found at the root
	CheckedAt    time.Time `json:"checkedAt,omitzero"`
	ApplyState   string    `json:"applyState,omitempty"` // host-apply: queued | applied | failed: <msg>
}

// TargetWithStatus is returned by listings.
type TargetWithStatus struct {
	Target
	Status Status `json:"status"`
	Active bool   `json:"active"`
}

// Capabilities describe what this environment allows.
type Capabilities struct {
	OS           string          `json:"os"`
	Container    string          `json:"container"` // docker | podman | lxc | "" (bare metal / VM)
	InitUserNS   bool            `json:"initUserNs"`
	UIDMapOffset int64           `json:"uidMapOffset"` // host uid = offset + container uid (LXC)
	GIDMapOffset int64           `json:"gidMapOffset"` // host gid = offset + container gid (LXC)
	UID          int             `json:"uid"`
	GID          int             `json:"gid"`
	Systemd      bool            `json:"systemd"`
	Filesystems  map[string]bool `json:"filesystems"` // cifs, smb3, nfs, nfs4 known to the kernel
	MountHelpers map[string]bool `json:"mountHelpers"`
	// HostApply: the root helper is installed (install.sh creates
	// /etc/picache/host-apply.enabled and the systemd path unit), so the UI
	// can mount targets without a shell.
	HostApply  bool   `json:"hostApply"`
	MountRoot  string `json:"mountRoot"`
	DockerMode string `json:"dockerMode,omitempty"` // host | bridge (best effort)
}

// TestResult is the outcome of a full functional test.
type TestResult struct {
	OK     bool     `json:"ok"`
	Steps  []string `json:"steps"` // human-readable step log
	Error  string   `json:"error,omitempty"`
	Hint   string   `json:"hint,omitempty"`
	Status Status   `json:"status"`
}

// Snippets are ready-to-paste configuration fragments. They never contain
// the NAS password (placeholder "<your NAS password>").
type Snippets struct {
	CredentialsFile string   `json:"credentialsFile,omitempty"` // path + content template (smb)
	Fstab           string   `json:"fstab,omitempty"`
	SystemdMount    string   `json:"systemdMount,omitempty"`
	DockerCompose   string   `json:"dockerCompose,omitempty"`
	Proxmox         string   `json:"proxmox,omitempty"`
	HostApply       string   `json:"hostApply,omitempty"` // the sudo command
	Notes           []string `json:"notes,omitempty"`
}

// InitResult is returned by InitStore.
type InitResult struct {
	StoreID string `json:"storeId"`
	Adopted bool   `json:"adopted"`
}

// Bounds and timings.
const (
	maxTargets    = 32               // storage targets per installation
	maxListeners  = 16               // OnStatusChange callbacks
	guardInterval = 30 * time.Second // mount guard period
	checkTimeout  = 15 * time.Second // one guard check (statfs, marker, write test)
	testTimeout   = 90 * time.Second // Test waits this long for a fresh check
	initTimeout   = 30 * time.Second // InitStore file system work
	shutdownGrace = 10 * time.Second // Start waits this long for checks stuck in the kernel
)

// Manager owns targets and their health. It is safe for concurrent use.
type Manager struct {
	db            *db.DB
	box           *secrets.Box
	cfg           *config.Config
	log           *slog.Logger
	sliceSize     func() int64
	hostApplyFlag string
	caps          Capabilities // static part, detected once in New
	probeFn       func(Target) checkResult

	opMu sync.Mutex // serialises Create, Update, Delete and InitStore

	mu        sync.Mutex // guards the fields below
	targets   map[string]*entry
	listeners []func(id string, st Status)
	closed    bool

	kick chan struct{}  // wakes the guard loop after changes
	wg   sync.WaitGroup // check and init goroutines
}

// entry is the in-memory state of one target.
type entry struct {
	t        Target
	gen      uint64 // bumped when the location changes; older check results are discarded
	st       Status
	uninit   bool      // offline only because no store has been initialised or adopted
	notified Status    // last status passed to listeners
	run      *checkRun // in-flight check (single flight)
	busy     bool      // InitStore is writing; the guard leaves the target alone
}

// New creates the manager, ensuring the built-in local target exists (and
// initialising its store marker when the cache dir is empty). sliceSize is
// used for new store markers.
func New(ctx context.Context, d *db.DB, box *secrets.Box, cfg *config.Config, sliceSize func() int64, log *slog.Logger) (*Manager, error) {
	if d == nil || cfg == nil || sliceSize == nil {
		return nil, errors.New("storage: missing dependency")
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		db:            d,
		box:           box,
		cfg:           cfg,
		log:           log.With(slog.String("component", "storage")),
		sliceSize:     sliceSize,
		hostApplyFlag: HostApplyFlagFile,
		targets:       make(map[string]*entry),
		kick:          make(chan struct{}, 1),
	}
	m.probeFn = m.probe
	if err := d.Migrate(ctx, "storage", migrations); err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	m.caps = detectStaticCapabilities(cfg)
	if err := m.load(ctx); err != nil {
		return nil, err
	}
	m.autoInitLocal(ctx)
	return m, nil
}

// load makes sure the built-in target exists and reads all targets.
func (m *Manager) load(ctx context.Context) error {
	now := db.NowMs()
	if _, err := m.db.W.ExecContext(ctx, `INSERT INTO storage_targets
		(id, name, kind, mode, path, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		LocalTargetID, "Local disk", string(KindLocal), string(ModeExternal), localPath(m.cfg), now, now); err != nil {
		return fmt.Errorf("storage: create built-in target: %w", err)
	}
	ts, err := loadTargets(ctx, m.db.R)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, t := range ts {
		t = resolveTarget(t, m.cfg)
		m.targets[t.ID] = &entry{t: t, st: pendingStatus(t, "not checked yet")}
	}
	return nil
}

// autoInitLocal initialises the built-in store when the cache directory is
// empty, or adopts the marker already there when the database has no store
// id for it (for example after the configuration database was recreated).
func (m *Manager) autoInitLocal(ctx context.Context) {
	t, ok := m.get(LocalTargetID)
	if !ok || t.StoreID != "" {
		return
	}
	dir := t.Path
	mk, err := cachestore.ReadMarker(dir)
	switch {
	case err == nil:
		m.log.Info("adopted the existing cache store in the cache directory", slog.String("dir", dir), slog.String("store", mk.StoreID))
	case errors.Is(err, cachestore.ErrNoMarker):
		if mk, err = cachestore.InitRoot(dir, newID(), m.sliceSize()); err != nil {
			m.log.Warn("cannot initialise the built-in cache store; initialise it under Cache → Storage",
				slog.String("dir", dir), slog.Any("err", err))
			return
		}
		m.log.Info("initialised the built-in cache store", slog.String("dir", dir), slog.String("store", mk.StoreID))
	default:
		m.log.Warn("cannot read the built-in cache store", slog.String("dir", dir), slog.Any("err", err))
		return
	}
	if err := m.setStoreID(ctx, LocalTargetID, mk.StoreID); err != nil {
		m.log.Warn("cannot record the built-in store id", slog.Any("err", err))
	}
}

// Capabilities returns the detected environment capabilities.
func (m *Manager) Capabilities() Capabilities {
	c := m.caps
	c.Filesystems = kernelFilesystems()
	c.MountHelpers = mountHelpers()
	c.HostApply = fileExists(m.hostApplyFlag)
	return c
}

// Targets lists targets with status; active marks cache.activeStoreId.
func (m *Manager) Targets(ctx context.Context, activeID string) ([]TargetWithStatus, error) {
	m.mu.Lock()
	out := make([]TargetWithStatus, 0, len(m.targets))
	for id, e := range m.targets {
		out = append(out, TargetWithStatus{Target: e.t, Status: e.st, Active: id == activeID})
	}
	m.mu.Unlock()
	slices.SortFunc(out, func(a, b TargetWithStatus) int {
		if (a.ID == LocalTargetID) != (b.ID == LocalTargetID) {
			if a.ID == LocalTargetID {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Target returns one target.
func (m *Manager) Target(ctx context.Context, id string) (Target, error) {
	t, ok := m.get(id)
	if !ok {
		return Target{}, apperr.NotFound("storage target", id)
	}
	return t, nil
}

// Status returns the last known status (in memory).
func (m *Manager) Status(id string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.targets[id]; ok {
		return e.st
	}
	return Status{Reason: "unknown storage target"}
}

// StoreRoot returns the validated store root and store id of an online,
// initialised target, or an apperr error (Unavailable with the reason, or
// ErrNotInitialised).
func (m *Manager) StoreRoot(id string) (root, storeID string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.targets[id]
	switch {
	case !ok:
		return "", "", apperr.NotFound("storage target", id)
	case e.st.Online && e.t.StoreID != "" && e.st.StoreID == e.t.StoreID:
		return e.st.StoreRoot, e.t.StoreID, nil
	case e.uninit || (e.t.StoreID == "" && e.st.CheckedAt.IsZero()):
		return "", "", ErrNotInitialised
	}
	reason := e.st.Reason
	if reason == "" {
		reason = "storage is offline"
	}
	return "", "", &apperr.Error{Kind: apperr.KindUnavailable, Message: reason}
}

// OnStatusChange registers a callback for status transitions (online,
// offline, space). Callbacks run on the guard goroutine and must not block.
func (m *Manager) OnStatusChange(fn func(id string, st Status)) {
	if fn == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.listeners) >= maxListeners {
		m.log.Error("too many storage status listeners; ignoring one")
		return
	}
	m.listeners = append(m.listeners, fn)
}

// Snippets renders configuration snippets for a target (never the password).
func (m *Manager) Snippets(ctx context.Context, id string) (Snippets, error) {
	t, err := m.Target(ctx, id)
	if err != nil {
		return Snippets{}, err
	}
	return renderSnippets(t, m.Capabilities(), m.cfg), nil
}

// get returns a copy of a target.
func (m *Manager) get(id string) (Target, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.targets[id]
	if !ok {
		return Target{}, false
	}
	return e.t, true
}

// notify calls the status listeners (never with m.mu held).
func (m *Manager) notify(id string, st Status) {
	m.mu.Lock()
	fns := slices.Clone(m.listeners)
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}
	for _, fn := range fns {
		fn(id, st)
	}
}

// localPath is the absolute path of the built-in store (PICACHE_CACHE_DIR).
func localPath(cfg *config.Config) string {
	p := filepath.Clean(cfg.CacheDir)
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	return p
}

// hostApplyPath is the only mountpoint the root helper ever uses for id.
func hostApplyPath(cfg *config.Config, id string) string {
	return filepath.Join(cfg.MountRoot, id)
}

// resolveTarget fills in paths that are computed, never stored: the
// built-in target uses PICACHE_CACHE_DIR, host-apply targets MountRoot/<id>.
func resolveTarget(t Target, cfg *config.Config) Target {
	switch {
	case t.ID == LocalTargetID:
		t.Path = localPath(cfg)
	case t.Mode == ModeHostApply:
		t.Path = hostApplyPath(cfg, t.ID)
	}
	return t
}

// storeRootPath is the directory holding the store marker.
func storeRootPath(t Target) string {
	if t.Subdir == "" {
		return t.Path
	}
	return filepath.Join(t.Path, filepath.FromSlash(t.Subdir))
}

// pendingStatus is the status of a target that has not been checked (again) yet.
func pendingStatus(t Target, reason string) Status {
	return Status{Reason: reason, StoreRoot: storeRootPath(t)}
}

// newID returns 32 random lower-case hex characters.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// passwordAAD binds a sealed password to its target.
func passwordAAD(id string) string { return "picache/storage/" + id + "/password" }
