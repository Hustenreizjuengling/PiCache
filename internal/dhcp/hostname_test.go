package dhcp

import "testing"

func TestSanitizeHostname(t *testing.T) {
	for in, want := range map[string]string{
		"laptop":                  "laptop",
		"Anna's iPhone":           "annas-iphone",
		"DESKTOP-4F2K_01":         "desktop-4f2k-01",
		"nas.fritz.box":           "nas",
		"--odd--name--":           "odd-name",
		"émile":                   "mile",
		"":                        "",
		"...":                     "",
		"-":                       "",
		"a b  c":                  "a-b-c",
		"x\x00y\nz":               "xyz",
		string(make([]byte, 300)): "",
		"0123456789012345678901234567890123456789012345678901234567890123456789": "012345678901234567890123456789012345678901234567890123456789012",
		"abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghij-x":       "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghij",
	} {
		if got := SanitizeHostname(in); got != want {
			t.Errorf("SanitizeHostname(%q) = %q, want %q", in, got, want)
		}
		if got := SanitizeHostname(in); got != "" && !validLabel(got) {
			t.Errorf("%q is not a valid label", got)
		}
	}
}

func TestNormalizeMAC(t *testing.T) {
	for in, want := range map[string]string{
		"AA:BB:CC:DD:EE:F0":   "aa:bb:cc:dd:ee:f0",
		"aa-bb-cc-dd-ee-f0":   "aa:bb:cc:dd:ee:f0",
		" aabbccddeef0 ":      "aa:bb:cc:dd:ee:f0",
		"01:00:5e:00:00:01":   "", // multicast
		"ff:ff:ff:ff:ff:ff":   "", // broadcast
		"00:00:00:00:00:00":   "",
		"aa:bb:cc:dd:ee":      "",
		"aabb.ccdd.eef0":      "",
		"zz:bb:cc:dd:ee:f0":   "",
		"aa:bb:cc:dd:ee:f0:1": "",
	} {
		got, ok := NormalizeMAC(in)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizeMAC(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
