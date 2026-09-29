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
	"errors"
	"time"

	"github.com/hustenreizjuengling/picache/internal/version"
)

// Repository is the only source of releases. Nothing a request contains is
// ever used as a URL, path or command.
const Repository = "Hustenreizjuengling/PiCache"

// HelperMarker exists when install.sh installed the root update helper
// (picache-update.path and picache-update.service).
const HelperMarker = "/etc/picache/updater.enabled"

// NightlyMarker exists when install.sh --nightly allowed nightly builds on
// this host: the root helper installs a nightly build only while it is a
// regular file owned by root (not a link), so a compromised service cannot
// move a stable host onto unreviewed builds. `sudo picache update` (the
// admin at the console) needs no marker.
const NightlyMarker = "/etc/picache/nightly.enabled"

// ErrNightlyNotEnabled is the helper's answer to a nightly build without
// the marker.
var ErrNightlyNotEnabled = errors.New("nightly builds are not enabled on this host (install.sh --nightly)")

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

// Update modes reported to the UI (Overview.Mode), in the order they are
// decided (ARCHITECTURE 14.4): docker, package, helper, manual.
const (
	ModeHelper  = "helper"  // the root helper installs updates queued in the UI
	ModeDocker  = "docker"  // pull the new image
	ModeManual  = "manual"  // sudo picache update on the host
	ModePackage = "package" // installed as a Debian package: apt install the next .deb
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
	// Package: the check ran in package mode, so it offered only releases
	// with the .deb of this architecture. A stored result of the other
	// kind is not used after the installation changed between install.sh
	// and the Debian package (v0.16.0).
	Package bool `json:"package,omitempty"`
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
	Channel            string       `json:"channel"`        // stable | beta | nightly
	NightlyAllowed     bool         `json:"nightlyAllowed"` // NightlyMarker exists
	// InstallProxy is the proxy installs use (PICACHE_UPDATE_PROXY,
	// scheme://host:port; absent: none).
	InstallProxy    string    `json:"installProxy,omitempty"`
	Latest          *Release  `json:"latest,omitempty"`
	UpdateAvailable bool      `json:"updateAvailable"`
	CheckedAt       time.Time `json:"checkedAt,omitzero"`
	CheckError      string    `json:"checkError,omitempty"`
	Status          *Status   `json:"status,omitempty"`
	Commands        Commands  `json:"commands"`
	// Package describes the Debian package of this host (mode package
	// only).
	Package *PackageInfo `json:"package,omitempty"`
}

// PackageInfo is Overview.Package: how the Debian package of this host is
// updated by hand.
type PackageInfo struct {
	Format string `json:"format"` // deb
	Arch   string `json:"arch"`   // the Debian architecture (DebianArch)
	// File and URL name the .deb of Latest on its release (only while
	// Latest is set).
	File string `json:"file,omitempty"`
	URL  string `json:"url,omitempty"`
}

// NewOverview combines the running version, the last check and the state
// of the update queue. A release found for another channel is not offered
// once the channel changed.
func NewOverview(current, mode string, checkEnabled bool, channel string, last CheckResult, st *Status) Overview {
	info := version.Get()
	info.Version = current
	run := ParseRunning(current)
	o := Overview{
		Current: info, CurrentIsDevBuild: run.Dev, Mode: mode,
		CheckEnabled: checkEnabled, IncludePrereleases: channel != ChannelStable, Channel: channel,
		CheckedAt: last.CheckedAt, CheckError: last.Error, Status: st,
		Commands: Commands{CLI: CLICommand},
	}
	if l := last.Latest; l != nil {
		if v, err := ParseVersion(l.Version); err == nil && Offered(channel, v) {
			o.Latest = l
			if run.Accepts(v) {
				o.UpdateAvailable = true
				o.Commands.CLI = CLICommand + " --version " + l.Version
			}
		}
	}
	if mode == ModeDocker {
		o.Commands.Docker = DockerCommand
	}
	return o
}
