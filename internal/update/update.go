// Package update finds, verifies and installs PiCache releases
// (docs/ARCHITECTURE.md 14).
//
// The service only checks for updates (Client.Latest) and queues a request
// for the root helper (QueueRequest). Everything that downloads or replaces
// a binary (Apply, ApplyPending) runs as root in the CLI or in the helper,
// never in the service. Release files are trusted only after SHA256SUMS was
// verified against the compiled-in keys (keys.go) and the binary against
// SHA256SUMS.
package update

import (
	"time"

	"github.com/hustenreizjuengling/picache/internal/version"
)

// Repository is the only source of releases. Nothing a request contains is
// ever used as a URL, path or command.
const Repository = "Hustenreizjuengling/PiCache"

// HelperMarker exists when install.sh installed the root update helper
// (picache-update.path and picache-update.service).
const HelperMarker = "/etc/picache/updater.enabled"

// Service is the systemd unit that runs PiCache.
const Service = "picache.service"

// Release file names (docs/ARCHITECTURE.md 14.1).
const (
	SumsFile = "SHA256SUMS"
	SigFile  = "SHA256SUMS.sig"
)

// States of an update run (Status.State).
const (
	StateRunning    = "running"
	StateSucceeded  = "succeeded"
	StateFailed     = "failed"
	StateRolledBack = "rolled-back"
)

// Steps of an update run (Status.Step), in this order.
const (
	StepDownload = "download"
	StepVerify   = "verify"
	StepInstall  = "install"
	StepRestart  = "restart"
	StepHealth   = "health"
	StepRollback = "rollback"
	StepDone     = "done"
)

// Update modes reported to the UI (Overview.Mode).
const (
	ModeHelper = "helper" // the root helper installs updates queued in the UI
	ModeDocker = "docker" // pull the new image
	ModeManual = "manual" // sudo picache update on the host
)

// Commands shown in the UI for the modes without the helper.
const (
	CLICommand    = "sudo picache update"
	DockerCommand = "docker compose pull && docker compose up -d"
)

// Release is the newest eligible release found by a check.
type Release struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"publishedAt"`
	URL         string    `json:"url"`   // release page
	Notes       string    `json:"notes"` // Markdown, ≤ 64 KiB, untrusted: shown as text only
	Prerelease  bool      `json:"prerelease"`
}

// CheckResult is the outcome of the last check. A failed check keeps the
// release found before.
type CheckResult struct {
	Latest    *Release  `json:"latest,omitempty"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// Status is the progress or result of an update run
// (<data>/update-requests/status.json).
type Status struct {
	State      string    `json:"state"`
	Step       string    `json:"step"`
	Version    string    `json:"version"` // target version
	From       string    `json:"from"`    // version before the update
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
	Message    string    `json:"message,omitempty"`
}

// Busy reports whether an update is queued or running.
func (s *Status) Busy() bool { return s != nil && s.State == StateRunning }

// Commands are the update commands the UI shows.
type Commands struct {
	CLI    string `json:"cli"`
	Docker string `json:"docker,omitempty"`
}

// Overview is GET /system/update (docs/API.md).
type Overview struct {
	Current            version.Info `json:"current"`
	CurrentIsDevBuild  bool         `json:"currentIsDevBuild"`
	Mode               string       `json:"mode"`
	CheckEnabled       bool         `json:"checkEnabled"`
	IncludePrereleases bool         `json:"includePrereleases"`
	Latest             *Release     `json:"latest,omitempty"`
	UpdateAvailable    bool         `json:"updateAvailable"`
	CheckedAt          time.Time    `json:"checkedAt,omitzero"`
	CheckError         string       `json:"checkError,omitempty"`
	Status             *Status      `json:"status,omitempty"`
	Commands           Commands     `json:"commands"`
}

// NewOverview combines the running version, the last check and the state
// of the update queue. A pre-release found while pre-releases were allowed
// is not offered once they are no longer.
func NewOverview(current, mode string, checkEnabled, includePre bool, last CheckResult, st *Status) Overview {
	info := version.Get()
	info.Version = current
	run := ParseRunning(current)
	o := Overview{
		Current: info, CurrentIsDevBuild: run.Dev, Mode: mode,
		CheckEnabled: checkEnabled, IncludePrereleases: includePre,
		CheckedAt: last.CheckedAt, CheckError: last.Error, Status: st,
		Commands: Commands{CLI: CLICommand},
	}
	if l := last.Latest; l != nil && (includePre || !l.Prerelease) {
		o.Latest = l
		if v, err := ParseVersion(l.Version); err == nil && run.Accepts(v) {
			o.UpdateAvailable = true
			o.Commands.CLI = CLICommand + " --version " + l.Version
		}
	}
	if mode == ModeDocker {
		o.Commands.Docker = DockerCommand
	}
	return o
}
