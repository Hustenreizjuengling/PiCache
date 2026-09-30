#!/bin/sh
# PiCache installer for Linux with systemd 247 or later (bare metal, VM,
# Proxmox LXC). Supported: Debian 12/13, Ubuntu 22.04+, Fedora, RHEL/Alma/
# Rocky 9+, Arch, openSUSE Tumbleweed and Leap 16; other distributions get a
# warning and continue. Hosts without systemd (OpenRC, …) are not supported.
#
#   sudo sh deploy/install.sh --binary ./picache-linux-amd64 [--with-host-apply] [--without-updater]
#                             [--with-dhcp | --without-dhcp] [--nightly]
#   sudo sh deploy/install.sh --uninstall [--purge [--yes]]
#
# Idempotent: run it again with a newer binary to upgrade. It installs only
# the local files you give it (it never downloads anything), never runs a
# package manager and never changes systemd-resolved, the firewall or any
# other DNS service; it prints the commands instead.
#
# Constants and functions are defined at the top level and everything else
# happens in main, called on the last line: the maintainer scripts of the
# Debian package are this file without its first and last line plus
# deploy/debian/<name>.sh (make dist), so they share these functions.
set -eu

# ME names this script in error messages (the maintainer scripts set their
# own name).
ME=install.sh
BIN=/usr/local/bin/picache
UNIT_DIR=/usr/local/lib/systemd/system
DOC_DIR=/usr/share/doc/picache
CONF_DIR=/etc/picache
ENV_FILE=$CONF_DIR/picache.env
CRED_DIR=$CONF_DIR/credentials
HOST_APPLY_MARKER=$CONF_DIR/host-apply.enabled
UPDATER_MARKER=$CONF_DIR/updater.enabled
# The update helper installs nightly builds only while this root-owned file
# exists (--nightly; removed by --uninstall).
NIGHTLY_MARKER=$CONF_DIR/nightly.enabled
# Marker and unit drop-in of --with-dhcp before 0.8.0 (the base unit carries
# the drop-in's content now); every run removes them.
OLD_DHCP_MARKER=$CONF_DIR/dhcp.enabled
OLD_DHCP_DROPIN_DIR=/etc/systemd/system/picache.service.d
OLD_DHCP_DROPIN=60-dhcp.conf
DEFAULT_DATA_DIR=/var/lib/picache
DEFAULT_CACHE_DIR=/var/cache/picache
DEFAULT_MOUNT_ROOT=/srv/picache
# The log directory of picache.service (LogsDirectory=picache, where
# PICACHE_LOG_FILE may write); the purge deletes it like the defaults above.
DEFAULT_LOG_DIR=/var/log/picache
# PICACHE_DATA_DIR / PICACHE_MOUNT_ROOT from the env file (read_paths).
DATA_DIR=$DEFAULT_DATA_DIR
MOUNT_ROOT=$DEFAULT_MOUNT_ROOT
# Drop-in install.sh writes for the helper units when those paths differ.
PATHS_DROPIN=50-picache-paths.conf
SHARED_MOUNTS_UNIT=picache-shared-mounts.service
DEPLOY_SRC=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
UNIT_SRC=$DEPLOY_SRC/systemd
# License texts that go with every copy of the binary, taken from the source
# tree that contains deploy/.
DOC_SRC=$(dirname -- "$DEPLOY_SRC")
DOC_FILES="LICENSE THIRD_PARTY_NOTICES.md"
# The marker of the Debian package (update.PackageMarker): install.sh
# refuses to install over the package or to remove it.
PACKAGE_MARKER=/usr/lib/picache/packaged
# The oldest systemd whose units PiCache supports (ProtectProc=invisible).
MIN_SYSTEMD=247
SUPPORTED_OS="Debian 12+, Ubuntu 22.04+, RHEL/Alma/Rocky 9+, Fedora, Arch, openSUSE Tumbleweed and Leap 16"

say() { printf '%s\n' "$*"; }
warn() { printf '\nWARNING: %s\n' "$*" >&2; }
die() {
	printf '%s: error: %s\n' "$ME" "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
usage: install.sh --binary PATH [--with-host-apply] [--without-updater]
                  [--with-dhcp | --without-dhcp] [--nightly]
       install.sh --uninstall [--purge [--yes]]

  --binary PATH       the picache binary to install (for example the
                      picache-linux-arm64 file from `make build-all`)
  --with-host-apply   also install the root helper that lets the web UI
                      mount SMB/NFS shares (bare metal, VMs, privileged LXC)
  --without-updater   do not install (or remove) the root helper that installs
                      updates queued in the web UI; `sudo picache update`
                      keeps working
  --with-dhcp         allow PiCache's DHCP server again after --without-dhcp
                      (removes PICACHE_DHCP=off); it is switched on in the web
                      UI (DNS -> DHCP), which is possible by default
  --without-dhcp      prevent it (PICACHE_DHCP=off): the DHCP server can then
                      not be switched on in the web UI (hosts that run another
                      DHCP server)
  --nightly           allow the update helper to install nightly builds (the
                      update channel "nightly" in the web UI; untested builds
                      of main). Stays until --uninstall or
                      rm /etc/picache/nightly.enabled
  --uninstall         stop and remove PiCache; configuration and data are kept
  --purge             with --uninstall: also unmount the NAS shares of the
                      web UI and delete the configuration, the data, the
                      local cache and the picache user (default paths only)
  --yes               do not ask before --purge

The update helper (picache-update.path) is installed by default. A binary
of a release older than the installed one is refused unless
PICACHE_ALLOW_DOWNGRADE=1 is set (stop PiCache and put the database copy
made before the upgrade back first, docs/DEPLOYMENT.md "Going back to an
earlier version").
EOF
}

# --- helpers ---------------------------------------------------------------

# os_release KEY prints a value from /etc/os-release without sourcing it.
os_release() {
	sed -n "s/^$1=//p" /etc/os-release 2>/dev/null | tr -d "\"'" | head -n 1
}

# os_family prints debian, fedora, arch, suse or unknown (ID and ID_LIKE of
# /etc/os-release): the package names of the hints.
os_family() {
	case " $(os_release ID) $(os_release ID_LIKE) " in
	*" debian "* | *" ubuntu "*) echo debian ;;
	*" fedora "* | *" rhel "* | *" centos "*) echo fedora ;;
	*" arch "*) echo arch ;;
	*" suse "* | *" opensuse "* | *" sles "*) echo suse ;;
	*) echo unknown ;;
	esac
}

# major_at_least VERSION MIN succeeds when the major number of VERSION
# (the digits before the first ".") is at least MIN.
major_at_least() {
	major=${1%%.*}
	case $major in '' | *[!0-9]*) return 1 ;; esac
	[ "$major" -ge "$2" ]
}

# check_os warns (and continues) on distributions other than the supported
# ones.
check_os() {
	id=$(os_release ID)
	ver=$(os_release VERSION_ID)
	case $id in
	# Raspberry Pi OS (32-bit) calls itself raspbian; it is Debian.
	debian | raspbian) case $ver in 12 | 13) return 0 ;; esac ;;
	ubuntu) if major_at_least "$ver" 22; then return 0; fi ;;
	fedora | arch | opensuse-tumbleweed) return 0 ;;
	rhel | almalinux | rocky) if major_at_least "$ver" 9; then return 0; fi ;;
	opensuse-leap) if major_at_least "$ver" 16; then return 0; fi ;;
	esac
	warn "${id:-this distribution} ${ver} is untested; supported are $SUPPORTED_OS; continuing"
}

# check_systemd dies unless systemd is 247 or later: older versions silently
# ignore directives of the units (ProtectProc= and others).
check_systemd() {
	line=$(systemctl --version 2>/dev/null | head -n 1) || line=""
	v=""
	case $line in
	"systemd "*)
		v=${line#systemd }
		v=${v%%[!0-9]*}
		;;
	esac
	if [ -z "$v" ]; then
		die "cannot read the systemd version (\"$line\"): PiCache's sandbox needs systemd $MIN_SYSTEMD or later (ProtectProc=invisible). Supported: $SUPPORTED_OS"
	fi
	if [ "$v" -lt "$MIN_SYSTEMD" ]; then
		die "systemd $v is too old: PiCache's sandbox needs systemd $MIN_SYSTEMD or later (ProtectProc=invisible); older versions silently ignore some protections. Supported: $SUPPORTED_OS"
	fi
}

