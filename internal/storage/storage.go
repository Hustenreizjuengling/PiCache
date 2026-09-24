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
package storage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

var errNotImplemented = errors.New("storage: not implemented")

// ErrNotInitialised is wrapped by StoreRoot when the target has no store
// marker yet (activate with initialize=true or adopt=true).
var ErrNotInitialised = &apperr.Error{Kind: apperr.KindConflict, Message: "storage is not initialised as a PiCache cache store yet"}

// LocalTargetID is the built-in target backed by PICACHE_CACHE_DIR. It is
// initialised automatically when its directory is empty.
const LocalTargetID = "local"

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

// Manager owns targets and their health.
type Manager struct {
	db  *db.DB
	box *secrets.Box
	cfg *config.Config
	log *slog.Logger
}

// New creates the manager, ensuring the built-in local target exists (and
// initialising its store marker when the cache dir is empty). sliceSize is
// used for new store markers.
func New(ctx context.Context, d *db.DB, box *secrets.Box, cfg *config.Config, sliceSize func() int64, log *slog.Logger) (*Manager, error) {
	return &Manager{db: d, box: box, cfg: cfg, log: log}, nil
}

// Start runs the 30 s health loop (statfs with timeout, single-flight) until
// ctx ends. Start blocks until ctx is done and its goroutines have exited.
func (m *Manager) Start(ctx context.Context) { <-ctx.Done() }

// Capabilities returns the detected environment capabilities.
func (m *Manager) Capabilities() Capabilities { return Capabilities{} }

// Targets lists targets with status; active marks cache.activeStoreId.
func (m *Manager) Targets(ctx context.Context, activeID string) ([]TargetWithStatus, error) {
	return nil, errNotImplemented
}

// Target returns one target.
func (m *Manager) Target(ctx context.Context, id string) (Target, error) {
	return Target{}, errNotImplemented
}

// Create adds a target (ValidateTarget).
func (m *Manager) Create(ctx context.Context, in TargetInput) (Target, error) {
	return Target{}, errNotImplemented
}

// Update changes a target (ValidateTarget). The built-in local target only
// allows renaming.
func (m *Manager) Update(ctx context.Context, id string, in TargetInput) (Target, error) {
	return Target{}, errNotImplemented
}

// Delete removes a target (not "local", not the active one).
func (m *Manager) Delete(ctx context.Context, id, activeID string) error { return errNotImplemented }

// Test runs a full functional test (mount guard, write/rename/read/delete in
// tmp/, statfs) without changing anything else.
func (m *Manager) Test(ctx context.Context, id string) TestResult {
	return TestResult{Error: errNotImplemented.Error()}
}

// RequestApply queues a host-apply request for the root helper (writes
// <data>/storage-requests/<id>); apperr.Unavailable if the helper is not installed.
func (m *Manager) RequestApply(ctx context.Context, id string) (Status, error) {
	return Status{}, errNotImplemented
}

// InitStore writes the store marker into an empty, guarded root (adopt=false),
// or adopts an existing marker (adopt=true) and records its store id.
func (m *Manager) InitStore(ctx context.Context, id string, adopt bool) (InitResult, error) {
	return InitResult{}, errNotImplemented
}

// Status returns the last known status (in memory).
func (m *Manager) Status(id string) Status { return Status{} }

// StoreRoot returns the validated store root and store id of an online,
// initialised target, or an apperr error (Unavailable with the reason, or
// ErrNotInitialised).
func (m *Manager) StoreRoot(id string) (root, storeID string, err error) {
	return "", "", errNotImplemented
}

// OnStatusChange registers a callback for status transitions (online,
// offline, space).
func (m *Manager) OnStatusChange(fn func(id string, st Status)) {}

// Snippets renders configuration snippets for a target (never the password).
func (m *Manager) Snippets(ctx context.Context, id string) (Snippets, error) {
	return Snippets{}, errNotImplemented
}

// ValidateTarget checks every field of t with strict allowlists (id, IP
// literal, share/export/username/domain charsets, versions, relative subdir,
// path below cfg.MountRoot and not inside cfg.DataDir).
func ValidateTarget(t Target, cfg *config.Config) error { return nil }

// ApplyHost is the root-only `picache storage apply <id>` implementation. It
// opens picache.db read-only (db.OpenReadOnly), re-validates the target,
// loads the master key without generating one (secrets.Load) to decrypt the
// password (or reads it from passwordStdin when non-nil), writes
// /etc/picache/credentials/<id>.cred (0600, O_EXCL|O_NOFOLLOW, root-owned
// 0700 dir) and a systemd .mount unit for MountRoot/<id>, then runs
// `systemctl daemon-reload` and `systemctl enable --now <unit>`.
func ApplyHost(ctx context.Context, cfg *config.Config, id string, passwordStdin []byte, log *slog.Logger) error {
	return errNotImplemented
}

// ApplyPending processes all queued requests (root helper started by the
// picache-storage.path unit). Results are written to
// <data>/storage-requests/<id>.result for the service to display.
func ApplyPending(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	return errNotImplemented
}
