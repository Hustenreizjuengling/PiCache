package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Precedence: env > listeners.json > default; listeners.next.json is never
// read by the tools (the running process wrote listeners.json); the files
// are ignored with PICACHE_RUN_AS.
func TestEffectiveListeners(t *testing.T) {
	dir := t.TempDir()
	if err := WriteListenersFile(dir, ListenersFile, EncodeListeners(map[string][]string{
		RoleWeb: {"127.0.0.1:9090"}, RoleDNS: {"192.168.1.2:53", "[::1]:53"}, RoleSNI: {}})); err != nil {
		t.Fatal(err)
	}
	if err := WriteListenersFile(dir, ListenersNextFile, EncodeListeners(map[string][]string{RoleWeb: {":7070"}})); err != nil {
		t.Fatal(err)
	}
	l, warn := EffectiveListeners(envOf(map[string]string{"PICACHE_DNS_LISTEN": ":5353"}), dir)
	if len(warn) != 0 {
		t.Fatal(warn)
	}
	for role, want := range map[string][]string{RoleWeb: {"127.0.0.1:9090"}, RoleDNS: {":5353"}, RoleSNI: {},
		RoleCache: {":80"}, RoleWebTLS: {":8443"}, RoleNTP: {}} {
		if !slices.Equal(l.Roles[role], want) {
			t.Errorf("%s = %v, want %v", role, l.Roles[role], want)
		}
	}
	if l.Source[RoleWeb] != SourceFile || l.Source[RoleDNS] != SourceEnv || l.Source[RoleCache] != SourceDefault {
		t.Fatalf("sources %v", l.Source)
	}
	l, _ = EffectiveListeners(envOf(map[string]string{"PICACHE_RUN_AS": "65532:65532"}), dir)
	if !slices.Equal(l.Roles[RoleWeb], []string{":8080"}) || l.Source[RoleWeb] != SourceDefault {
		t.Fatalf("with PICACHE_RUN_AS the files must be ignored: %v", l.Roles[RoleWeb])
	}
}

func TestListenersFileChecks(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, content string }{
		{"unknown member", `{"version":1,"dns":[":53"],"foo":[]}`},
		{"duplicate", `{"version":1,"dns":[":53"],"dns":[":54"]}`},
		{"version", `{"version":2,"dns":[":53"]}`},
		{"no version", `{"dns":[":53"]}`},
		{"dns off", `{"version":1,"dns":[]}`},
		{"cache off", `{"version":1,"cache":[]}`},
		{"web off without tls", `{"version":1,"web":[],"webTls":[]}`},
		{"name", `{"version":1,"web":["localhost:8080"]}`},
		{"zone", `{"version":1,"web":["[fe80::1%eth0]:8080"]}`},
		{"port", `{"version":1,"web":[":0"]}`},
		{"port range", `{"version":1,"web":[":65536"]}`},
		{"twice", `{"version":1,"web":[":8080",":8080"]}`},
		{"nine", `{"version":1,"web":[":1",":2",":3",":4",":5",":6",":7",":8",":9"]}`},
		{"not json", `{"version":1,"dns":[":53"`},
		{"null", `{"version":1,"dns":null}`},
		{"secret content", `{"version":1,"web":["hunter2"]}`},
	} {
		write(ListenersFile, tc.content)
		l, warn := EffectiveListeners(envOf(nil), dir)
		if len(warn) != 1 || !slices.Equal(l.Roles[RoleWeb], []string{":8080"}) {
			t.Errorf("%s: warnings %v, web %v", tc.name, warn, l.Roles[RoleWeb])
		}
		if len(warn) == 1 && strings.Contains(warn[0], "hunter2") {
			t.Errorf("%s: the warning quotes the content: %s", tc.name, warn[0])
		}
	}
	// web may be off while webTls has an address.
	write(ListenersFile, `{"version":1,"web":[],"webTls":["[::]:8443"],"ntp":["0.0.0.0:123"]}`)
	if l, warn := EffectiveListeners(envOf(nil), dir); len(warn) != 0 || len(l.Roles[RoleWeb]) != 0 || l.Roles[RoleNTP][0] != "0.0.0.0:123" {
		t.Fatalf("%v %v", l.Roles, warn)
	}
	// Too large.
	write(ListenersFile, `{"version":1,"web":[":8080"]}`+strings.Repeat(" ", MaxListenersFile))
	if _, warn := EffectiveListeners(envOf(nil), dir); len(warn) != 1 {
		t.Fatal("an oversized file must be ignored")
	}
	// A symbolic link in its place.
	os.Remove(filepath.Join(dir, ListenersFile))
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	os.WriteFile(target, []byte(`{"version":1,"web":["127.0.0.1:1"]}`), 0o644)
	if err := os.Symlink(target, filepath.Join(dir, ListenersFile)); err == nil {
		if l, warn := EffectiveListeners(envOf(nil), dir); len(warn) != 1 || l.Roles[RoleWeb][0] != ":8080" {
			t.Fatalf("a symbolic link must be ignored: %v %v", l.Roles[RoleWeb], warn)
		}
		os.Remove(filepath.Join(dir, ListenersFile))
	}
	// A hard link (two links).
	if runtime.GOOS == "linux" {
		write("other.json", `{"version":1,"web":["127.0.0.1:1"]}`)
		if err := os.Link(filepath.Join(dir, "other.json"), filepath.Join(dir, ListenersFile)); err == nil {
			if _, warn := EffectiveListeners(envOf(nil), dir); len(warn) != 1 || !strings.Contains(warn[0], "more than one link") {
				t.Fatalf("a hard link must be ignored: %v", warn)
			}
		}
	}
}