# check_not_packaged dies when the Debian package installed PiCache here
# (dpkg knows it in a state other than not-installed or config-files, or
# its marker exists): install.sh must not install over it or remove it.
check_not_packaged() {
	st=""
	if command -v dpkg-query >/dev/null 2>&1; then
		st=$(dpkg-query -W -f='${Status}' picache 2>/dev/null) || st=""
	fi
	case $st in
	"" | *" not-installed" | *" config-files") [ ! -e "$PACKAGE_MARKER" ] && [ ! -L "$PACKAGE_MARKER" ] && return 0 ;;
	esac
	die "PiCache is installed as a Debian package here: update it with apt (docs/DEPLOYMENT.md \"Debian package\") or remove it with apt remove picache"
}

# selinux_relabel gives the installed files their default SELinux labels
# (restorecon; never chcon, setenforce or a permissive domain) while SELinux
# is enabled: the binary, the configuration directory, the unit directory
# itself and PiCache's units in it (other units there are left alone).
selinux_relabel() {
	command -v selinuxenabled >/dev/null 2>&1 || return 0
	selinuxenabled || return 0
	if ! command -v restorecon >/dev/null 2>&1; then
		warn "SELinux is enabled but restorecon is missing: relabel $BIN, $CONF_DIR and $UNIT_DIR/picache* yourself"
		return 0
	fi
	units=""
	for f in "$UNIT_DIR"/picache*.service "$UNIT_DIR"/picache*.path; do
		[ -e "$f" ] && units="$units $f"
	done
	# shellcheck disable=SC2086 # word splitting is intended (no spaces in these paths)
	restorecon -RF "$BIN" "$CONF_DIR" $units || warn "restorecon failed; check the labels with ls -Z"
	restorecon -F "$UNIT_DIR" || true
	say "SELinux: restored the default labels of $BIN, $CONF_DIR and the units"
}

is_unprivileged_container() {
	[ -r /proc/self/uid_map ] &&
		awk 'NR == 1 && $2 != 0 { found = 1 } END { exit !found }' /proc/self/uid_map
}

install_unit() { install -m 0644 -o root -g root "$UNIT_SRC/$1" "$UNIT_DIR/$1"; }

# login_def KEY DEFAULT prints a numeric value from /etc/login.defs.
login_def() {
	v=$(awk -v k="$1" '$1 == k && $2 ~ /^[0-9]+$/ { v = $2 } END { print v }' /etc/login.defs 2>/dev/null) || v=""
	printf '%s' "${v:-$2}"
}

# sys_id_max UID|GID prints the highest system UID or GID the way
# useradd/groupadd compute it: SYS_UID_MAX, else UID_MIN - 1 (same for GID).
sys_id_max() {
	min=$(login_def "$1_MIN" 1000)
	login_def "SYS_$1_MAX" "$((min - 1))"
}

# check_user refuses existing accounts named picache that the service must
# not use (it sets entry and gid for ensure_user). The service owns the
# database and the master key, so it must never run as an account that
# people log in with (for example a user named picache created by the
# distribution's installer).
check_user() {
	entry=$(getent passwd picache) || entry=""
	if [ -n "$entry" ]; then
		uid=$(printf '%s' "$entry" | cut -d: -f3)
		shell=$(printf '%s' "$entry" | cut -d: -f7)
		if [ "$uid" -eq 0 ] || [ "$uid" -gt "$(sys_id_max UID)" ]; then
			die "a login account named picache exists (uid $uid).
The service needs its own system account of that name, which owns the
database and the master key and must not be used to log in. Rename the login
account from another session (root on the console or another admin account;
end its sessions first: loginctl terminate-user picache), for example:
    usermod -l NEWNAME -d /home/NEWNAME -m picache && groupmod -n NEWNAME picache
then run install.sh again."
		fi
		case $shell in
		*/nologin | */false) ;;
		*)
			die "the system account picache has the login shell '${shell:-/bin/sh}'.
The service account must not be usable for logins. Fix it with:
    usermod -s /usr/sbin/nologin picache && passwd -l picache
then run install.sh again."
			;;
		esac
	fi
	gid=$(getent group picache | cut -d: -f3)
	if [ -n "$gid" ] && [ "$gid" -gt "$(sys_id_max GID)" ]; then
		die "a group named picache exists that is not a system group (gid $gid).
Rename it (groupmod -n NEWNAME picache) or remove it, then run install.sh again."
	fi
}

# ensure_user creates the system user and group picache or checks existing
# ones (check_user).
ensure_user() {
	check_user
	if [ -z "$gid" ]; then
		groupadd --system picache
	fi
	if [ -z "$entry" ]; then
		useradd --system --gid picache --home-dir "$DATA_DIR" --no-create-home \
			--shell /usr/sbin/nologin --comment "PiCache" picache
		say "created system user picache"
	fi
}

install_binary() {
	[ -f "$binary" ] || die "binary not found: $binary"
	tmp=$(dirname "$BIN")/.picache.new
	rm -f "$tmp"
	install -m 0755 -o root -g root "$binary" "$tmp"
	if ! out=$("$tmp" version 2>&1); then
		rm -f "$tmp"
		die "$binary does not run on this machine ($(uname -m)): $out"
	fi
	case $out in
	"picache "*) ;;
	*)
		rm -f "$tmp"
		die "$binary is not a PiCache binary"
		;;
	esac
	check_downgrade "$out"
	mv -f "$tmp" "$BIN"
	say "installed $BIN: $out"
}

# release_version OUTPUT prints the version in the output of `picache
# version` ("picache v1.2.3 (commit …)") when it is a release version
# (vX.Y.Z or vX.Y.Z-pre), else nothing: development builds are never
# compared.
release_version() {
	rv=$(printf '%s\n' "$1" | sed -n '1s/^picache \(v[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\(-[0-9A-Za-z.]*\)\{0,1\}\) .*/\1/p')
	case $rv in *-dirty) rv="" ;; esac
	printf '%s' "$rv"
}

# semver_lt A B succeeds when the release version A sorts below B by SemVer
# precedence (as `picache update` compares them: numeric identifiers
# numerically and below alphanumeric ones, a release above its
# pre-releases).
semver_lt() {
	awk -v a="$1" -v b="$2" '
	function cmpid(x, y) {
		if (x ~ /^[0-9]+$/ && y ~ /^[0-9]+$/) {
			if (length(x) != length(y)) return length(x) < length(y) ? -1 : 1
		} else if (x ~ /^[0-9]+$/) {
			return -1
		} else if (y ~ /^[0-9]+$/) {
			return 1
		}
		if (x < y) return -1
		if (x > y) return 1
		return 0
	}
	function cmp(x, y,   i, c, n, m, xp, yp, xc, yc, xi, yi) {
		sub(/^v/, "", x)
		sub(/^v/, "", y)
		xp = ""
		yp = ""
		if ((i = index(x, "-")) > 0) { xp = substr(x, i + 1); x = substr(x, 1, i - 1) }
		if ((i = index(y, "-")) > 0) { yp = substr(y, i + 1); y = substr(y, 1, i - 1) }
		split(x, xc, ".")
		split(y, yc, ".")
		for (i = 1; i <= 3; i++) if ((c = cmpid(xc[i], yc[i])) != 0) return c
		if (xp == "" && yp == "") return 0
		if (xp == "") return 1
		if (yp == "") return -1
		n = split(xp, xi, ".")
		m = split(yp, yi, ".")
		for (i = 1; i <= n && i <= m; i++) if ((c = cmpid(xi[i], yi[i])) != 0) return c
		return n < m ? -1 : n > m ? 1 : 0
	}
	BEGIN { exit !(cmp(a, b) < 0) }'
}

