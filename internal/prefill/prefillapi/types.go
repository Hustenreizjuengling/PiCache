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
	Epic     Epic      `json:"epic"`
	// Job is the running job, null when none runs.
	Job     *Job       `json:"job"`
	NextRun *time.Time `json:"nextRun"`
	// Hosts lists every destination the service talks to and why.
	Hosts []Host `json:"hosts"`
	// Exits are the last runs that did not end by themselves, newest
	// first (at most 10).
	Exits []Exit `json:"exits"`
}

// Exit kinds.
const (
	// ExitCrash: the service crashed (Detail is Go's output).
	ExitCrash = "crash"
	// ExitKilled: it was ended from outside without a chance to stop
	// (the kernel at its memory limit, a hard stop, a power cut).
	ExitKilled = "killed"
)

// Exit is a run that did not end by itself.
type Exit struct {
	Kind    string    `json:"kind"`
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	// At is when it ended: the crash, else the last line of its log.
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

// Log is GET /v1/log: the service's last log lines, oldest first.
type Log struct {
	Lines []string `json:"lines"`
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
	Purpose  string     `json:"purpose"`  // steam-directory, steam-cm, battlenet-versions, epic-account, epic-launcher, cache
	LastUsed *time.Time `json:"lastUsed"`
}

// Settings is GET and PUT /v1/settings.
type Settings struct {
	Schedule Schedule `json:"schedule"`
	// MaxMbps limits the download rate of a job (0: no limit).
	MaxMbps int `json:"maxMbps"`
	// Connections is the number of parallel downloads (1–32).
	Connections int               `json:"connections"`
	Steam       SteamSettings     `json:"steam"`
	BattleNet   BattleNetSettings `json:"battleNet"`
	Epic        EpicSettings      `json:"epic"`
}

// BattleNetSettings choose what a Battle.net job downloads (no sign-in:
// Blizzard's content is public).
type BattleNetSettings struct {
	// Region is the patch region whose versions and CDN are used: eu, us,
	// kr or tw.
	Region string `json:"region"`
	// Locale is the language of the installs (enUS, deDE, …).
	Locale string `json:"locale"`
	// Products are the chosen product codes (wow, pro, fenris, …).
	Products []string `json:"products"`
}

// EpicSettings choose what an Epic job downloads.
type EpicSettings struct {
	// Apps are the chosen apps (the launcher's app names).
	Apps []string `json:"apps"`
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

// Epic is the Epic part of the status.
type Epic struct {
	Account *EpicAccount `json:"account"`
	Login   *EpicLogin   `json:"login"`
}

// EpicAccount is the signed-in Epic account. Its tokens never leave the
// service.
type EpicAccount struct {
	Name      string     `json:"name"`
	AccountID string     `json:"accountId"`
	Since     time.Time  `json:"since"`
	Expires   *time.Time `json:"expires"`
	Error     string     `json:"error"`
}

// EpicLogin is a pending device-code sign-in: the user opens
// VerificationURL (shown as a QR code), signs in at Epic and confirms
// UserCode.
type EpicLogin struct {
	// State: waiting, done, failed, expired, cancelled.
	State           string     `json:"state"`
	VerificationURL string     `json:"verificationUrl"`
	UserCode        string     `json:"userCode"`
	Started         time.Time  `json:"started"`
	Expires         *time.Time `json:"expires"`
	Error           string     `json:"error"`
}

// BattleNetProducts is GET /v1/battlenet/products.
type BattleNetProducts struct {
	Products []BattleNetProduct `json:"products"`
	Updated  *time.Time         `json:"updated"`
	Error    string             `json:"error"`
}

// BattleNetProduct is one Battle.net game.
type BattleNetProduct struct {
	Code string `json:"code"`
	Name string `json:"name"`
	// Version is the current version in the chosen region ("" until read).
	Version   string     `json:"version"`
	Selected  bool       `json:"selected"`
	Prefilled *time.Time `json:"prefilled"`
	Current   bool       `json:"current"`
	// Error says why the version could not be read (or the product cannot
	// be prefilled).
	Error string `json:"error,omitempty"`
}

// EpicApps is GET /v1/epic/apps.
type EpicApps struct {
	Apps    []EpicApp  `json:"apps"`
	Updated *time.Time `json:"updated"`
	Error   string     `json:"error"`
}

// EpicApp is one game of the Epic account.
type EpicApp struct {
	// ID is the launcher's app name.
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Version   string     `json:"version"`
	Selected  bool       `json:"selected"`
	Prefilled *time.Time `json:"prefilled"`
	Current   bool       `json:"current"`
}

// BattleNetSelection is PUT /v1/battlenet/selection.
type BattleNetSelection struct {
	Products []string `json:"products"`
}

// EpicSelection is PUT /v1/epic/selection.
type EpicSelection struct {
	Apps []string `json:"apps"`
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
	Service string `json:"service"` // steam, battlenet, epic
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