func TestLoadListenerLocksAndNewVariables(t *testing.T) {
	cfg, err := Load([]string{"--web-listen", ":9000", "--ntp-listen", ":123"}, envOf(map[string]string{
		"PICACHE_DATA_DIR": "/var/lib/picache", "PICACHE_DNS_LISTEN": "127.0.0.1:53", "PICACHE_PPROF": "on",
		"PICACHE_LOG_FILE": "/var/log/picache/picache.log", "PICACHE_LOG_SYSLOG": "tcp://logs.lan:6514",
		"PICACHE_INITIAL_CONFIG": "/etc/picache/initial.json",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenerLock[RoleWeb] != "flag" || cfg.ListenerLock[RoleDNS] != "PICACHE_DNS_LISTEN" || cfg.ListenerLock[RoleNTP] != "flag" ||
		cfg.ListenerLock[RoleCache] != "" {
		t.Fatalf("locks %v", cfg.ListenerLock)
	}
	if !cfg.PProf || cfg.LogSyslogNetwork != "tcp" || cfg.LogSyslogAddr != "logs.lan:6514" ||
		cfg.InitialConfig != "/etc/picache/initial.json" || !slices.Equal(cfg.NTPListen, []string{":123"}) {
		t.Fatalf("config %+v", cfg)
	}
	for _, tc := range []struct{ key, value, want string }{
		{"PICACHE_PPROF", "yes", "PICACHE_PPROF must be on or off"},
		{"PICACHE_LOG_SYSLOG", "https://logs.lan:514", "PICACHE_LOG_SYSLOG"},
		{"PICACHE_LOG_SYSLOG", "udp://logs.lan", "PICACHE_LOG_SYSLOG"},
		{"PICACHE_LOG_SYSLOG", "udp://u@logs.lan:514", "PICACHE_LOG_SYSLOG"},
		{"PICACHE_LOG_SYSLOG", "udp://logs.lan:514/x", "PICACHE_LOG_SYSLOG"},
		{"PICACHE_LOG_FILE", "/etc/passwd", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "picache.log", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/log/picache/../x.log", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/log/picache", "PICACHE_LOG_FILE must be a <name>.log file below"},
		// PiCache's own state in the data directory is never a log file.
		{"PICACHE_LOG_FILE", "/var/lib/picache/keys/master.key", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/lib/picache/picache.db", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/lib/picache/logs.db", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/lib/picache/listeners.json", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/lib/picache/tls/key.log", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/lib/picache/update-requests/x.log", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_LOG_FILE", "/var/log/picache/.log", "PICACHE_LOG_FILE must be a <name>.log file below"},
		{"PICACHE_NTP_LISTEN", "ntp.lan:123", "PICACHE_NTP_LISTEN"},
	} {
		_, err := Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": "/var/lib/picache", tc.key: tc.value}))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s: %v", tc.key, tc.value, err)
		}
	}
	if runtime.GOOS == "linux" {
		if !LogFileAllowed("/var/lib/picache/logs/picache.log", "/var/lib/picache") || !LogFileAllowed("/data/picache.log", "/data") ||
			!LogFileAllowed("/var/log/picache/a/b.log", "/data") || LogFileAllowed("/var/lib/picachex/a.log", "/var/lib/picache") ||
			LogFileAllowed("/data/lists/a.log", "/data") {
			t.Fatal("LogFileAllowed")
		}
	}
}

// The listeners files are written by the service and read by root: any
// content is either refused or a valid set.
func FuzzParseListeners(f *testing.F) {
	f.Add([]byte(`{"version":1,"dns":[":53"],"web":["127.0.0.1:8080"],"ntp":[]}`))
	f.Add([]byte(`{"version":1,"webTls":["[::1]:8443"],"web":[]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		roles, err := ParseListeners(b)
		if err != nil {
			return
		}
		again, err := ParseListeners(EncodeListeners(roles))
		if err != nil {
			t.Fatalf("an accepted set does not round-trip: %v", err)
		}
		for role, addrs := range roles {
			if !slices.Equal(addrs, again[role]) || len(addrs) > MaxListenerAddrs {
				t.Fatalf("%s: %v vs %v", role, addrs, again[role])
			}
		}
	})
}

// The names of the bound listeners map to their roles; dns-udp and ntp are
// the UDP listeners.
func TestBoundRole(t *testing.T) {
	for name, want := range map[string]struct {
		role string
		tcp  bool
	}{
		"dns-udp": {RoleDNS, false}, "dns-tcp": {RoleDNS, true}, "web-tls": {RoleWebTLS, true}, "ntp": {RoleNTP, false},
		"web": {RoleWeb, true}, "cache": {RoleCache, true}, "sni": {RoleSNI, true}, "dot": {RoleDoT, true}, "doh": {RoleDoH, true},
	} {
		if role, tcp := BoundRole(name); role != want.role || tcp != want.tcp {
			t.Errorf("%s: %s %v", name, role, tcp)
		}
	}
}
