package storage

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/apperr"
	"github.com/hustenreizjuengling/picache/internal/config"
)

func validTarget(cfg *config.Config, kind Kind) Target {
	t := Target{ID: testID, Name: "NAS", Kind: kind, Mode: ModeExternal, Path: filepath.Join(cfg.MountRoot, "nas")}
	switch kind {
	case KindSMB:
		t.Server, t.Share, t.Username, t.Domain = "192.168.1.10", "pi$cache", "picache@corp", "WORKGROUP"
		t.HasPassword, t.SMBVersion, t.RequireMountpoint = true, "3.1.1", true
	case KindNFS:
		t.Server, t.Export, t.NFSVersion, t.NFSNConnect, t.RequireMountpoint = "2001:db8::10", "/volume1/picache", "4.2", 4, true
	}
	return t
}

func TestValidateTarget(t *testing.T) {
	cfg := &config.Config{DataDir: filepath.FromSlash("/var/lib/picache"), CacheDir: filepath.FromSlash("/var/cache/picache"),
		MountRoot: filepath.FromSlash("/srv/picache")}
	if !filepath.IsAbs(cfg.MountRoot) { // Windows: make the paths absolute on the current volume
		abs := func(p string) string { a, _ := filepath.Abs(p); return a }
		cfg.DataDir, cfg.CacheDir, cfg.MountRoot = abs(cfg.DataDir), abs(cfg.CacheDir), abs(cfg.MountRoot)
	}
	mr := cfg.MountRoot
	tests := []struct {
		name  string
		kind  Kind
		mod   func(*Target)
		cfg   func(*config.Config)
		field string // "" = valid
	}{
		{name: "smb", kind: KindSMB},
		{name: "nfs ipv6", kind: KindNFS},
		{name: "local disk", kind: KindLocal},
		{name: "smb subdir", kind: KindSMB, mod: func(t *Target) { t.Subdir = "cache/store-1" }},
		{name: "smb guest", kind: KindSMB, mod: func(t *Target) { t.Username, t.HasPassword = "", false }},
		{name: "smb host-apply", kind: KindSMB, mod: func(t *Target) { t.Mode, t.Path = ModeHostApply, filepath.Join(mr, testID) }},
		{name: "builtin", kind: KindLocal, mod: func(t *Target) { t.ID, t.Path = LocalTargetID, cfg.CacheDir }},

		{name: "traversal id", kind: KindSMB, mod: func(t *Target) { t.ID = "../../../../etc/passwd" }, field: "id"},
		{name: "upper-case id", kind: KindSMB, mod: func(t *Target) { t.ID = strings.ToUpper(testID) }, field: "id"},
		{name: "id with newline", kind: KindSMB, mod: func(t *Target) { t.ID = testID[:31] + "\n" }, field: "id"},
		{name: "empty name", kind: KindSMB, mod: func(t *Target) { t.Name = "" }, field: "name"},
		{name: "name with newline", kind: KindSMB, mod: func(t *Target) { t.Name = "NAS\n[Mount]" }, field: "name"},
		{name: "long name", kind: KindSMB, mod: func(t *Target) { t.Name = strings.Repeat("n", 65) }, field: "name"},
		{name: "bad store id", kind: KindSMB, mod: func(t *Target) { t.StoreID = "../x" }, field: "storeId"},

		{name: "host name", kind: KindSMB, mod: func(t *Target) { t.Server = "nas.lan" }, field: "server"},
		{name: "server newline", kind: KindSMB, mod: func(t *Target) { t.Server = "192.168.1.10\nx" }, field: "server"},
		{name: "server with zone", kind: KindNFS, mod: func(t *Target) { t.Server = "fe80::1%eth0" }, field: "server"},
		{name: "non-canonical ipv6", kind: KindNFS, mod: func(t *Target) { t.Server = "2001:DB8::10" }, field: "server"},
		{name: "unspecified", kind: KindNFS, mod: func(t *Target) { t.Server = "0.0.0.0" }, field: "server"},
		{name: "missing server", kind: KindSMB, mod: func(t *Target) { t.Server = "" }, field: "server"},

		{name: "share slash", kind: KindSMB, mod: func(t *Target) { t.Share = "a/b" }, field: "share"},
		{name: "share newline", kind: KindSMB, mod: func(t *Target) { t.Share = "pic\nache" }, field: "share"},
		{name: "share dotdot", kind: KindSMB, mod: func(t *Target) { t.Share = ".." }, field: "share"},
		{name: "share percent", kind: KindSMB, mod: func(t *Target) { t.Share = "a%i" }, field: "share"},
		{name: "share too long", kind: KindSMB, mod: func(t *Target) { t.Share = strings.Repeat("a", 81) }, field: "share"},
		{name: "username injection", kind: KindSMB, mod: func(t *Target) { t.Username = "u\npassword=x" }, field: "username"},
		{name: "username comma", kind: KindSMB, mod: func(t *Target) { t.Username = "u,uid=0" }, field: "username"},
		{name: "domain space", kind: KindSMB, mod: func(t *Target) { t.Domain = "WORK GROUP" }, field: "domain"},
		{name: "smb version", kind: KindSMB, mod: func(t *Target) { t.SMBVersion = "2.1" }, field: "smbVersion"},
		{name: "smb not mountpoint", kind: KindSMB, mod: func(t *Target) { t.RequireMountpoint = false }, field: "requireMountpoint"},
		{name: "smb with export", kind: KindSMB, mod: func(t *Target) { t.Export = "/x" }, field: "export"},
		{name: "password without user", kind: KindSMB, mod: func(t *Target) { t.Username = "" }, field: "password"},

		{name: "export traversal", kind: KindNFS, mod: func(t *Target) { t.Export = "/volume1/../etc" }, field: "export"},
		{name: "export relative", kind: KindNFS, mod: func(t *Target) { t.Export = "volume1" }, field: "export"},
		{name: "export newline", kind: KindNFS, mod: func(t *Target) { t.Export = "/v\n1" }, field: "export"},
		{name: "export comma", kind: KindNFS, mod: func(t *Target) { t.Export = "/v,rw" }, field: "export"},
		{name: "nfs version", kind: KindNFS, mod: func(t *Target) { t.NFSVersion = "4.3" }, field: "nfsVersion"},
		{name: "nconnect 17", kind: KindNFS, mod: func(t *Target) { t.NFSNConnect = 17 }, field: "nfsNconnect"},
		{name: "nconnect 0", kind: KindNFS, mod: func(t *Target) { t.NFSNConnect = 0 }, field: "nfsNconnect"},
		{name: "nfs password", kind: KindNFS, mod: func(t *Target) { t.HasPassword = true }, field: "share"},

		{name: "local with server", kind: KindLocal, mod: func(t *Target) { t.Server = "192.168.1.10" }, field: "server"},
		{name: "local host-apply", kind: KindLocal, mod: func(t *Target) { t.Mode, t.Path = ModeHostApply, filepath.Join(mr, testID) }, field: "mode"},
		{name: "unknown kind", kind: KindSMB, mod: func(t *Target) { t.Kind = "iscsi" }, field: "kind"},
		{name: "unknown mode", kind: KindSMB, mod: func(t *Target) { t.Mode = "in-process" }, field: "mode"},

		{name: "subdir traversal", kind: KindSMB, mod: func(t *Target) { t.Subdir = "../../etc" }, field: "subdir"},
		{name: "subdir absolute", kind: KindSMB, mod: func(t *Target) { t.Subdir = "/etc" }, field: "subdir"},
		{name: "subdir dot", kind: KindSMB, mod: func(t *Target) { t.Subdir = "a/./b" }, field: "subdir"},
		{name: "subdir backslash", kind: KindSMB, mod: func(t *Target) { t.Subdir = `a\..\..\b` }, field: "subdir"},
		{name: "subdir newline", kind: KindSMB, mod: func(t *Target) { t.Subdir = "a\nb" }, field: "subdir"},

		{name: "path outside mount root", kind: KindSMB, mod: func(t *Target) { t.Path = filepath.Join(filepath.Dir(mr), "etc") }, field: "path"},
		{name: "path is mount root", kind: KindSMB, mod: func(t *Target) { t.Path = mr }, field: "path"},
		{name: "path traversal", kind: KindSMB, mod: func(t *Target) { t.Path = mr + string(filepath.Separator) + ".." + string(filepath.Separator) + "etc" }, field: "path"},
		{name: "path relative", kind: KindLocal, mod: func(t *Target) { t.Path = "nas" }, field: "path"},
		{name: "path empty", kind: KindLocal, mod: func(t *Target) { t.Path = "" }, field: "path"},
		{name: "path newline", kind: KindSMB, mod: func(t *Target) { t.Path = filepath.Join(mr, "a\nb") }, field: "path"},
		{name: "path space", kind: KindSMB, mod: func(t *Target) { t.Path = filepath.Join(mr, "a b") }, field: "path"},
		{name: "path inside data dir", kind: KindLocal, cfg: func(c *config.Config) { c.DataDir = filepath.Join(mr, "nas") },
			mod: func(t *Target) { t.Path = filepath.Join(mr, "nas", "cache") }, field: "path"},
		{name: "path contains data dir", kind: KindLocal, cfg: func(c *config.Config) { c.DataDir = filepath.Join(mr, "nas", "data") }, field: "path"},
		{name: "path equals data dir", kind: KindLocal, cfg: func(c *config.Config) { c.DataDir = filepath.Join(mr, "nas") }, field: "path"},
		{name: "host-apply path from db", kind: KindSMB, mod: func(t *Target) { t.Mode = ModeHostApply }, field: "path"},
		{name: "builtin other path", kind: KindLocal, mod: func(t *Target) { t.ID = LocalTargetID }, field: "path"},
		{name: "builtin smb", kind: KindSMB, mod: func(t *Target) { t.ID, t.Path = LocalTargetID, cfg.CacheDir }, field: "kind"},
		{name: "relative mount root", kind: KindLocal, cfg: func(c *config.Config) { c.MountRoot = "mounts" }, field: "path"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := *cfg
			if tc.cfg != nil {
				tc.cfg(&c)
			}
			tg := validTarget(cfg, tc.kind)
			if tc.mod != nil {
				tc.mod(&tg)
			}
			err := ValidateTarget(tg, &c)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			ae, ok := apperr.As(err)
			if !ok || ae.Kind != apperr.KindInvalid {
				t.Fatalf("want invalid %s, got %v", tc.field, err)
			}
			if ae.Field != tc.field {
				t.Fatalf("want field %s, got %s (%v)", tc.field, ae.Field, err)
			}
		})
	}
	if err := ValidateTarget(validTarget(cfg, KindSMB), nil); err == nil {
		t.Fatal("nil config must fail")
	}
}

func TestValidatePassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"":                       true,
		"pa,ss w0rd!$%\"'":       true,
		"ünïcode":                true,
		"line\nusername=root":    false,
		"tab\tpw":                false,
		"nul\x00":                false,
		"\xff\xfe":               false,
		strings.Repeat("x", 257): false,
	} {
		if err := validatePassword(pw); (err == nil) != ok {
			t.Errorf("validatePassword(%q) = %v, want ok=%v", pw, err, ok)
		}
	}
}

func TestValidTargetID(t *testing.T) {
	for id, ok := range map[string]bool{
		"local": true, testID: true, "LOCAL": false, "": false, testID + "0": false, "..": false,
		"0123456789abcdef0123456789abcdeg": false,
	} {
		if ValidTargetID(id) != ok {
			t.Errorf("ValidTargetID(%q) != %v", id, ok)
		}
	}
}
