//go:build linux

package storage

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// readSmall reads at most limit bytes of a (proc) file.
func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// detectStaticCapabilities detects what does not change while running.
func detectStaticCapabilities(cfg *config.Config) Capabilities {
	c := Capabilities{OS: runtime.GOOS, UID: os.Getuid(), GID: os.Getgid(), MountRoot: cfg.MountRoot}
	c.Container = detectContainer()
	if b, err := readSmall("/proc/self/uid_map", 64<<10); err == nil {
		c.InitUserNS, c.UIDMapOffset = parseIDMap(b, int64(c.UID))
	}
	if b, err := readSmall("/proc/self/gid_map", 64<<10); err == nil {
		_, c.GIDMapOffset = parseIDMap(b, int64(c.GID))
	}
	c.Systemd = fileExists("/run/systemd/system")
	if c.Container == "docker" || c.Container == "podman" {
		if ifs, err := net.Interfaces(); err == nil {
			names := make([]string, 0, len(ifs))
			for _, i := range ifs {
				names = append(names, i.Name)
			}
			c.DockerMode = classifyNetworkMode(names)
		}
	}
	return c
}

// detectContainer identifies docker, podman or lxc. /proc/1/environ is only
// searched for container= (readable when PID 1 is ours, e.g. in Docker).
func detectContainer() string {
	switch {
	case fileExists("/.dockerenv"):
		return "docker"
	case fileExists("/run/.containerenv"):
		return "podman"
	}
	if b, err := readSmall("/run/systemd/container", 256); err == nil {
		if c := normalizeContainer(string(b)); c != "" {
			return c
		}
	}
	if b, err := readSmall("/proc/1/environ", 64<<10); err == nil {
		return containerFromEnviron(b)
	}
	return ""
}

// kernelFilesystems reports the network file systems known to the kernel.
func kernelFilesystems() map[string]bool {
	b, _ := readSmall("/proc/filesystems", 64<<10)
	return parseFilesystems(b)
}

// mountHelpers reports whether mount.cifs / mount.nfs are installed.
func mountHelpers() map[string]bool {
	out := make(map[string]bool, len(capHelpers))
	for _, h := range capHelpers {
		out[h] = false
		for _, dir := range []string{"/sbin", "/usr/sbin", "/bin", "/usr/bin"} {
			if fileExists(filepath.Join(dir, h)) {
				out[h] = true
				break
			}
		}
	}
	return out
}
