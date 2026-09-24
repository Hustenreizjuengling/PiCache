// Package storage manages cache storage targets (local path, SMB, NFS):
// capability detection, the mount guard, optional in-process mounts,
// host-apply via the root CLI and configuration snippets
// (docs/ARCHITECTURE.md 10).
package storage

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/hustenreizjuengling/picache/internal/config"
	"github.com/hustenreizjuengling/picache/internal/db"
	"github.com/hustenreizjuengling/picache/internal/netutil"
	"github.com/hustenreizjuengling/picache/internal/secrets"
)

var errNotImplemented = errors.New("storage: not implemented")

// LocalTargetID is the built-in target backed by PICACHE_CACHE_DIR.
const LocalTargetID = "local"

// Kind of storage.
type Kind string

const (
	KindLocal Kind = "local"
	KindSMB   Kind = "smb"
	KindNFS   Kind = "nfs"
)

// Mode says who mounts a network target.
type Mode string

const (
	ModeExternal  Mode = "external"   // host/Docker/Proxmox mounts it; PiCache uses Path
	ModeHostApply Mode = "host-apply" // `sudo picache storage apply <id>` writes a systemd mount
	ModeInProcess Mode = "in-process" // PiCache mounts it itself (CAP_SYS_ADMIN, opt-in)
)