# check_downgrade NEWOUT dies (removing the staged $tmp) when the binary
# being installed (its `version` output NEWOUT) is older than the installed
# $BIN, unless PICACHE_ALLOW_DOWNGRADE=1 (the name the Debian package uses):
# the installed version may have migrated the database, which the older one
# refuses, so PiCache would not start and the network would lose DNS. The
# message puts the database copy back before the older version starts (a
# version before 1.0.0 started on a newer database copies it, prunes the
# copies and records its own version) and repeats the options of this run.
check_downgrade() {
	[ "${PICACHE_ALLOW_DOWNGRADE:-}" != 1 ] || return 0
	[ -x "$BIN" ] || return 0
	new_version=$(release_version "$1")
	installed_version=$(release_version "$("$BIN" version 2>/dev/null)")
	[ -n "$new_version" ] && [ -n "$installed_version" ] || return 0
	semver_lt "$new_version" "$installed_version" || return 0
	rm -f "$tmp"
	data=$(env_path PICACHE_DATA_DIR "$DEFAULT_DATA_DIR")
	die "picache $new_version is older than the installed $installed_version. $installed_version may have migrated the database, which $new_version then refuses: PiCache would not start. To go back anyway (docs/DEPLOYMENT.md \"Going back to an earlier version\"): unless the upgrade notes say that $new_version opens this database, first stop PiCache (sudo systemctl stop picache) and put the copy made before the upgrade ($data/backups/picache-$new_version-*.db) in place as $data/picache.db, deleting picache.db-wal and picache.db-shm; then run: sudo PICACHE_ALLOW_DOWNGRADE=1 sh $0 --binary $binary$(given_options)"
}

# given_options prints the options of this run for a command in a message
# (each with a leading space): --without-updater is not kept between runs,
# so a run without it would install the update helper again.
given_options() {
	[ "$with_host_apply" -eq 0 ] || printf ' --with-host-apply'
	[ "$without_updater" -eq 0 ] || printf ' --without-updater'
	[ "$with_dhcp" -eq 0 ] || printf ' --with-dhcp'
	[ "$without_dhcp" -eq 0 ] || printf ' --without-dhcp'
	[ "$nightly" -eq 0 ] || printf ' --nightly'
}

# install_docs copies the license texts to $DOC_DIR. A missing file is only
# reported: PiCache runs without it. Symbolic links are not followed, so a
# link in the source tree cannot turn another file into a world-readable copy.
install_docs() {
	absent=""
	for f in $DOC_FILES; do
		if [ -f "$DOC_SRC/$f" ] && [ ! -L "$DOC_SRC/$f" ]; then
			install -d -m 0755 -o root -g root "$DOC_DIR"
			install -m 0644 -o root -g root "$DOC_SRC/$f" "$DOC_DIR/$f"
		else
			absent="$absent $f"
		fi
	done
	if [ -n "$absent" ]; then
		warn "not found in $DOC_SRC:$absent
Copies of PiCache must carry their license texts. Put LICENSE and
THIRD_PARTY_NOTICES.md from the PiCache source tree next to deploy/ and run
install.sh again; they are installed to $DOC_DIR."
	else
		say "installed the license texts to $DOC_DIR"
	fi
}

env_template() {
	cat <<'EOF'
# PiCache bootstrap configuration.
#
# Read by systemd (EnvironmentFile=) and by every `picache` CLI command.
# Everything else (DNS, filtering, download cache, logs, web) is configured
# in the web UI. Apply changes with: systemctl restart picache
# Format: KEY=value, one per line. Listener values are comma-separated
# host:port lists; "off" disables a listener. All variables are described in
# docs/DEPLOYMENT.md ("Environment variables").

# DNS (mandatory). If port 53 is shared with another service, bind specific
# addresses, for example: PICACHE_DNS_LISTEN=192.168.1.5:53,127.0.0.1:53
#PICACHE_DNS_LISTEN=:53
# Download cache over HTTP and HTTPS (SNI) pass-through.
#PICACHE_CACHE_LISTEN=:80
#PICACHE_SNI_LISTEN=:443
# Web UI and API over HTTP and HTTPS (a certificate of PiCache's local CA unless
# a certificate is set).
#PICACHE_WEB_LISTEN=:8080
#PICACHE_WEB_TLS_LISTEN=:8443
# DNS over TLS for devices (serves while DoT is switched on in the web UI).
#PICACHE_DOT_LISTEN=:853
# DNS over HTTPS on an own port (DoH is served on PICACHE_WEB_TLS_LISTEN too).
#PICACHE_DOH_LISTEN=off
# Own certificate and key (PEM), readable by the picache group (0640 root:picache).
#PICACHE_WEB_TLS_CERT=/etc/picache/tls/cert.pem
#PICACHE_WEB_TLS_KEY=/etc/picache/tls/key.pem
# Extra host names for the web UI (IP addresses and localhost always work).
#PICACHE_WEB_HOSTS=picache.lan

# NTP server for the network (answers while it is switched on in the web UI).
#PICACHE_NTP_LISTEN=:123

# Logging: debug | info | warn | error, and text | json.
#PICACHE_LOG_LEVEL=info
#PICACHE_LOG_FORMAT=text
# Also write the log to a file (rotated at 10 MiB, 5 compressed generations;
# on an SD card this adds writes) or send it to a syslog server (plain text).
#PICACHE_LOG_FILE=/var/log/picache/picache.log
#PICACHE_LOG_SYSLOG=udp://192.168.1.10:514
# Go profiles on /debug/pprof/ for admins on this machine (on | off).
#PICACHE_PPROF=off

# Optional: a settings document (JSON, like `picache config get`) applied
# once on the first start. Readable by the picache group only (0640).
#PICACHE_INITIAL_CONFIG=/etc/picache/initial-config.json

# Optional: the proxy the update helper and `sudo picache update` download
# releases through (http://host:port or socks5://host:port, no user name).
#PICACHE_UPDATE_PROXY=http://192.168.1.10:3128

# Optional: create the first admin at start instead of using the setup token.
# The file (at least 10 characters) must be readable by the picache group.
# Remove this line and the file after the first start.
#PICACHE_ADMIN_USER=admin
#PICACHE_ADMIN_PASSWORD_FILE=/etc/picache/admin-password

# DHCP server: switched on in the web UI (DNS -> DHCP); PiCache holds no
# DHCP port while it is off. PICACHE_DHCP=off prevents switching it on (for
# hosts that run another DHCP server; install.sh --without-dhcp sets it).
#PICACHE_DHCP=off

# Paths. The systemd unit only allows writes to these defaults; change them
# only together with a matching drop-in (systemctl edit picache).
#PICACHE_DATA_DIR=/var/lib/picache
#PICACHE_CACHE_DIR=/var/cache/picache
#PICACHE_MOUNT_ROOT=/srv/picache
#PICACHE_MASTER_KEY_FILE=/var/lib/picache/keys/master.key
EOF
}

write_env_file() {
	if [ -e "$ENV_FILE" ]; then
		chown root:picache "$ENV_FILE"
		chmod 0640 "$ENV_FILE"
		say "kept existing $ENV_FILE"
		return
	fi
	tmp=$ENV_FILE.new
	(
		umask 077
		env_template >"$tmp"
	)
	chown root:picache "$tmp"
	chmod 0640 "$tmp"
	mv -f "$tmp" "$ENV_FILE"
	say "created $ENV_FILE"
}

# env_value KEY prints the last uncommented value of KEY in the env file.
env_value() {
	[ -r "$ENV_FILE" ] || return 0
	sed -n "s/^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$1=//p" "$ENV_FILE" | tail -n 1
}

