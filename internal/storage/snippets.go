package storage

import (
	"encoding/json/v2"
	"fmt"
	"net/netip"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hustenreizjuengling/picache/internal/config"
)

// passwordPlaceholder stands in for the NAS password in every snippet.
const passwordPlaceholder = "<your NAS password>"

// mountParams are the host-side values of a mount.
type mountParams struct {
	uid, gid int64  // owner of the files on CIFS (the PiCache user as the host sees it)
	credPath string // SMB credentials file ("" = guest access)
}

// credentialsPath is where the root helper keeps the credentials of id
// (snippets are for Linux hosts: always slash-separated).
func credentialsPath(id string) string { return path.Join(CredentialsDir, id+".cred") }

// mountSource is the "what" of the mount: //ip/share or ip:/export.
func mountSource(t Target) string {
	if t.Kind == KindSMB {
		return "//" + t.Server + "/" + t.Share
	}
	host := t.Server
	if a, err := netip.ParseAddr(host); err == nil && a.Is6() {
		host = "[" + host + "]"
	}
	return host + ":" + t.Export
}

// mountFSType is the fstab/.mount type.
func mountFSType(t Target) string {
	switch {
	case t.Kind == KindSMB:
		return "cifs"
	case t.NFSVersion == "3":
		return "nfs"
	}
	return "nfs4"
}

// mountOptions are the options for fstab and .mount units (never the
// password). CIFS parses file_mode/dir_mode with base 0: the leading zero
// makes them octal.
func mountOptions(t Target, p mountParams) string {
	var o []string
	switch t.Kind {
	case KindSMB:
		if p.credPath != "" {
			o = append(o, "credentials="+p.credPath)
		} else {
			o = append(o, "guest")
		}
		o = append(o, "vers="+t.SMBVersion, fmt.Sprintf("uid=%d", p.uid), fmt.Sprintf("gid=%d", p.gid),
			"file_mode=0640", "dir_mode=0750", "soft")
		if t.SMBSeal {
			o = append(o, "seal")
		}
	case KindNFS:
		o = append(o, "vers="+t.NFSVersion, "proto=tcp", "softerr", "timeo=100", "retrans=2",
			fmt.Sprintf("nconnect=%d", t.NFSNConnect))
		if t.NFSVersion == "3" {
			o = append(o, "nolock") // PiCache needs no NFS locks; v3 locking would need rpc.statd
		}
	}
	return strings.Join(append(o, "nosuid", "nodev", "noexec", "noatime", "_netdev", "nofail",
		"x-systemd.mount-timeout=30"), ",")
}

// renderMountUnit renders the systemd .mount unit for t mounted at where.
func renderMountUnit(t Target, where string, p mountParams) string {
	return fmt.Sprintf(`[Unit]
Description=PiCache cache store %s
Wants=network-online.target
After=network-online.target

[Mount]
What=%s
Where=%s
Type=%s
Options=%s
TimeoutSec=30

[Install]
WantedBy=remote-fs.target
`, t.ID, mountSource(t), where, mountFSType(t), mountOptions(t, p))
}

// systemdEscapePath is `systemd-escape --path`: "/srv/picache/nas" →
// "srv-picache-nas". "/" becomes "-"; bytes other than ASCII letters, digits,
// ':', '_' and '.' (and a leading '.') become \xNN.
func systemdEscapePath(p string) string {
	p = strings.Trim(path.Clean("/"+filepath.ToSlash(p)), "/")
	if p == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && i == 0, !unitNameChar(c):
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func unitNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == ':' || c == '_' || c == '.'
}

// mountUnitName is the unit file name systemd requires for a mount point.
func mountUnitName(where string) string {
	return systemdEscapePath(strings.TrimPrefix(where, filepath.VolumeName(where))) + ".mount"
}

var yamlPlainRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// yamlString quotes s for docker-compose ($ is interpolated by compose).
func yamlString(s string) string {
	if yamlPlainRE.MatchString(s) {
		return s
	}
	b, _ := json.Marshal(strings.ReplaceAll(s, "$", "$$"))
	return string(b)
}

