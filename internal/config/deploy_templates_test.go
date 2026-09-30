package config

import (
	"encoding/xml"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The container templates of deploy/ keep the hardening of
// docker-compose.yml (docs/DEPLOYMENT.md "NAS", "macvlan"): no privileged
// mode, no SYS_ADMIN, no host networking by default, a read-only root with
// a /tmp tmpfs, every capability dropped but the ones PiCache needs, and a
// PICACHE_RUN_AS that ParseRunAs accepts (never empty or root).

// composeTemplates are the compose files checked line by line, with
// whether NET_RAW may be in the active cap_add (the macvlan file: IPv6
// router advertisements; the NAS templates have it only as a comment).
var composeTemplates = map[string]bool{
	"../../deploy/docker/docker-compose.macvlan.yml": true,
	"../../deploy/truenas/docker-compose.yml":        false,
	"../../deploy/synology/docker-compose.yml":       false,
}

// activeLines returns the lines of a YAML file without comments.
func activeLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if i := strings.Index(l, "#"); i >= 0 && !strings.Contains(l[:i], `"`) {
			l = l[:i]
		}
		if strings.TrimSpace(l) != "" {
			out = append(out, strings.TrimRight(l, " "))
		}
	}
	return out
}

func TestComposeTemplatesHardening(t *testing.T) {
	runAs := regexp.MustCompile(`^\s+PICACHE_RUN_AS:\s*"?([^"\s]*)"?\s*$`)
	for path, netRaw := range composeTemplates {
		lines := activeLines(t, path)
		text := strings.Join(lines, "\n")
		want := []string{
			"    read_only: true",
			"    cap_drop: [ALL]",
			`    security_opt: ["no-new-privileges:true"]`,
			"      - /tmp:size=64m",
			"    driver: macvlan",
			"      parent: CHANGE_ME",
			// A fixed host name: the local CA names it; Docker's default is
			// the container ID, new with every recreated container.
			"    hostname: picache",
		}
		if netRaw {
			want = append(want, "    cap_add: [NET_BIND_SERVICE, SETUID, SETGID, NET_RAW]")
		} else {
			want = append(want, "    cap_add: [NET_BIND_SERVICE, SETUID, SETGID]", "    privileged: false")
		}
		for _, w := range want {
			if !slices.Contains(lines, w) {
				t.Errorf("%s lacks the line %q", path, w)
			}
		}
		for _, bad := range []string{"privileged: true", "SYS_ADMIN", "network_mode: host", "network_mode: \"host\"", "cap_add: [ALL]",
			"seccomp:unconfined", "apparmor:unconfined"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains %q", path, bad)
			}
		}
		if strings.Count(text, "cap_add:") != 1 || strings.Count(text, "ipv4_address:") != 1 {
			t.Errorf("%s: one cap_add and one ipv4_address expected", path)
		}
		// The raw socket stays a documented option in the NAS templates.
		if !netRaw {
			b, _ := os.ReadFile(path)
			if !strings.Contains(string(b), "# cap_add: [NET_BIND_SERVICE, SETUID, SETGID, NET_RAW] # only for IPv6 router advertisements") {
				t.Errorf("%s lacks the commented NET_RAW line", path)
			}
		}
		n := 0
		for _, l := range lines {
			if m := runAs.FindStringSubmatch(l); m != nil {
				n++
				if _, _, err := ParseRunAs(m[1]); err != nil {
					t.Errorf("%s: %v", path, err)
				}
			}
		}
		if !netRaw && n != 1 {
			t.Errorf("%s: %d PICACHE_RUN_AS lines, want 1", path, n)
		}
	}
}

// The bridge compose file (and every other file without host networking)
// sets a fixed host name: the local CA is created for the host name, and
// Docker's default, the container ID, changes with every recreated
// container (docker compose pull && docker compose up -d), so the tls
// health check warned after every update.
func TestComposeTemplatesHostname(t *testing.T) {
	for _, path := range []string{"../../deploy/docker/docker-compose.bridge.yml", "../../deploy/docker/docker-compose.macvlan.yml",
		"../../deploy/truenas/docker-compose.yml", "../../deploy/synology/docker-compose.yml"} {
		if !slices.Contains(activeLines(t, path), "    hostname: picache") {
			t.Errorf("%s sets no fixed hostname", path)
		}
	}
}

