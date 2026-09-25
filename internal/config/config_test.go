package config

import "testing"

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

// PICACHE_DHCP opens the DHCP sockets at start; off by default, on with
// on/1/true.
func TestDHCPSwitch(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"on", true, false},
		{"ON", true, false},
		{"1", true, false},
		{"true", true, false},
		{"off", false, false},
		{"0", false, false},
		{"enabled", false, true},
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
