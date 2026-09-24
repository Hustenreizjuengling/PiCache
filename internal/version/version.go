// Package version exposes build information set via -ldflags.
package version

import "runtime"

// Set at build time:
//
//	-ldflags "-X github.com/hustenreizjuengling/picache/internal/version.Version=v0.1.0
//	          -X github.com/hustenreizjuengling/picache/internal/version.Commit=abc123
//	          -X github.com/hustenreizjuengling/picache/internal/version.Date=2026-09-24T10:00:00Z"
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info is the build information returned by the API.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	GoVersion string `json:"goVersion"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
}

// Get returns the build information of the running binary.
func Get() Info {
	return Info{
		Version:   Version,
		Commit:    Commit,
		Date:      Date,
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}
}