// unraidTemplate is the part of an Unraid template the test reads.
type unraidTemplate struct {
	XMLName     xml.Name `xml:"Container"`
	Repository  string   `xml:"Repository"`
	Network     string   `xml:"Network"`
	MyIP        string   `xml:"MyIP"`
	Privileged  string   `xml:"Privileged"`
	ExtraParams string   `xml:"ExtraParams"`
	Icon        string   `xml:"Icon"`
	Configs     []struct {
		Name    string `xml:"Name,attr"`
		Target  string `xml:"Target,attr"`
		Default string `xml:"Default,attr"`
		Type    string `xml:"Type,attr"`
		Value   string `xml:",chardata"`
	} `xml:"Config"`
}

func TestUnraidTemplate(t *testing.T) {
	b, err := os.ReadFile("../../deploy/unraid/picache.xml")
	if err != nil {
		t.Fatal(err)
	}
	var c unraidTemplate
	if err := xml.Unmarshal(b, &c); err != nil {
		t.Fatalf("picache.xml: %v", err)
	}
	if c.Network != "br0" || c.MyIP == "" || c.Privileged != "false" || !strings.HasPrefix(c.Repository, "ghcr.io/hustenreizjuengling/picache:") {
		t.Fatalf("template %+v", c)
	}
	// OPS-1: restarted like the compose files (restart: unless-stopped,
	// stop_grace_period: 30s); dockerMan sets no restart policy itself, so an
	// in-app restart (exit 75) or a crash would leave the network without DNS.
	// A fixed host name, as in the compose files (the local CA names it).
	if c.ExtraParams != "--restart=unless-stopped --stop-timeout=30 --hostname=picache --cap-drop=ALL --cap-add=NET_BIND_SERVICE --cap-add=SETUID --cap-add=SETGID "+
		"--security-opt=no-new-privileges:true --read-only --tmpfs=/tmp:size=64m" {
		t.Fatalf("ExtraParams %q", c.ExtraParams)
	}
	for _, bad := range []string{"SYS_ADMIN", "--privileged", "--network=host", "--net=host"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("picache.xml contains %q", bad)
		}
	}
	if c.Icon != "" && !strings.HasPrefix(c.Icon, "https://") {
		t.Errorf("Icon %q: a URL or empty", c.Icon)
	}
	paths := map[string]string{}
	runAs := 0
	for _, cfg := range c.Configs {
		switch {
		case cfg.Type == "Path":
			paths[cfg.Target] = cfg.Value
			if strings.HasPrefix(cfg.Value, "/mnt/user/") || cfg.Value != cfg.Default {
				t.Errorf("path %s = %q (default %q): a pool path, not /mnt/user", cfg.Target, cfg.Value, cfg.Default)
			}
		case cfg.Target == "PICACHE_RUN_AS":
			runAs++
			for _, v := range []string{cfg.Value, cfg.Default} {
				if uid, gid, err := ParseRunAs(v); err != nil || uid != 99 || gid != 100 {
					t.Errorf("PICACHE_RUN_AS %q: %d:%d %v", v, uid, gid, err)
				}
			}
		}
	}
	if runAs != 1 || paths["/data"] != "/mnt/cache/appdata/picache/data" || paths["/cache"] != "/mnt/cache/appdata/picache/cache" {
		t.Fatalf("configs: run as %d, paths %v", runAs, paths)
	}
}

// ParseRunAs never accepts root or an empty value (the templates rely on
// it).
func TestParseRunAsRefusesRoot(t *testing.T) {
	for _, v := range []string{"", "0:0", "0:100", "99:0", ":", "root:root", "99", "-1:100"} {
		if _, _, err := ParseRunAs(v); err == nil {
			t.Errorf("ParseRunAs(%q) accepted", v)
		}
	}
}
