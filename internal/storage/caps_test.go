package storage

import (
	"strings"
	"testing"
)

func TestParseIDMap(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		id     int64
		initNS bool
		offset int64
	}{
		{"init namespace", "         0          0 4294967295\n", 999, true, 0},
		{"unprivileged lxc", "         0     100000      65536\n", 999, false, 100000},
		{"1:1 mapped user", "0 100000 1005\n1005 1005 1\n1006 101006 64530\n", 1005, false, 0},
		{"above the mapped user", "0 100000 1005\n1005 1005 1\n1006 101006 64530\n", 2000, false, 100000},
		{"docker userns-remap", "0 231072 65536\n", 65532, false, 231072},
		{"garbage", "a b c\n\n0 0\n", 0, false, 0},
	}
	for _, tc := range tests {
		initNS, off := parseIDMap([]byte(tc.in), tc.id)
		if initNS != tc.initNS || off != tc.offset {
			t.Errorf("%s: got (%v, %d), want (%v, %d)", tc.name, initNS, off, tc.initNS, tc.offset)
		}
	}
}

func TestContainerFromEnviron(t *testing.T) {
	env := "PATH=/usr/bin\x00SECRET_TOKEN=hunter2\x00container=lxc\x00HOME=/root\x00"
	if got := containerFromEnviron([]byte(env)); got != "lxc" {
		t.Fatalf("got %q", got)
	}
	// Only the allowlisted names are reported; nothing else from environ.
	for in, want := range map[string]string{
		"container=hunter2\x00":       "",
		"container=podman":            "podman",
		"container=Docker\x00":        "docker",
		"container=systemd-nspawn":    "",
		"PATH=/bin\x00xcontainer=lxc": "",
		"":                            "",
	} {
		if got := containerFromEnviron([]byte(in)); got != want {
			t.Errorf("containerFromEnviron(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseFilesystems(t *testing.T) {
	in := "nodev\tsysfs\nnodev\ttmpfs\n\text4\nnodev\tcifs\nnodev\tsmb3\nnodev\tnfs4\n\txfs\n"
	got := parseFilesystems([]byte(in))
	want := map[string]bool{"cifs": true, "smb3": true, "nfs": false, "nfs4": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %v", k, got[k])
		}
	}
}

func TestClassifyNetworkMode(t *testing.T) {
	for in, want := range map[string]string{
		"lo,eth0":                    "bridge",
		"lo,eth0,docker0,enp3s0":     "host",
		"lo,enp3s0,br-1a2b3c,veth12": "host",
		"lo,eth0,eth1":               "",
		"lo":                         "",
	} {
		if got := classifyNetworkMode(strings.Split(in, ",")); got != want {
			t.Errorf("classifyNetworkMode(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestCapabilitiesMaps(t *testing.T) {
	cfg := testConfig(t)
	m := bareManager(cfg)
	m.caps = detectStaticCapabilities(cfg)
	m.hostApplyFlag = cfg.DataDir // exists
	c := m.Capabilities()
	if !c.HostApply || c.MountRoot != cfg.MountRoot || c.OS == "" {
		t.Fatalf("caps %+v", c)
	}
	for _, k := range capFilesystems {
		if _, ok := c.Filesystems[k]; !ok {
			t.Errorf("filesystem %s missing", k)
		}
	}
	for _, k := range capHelpers {
		if _, ok := c.MountHelpers[k]; !ok {
			t.Errorf("helper %s missing", k)
		}
	}
}