// renderSnippets renders the configuration fragments for t. The password is
// never included, only passwordPlaceholder.
func renderSnippets(t Target, c Capabilities, cfg *config.Config) Snippets {
	var s Snippets
	uid, gid := int64(c.UID), int64(c.GID)
	hostUID, hostGID := uid+c.UIDMapOffset, gid+c.GIDMapOffset
	docker := c.Container == "docker" || c.Container == "podman"
	// fstab and .mount run on the host: in Docker that is the container
	// user as the host sees it; on bare metal and in privileged LXC the offset is 0.
	mUID, mGID := uid, gid
	if docker {
		mUID, mGID = hostUID, hostGID
	}
	compose := fmt.Sprintf(`services:
  picache:
    volumes:
      - type: bind
        source: %s
        target: %s
        bind:
          propagation: rslave
`, yamlString(cfg.MountRoot), yamlString(cfg.MountRoot))
	proxmoxHost := "/mnt/picache-" + t.ID

	if t.Kind == KindLocal {
		if t.ID == LocalTargetID {
			s.Notes = []string{"The built-in store lives in PICACHE_CACHE_DIR (" + t.Path + "). There is nothing to mount."}
			return s
		}
		s.Fstab = fmt.Sprintf("UUID=<disk UUID>  %s  ext4  defaults,noatime,nofail  0  2", t.Path)
		s.DockerCompose = compose
		s.Proxmox = fmt.Sprintf("# On the Proxmox host: mount the disk at %s, then\npct set <CT> -mp0 %s,mp=%s\n", proxmoxHost, proxmoxHost, t.Path)
		s.Notes = []string{
			fmt.Sprintf("Mount the disk at %s (find the UUID with `blkid`) and make it writable for uid %d / gid %d: chown %d:%d %s",
				t.Path, mUID, mGID, mUID, mGID, t.Path),
			"XFS or ext4 (default inode ratio) suit a download cache; do not format ext4 with -T largefile.",
		}
		return s
	}

	credPath := ""
	if t.Kind == KindSMB && t.Username != "" {
		credPath = credentialsPath(t.ID)
		var b strings.Builder
		fmt.Fprintf(&b, "# %s (owner root, mode 0600)\nusername=%s\npassword=%s\n", credPath, t.Username, passwordPlaceholder)
		if t.Domain != "" {
			fmt.Fprintf(&b, "domain=%s\n", t.Domain)
		}
		s.CredentialsFile = b.String()
	}
	src, typ := mountSource(t), mountFSType(t)
	opts := mountOptions(t, mountParams{uid: mUID, gid: mGID, credPath: credPath})
	s.Fstab = fmt.Sprintf("%s  %s  %s  %s  0  0", src, t.Path, typ, opts)
	unit := mountUnitName(t.Path)
	s.SystemdMount = "# /etc/systemd/system/" + unit + "\n" +
		renderMountUnit(t, t.Path, mountParams{uid: mUID, gid: mGID, credPath: credPath}) +
		"# then: systemctl daemon-reload && systemctl enable --now '" + unit + "'\n"
	s.DockerCompose = compose

	var px strings.Builder
	px.WriteString("# On the Proxmox host (as root):\n")
	fmt.Fprintf(&px, "install -d -m 0750 %s\n", proxmoxHost)
	if credPath != "" {
		fmt.Fprintf(&px, "install -d -m 0700 %s\n# create %s (mode 0600) as shown in the credentials file\n", CredentialsDir, credPath)
	}
	pxOpts := mountOptions(t, mountParams{uid: hostUID, gid: hostGID, credPath: credPath})
	fmt.Fprintf(&px, "cat >> /etc/fstab <<'EOF'\n%s  %s  %s  %s  0  0\nEOF\n", src, proxmoxHost, typ, pxOpts)
	fmt.Fprintf(&px, "systemctl daemon-reload\nmount %s\npct set <CT> -mp0 %s,mp=%s\n", proxmoxHost, proxmoxHost, t.Path)
	s.Proxmox = px.String()

	if t.Mode == ModeHostApply {
		s.HostApply = "sudo picache storage apply " + t.ID
	}

	pkg := "cifs-utils"
	if t.Kind == KindNFS {
		pkg = "nfs-common"
	}
	s.Notes = append(s.Notes,
		"Use the NAS's IP address: the kernel does not resolve host names when mounting, and PiCache may be the DNS server itself.",
		"Install "+pkg+" on the machine that mounts the share (apt install "+pkg+").",
		"Keep the mount below "+cfg.MountRoot+" (PICACHE_MOUNT_ROOT): it is the only NAS location PiCache writes to.",
	)
	if credPath != "" {
		s.Notes = append(s.Notes, "The NAS password is never shown here: replace "+passwordPlaceholder+" in the credentials file.")
	}
	if t.Kind == KindNFS {
		s.Notes = append(s.Notes, fmt.Sprintf("NFS permissions are checked on the NAS: give uid %d / gid %d write access "+
			"(owner of the export, or all_squash,anonuid=%d,anongid=%d). In an unprivileged Proxmox container use uid %d / gid %d.",
			mUID, mGID, mUID, mGID, hostUID, hostGID))
	}
	if docker {
		s.Notes = append(s.Notes, "Docker: mount the share on the host below "+cfg.MountRoot+
			" and bind that directory into the container with propagation rslave, so mounts made after the container started become visible.")
	}
	s.Notes = append(s.Notes, fmt.Sprintf("Proxmox (unprivileged container): the host mounts the share and passes it in. "+
		"Host ids = container ids + %d (from /proc/self/uid_map), so PiCache's files belong to uid %d / gid %d on the host. "+
		"Replace <CT> with the container id and use the next free mpN.", c.UIDMapOffset, hostUID, hostGID))
	if t.Mode == ModeHostApply {
		s.Notes = append(s.Notes, "Host-apply: the root helper writes "+credentialsPath(t.ID)+" and "+
			hostApplyUnitPath(t.Path)+", then starts the mount. Without the helper run the command above; add --password-stdin to type the password instead of using the stored one.")
	}
	return s
}

// hostApplyUnitPath is where the root helper writes the unit for where.
func hostApplyUnitPath(where string) string { return "/etc/systemd/system/" + mountUnitName(where) }