# env_path KEY DEFAULT prints a path setting from the env file (quotes and a
# trailing slash removed) or DEFAULT. The paths go into unit files, so only
# plain absolute paths are accepted.
env_path() {
	v=$(env_value "$1")
	case $v in
	\"*\") v=${v#\"}; v=${v%\"} ;;
	\'*\') v=${v#\'}; v=${v%\'} ;;
	esac
	[ "$v" = / ] || v=${v%/}
	[ -n "$v" ] || v=$2
	case $v in
	/*) ;;
	*) die "$1 in $ENV_FILE must be an absolute path (found '$v')" ;;
	esac
	case $v in
	*[!A-Za-z0-9._/-]*) die "$1=$v: install.sh supports only letters, digits and . _ / - in paths" ;;
	esac
	printf '%s' "$v"
}

read_paths() {
	DATA_DIR=$(env_path PICACHE_DATA_DIR "$DEFAULT_DATA_DIR")
	MOUNT_ROOT=$(env_path PICACHE_MOUNT_ROOT "$DEFAULT_MOUNT_ROOT")
}

# write_helper_dropins points the helper units at a custom data directory or
# mount root (the shipped units use the defaults), or removes the drop-ins.
write_helper_dropins() {
	path_dir=/etc/systemd/system/picache-storage.path.d
	svc_dir=/etc/systemd/system/picache-storage.service.d
	if [ "$DATA_DIR" = "$DEFAULT_DATA_DIR" ] && [ "$MOUNT_ROOT" = "$DEFAULT_MOUNT_ROOT" ]; then
		rm -f "$path_dir/$PATHS_DROPIN" "$svc_dir/$PATHS_DROPIN"
		rmdir "$path_dir" "$svc_dir" 2>/dev/null || true
		return
	fi
	id_glob='????????????????????????????????'
	install -d -m 0755 "$path_dir" "$svc_dir"
	cat >"$path_dir/$PATHS_DROPIN" <<EOF
# Written by install.sh for PICACHE_DATA_DIR=$DATA_DIR and rewritten on every
# run. The empty assignment drops the default paths.
[Path]
PathExistsGlob=
PathExistsGlob=$DATA_DIR/storage-requests/$id_glob
PathExistsGlob=$DATA_DIR/storage-requests/.$id_glob.claim
EOF
	cat >"$svc_dir/$PATHS_DROPIN" <<EOF
# Written by install.sh for PICACHE_DATA_DIR=$DATA_DIR and
# PICACHE_MOUNT_ROOT=$MOUNT_ROOT; rewritten on every run.
[Service]
ReadWritePaths=-$MOUNT_ROOT -$DATA_DIR/storage-requests
EOF
	say "host-apply: wrote drop-ins for PICACHE_DATA_DIR=$DATA_DIR, PICACHE_MOUNT_ROOT=$MOUNT_ROOT"
}

# setup_shared_mounts: inside a container systemd does not make / a shared
# mount, so NAS mounts the helper starts later never reach picache.service's
# own mount namespace. A small unit makes / shared before PiCache starts.
setup_shared_mounts() {
	command -v systemd-detect-virt >/dev/null 2>&1 || return 0
	systemd-detect-virt --container --quiet || return 0
	if [ -e "$UNIT_DIR/$SHARED_MOUNTS_UNIT" ]; then
		install_unit "$SHARED_MOUNTS_UNIT" # keep it up to date
		shared_mounts=1
		return 0
	fi
	case $(findmnt -no PROPAGATION / 2>/dev/null) in
	shared*) return 0 ;;
	esac
	install_unit "$SHARED_MOUNTS_UNIT"
	shared_mounts=1
	say "host-apply: / is not a shared mount in this container; installed $SHARED_MOUNTS_UNIT
    (mount --make-rshared / before PiCache starts, so that it sees NAS mounts made later)"
}

setup_host_apply() {
	if is_unprivileged_container; then
		warn "this is an unprivileged container: the kernel refuses SMB/NFS mounts here,
so the host-apply helper cannot work and is not installed. Mount the share on
the Proxmox host and bind-mount it below $MOUNT_ROOT (deploy/lxc/README.md)."
		return
	fi
	install_unit picache-storage.service
	install_unit picache-storage.path
	install -d -m 0700 -o root -g root "$CRED_DIR"
	install -m 0644 -o root -g root /dev/null "$HOST_APPLY_MARKER"
	write_helper_dropins
	setup_shared_mounts
	host_apply_active=1
	mount_helper_hint
	say "host-apply helper installed (picache-storage.path)"
}

# mount_helper_hint names the packages of missing mount programs with one
# command for the distribution family (it never runs a package manager).
mount_helper_hint() {
	family=$(os_family)
	case $family in
	debian) nfs="nfs-common" ;;
	suse) nfs="nfs-client" ;;
	*) nfs="nfs-utils" ;;
	esac
	missing=""
	command -v mount.cifs >/dev/null 2>&1 || missing="$missing cifs-utils"
	command -v mount.nfs >/dev/null 2>&1 || missing="$missing $nfs"
	[ -n "$missing" ] || return 0
	say "host-apply: mount helpers are missing; install them for the share types you use:"
	case $family in
	debian) say "    apt install$missing" ;;
	fedora) say "    dnf install$missing" ;;
	arch) say "    pacman -S --needed$missing" ;;
	suse) say "    zypper install$missing" ;;
	*) say "    the packages$missing (Debian/Ubuntu: cifs-utils nfs-common; Fedora/RHEL/Arch: cifs-utils nfs-utils; openSUSE: cifs-utils nfs-client)" ;;
	esac
	case $missing in
	*nfs*)
		say "  NFS 4 does not need the rpcbind service that comes with the NFS client; turn it off with:"
		say "    systemctl mask --now rpcbind.service rpcbind.socket"
		;;
	esac
}

# write_update_dropins points the update helper at a custom data directory
# (the shipped units use the default), or removes the drop-ins.
write_update_dropins() {
	path_dir=/etc/systemd/system/picache-update.path.d
	svc_dir=/etc/systemd/system/picache-update.service.d
	if [ "$DATA_DIR" = "$DEFAULT_DATA_DIR" ]; then
		rm -f "$path_dir/$PATHS_DROPIN" "$svc_dir/$PATHS_DROPIN"
		rmdir "$path_dir" "$svc_dir" 2>/dev/null || true
		return
	fi
	install -d -m 0755 "$path_dir" "$svc_dir"
	cat >"$path_dir/$PATHS_DROPIN" <<EOF
# Written by install.sh for PICACHE_DATA_DIR=$DATA_DIR and rewritten on every
# run. The empty assignment drops the default paths.
[Path]
PathExists=
PathExists=$DATA_DIR/update-requests/request
PathExists=$DATA_DIR/update-requests/.claim
EOF
	cat >"$svc_dir/$PATHS_DROPIN" <<EOF
# Written by install.sh for PICACHE_DATA_DIR=$DATA_DIR; rewritten on every run.
[Service]
ReadWritePaths=-$DATA_DIR
EOF
	say "updater: wrote drop-ins for PICACHE_DATA_DIR=$DATA_DIR"
}

# setup_updater installs the root helper that installs updates queued in
# the web UI: picache-update.path starts picache-update.service, which
# verifies and installs exactly the requested signed release.
setup_updater() {
	install_unit picache-update.service
	install_unit picache-update.path
	install -m 0644 -o root -g root /dev/null "$UPDATER_MARKER"
	write_update_dropins
	updater_active=1
	say "update helper installed (picache-update.path): updates can be installed from the web UI"
}

# remove_updater disables and deletes the update helper, its drop-ins and
# its marker (--without-updater, --uninstall).
remove_updater() {
	for unit in picache-update.path picache-update.service; do
		systemctl disable --now "$unit" >/dev/null 2>&1 || true
		rm -f "$UNIT_DIR/$unit"
	done
	for d in picache-update.path.d picache-update.service.d; do
		rm -f "/etc/systemd/system/$d/$PATHS_DROPIN"
		rmdir "/etc/systemd/system/$d" 2>/dev/null || true
	done
	rm -f "$UPDATER_MARKER"
}

# rewrite_env KEY [VALUE] removes the uncommented assignments of KEY from
# the env file and, with VALUE, appends KEY=VALUE; owner and mode are kept
# (root:picache 0640).
rewrite_env() {
	tmp=$ENV_FILE.new
	(
		umask 077
		grep -v "^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}$1=" "$ENV_FILE" >"$tmp" || true
		if [ $# -ge 2 ]; then
			printf '%s=%s\n' "$1" "$2" >>"$tmp"
		fi
	)
	chown root:picache "$tmp"
	chmod 0640 "$tmp"
	mv -f "$tmp" "$ENV_FILE"
}

# refresh_dhcp_comment replaces the DHCP comment of the 0.7.0 env template,
# which described PICACHE_DHCP=on as the install option that allows the DHCP
# server (and --without-dhcp as its removal), by the current one. Only the
# unchanged block is replaced; nothing else in the file changes.
refresh_dhcp_comment() {
	grep -qx '# DHCP server (DNS -> DHCP in the web UI): set by install.sh --with-dhcp,' "$ENV_FILE" || return 0
	tmp=$ENV_FILE.new
	(
		umask 077
		awk '
			{ l[NR] = $0 }
			END {
				for (i = 1; i <= NR; i++) {
					if (i + 3 <= NR && l[i] == "# DHCP server (DNS -> DHCP in the web UI): set by install.sh --with-dhcp," &&
						l[i + 1] == "# which also installs the unit drop-in it needs; remove it with" &&
						l[i + 2] == "# install.sh --without-dhcp rather than by hand." && l[i + 3] == "#PICACHE_DHCP=on") {
						print "# DHCP server: switched on in the web UI (DNS -> DHCP); PiCache holds no"
						print "# DHCP port while it is off. PICACHE_DHCP=off prevents switching it on (for"
						print "# hosts that run another DHCP server; install.sh --without-dhcp sets it)."
						print "#PICACHE_DHCP=off"
						i += 3
					} else {
						print l[i]
					}
				}
			}' "$ENV_FILE" >"$tmp"
	)
	chown root:picache "$tmp"
	chmod 0640 "$tmp"
	mv -f "$tmp" "$ENV_FILE"
}

# cleanup_dhcp runs on every install, re-run and uninstall: it removes the
# drop-in and marker of --with-dhcp before 0.8.0 (the base unit carries the
# drop-in's content now) and normalises PICACHE_DHCP in the env file. The
# value (surrounding quotes and blanks removed, any case): on, yes, 1, t and
# true (the old opt-in) are removed (legacy_dhcp_on=1 then); off, no, 0, f
# and false are written as PICACHE_DHCP=off; anything else is reported
# (PiCache refuses to start with it) and left alone.
cleanup_dhcp() {
	legacy_dhcp_on=0
	rm -f "$OLD_DHCP_DROPIN_DIR/$OLD_DHCP_DROPIN" "$OLD_DHCP_MARKER"
	rmdir "$OLD_DHCP_DROPIN_DIR" 2>/dev/null || true
	[ -e "$ENV_FILE" ] || return 0
	refresh_dhcp_comment
	raw=$(env_value PICACHE_DHCP)
	[ -n "$raw" ] || return 0
	v=$(printf '%s' "$raw" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
	case $v in
	\"*\") v=${v#\"}; v=${v%\"} ;;
	\'*\') v=${v#\'}; v=${v%\'} ;;
	esac
	case $(printf '%s' "$v" | tr '[:upper:]' '[:lower:]') in
	on | yes | 1 | t | true)
		rewrite_env PICACHE_DHCP
		legacy_dhcp_on=1
		;;
	off | no | 0 | f | false)
		if [ "$raw" != off ] || [ "$(grep -c "^[[:space:]]*\(export[[:space:]]\{1,\}\)\{0,1\}PICACHE_DHCP=" "$ENV_FILE")" != 1 ]; then
			rewrite_env PICACHE_DHCP off
		fi
		;;
	*) warn "PICACHE_DHCP=$raw is not valid; PiCache refuses to start with it (remove the line, or set PICACHE_DHCP=off)" ;;
	esac
}

# seed_dhcp_markers runs when cleanup_dhcp removed the old opt-in
# (PICACHE_DHCP=on), which earlier versions never turned into markers: it
# creates the service's DHCP markers (empty files, picache:picache 0640) in
# the data directory, so the first start of this version opens the DHCP
# ports and the raw socket for router advertisements as the opt-in did; the
# service removes right after the start whatever its settings do not need.
# A marker that exists already, a symbolic link or anything else in its
# place is left alone, and install never follows a link (it creates the
# file with O_EXCL).
seed_dhcp_markers() {
	[ -d "$DATA_DIR" ] && [ ! -L "$DATA_DIR" ] || return 0
	for m in dhcp.sockets dhcp.ra; do
		if [ ! -e "$DATA_DIR/$m" ] && [ ! -L "$DATA_DIR/$m" ]; then
			install -m 0640 -o picache -g picache /dev/null "$DATA_DIR/$m"
		fi
	done
}

# dhcp_ports_note tells, unless PICACHE_DHCP=off is set, when another
# program uses the DHCP ports (another DHCP server on this host).
dhcp_ports_note() {
	command -v ss >/dev/null 2>&1 || return 0
	[ "$(env_value PICACHE_DHCP)" != off ] || return 0
	for port in 67 547; do
		if [ -n "$(listeners_on u "$port")" ]; then
			say "Note: UDP port $port is used by another program (another DHCP server on this host?): do not switch on
    PiCache's DHCP server, or set PICACHE_DHCP=off in $ENV_FILE:"
			listeners_on u "$port" | sed 's/^/    /'
		fi
	done
}

# listeners_on u|t PORT prints sockets on PORT that do not belong to PiCache.
listeners_on() {
	ss -H -ln"$1"p "sport = :$2" 2>/dev/null | grep -v '"picache"' || true
}

print_resolved_fix() {
	cat >&2 <<EOF
systemd-resolved's stub listener (127.0.0.53/127.0.0.54) holds port 53.
Choose one fix, then run: systemctl start picache

  A) Keep resolved and let PiCache listen on specific addresses: add
         PICACHE_DNS_LISTEN=$host_ip:53,127.0.0.1:53
     to $ENV_FILE (use this machine's static LAN address).

  B) Turn off the stub listener and let this host resolve through PiCache:
         mkdir -p /etc/systemd/resolved.conf.d
         printf '[Resolve]\nDNS=127.0.0.1\nDNSStubListener=no\n' \\
             > /etc/systemd/resolved.conf.d/picache.conf
         mv /etc/resolv.conf /etc/resolv.conf.backup
         ln -s /run/systemd/resolve/resolv.conf /etc/resolv.conf
         systemctl reload-or-restart systemd-resolved
EOF
}

print_generic_fix() {
	cat >&2 <<EOF
Another DNS server (for example dnsmasq, bind9, unbound or another DNS
filter) listens on port 53. Stop and disable it, or bind PiCache to addresses
that server does not use by adding to $ENV_FILE, for example:
    PICACHE_DNS_LISTEN=$host_ip:53
Then run: systemctl start picache
EOF
}

# check_ports sets port53_blocked=1 when PiCache cannot bind its default DNS
# listener. It never changes other services.
check_ports() {
	port53_blocked=0
	if ! command -v ss >/dev/null 2>&1; then
		warn "ss (iproute2) is not installed; skipping the port conflict check"
		return
	fi
	conflicts=$(
		listeners_on u 53
		listeners_on t 53
	)
	if [ -n "$conflicts" ]; then
		dns_listen=$(env_value PICACHE_DNS_LISTEN)
		warn "port 53 is already in use:"
		printf '%s\n' "$conflicts" | sed 's/^/    /' >&2
		if [ -n "$dns_listen" ]; then
			say "PICACHE_DNS_LISTEN=$dns_listen is set; starting anyway." >&2
		else
			port53_blocked=1
			if printf '%s\n' "$conflicts" | grep -Eq 'systemd-resolve|127\.0\.0\.5[34]'; then
				print_resolved_fix
			else
				print_generic_fix
			fi
		fi
	fi
	for port in 80 443 853 8080 8443; do
		if [ -n "$(listeners_on t "$port")" ]; then
			warn "TCP port $port is used by another program. PiCache keeps running without
that listener (see System -> Health & about); free the port or change the
matching PICACHE_*_LISTEN setting in $ENV_FILE."
		fi
	done
}

primary_ip() {
	addr=$(ip -4 route get 1.1.1.1 2>/dev/null |
		awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }') || true
	[ -n "$addr" ] || addr=$(hostname -I 2>/dev/null | awk '{ print $1 }') || true
	printf '%s' "${addr:-<this-host>}"
}

# primary_iface prints the interface of the IPv4 route to the internet ("" if
# none).
primary_iface() {
	ip -4 route get 1.1.1.1 2>/dev/null |
		awk '{ for (i = 1; i < NF; i++) if ($i == "dev") { print $(i + 1); exit } }' || true
}

# primary_cidr prints the IPv4 address of the primary interface with its
# prefix length, e.g. 192.168.1.10/24 ("" if unknown).
primary_cidr() {
	iface=$(primary_iface)
	[ -n "$iface" ] || return 0
	ip -4 -o addr show dev "$iface" 2>/dev/null |
		awk '{ for (i = 1; i < NF; i++) if ($i == "inet") { print $(i + 1); exit } }' || true
}

# doh_port prints the port of the first PICACHE_DOH_LISTEN address, or the
# placeholder <DoH port> while it is unset or off.
doh_port() {
	v=$(env_value PICACHE_DOH_LISTEN | tr -d "\"' ")
	v=${v%%,*}
	case $v in
	'' | off) printf '%s' '<DoH port>' ;;
	*) printf '%s' "${v##*:}" ;;
	esac
}

# firewall_hints prints the commands that open PiCache's ports to the LAN
# while firewalld or ufw is active. It never changes the firewall (like
# systemd-resolved: the admin decides). firewalld: rich rules for
# <LAN-CIDR> in the zone of the primary interface, never moving the LAN
# into another zone (that would change the rules of ssh); ufw: allow rules
# from <LAN-CIDR>. DHCP requests come from 0.0.0.0 (and DHCPv6 from
# link-local addresses), so the DHCP lines are limited to the interface
# instead.
firewall_hints() {
	fw=""
	if command -v firewall-cmd >/dev/null 2>&1 && [ "$(firewall-cmd --state 2>/dev/null)" = running ]; then
		fw=firewalld
	elif command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
		fw=ufw
	fi
	[ -n "$fw" ] || return 0
	cidr=$(primary_cidr)
	iface=$(primary_iface)
	doh=$(doh_port)
	say ""
	say "$fw is active: open PiCache's ports to your LAN yourself (install.sh never changes the firewall)."
	say "Replace <LAN-CIDR> with your network${cidr:+ (this machine: $cidr)}; the ports in use are listed under"
	say "System -> Network -> Listeners in the web UI. For IPv6 clients add the same rules for your ULA prefix."
	if [ "$fw" = firewalld ]; then
		zone=""
		[ -z "$iface" ] || zone=$(firewall-cmd --get-zone-of-interface="$iface" 2>/dev/null) || zone=""
		[ -n "$zone" ] || zone=$(firewall-cmd --get-default-zone 2>/dev/null) || zone=""
		zone=${zone:-public}
		rule() {
			say "${1}firewall-cmd --permanent --zone=$zone --add-rich-rule='rule family=\"ipv4\" source address=\"<LAN-CIDR>\" port port=\"$2\" protocol=\"$3\" accept'"
		}
		say "    # the zone of ${iface:-the primary interface}: $zone"
		rule "    " 53 udp
		rule "    " 53 tcp
		rule "    " 8080 tcp
		rule "    " 8443 tcp
		say "    # with the download cache on:"
		rule "    # " 80 tcp
		rule "    # " 443 tcp
		say "    # with DoT on:"
		rule "    # " 853 tcp
		say "    # with PICACHE_DOH_LISTEN set:"
		rule "    # " "$doh" tcp
		say "    # while the DHCP server is on (clients have no address yet):"
		say "    # firewall-cmd --permanent --zone=$zone --add-port=67/udp --add-port=547/udp"
		say "    # with the NTP server on:"
		rule "    # " 123 udp
		say "    firewall-cmd --reload"
	else
		rule() { say "${1}ufw allow from <LAN-CIDR> to any port $2 proto $3"; }
		rule "    " 53 udp
		rule "    " 53 tcp
		rule "    " 8080 tcp
		rule "    " 8443 tcp
		say "    # with the download cache on:"
		rule "    # " 80 tcp
		rule "    # " 443 tcp
		say "    # with DoT on:"
		rule "    # " 853 tcp
		say "    # with PICACHE_DOH_LISTEN set:"
		rule "    # " "$doh" tcp
		say "    # while the DHCP server is on (clients have no address yet):"
		say "    # ufw allow in on ${iface:-<interface>} to any port 67 proto udp"
		say "    # ufw allow in on ${iface:-<interface>} to any port 547 proto udp"
		say "    # with the NTP server on:"
		rule "    # " 123 udp
	fi
}

# journal_tail prints the last log lines of picache.service to stderr
# without the first-run setup token, which PiCache logs at every start
# until the setup is done: this output may be kept (apt writes the
# package's output to /var/log/apt/term.log).
journal_tail() {
	journalctl -u picache.service -n 30 --no-pager 2>/dev/null | grep -v setupToken >&2 || true
}

start_service() {
	# A unit that hit its start limit earlier (e.g. port 53 was busy) needs a reset.
	systemctl reset-failed picache.service >/dev/null 2>&1 || true
	systemctl restart picache.service
	sleep 3
	i=0
	while [ "$i" -lt 15 ] && [ ! -e "$DATA_DIR/picache.db" ]; do
		sleep 1
		i=$((i + 1))
	done
	if ! systemctl is-active --quiet picache.service; then
		warn "PiCache did not start. Last log lines (without the setup token):"
		journal_tail
		if [ "$(systemctl show -p ExecMainStatus --value picache.service 2>/dev/null)" = 226 ]; then
			say "Exit status 226 (NAMESPACE): in a Proxmox LXC, enable the container's
nesting feature (pct set <ctid> --features nesting=1) and restart it." >&2
		fi
		exit 1
	fi
}

print_summary() {
	web_listen=$(env_value PICACHE_WEB_LISTEN)
	say ""
	say "PiCache is running."
	say "  Web UI:        http://$host_ip:8080/  (HTTPS: https://$host_ip:8443/, PiCache's local CA)"
	if [ -n "$web_listen" ]; then
		say "                 (PICACHE_WEB_LISTEN=$web_listen is set; adjust the address)"
	fi
	say "  Setup token:   sudo picache setup-token  (first start only)"
	if [ "$updater_active" -eq 1 ]; then
		say "  Updates:       in the web UI (System), or: sudo picache update"
	else
		say "  Updates:       sudo picache update"
	fi
	say "  Logs:          journalctl -u picache -f"
	say "  Configuration: $ENV_FILE (then: systemctl restart picache)"
	say ""
	say "Next: finish the setup in the web UI, give this machine a static address and"
	say "point your router's DHCP DNS server option to $host_ip."
	if [ "$(env_value PICACHE_DHCP)" != off ]; then
		say "If the router cannot hand out another DNS server, switch on PiCache's own DHCP"
		say "server under DNS -> DHCP."
	fi
}


# print_kept lists what --uninstall (and the package's remove) keeps.
print_kept() {
	say "  configuration   $CONF_DIR  (NAS credentials in $CRED_DIR)"
	say "  data            $DATA_DIR  (configuration database, logs, keys)"
	say "  local cache     $(env_path PICACHE_CACHE_DIR "$DEFAULT_CACHE_DIR" 2>/dev/null || printf '%s' "$DEFAULT_CACHE_DIR")"
	if [ -d "$DEFAULT_LOG_DIR" ]; then
		say "  log files       $DEFAULT_LOG_DIR  (PICACHE_LOG_FILE)"
	fi
	say "  mount root      $MOUNT_ROOT  (unmount NAS shares before deleting anything)"
	say "  system user     picache  (userdel picache)"
	# Mount units written by the host-apply helper (unit name = escaped mount point).
	prefix=$(systemd-escape --path "$MOUNT_ROOT" 2>/dev/null) || prefix=srv-picache
	for f in /etc/systemd/system/"$prefix"-*.mount; do
		[ -e "$f" ] || continue
		unit=$(basename "$f")
		id=${unit#"$prefix"-}
		id=${id%.mount}
		say "  NAS mount unit  $f"
		say "      systemctl disable --now $unit && rm -f $f $CRED_DIR/$id.cred $CRED_DIR/$id.applied && rmdir $MOUNT_ROOT/$id"
	done
}

do_uninstall() {
	read_paths
	remove_updater
	cleanup_dhcp
	for unit in picache-storage.path picache-storage.service picache.service "$SHARED_MOUNTS_UNIT"; do
		systemctl disable --now "$unit" >/dev/null 2>&1 || true
	done
	for unit in picache.service picache-storage.service picache-storage.path "$SHARED_MOUNTS_UNIT"; do
		rm -f "$UNIT_DIR/$unit"
	done
	for d in picache-storage.path.d picache-storage.service.d; do
		rm -f "/etc/systemd/system/$d/$PATHS_DROPIN"
		rmdir "/etc/systemd/system/$d" 2>/dev/null || true
	done
	# picache.prev, a staged download and the <unit>.prev copies are left by
	# `picache update`.
	rm -f "$HOST_APPLY_MARKER" "$NIGHTLY_MARKER" "$BIN" "$BIN.prev" "$(dirname "$BIN")/.picache.update" "$UNIT_DIR"/picache*.prev
	for f in $DOC_FILES; do
		rm -f "$DOC_DIR/$f"
	done
	rmdir "$DOC_DIR" 2>/dev/null || true
	systemctl daemon-reload
	if [ "$purge" -eq 1 ]; then
		say "PiCache was stopped and removed."
		return
	fi
	say "PiCache was stopped and removed. Kept, delete them yourself if no longer needed:"
	print_kept
}

# is_mountpoint DIR succeeds when DIR itself is a mount point.
is_mountpoint() {
	findmnt -rn -o TARGET 2>/dev/null | awk -v d="$1" '$0 == d { found = 1 } END { exit !found }'
}

# mounted_below DIR succeeds when something is mounted below DIR.
mounted_below() {
	findmnt -rn -o TARGET 2>/dev/null | awk -v p="$1/" 'index($0, p) == 1 { found = 1 } END { exit !found }'
}

# confirm_purge asks before --purge deletes anything. It reads the answer
# from the terminal, because the script itself may arrive through a pipe
# (curl ... | sudo sh); --yes skips the question.
confirm_purge() {
	read_paths
	# Refused here, before anything is removed (do_purge needs it too).
	env_path PICACHE_CACHE_DIR "$DEFAULT_CACHE_DIR" >/dev/null
	[ "$assume_yes" -eq 0 ] || return 0
	# In a subshell: a failed redirection of the special built-in `:` would
	# end the whole script at once (POSIX), without the message below.
	if ! (: </dev/tty) 2>/dev/null; then
		die "--purge deletes the configuration and all data; run it in a terminal or add --yes"
	fi
	cat >/dev/tty <<EOF
--purge stops PiCache, unmounts the NAS shares it mounted and deletes
$CONF_DIR, $DATA_DIR (database, logs, keys and backups), the local cache,
$DEFAULT_LOG_DIR and the picache user. Directories outside the default
paths are kept.
EOF
	printf 'Type "purge" to continue: ' >/dev/tty
	answer=""
	read -r answer </dev/tty || true
	[ "$answer" = purge ] || die "cancelled; nothing was changed"
}

# purge_path_of KEY DEFAULT prints a path setting for do_purge: the value of
# env_path, or an empty line when it is invalid (the package mode never
# fails on it; such a path is neither deleted nor checked, so it keeps the
# account, see do_purge).
purge_path_of() {
	(env_path "$1" "$2") 2>/dev/null || true
}

# do_purge MODE deletes what --uninstall keeps: install.sh --uninstall
# --purge (MODE ask: confirmed before) and the package's postrm purge (MODE
# package: never asks and never fails). Only the default paths are deleted,
# never a mount point: a custom PICACHE_DATA_DIR, PICACHE_CACHE_DIR or
# PICACHE_MOUNT_ROOT may point to a directory with other data, and a mount
# point may be a volume or a NAS share, so those are kept and listed. With
# something mounted below a path install.sh refuses (nothing is deleted);
# the package keeps that path and lists it. The NAS mount units of the
# host-apply helper are disabled and removed. The account and the group
# are deleted only when no kept path holds a file of them and every path
# setting could be read; otherwise the account is kept and locked, so that
# a later system account of that name can never inherit PiCache's
# database, keys, backups and logs.
do_purge() {
	mode=$1
	# unchecked: the path settings the package mode cannot read (not a
	# plain absolute path; install.sh refuses them before it starts). Their
	# directories may hold PiCache's files but cannot be searched.
	unchecked=""
	unchecked_list=""
	if [ "$mode" = package ]; then
		data_dir=$(purge_path_of PICACHE_DATA_DIR "$DEFAULT_DATA_DIR")
		cache_dir=$(purge_path_of PICACHE_CACHE_DIR "$DEFAULT_CACHE_DIR")
		mount_root=$(purge_path_of PICACHE_MOUNT_ROOT "$DEFAULT_MOUNT_ROOT")
		for pair in "PICACHE_DATA_DIR:$data_dir" "PICACHE_CACHE_DIR:$cache_dir" "PICACHE_MOUNT_ROOT:$mount_root"; do
			[ -z "${pair#*:}" ] || continue
			key=${pair%%:*}
			unchecked="$unchecked $key"
			unchecked_list="$unchecked_list  $key=$(env_value "$key")  (not a plain absolute path: not checked)
"
		done
	else
		data_dir=$DATA_DIR
		cache_dir=$(env_path PICACHE_CACHE_DIR "$DEFAULT_CACHE_DIR")
		mount_root=$MOUNT_ROOT
	fi
	# NAS mount units written by the host-apply helper.
	prefix=$(systemd-escape --path "${mount_root:-$DEFAULT_MOUNT_ROOT}" 2>/dev/null) || prefix=srv-picache
	for f in /etc/systemd/system/"$prefix"-*.mount; do
		[ -e "$f" ] || continue
		if [ -d /run/systemd/system ]; then
			systemctl disable --now "$(basename "$f")" >/dev/null 2>&1 || true
		fi
		rm -f "$f"
	done
	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload || true
	fi
	if [ "$mode" != package ] && { mounted_below "$MOUNT_ROOT" || mounted_below "$cache_dir" || mounted_below "$DATA_DIR" ||
		mounted_below "$DEFAULT_LOG_DIR"; }; then
		die "something is still mounted below $MOUNT_ROOT, $cache_dir, $DATA_DIR or $DEFAULT_LOG_DIR (see findmnt);
unmount it and run --uninstall --purge again. Nothing else was deleted."
	fi
	kept=""
	for pair in "$data_dir:$DEFAULT_DATA_DIR" "$cache_dir:$DEFAULT_CACHE_DIR" "$DEFAULT_LOG_DIR:$DEFAULT_LOG_DIR"; do
		dir=${pair%%:*}
		default=${pair#*:}
		if [ "$dir" != "$default" ]; then
			# A custom (or unreadable) path is kept, and so is the default
			# directory: systemd creates it for the unit anyway
			# (StateDirectory=, CacheDirectory=), and the custom path may lie
			# below it. Both may hold files of the account.
			if [ -n "$dir" ] && [ -e "$dir" ]; then kept="$kept $dir"; fi
			if [ -e "$default" ]; then kept="$kept $default"; fi
			continue
		fi
		[ -e "$dir" ] || continue
		if is_mountpoint "$dir" || mounted_below "$dir"; then
			kept="$kept $dir"
			continue
		fi
		rm -rf -- "$dir" || kept="$kept $dir"
	done
	rm -rf -- "$CONF_DIR" || kept="$kept $CONF_DIR"
	if [ -n "$mount_root" ] && [ -d "$mount_root" ]; then
		# Empty mount points only: never delete files below the mount root.
		for d in "$mount_root"/*; do
			if [ -d "$d" ]; then rmdir -- "$d" 2>/dev/null || true; fi
		done
		if [ "$mount_root" != "$DEFAULT_MOUNT_ROOT" ] || ! rmdir -- "$mount_root" 2>/dev/null; then
			kept="$kept $mount_root"
		fi
	fi
	purge_account "$kept" "$unchecked"
	say "Deleted: the configuration, the data, the cache and the log files in their default paths."
	if [ -n "$kept$unchecked" ]; then
		say "Kept (custom paths, mount points or with something mounted below; delete them yourself if no longer needed):"
		for k in $kept; do
			say "  $k"
		done
		printf '%s' "$unchecked_list"
	fi
}

# purge_account deletes the account and group picache unless a kept path
# (the list in $1) holds a file of them or a path setting could not be
# checked (its keys in $2): then the account is kept and locked (password
# and expiry; its shell stays nologin).
purge_account() {
	entry=$(getent passwd picache) || entry=""
	gid=$(getent group picache | cut -d: -f3) || gid=""
	uid=""
	[ -z "$entry" ] || uid=$(printf '%s' "$entry" | cut -d: -f3)
	owned=""
	if [ -n "$uid$gid" ]; then
		for k in $1; do
			[ -e "$k" ] || continue
			if [ -n "$(find "$k" -xdev \( -uid "${uid:-$gid}" -o -gid "${gid:-$uid}" \) -print -quit 2>/dev/null)" ]; then
				owned="$owned $k"
			fi
		done
	fi
	why=""
	[ -z "$owned" ] || why="it owns kept files in${owned}"
	[ -z "${2:-}" ] || why="${why:+$why and }the purge cannot check the directories of${2}"
	if [ -n "$why" ] && [ -n "$uid$gid" ]; then
		if [ -n "$uid" ]; then
			usermod -L picache >/dev/null 2>&1 || true
			usermod -e 1 picache >/dev/null 2>&1 || true
			say "kept the account picache (uid $uid) because $why; it is locked"
		else
			say "kept the group picache (gid $gid) because $why"
		fi
		return 0
	fi
	if [ -n "$uid" ]; then
		if [ "$uid" -gt 0 ] && [ "$uid" -le "$(sys_id_max UID)" ]; then
			if userdel picache; then
				say "Deleted the account picache."
			else
				warn "could not delete the account picache (still in use?); delete it later with userdel picache"
			fi
		else
			warn "the account picache is not a system account (uid $uid); it was kept"
		fi
	fi
	gid=$(getent group picache | cut -d: -f3) || gid=""
	if [ -n "$gid" ] && [ "$gid" -gt 0 ] && [ "$gid" -le "$(sys_id_max GID)" ]; then
		groupdel picache 2>/dev/null || true
	fi
}

# --- main ------------------------------------------------------------------

main() {
	binary=""
	with_host_apply=0
	without_updater=0
	with_dhcp=0
	without_dhcp=0
	nightly=0
	uninstall=0
	purge=0
	assume_yes=0
	while [ $# -gt 0 ]; do
		case $1 in
		--binary)
			[ $# -ge 2 ] || die "--binary needs a path"
			binary=$2
			shift 2
			;;
		--binary=*)
			binary=${1#--binary=}
			shift
			;;
		--with-host-apply)
			with_host_apply=1
			shift
			;;
		--without-updater)
			without_updater=1
			shift
			;;
		--with-dhcp)
			with_dhcp=1
			shift
			;;
		--without-dhcp)
			without_dhcp=1
			shift
			;;
		--nightly)
			nightly=1
			shift
			;;
		--uninstall)
			uninstall=1
			shift
			;;
		--purge)
			purge=1
			shift
			;;
		--yes)
			assume_yes=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			die "unknown argument: $1"
			;;
		esac
	done

	[ "$(id -u)" -eq 0 ] || die "run as root (sudo sh $0 ...)"
	[ -d /run/systemd/system ] || die "systemd is not running; use the Docker image instead (docs/DEPLOYMENT.md)"
	umask 022

	if [ "$purge" -eq 1 ] && [ "$uninstall" -eq 0 ]; then
		die "--purge only goes with --uninstall"
	fi
	if [ "$with_dhcp" -eq 1 ] && [ "$without_dhcp" -eq 1 ]; then
		die "--with-dhcp and --without-dhcp exclude each other"
	fi
	check_not_packaged
	if [ "$uninstall" -eq 1 ]; then
		if [ "$purge" -eq 1 ]; then
			confirm_purge
		fi
		do_uninstall
		if [ "$purge" -eq 1 ]; then
			do_purge ask
		fi
		exit 0
	fi

	[ -n "$binary" ] || {
		usage >&2
		die "--binary PATH is required"
	}
	[ -f "$UNIT_SRC/picache.service" ] || die "unit files not found in $UNIT_SRC; run install.sh from the PiCache source tree"
	check_systemd
	check_os
	ensure_user
	install_binary
	install_docs

	install -d -m 0750 -o root -g picache "$CONF_DIR"
	write_env_file
	read_paths
	# Root-owned (group picache may enter): the service must not be able to swap
	# a mount point for a symbolic link that the root helper would then use.
	install -d -m 0750 -o root -g picache "$MOUNT_ROOT"

	install -d -m 0755 "$UNIT_DIR"
	install_unit picache.service
	if [ -e /etc/systemd/system/picache.service ]; then
		warn "/etc/systemd/system/picache.service exists and overrides the installed unit;
remove it unless you maintain it on purpose (use drop-ins for local changes)."
	fi

	host_apply_active=0
	shared_mounts=0
	if [ "$with_host_apply" -eq 1 ] || [ -e "$HOST_APPLY_MARKER" ]; then
		setup_host_apply
	fi
	updater_active=0
	if [ "$without_updater" -eq 0 ]; then
		setup_updater
	elif [ -e "$UPDATER_MARKER" ] || [ -e "$UNIT_DIR/picache-update.path" ]; then
		remove_updater
		say "update helper removed (--without-updater); update with: sudo picache update"
	fi
	cleanup_dhcp
	if [ "$with_dhcp" -eq 1 ]; then
		if [ -n "$(env_value PICACHE_DHCP)" ]; then
			rewrite_env PICACHE_DHCP
		fi
		say "DHCP server allowed (--with-dhcp): switch it on in the web UI under DNS -> DHCP"
	elif [ "$without_dhcp" -eq 1 ]; then
		rewrite_env PICACHE_DHCP off
		say "DHCP server prevented (--without-dhcp: PICACHE_DHCP=off); it cannot be switched on in the web UI"
	fi
	if [ "$legacy_dhcp_on" -eq 1 ] && [ "$without_dhcp" -eq 0 ]; then
		seed_dhcp_markers
	fi
	if [ "$nightly" -eq 1 ]; then
		# A regular file owned by root: the service cannot create it, so it
		# cannot move this host onto nightly builds by itself.
		rm -f "$NIGHTLY_MARKER"
		install -m 0644 -o root -g root /dev/null "$NIGHTLY_MARKER"
		say "nightly builds allowed (--nightly): choose the update channel \"nightly\" in the web UI (System -> Updates)"
	fi
	selinux_relabel

	systemctl daemon-reload
	systemctl enable picache.service >/dev/null
	if [ "$shared_mounts" -eq 1 ]; then
		systemctl enable --now "$SHARED_MOUNTS_UNIT" >/dev/null
	fi
	if [ "$host_apply_active" -eq 1 ]; then
		# A unit stopped by a start or trigger limit earlier needs a reset, and a
		# running path unit a restart to use changed unit files and drop-ins.
		systemctl reset-failed picache-storage.path picache-storage.service >/dev/null 2>&1 || true
		systemctl enable --now picache-storage.path >/dev/null
		systemctl restart picache-storage.path
	fi
	if [ "$updater_active" -eq 1 ]; then
		systemctl reset-failed picache-update.path picache-update.service >/dev/null 2>&1 || true
		systemctl enable --now picache-update.path >/dev/null
		systemctl restart picache-update.path
	fi

	host_ip=$(primary_ip)
	check_ports
	if [ "$with_dhcp" -eq 0 ] && [ "$without_dhcp" -eq 0 ]; then
		dhcp_ports_note
	fi
	firewall_hints
	if [ "$port53_blocked" -eq 1 ]; then
		say ""
		say "PiCache is installed and enabled but NOT started because port 53 is in use."
		say "After fixing that, run: systemctl start picache"
		say "Then open http://$host_ip:8080/ and get the one-time setup token with:"
		say "    sudo picache setup-token"
		exit 0
	fi
	start_service
	print_summary
}

main "$@"
