package storage

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
)

// Kernel file systems and mount helpers reported in Capabilities.
var (
	capFilesystems = []string{"cifs", "smb3", "nfs", "nfs4"}
	capHelpers     = []string{"mount.cifs", "mount.nfs"}
)

// normalizeContainer maps a container manager name to docker | podman | lxc
// ("" for anything else: bare metal, VMs, unknown managers).
func normalizeContainer(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "docker":
		return "docker"
	case "podman":
		return "podman"
	case "lxc", "lxc-libvirt":
		return "lxc"
	}
	return ""
}

// containerFromEnviron extracts only the container= variable from
// /proc/1/environ (NUL-separated); nothing else is ever returned.
func containerFromEnviron(b []byte) string {
	for kv := range bytes.SplitSeq(b, []byte{0}) {
		if v, ok := bytes.CutPrefix(kv, []byte("container=")); ok {
			return normalizeContainer(string(v))
		}
	}
	return ""
}

// parseIDMap parses /proc/self/uid_map (or gid_map): "inside outside count"
// per line. It reports whether this is the initial user namespace (the single
// identity mapping "0 0 4294967295") and the offset for id (host = offset + id).
func parseIDMap(b []byte, id int64) (initNS bool, offset int64) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	lines := 0
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 3 {
			continue
		}
		inside, err1 := strconv.ParseInt(f[0], 10, 64)
		outside, err2 := strconv.ParseInt(f[1], 10, 64)
		count, err3 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		lines++
		if lines == 1 && inside == 0 && outside == 0 && count == 4294967295 {
			initNS = true
		}
		if id >= inside && id < inside+count {
			offset = outside - inside
		}
	}
	return initNS && lines == 1, offset
}

// parseFilesystems reads /proc/filesystems ("nodev\tcifs" or "\text4") and
// reports the network file systems PiCache cares about.
func parseFilesystems(b []byte) map[string]bool {
	out := make(map[string]bool, len(capFilesystems))
	for _, name := range capFilesystems {
		out[name] = false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if _, ok := out[f[len(f)-1]]; ok {
			out[f[len(f)-1]] = true
		}
	}
	return out
}

// classifyNetworkMode guesses the Docker network mode from the interface
// names visible in the container: host networking shows the host's bridges.
func classifyNetworkMode(ifaces []string) string {
	other := 0
	for _, n := range ifaces {
		switch {
		case n == "docker0" || n == "podman0" || n == "cni-podman0" || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "veth"):
			return "host"
		case n != "lo":
			other++
		}
	}
	if other == 1 {
		return "bridge"
	}
	return ""
}
