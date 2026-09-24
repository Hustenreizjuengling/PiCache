//go:build !linux

package storage

import (
	"os"
	"runtime"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// detectStaticCapabilities reports a plain development host: no containers,
// no systemd, nothing mountable.
func detectStaticCapabilities(cfg *config.Config) Capabilities {
	return Capabilities{OS: runtime.GOOS, UID: os.Getuid(), GID: os.Getgid(), InitUserNS: true, MountRoot: cfg.MountRoot}
}

func kernelFilesystems() map[string]bool { return parseFilesystems(nil) }

func mountHelpers() map[string]bool {
	out := make(map[string]bool, len(capHelpers))
	for _, h := range capHelpers {
		out[h] = false
	}
	return out
}
