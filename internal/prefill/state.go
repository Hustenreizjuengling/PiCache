package prefill

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// The service keeps its state in small JSON files of its state directory,
// each written whole through a temporary file (0600, the directory 0700):
//
//	settings.json   schedule, limits, the Steam selection
//	steam.json      the Steam sign-in: account, refresh token, machine ID
//	steam-apps.json the account's games as last read from Steam
//	prefilled.json  per app the depot manifests last downloaded completely
//	history.json    the last jobs
//
// Only steam.json holds a secret. Nothing of it leaves the service: the
// socket API reports the account name and the token's expiry only.

const (
	fileSettings  = "settings.json"
	fileSteam     = "steam.json"
	fileSteamApps = "steam-apps.json"
	filePrefilled = "prefilled.json"
	fileHistory   = "history.json"

	maxStateFile = 16 << 20
	historyLen   = 20
)

// steamAccount is steam.json.
type steamAccount struct {
	Name         string    `json:"name"`
	SteamID      uint64    `json:"steamId"`
	RefreshToken string    `json:"refreshToken"`
	Since        time.Time `json:"since"`
	Expires      time.Time `json:"expires,omitzero"`
	// MachineID seeds the machine ID sent at sign-in, so that Steam sees
	// one device across restarts; LoginID keeps the service's sessions
	// apart from the account's Steam clients (equal IDs end each other).
	MachineID string `json:"machineId"`
	LoginID   uint32 `json:"loginId"`
	// Error is the last failure to sign in with the token.
	Error string `json:"error,omitempty"`
}

// steamApps is steam-apps.json.
type steamApps struct {
	Updated time.Time `json:"updated"`
	// Filter is the depot choice (platforms|languages) it was read with.
	Filter string        `json:"filter"`
	Apps   []steamAppRec `json:"apps"`
}

// steamAppRec is one game of the account with the depots a job would
// download under the settings it was read with.
type steamAppRec struct {
	ID     uint32          `json:"id"`
	Name   string          `json:"name"`
	Depots []steamDepotRec `json:"depots"`
}

type steamDepotRec struct {
	ID       uint32 `json:"id"`
	Manifest uint64 `json:"manifest"`
	Bytes    int64  `json:"bytes"`
	// App is the app the depot belongs to when it is not the game itself
	// (a shared depot of another app); request codes are asked for it.
	App uint32 `json:"app,omitzero"`
}

// prefilledRec is prefilled.json: per "steam:<app>" the time and the depot
// manifests of the last complete download.
type prefilledRec map[string]prefilledApp

type prefilledApp struct {
	At        time.Time         `json:"at"`
	Manifests map[string]uint64 `json:"manifests"`
}

// history is history.json, newest first.
type history struct {
	Jobs []prefillapi.Job `json:"jobs"`
	// LastWindow is the start of the last schedule window a scheduled job
	// finished in; a restart inside the window does not run it again.
	LastWindow time.Time `json:"lastWindow,omitzero"`
}

// store reads and writes the state files.
type store struct {
	mu   sync.Mutex
	root *os.Root
}

func openStore(dir string) (*store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("state directory %s is not a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("state directory %s: %w", dir, err)
		}
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &store{root: r}, nil
}

func (s *store) close() error { return s.root.Close() }

// load reads name into v; a missing file leaves v as it is.
func (s *store) load(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.root.OpenFile(name, os.O_RDONLY|oNoFollow, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStateFile+1))
	if err != nil {
		return err
	}
	if len(b) > maxStateFile {
		return fmt.Errorf("%s is larger than %d bytes", name, maxStateFile)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// save writes v to name (0600) through a temporary file.
func (s *store) save(name string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tmp := fmt.Sprintf(".%s.%d.tmp", name, time.Now().UnixNano())
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = s.root.Rename(tmp, name)
	}
	if err != nil {
		_ = s.root.Remove(tmp)
	}
	return err
}

// remove deletes name; a missing file is no error.
func (s *store) remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// defaultSettings are the settings of a new installation.
func defaultSettings() prefillapi.Settings {
	return prefillapi.Settings{
		Schedule:    prefillapi.Schedule{Days: []int{0, 1, 2, 3, 4, 5, 6}, Start: "03:00", End: "07:00"},
		Connections: 8,
		Steam:       prefillapi.SteamSettings{OS: []string{"windows"}, Languages: []string{"english"}, Apps: []uint32{}},
	}
}

// Steam's language names (the "language" of a depot's config).
var steamLanguages = []string{
	"arabic", "brazilian", "bulgarian", "czech", "danish", "dutch", "english", "finnish", "french", "german",
	"greek", "hungarian", "indonesian", "italian", "japanese", "koreana", "latam", "norwegian", "polish",
	"portuguese", "romanian", "russian", "schinese", "spanish", "swedish", "tchinese", "thai", "turkish",
	"ukrainian", "vietnamese",
}

const (
	maxSelected    = 2000
	maxConnections = 32
	maxMbps        = 100000
)

// validateSettings checks settings from the API and returns them normalised
// (sorted, without duplicates).
func validateSettings(s prefillapi.Settings) (prefillapi.Settings, error) {
	if _, err := parseClock(s.Schedule.Start); err != nil {
		return s, fmt.Errorf("schedule.start: %w", err)
	}
	if _, err := parseClock(s.Schedule.End); err != nil {
		return s, fmt.Errorf("schedule.end: %w", err)
	}
	for _, d := range s.Schedule.Days {
		if d < 0 || d > 6 {
			return s, fmt.Errorf("schedule.days: %d is not a weekday (0–6)", d)
		}
	}
	if s.Schedule.Enabled && len(s.Schedule.Days) == 0 {
		return s, errors.New("schedule.days: choose at least one day")
	}
	if s.Connections < 1 || s.Connections > maxConnections {
		return s, fmt.Errorf("connections: 1–%d", maxConnections)
	}
	if s.MaxMbps < 0 || s.MaxMbps > maxMbps {
		return s, fmt.Errorf("maxMbps: 0–%d", maxMbps)
	}
	if len(s.Steam.OS) == 0 {
		return s, errors.New("steam.os: choose at least one platform")
	}
	for _, o := range s.Steam.OS {
		if o != "windows" && o != "macos" && o != "linux" {
			return s, fmt.Errorf("steam.os: %q is not windows, macos or linux", o)
		}
	}
	if len(s.Steam.Languages) == 0 {
		return s, errors.New("steam.languages: choose at least one language")
	}
	for _, l := range s.Steam.Languages {
		if !slices.Contains(steamLanguages, l) {
			return s, fmt.Errorf("steam.languages: %q is not a Steam language", l)
		}
	}
	if len(s.Steam.Apps) > maxSelected {
		return s, fmt.Errorf("steam.apps: at most %d", maxSelected)
	}
	s.Schedule.Days = sortedUnique(s.Schedule.Days)
	s.Steam.OS = sortedUnique(s.Steam.OS)
	s.Steam.Languages = sortedUnique(s.Steam.Languages)
	s.Steam.Apps = sortedUnique(s.Steam.Apps)
	return s, nil
}

func sortedUnique[T int | uint32 | string](v []T) []T {
	v = slices.Clone(v)
	slices.Sort(v)
	return slices.Compact(v)
}
