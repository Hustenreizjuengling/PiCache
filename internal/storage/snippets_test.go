package storage

import (
	"context"
	"encoding/json/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hustenreizjuengling/picache/internal/config"
)

func TestSystemdEscapePath(t *testing.T) {
	for in, want := range map[string]string{
		"/srv/picache/nas":       "srv-picache-nas",
		"/srv/picache/" + testID: "srv-picache-" + testID,
		"/":                      "-",
		"//srv//picache/":        "srv-picache",
		"/srv/pi-cache/a b":      `srv-pi\x2dcache-a\x20b`,
		"/.hidden/x":             `\x2ehidden-x`,
		"/srv/a.b/c_d:e":         "srv-a.b-c_d:e",
		"/srv/über":              `srv-\xc3\xbcber`,
		"/srv/pct%i":             `srv-pct\x25i`,
	} {
		if got := systemdEscapePath(in); got != want {
			t.Errorf("systemdEscapePath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := mountUnitName("/srv/picache/nas"); got != "srv-picache-nas.mount" {
		t.Errorf("mountUnitName = %q", got)
	}
}

func snippetConfig() *config.Config {
	return &config.Config{DataDir: "/var/lib/picache", CacheDir: "/var/cache/picache", MountRoot: "/srv/picache"}
}

func TestSnippetsSMB(t *testing.T) {
	cfg := snippetConfig()
	tg := Target{ID: testID, Name: "NAS", Kind: KindSMB, Mode: ModeExternal, Path: "/srv/picache/nas",
		Server: "192.168.1.10", Share: "cache$", Username: "picache", Domain: "WORKGROUP", HasPassword: true,
		SMBVersion: "3.1.1", RequireMountpoint: true}
	caps := Capabilities{UID: 999, GID: 998, UIDMapOffset: 100000, GIDMapOffset: 100000, Container: "lxc"}
	s := renderSnippets(tg, caps, cfg)

	cred := "/etc/picache/credentials/" + testID + ".cred"
	opts := "credentials=" + cred + ",vers=3.1.1,uid=999,gid=998,file_mode=0640,dir_mode=0750,soft," +
		"nosuid,nodev,noexec,noatime,_netdev,nofail,x-systemd.mount-timeout=30"
	mustContain(t, "credentials", s.CredentialsFile, cred, "username=picache\n", "password=<your NAS password>\n", "domain=WORKGROUP\n")
	if want := "//192.168.1.10/cache$  /srv/picache/nas  cifs  " + opts + "  0  0"; s.Fstab != want {
		t.Errorf("fstab:\n got %s\nwant %s", s.Fstab, want)
	}
	mustContain(t, "systemd", s.SystemdMount, "# /etc/systemd/system/srv-picache-nas.mount\n", "[Mount]\n",
		"What=//192.168.1.10/cache$\n", "Where=/srv/picache/nas\n", "Type=cifs\n", "Options="+opts+"\n",
		"TimeoutSec=30\n", "WantedBy=remote-fs.target\n", "After=network-online.target\n",
		"systemctl enable --now 'srv-picache-nas.mount'")
	mustContain(t, "compose", s.DockerCompose, "source: /srv/picache\n", "target: /srv/picache\n", "propagation: rslave\n")
	host := "/mnt/picache-" + testID
	mustContain(t, "proxmox", s.Proxmox, "pct set <CT> -mp0 "+host+",mp=/srv/picache/nas\n",
		"//192.168.1.10/cache$  "+host+"  cifs  credentials="+cred+",vers=3.1.1,uid=100999,gid=100998,file_mode=0640,dir_mode=0750,",
		"<<'EOF'", "install -d -m 0700 /etc/picache/credentials")
	if s.HostApply != "" {
		t.Errorf("external target must not show the host-apply command: %q", s.HostApply)
	}
	if len(s.Notes) == 0 {
		t.Error("notes missing")
	}

	// Host-apply, SMB3 encryption, Docker (host ids in fstab), guest access.
	tg.Mode, tg.Path, tg.SMBSeal, tg.Username, tg.Domain, tg.HasPassword = ModeHostApply, "/srv/picache/"+testID, true, "", "", false
	caps.Container = "docker"
	s = renderSnippets(tg, caps, cfg)
	if s.CredentialsFile != "" {
		t.Errorf("guest access needs no credentials file: %q", s.CredentialsFile)
	}
	mustContain(t, "fstab", s.Fstab, "cifs  guest,vers=3.1.1,uid=100999,gid=100998,", ",soft,seal,nosuid,")
	if s.HostApply != "sudo picache storage apply "+testID {
		t.Errorf("host-apply command %q", s.HostApply)
	}
}

func TestSnippetsNFS(t *testing.T) {
	cfg := snippetConfig()
	tg := Target{ID: testID, Name: "NFS", Kind: KindNFS, Mode: ModeExternal, Path: "/srv/picache/nfs",
		Server: "192.168.1.10", Export: "/volume1/picache", NFSVersion: "4.2", NFSNConnect: 4, RequireMountpoint: true}
	s := renderSnippets(tg, Capabilities{UID: 999, GID: 999}, cfg)
	want := "192.168.1.10:/volume1/picache  /srv/picache/nfs  nfs4  vers=4.2,proto=tcp,softerr,timeo=100,retrans=2,nconnect=4," +
		"nosuid,nodev,noexec,noatime,_netdev,nofail,x-systemd.mount-timeout=30  0  0"
	if s.Fstab != want {
		t.Errorf("fstab:\n got %s\nwant %s", s.Fstab, want)
	}
	if s.CredentialsFile != "" {
		t.Error("NFS has no credentials file")
	}
	mustContain(t, "systemd", s.SystemdMount, "Type=nfs4\n", "What=192.168.1.10:/volume1/picache\n")

	tg.Server, tg.NFSVersion, tg.NFSNConnect = "2001:db8::10", "3", 2
	s = renderSnippets(tg, Capabilities{}, cfg)
	mustContain(t, "fstab v3", s.Fstab, "[2001:db8::10]:/volume1/picache  /srv/picache/nfs  nfs  vers=3,", ",nconnect=2,nolock,")
}

func TestSnippetsLocal(t *testing.T) {
	cfg := snippetConfig()
	s := renderSnippets(Target{ID: LocalTargetID, Kind: KindLocal, Path: "/var/cache/picache"}, Capabilities{}, cfg)
	if s.Fstab != "" || len(s.Notes) != 1 {
		t.Errorf("built-in target: %+v", s)
	}
	s = renderSnippets(Target{ID: testID, Kind: KindLocal, Mode: ModeExternal, Path: "/srv/picache/disk"}, Capabilities{UID: 5, GID: 6}, cfg)
	mustContain(t, "local fstab", s.Fstab, "UUID=<disk UUID>  /srv/picache/disk  ext4")
	mustContain(t, "local notes", strings.Join(s.Notes, "\n"), "chown 5:6 /srv/picache/disk")
}

func TestYAMLString(t *testing.T) {
	for in, want := range map[string]string{
		"/srv/picache":   "/srv/picache",
		"/srv/$HOME x":   `"/srv/$$HOME x"`,
		"/srv/a\"b: c #": `"/srv/a\"b: c #"`,
	} {
		if got := yamlString(in); got != want {
			t.Errorf("yamlString(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestSecretsNeverReturned creates targets with a password and checks every
// output (target, listing, snippets, status) for the password and the
// sealed value.
func TestSecretsNeverReturned(t *testing.T) {
	cfg := testConfig(t)
	m, d, _ := newTestManager(t, cfg)
	const pw = "Tr0ub4dor&3,secret"
	ctx := context.Background()
	in := smbInput(ptr(pw))
	in.Mode, in.Path = ModeExternal, filepath.Join(cfg.MountRoot, "nas")
	tg, err := m.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !tg.HasPassword {
		t.Fatal("HasPassword not set")
	}
	var sealed string
	if err := d.R.QueryRow(`SELECT password_sealed FROM storage_targets WHERE id = ?`, tg.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "v1:") || strings.Contains(sealed, pw) {
		t.Fatalf("password not sealed: %q", sealed)
	}

	got, _ := m.Target(ctx, tg.ID)
	list, _ := m.Targets(ctx, LocalTargetID)
	snip, err := m.Snippets(ctx, tg.ID)
	if err != nil {
		t.Fatal(err)
	}
	test := m.Test(ctx, tg.ID)
	for name, v := range map[string]any{"create": tg, "target": got, "list": list, "snippets": snip, "test": test,
		"status": m.Status(tg.ID), "caps": m.Capabilities()} {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), pw) || strings.Contains(string(b), sealed) || strings.Contains(string(b), "Tr0ub4dor") {
			t.Errorf("%s leaks the password: %s", name, b)
		}
	}
	mustContain(t, "snippets", snip.CredentialsFile, passwordPlaceholder)
}

func mustContain(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s: missing %q in:\n%s", what, sub, s)
		}
	}
}
