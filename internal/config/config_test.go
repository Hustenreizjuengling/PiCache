package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// PICACHE_WEB_SECURE_COOKIES (for a TLS-terminating reverse proxy) is off by
// default and parsed as a boolean.
func TestWebSecureCookies(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"true", true, false},
		{"1", true, false},
		{"false", false, false},
		{"yes please", false, true},
	} {
		c, err := Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(), "PICACHE_WEB_SECURE_COOKIES": tc.val}))
		if (err != nil) != tc.wantErr {
			t.Fatalf("%q: err = %v", tc.val, err)
		}
		if err == nil && c.WebSecureCookies != tc.want {
			t.Fatalf("%q: WebSecureCookies = %v", tc.val, c.WebSecureCookies)
		}
	}
}

// PICACHE_DHCP has three modes: unset (the markers decide), every off
// spelling (opt-out) and every on spelling (the opt-in of earlier
// versions); anything else refuses to start.
func TestDHCPSwitch(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    DHCPMode
		wantErr bool
	}{
		{"", DHCPAuto, false},
		{"on", DHCPOn, false},
		{"ON", DHCPOn, false},
		{"yes", DHCPOn, false},
		{"1", DHCPOn, false},
		{"t", DHCPOn, false},
		{"true", DHCPOn, false},
		{"TRUE", DHCPOn, false},
		{" on ", DHCPOn, false},
		{"off", DHCPOff, false},
		{"no", DHCPOff, false},
		{"0", DHCPOff, false},
		{"f", DHCPOff, false},
		{"False", DHCPOff, false},
		{"enabled", DHCPAuto, true},
		{"maybe", DHCPAuto, true},
	} {
		c, err := Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(), "PICACHE_DHCP": tc.val}))
		if (err != nil) != tc.wantErr {
			t.Fatalf("%q: err = %v", tc.val, err)
		}
		if err == nil && c.DHCP != tc.want {
			t.Fatalf("%q: DHCP = %v", tc.val, c.DHCP)
		}
	}
}

// PICACHE_CONFIG_LOCKED (default off) and PICACHE_DESTRUCTIVE_API (default
// on) are switches; an invalid value refuses to start and names the
// variable.
func TestConfigLockAndDestructiveSwitch(t *testing.T) {
	c, err := Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir()}))
	if err != nil || c.ConfigLocked || !c.DestructiveAPI {
		t.Fatalf("defaults: locked %v destructive %v (%v)", c.ConfigLocked, c.DestructiveAPI, err)
	}
	c, err = Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(),
		"PICACHE_CONFIG_LOCKED": "on", "PICACHE_DESTRUCTIVE_API": "false"}))
	if err != nil || !c.ConfigLocked || c.DestructiveAPI {
		t.Fatalf("set: locked %v destructive %v (%v)", c.ConfigLocked, c.DestructiveAPI, err)
	}
	for _, key := range []string{"PICACHE_CONFIG_LOCKED", "PICACHE_DESTRUCTIVE_API"} {
		_, err := Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(), key: "locked"}))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s=locked: %v", key, err)
		}
	}
}

// Only serve reads PICACHE_ADMIN_PASSWORD_FILE: the maintenance commands
// load the configuration without it, so a missing or unreadable (root-only)
// secret file does not break them.
func TestLoadWithoutSecrets(t *testing.T) {
	unreadable := t.TempDir() // a directory cannot be read as the file
	env := envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(), "PICACHE_ADMIN_PASSWORD_FILE": unreadable})
	if _, err := Load(nil, env); err == nil || !strings.Contains(err.Error(), "PICACHE_ADMIN_PASSWORD_FILE") {
		t.Fatalf("Load: err = %v, want the password file error", err)
	}
	c, err := LoadWithoutSecrets(env)
	if err != nil {
		t.Fatalf("LoadWithoutSecrets: %v", err)
	}
	if c.AdminPassword != "" || c.AdminPasswordFromEnv || c.AdminPasswordFileMissing != "" {
		t.Fatalf("secret read: %+v", c)
	}
}

// OPS-5: a PICACHE_ADMIN_PASSWORD_FILE that does not exist (the line left
// after the first start, the file removed) is no configuration error: Load
// records it and the app fails only while no account exists.
func TestAdminPasswordFileMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "admin-password")
	c, err := Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(), "PICACHE_ADMIN_PASSWORD_FILE": missing,
		"PICACHE_ADMIN_PASSWORD": "not used when the file variable is set"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AdminPasswordFileMissing != missing || c.AdminPassword != "" || c.AdminPasswordFromEnv {
		t.Fatalf("config %+v", c)
	}
	if err := os.WriteFile(missing, []byte("correct horse battery\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = Load(nil, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(), "PICACHE_ADMIN_PASSWORD_FILE": missing})); err != nil ||
		c.AdminPassword != "correct horse battery" || c.AdminPasswordFileMissing != "" {
		t.Fatalf("with the file: %+v %v", c, err)
	}
}

// PICACHE_DOT_LISTEN (default :853) and PICACHE_DOH_LISTEN (default off)
// are lists like the other listeners: off, none and - mean no socket; the
// flags win over the environment.
func TestEncryptedDNSListeners(t *testing.T) {
	for _, tc := range []struct {
		dot, doh         string
		args             []string
		wantDoT, wantDoH string
	}{
		{"", "", nil, ":853", ""},
		{"off", "", nil, "", ""},
		{"none", ":4443", nil, "", ":4443"},
		{"-", "192.168.1.5:443, [::1]:4443", nil, "", "192.168.1.5:443,[::1]:4443"},
		{":8853", "off", []string{"--dot-listen", "off", "--doh-listen", ":4443"}, "", ":4443"},
	} {
		c, err := Load(tc.args, envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(),
			"PICACHE_DOT_LISTEN": tc.dot, "PICACHE_DOH_LISTEN": tc.doh}))
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if got := strings.Join(c.DoTListen, ","); got != tc.wantDoT {
			t.Fatalf("%+v: DoT %q", tc, got)
		}
		if got := strings.Join(c.DoHListen, ","); got != tc.wantDoH {
			t.Fatalf("%+v: DoH %q", tc, got)
		}
	}
}
