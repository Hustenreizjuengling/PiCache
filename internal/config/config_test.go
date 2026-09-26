package config

import (
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
	env := envOf(map[string]string{"PICACHE_DATA_DIR": t.TempDir(), "PICACHE_ADMIN_PASSWORD_FILE": t.TempDir() + "/missing"})
	if _, err := Load(nil, env); err == nil || !strings.Contains(err.Error(), "PICACHE_ADMIN_PASSWORD_FILE") {
		t.Fatalf("Load: err = %v, want the password file error", err)
	}
	c, err := LoadWithoutSecrets(env)
	if err != nil {
		t.Fatalf("LoadWithoutSecrets: %v", err)
	}
	if c.AdminPassword != "" || c.AdminPasswordFromEnv {
		t.Fatalf("secret read: %+v", c)
	}
}