// Target is a storage target. Password is never returned (HasPassword only).
type Target struct {
	ID                string    `json:"id"`
	Name              string    `json:"name"`
	Kind              Kind      `json:"kind"`
	Mode              Mode      `json:"mode"`
	Path              string    `json:"path"`   // path as seen by PiCache (store root = Path/Subdir for external)
	Server            string    `json:"server"` // IP literal (smb/nfs)
	Share             string    `json:"share"`  // smb share name
	Export            string    `json:"export"` // nfs export path
	Subdir            string    `json:"subdir"` // optional sub-directory inside the share
	Username          string    `json:"username"`
	Domain            string    `json:"domain"`
	HasPassword       bool      `json:"hasPassword"`
	SMBVersion        string    `json:"smbVersion"` // 3.1.1 (default) | 3.0 | 3
	SMBSeal           bool      `json:"smbSeal"`    // SMB3 encryption
	NFSVersion        string    `json:"nfsVersion"` // 4.2 (default) | 4.1 | 4 | 3
	NFSNConnect       int       `json:"nfsNconnect"`
	RequireMountpoint bool      `json:"requireMountpoint"`
	StoreID           string    `json:"storeId"` // id written to .picache-store
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

// TargetInput creates or updates a target. Password: nil = keep, "" = clear.
type TargetInput struct {
	Name              string  `json:"name"`
	Kind              Kind    `json:"kind"`
	Mode              Mode    `json:"mode"`
	Path              string  `json:"path"`
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

// Status is the live state of a target (mount guard result).
type Status struct {
	Online      bool      `json:"online"`
	Reason      string    `json:"reason,omitempty"` // why offline, user-facing
	Hint        string    `json:"hint,omitempty"`   // how to fix
	Mounted     bool      `json:"mounted"`
	FSType      string    `json:"fsType,omitempty"` // ext4, xfs, cifs, nfs, zfs, …
	TotalBytes  uint64    `json:"totalBytes"`
	FreeBytes   uint64    `json:"freeBytes"`
	LatencyMs   float64   `json:"latencyMs"` // last health-check write/rename latency
	StoreRoot   string    `json:"storeRoot"`
	CheckedAt   time.Time `json:"checkedAt,omitzero"`
	MountLog    []string  `json:"mountLog,omitempty"` // fs_context messages of the last mount attempt
}

// TargetWithStatus is returned by listings.
type TargetWithStatus struct {
	Target
	Status Status `json:"status"`
	Active bool   `json:"active"`
}

// Capabilities describe what this environment allows.
type Capabilities struct {
	OS              string          `json:"os"`
	Container       string          `json:"container"` // docker | podman | lxc | "" (bare metal / VM)
	InitUserNS      bool            `json:"initUserNs"`
	UIDMapOffset    int64           `json:"uidMapOffset"` // host uid = offset + container uid (LXC)
	UID             int             `json:"uid"`
	GID             int             `json:"gid"`
	CapSysAdmin     bool            `json:"capSysAdmin"`
	CapNetBind      bool            `json:"capNetBind"`
	NoNewPrivs      bool            `json:"noNewPrivs"`
	AppArmor        string          `json:"apparmor,omitempty"`
	Systemd         bool            `json:"systemd"`
	Filesystems     map[string]bool `json:"filesystems"` // cifs, smb3, nfs, nfs4 available in kernel
	MountHelpers    map[string]bool `json:"mountHelpers"`
	MountsEnabled   bool            `json:"mountsEnabled"` // PICACHE_ENABLE_MOUNTS
	InProcess       bool            `json:"inProcess"`     // in-process mounts possible
	InProcessReason string          `json:"inProcessReason,omitempty"`
	HostApply       bool            `json:"hostApply"`
}

// TestResult is the outcome of a full functional test.
type TestResult struct {
	OK     bool     `json:"ok"`
	Steps  []string `json:"steps"` // human-readable step log
	Error  string   `json:"error,omitempty"`
	Hint   string   `json:"hint,omitempty"`
	Status Status   `json:"status"`
}

// Snippets are ready-to-paste configuration fragments for external/host modes.
type Snippets struct {
	CredentialsFile string `json:"credentialsFile,omitempty"` // path + content (smb)
	Fstab           string `json:"fstab,omitempty"`
	SystemdMount    string `json:"systemdMount,omitempty"`
	DockerCompose   string `json:"dockerCompose,omitempty"`
	Proxmox         string `json:"proxmox,omitempty"`
	HostApply       string `json:"hostApply,omitempty"` // the sudo command
	Notes           []string `json:"notes,omitempty"`
}

// Manager owns targets and their health.
type Manager struct {
	db  *db.DB
	box *secrets.Box
	cfg *config.Config
	log *slog.Logger
}

// New creates the manager, ensuring the built-in local target exists.
// lookup resolves nothing by default (targets use IP literals); it is kept
// for future hostname support.
func New(ctx context.Context, d *db.DB, box *secrets.Box, cfg *config.Config, lookup netutil.Resolver, log *slog.Logger) (*Manager, error) {
	return &Manager{db: d, box: box, cfg: cfg, log: log}, nil
}

// Start mounts in-process targets and runs the 30 s health loop until ctx ends.
func (m *Manager) Start(ctx context.Context) {}

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

// Create adds a target.
func (m *Manager) Create(ctx context.Context, in TargetInput) (Target, error) {
	return Target{}, errNotImplemented
}

// Update changes a target.
func (m *Manager) Update(ctx context.Context, id string, in TargetInput) (Target, error) {
	return Target{}, errNotImplemented
}

// Delete removes a target (not "local", not the active one).
func (m *Manager) Delete(ctx context.Context, id, activeID string) error { return errNotImplemented }

// Test runs a full functional test (mount if in-process, write/rename/read/delete, statfs).
func (m *Manager) Test(ctx context.Context, id string) TestResult {
	return TestResult{Error: errNotImplemented.Error()}
}

// Mount mounts an in-process target now.
func (m *Manager) Mount(ctx context.Context, id string) error { return errNotImplemented }

// Unmount unmounts an in-process target.
func (m *Manager) Unmount(ctx context.Context, id string) error { return errNotImplemented }

// Status returns the last known status (in memory).
func (m *Manager) Status(id string) Status { return Status{} }

// StoreRoot returns the validated store root of an online target, or an
// apperr.Unavailable error explaining why it cannot be used.
func (m *Manager) StoreRoot(id string) (root, storeID string, err error) {
	return "", "", errNotImplemented
}

// OnStatusChange registers a callback for online/offline transitions.
func (m *Manager) OnStatusChange(fn func(id string, st Status)) {}

// Snippets renders configuration snippets for a target.
func (m *Manager) Snippets(ctx context.Context, id string) (Snippets, error) {
	return Snippets{}, errNotImplemented
}

// ApplyHost is the root-only `picache storage apply <id>` implementation:
// writes /etc/picache/credentials/<id>.cred and a systemd .mount unit, then
// enables it. It opens the config database read-only.
func ApplyHost(ctx context.Context, cfg *config.Config, id string, log *slog.Logger) error {
	return errNotImplemented
}
