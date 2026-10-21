// Package prefillapi is the interface between PiCache and the prefill
// service (picache prefill, docs/ARCHITECTURE.md 20): the JSON types of its
// local socket API and the client PiCache uses to reach it. The prefill
// service owns the account tokens; nothing here ever carries one.
package prefillapi

import "time"

// DefaultSocket is where the prefill service listens (its systemd unit's
// RuntimeDirectory).
const DefaultSocket = "/run/picache-prefill/prefill.sock"

// Status is GET /v1/status.
type Status struct {
	Version  string    `json:"version"`
	Started  time.Time `json:"started"`
	Cache    Cache     `json:"cache"`
	Settings Settings  `json:"settings"`
	Steam    Steam     `json:"steam"`
	// Job is the running job, null when none runs.
	Job     *Job       `json:"job"`
	NextRun *time.Time `json:"nextRun"`
	// Hosts lists every destination the service talks to and why.
	Hosts []Host `json:"hosts"`
}

// Cache is the download cache the prefill fills, checked with the
// heartbeat path before every job and every minute while the page is open.
type Cache struct {
	URL        string     `json:"url"`
	OK         bool       `json:"ok"`
	InstanceID string     `json:"instanceId"`
	Error      string     `json:"error"`
	CheckedAt  *time.Time `json:"checkedAt"`
}

// Host is one destination of the service.
type Host struct {
	Host     string     `json:"host"`
	Protocol string     `json:"protocol"` // https, wss, http
	Purpose  string     `json:"purpose"`  // steam-directory, steam-cm, cache
	LastUsed *time.Time `json:"lastUsed"`
}

// Settings is GET and PUT /v1/settings.
type Settings struct {
	Schedule Schedule `json:"schedule"`
	// MaxMbps limits the download rate of a job (0: no limit).
	MaxMbps int `json:"maxMbps"`
	// Connections is the number of parallel downloads (1–32).
	Connections int           `json:"connections"`
	Steam       SteamSettings `json:"steam"`
}

// Schedule is the time window of scheduled jobs (the host's time zone).
type Schedule struct {
	Enabled bool `json:"enabled"`
	// Days are the weekdays the window starts on, 0 = Sunday.
	Days []int `json:"days"`
	// Start and End are "HH:MM"; an End at or before Start ends the next day.
	Start string `json:"start"`
	End   string `json:"end"`
}

// SteamSettings choose what a Steam job downloads.
type SteamSettings struct {
	// OS are the platforms whose depots are downloaded: windows, macos,
	// linux.
	OS []string `json:"os"`
	// Languages are Steam language names (english, german, …): depots of
	// these languages and those without one are downloaded.
	Languages []string `json:"languages"`
	// Apps are the selected app IDs.
	Apps []uint32 `json:"apps"`
}

// Steam is the Steam part of the status.
type Steam struct {
	// DeviceName is how a sign-in appears in the account's Steam Guard
	// device list.
	DeviceName string        `json:"deviceName"`
	Account    *SteamAccount `json:"account"`
	Login      *SteamLogin   `json:"login"`
}

// SteamAccount is the signed-in account. The token itself never leaves the
// service.
type SteamAccount struct {
	Name    string `json:"name"`
	SteamID string `json:"steamId"`
	// DeviceName is how the sign-in appears in the account's Steam Guard
	// device list.
	DeviceName string     `json:"deviceName"`
	Since      time.Time  `json:"since"`
	Expires    *time.Time `json:"expires"`
	// Error is the last failure to use the sign-in (for example a token
	// revoked in Steam).
	Error string `json:"error"`
}

// SteamLogin is a pending QR sign-in.
type SteamLogin struct {
	// State: waiting (show the code), done, failed, expired, cancelled.
	State string `json:"state"`
	// ChallengeURL is what the QR code encodes (https://s.team/q/…). It
	// changes while the sign-in waits.
	ChallengeURL string `json:"challengeUrl"`
	// Scanned: the code was scanned, the app asks for the confirmation.
	Scanned bool      `json:"scanned"`
	Started time.Time `json:"started"`
	Error   string    `json:"error"`
}

// SteamApps is GET /v1/steam/apps.
type SteamApps struct {
	Apps    []SteamApp `json:"apps"`
	Updated *time.Time `json:"updated"`
	Error   string     `json:"error"`
}

// SteamApp is one game of the account.
type SteamApp struct {
	ID   uint32 `json:"id"`
	Name string `json:"name"`
	// Bytes is the download size of the depots a job would fetch (0 when
	// Steam does not say).
	Bytes    int64 `json:"bytes"`
	Selected bool  `json:"selected"`
	// Prefilled is when the app was last downloaded completely; Current
	// says whether that is still its current version.
	Prefilled *time.Time `json:"prefilled"`
	Current   bool       `json:"current"`
}

// Job states.
const (
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
	// JobStopped: the schedule's window ended; the next window continues.
	JobStopped = "stopped"
)

// Job is one run.
type Job struct {
	ID       int64      `json:"id"`
	Trigger  string     `json:"trigger"` // manual, schedule
	State    string     `json:"state"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished"`
	Error    string     `json:"error"`
	// Current says what is being done right now.
	Current string   `json:"current"`
	Apps    []JobApp `json:"apps"`
	// Bytes counts what was read through the cache, FromCache the part the
	// cache already had (HIT), Total what the job plans to read.
	Bytes     int64 `json:"bytes"`
	FromCache int64 `json:"fromCache"`
	Total     int64 `json:"total"`
	// Hosts are the destinations this job talked to.
	Hosts []string `json:"hosts"`
}

// JobApp states.
const (
	AppPending = "pending"
	AppRunning = "running"
	AppDone    = "done"
	AppCurrent = "current" // already prefilled in this version: skipped
	AppFailed  = "failed"
)

// JobApp is one app of a job.
type JobApp struct {
	Service string `json:"service"` // steam
	ID      string `json:"id"`
	Name    string `json:"name"`
	State   string `json:"state"`
	Bytes   int64  `json:"bytes"`
	Total   int64  `json:"total"`
	Error   string `json:"error"`
}

// History is GET /v1/history: the last jobs, newest first.
type History struct {
	Jobs []Job `json:"jobs"`
}

// StartJob is POST /v1/jobs.
type StartJob struct {
	// Force downloads apps that are already current too.
	Force bool `json:"force"`
}

// Selection is PUT /v1/steam/selection.
type Selection struct {
	Apps []uint32 `json:"apps"`
}

// Error codes of an error answer ({"error": {"code", "message"}}), the
// same as PiCache's API.
const (
	CodeInvalid     = "invalid"
	CodeNotFound    = "not_found"
	CodeConflict    = "conflict"
	CodeForbidden   = "forbidden"
	CodeUnavailable = "unavailable"
	CodeInternal    = "internal"
)

// ErrorBody is the body of every answer with a status of 400 or more.
type ErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
