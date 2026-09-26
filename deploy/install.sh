#!/bin/sh
# PiCache installer for Debian 12/13 (bare metal, VM, Proxmox LXC).
#
#   sudo sh deploy/install.sh --binary ./picache-linux-amd64 [--with-host-apply] [--without-updater]
#                             [--with-dhcp | --without-dhcp]
#   sudo sh deploy/install.sh --uninstall [--purge [--yes]]
#
# Idempotent: run it again with a newer binary to upgrade. It installs only
# the local files you give it (it never downloads anything) and never changes
# systemd-resolved or any other DNS service; it prints the fix instead.
set -eu

BIN=/usr/local/bin/picache
UNIT_DIR=/usr/local/lib/systemd/system
DOC_DIR=/usr/share/doc/picache
CONF_DIR=/etc/picache
ENV_FILE=$CONF_DIR/picache.env
CRED_DIR=$CONF_DIR/credentials
HOST_APPLY_MARKER=$CONF_DIR/host-apply.enabled
UPDATER_MARKER=$CONF_DIR/updater.enabled
# Marker and unit drop-in of --with-dhcp before 0.8.0 (the base unit carries
# the drop-in's content now); every run removes them.
OLD_DHCP_MARKER=$CONF_DIR/dhcp.enabled
OLD_DHCP_DROPIN_DIR=/etc/systemd/system/picache.service.d
OLD_DHCP_DROPIN=60-dhcp.conf
DEFAULT_DATA_DIR=/var/lib/picache
DEFAULT_CACHE_DIR=/var/cache/picache
DEFAULT_MOUNT_ROOT=/srv/picache
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

say() { printf '%s\n' "$*"; }
warn() { printf '\nWARNING: %s\n' "$*" >&2; }
die() {
	printf 'install.sh: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
usage: install.sh --binary PATH [--with-host-apply] [--without-updater]
                  [--with-dhcp | --without-dhcp]
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
  --uninstall         stop and remove PiCache; configuration and data are kept
  --purge             with --uninstall: also unmount the NAS shares of the
                      web UI and delete the configuration, the data, the
                      local cache and the picache user (default paths only)
  --yes               do not ask before --purge

The update helper (picache-update.path) is installed by default.
EOF
}

# --- helpers ---------------------------------------------------------------

# os_release KEY prints a value from /etc/os-release without sourcing it.
os_release() {
	sed -n "s/^$1=//p" /etc/os-release 2>/dev/null | tr -d "\"'" | head -n 1
}

check_os() {
	id=$(os_release ID)
	like=$(os_release ID_LIKE)
	ver=$(os_release VERSION_ID)
	case " $id $like " in
	*" debian "*) ;;
	*) warn "this installer is made for Debian 12 and 13 (found '${id:-unknown}'); continuing" ;;
	esac
	if [ "$id" = debian ]; then
		case $ver in
		12 | 13) ;;
		*) warn "Debian $ver is untested; supported are Debian 12 and 13" ;;
		esac
	fi
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

# ensure_user creates the system user and group picache or checks existing
# ones. The service owns the database and the master key, so it must never
# run as an account that people log in with (for example a user named
# picache created by the Debian installer).
ensure_user() {
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
	if [ -z "$gid" ]; then
		groupadd --system picache
	elif [ "$gid" -gt "$(sys_id_max GID)" ]; then
		die "a group named picache exists that is not a system group (gid $gid).
Rename it (groupmod -n NEWNAME picache) or remove it, then run install.sh again."
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
	mv -f "$tmp" "$BIN"
	say "installed $BIN: $out"
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
# Own certificate and key (PEM), readable by the picache group (0640 root:picache).
#PICACHE_WEB_TLS_CERT=/etc/picache/tls/cert.pem
#PICACHE_WEB_TLS_KEY=/etc/picache/tls/key.pem
# Extra host names for the web UI (IP addresses and localhost always work).
#PICACHE_WEB_HOSTS=picache.lan

# Logging: debug | info | warn | error, and text | json.
#PICACHE_LOG_LEVEL=info
#PICACHE_LOG_FORMAT=text

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
	missing=""
	command -v mount.cifs >/dev/null 2>&1 || missing="$missing cifs-utils"
	command -v mount.nfs >/dev/null 2>&1 || missing="$missing nfs-common"
	if [ -n "$missing" ]; then
		say "host-apply: mount helpers are missing; install them for the share types you use:"
		say "    apt install$missing"
		case $missing in
		*nfs-common*)
			say "  NFS 4 does not need the rpcbind service that nfs-common brings; turn it off with:"
			say "    systemctl mask --now rpcbind.service rpcbind.socket"
			;;
		esac
	fi
	say "host-apply helper installed (picache-storage.path)"
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
	for port in 80 443 8080 8443; do
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
		warn "PiCache did not start. Last log lines:"
		journalctl -u picache.service -n 30 --no-pager >&2 || true
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
	rm -f "$HOST_APPLY_MARKER" "$BIN" "$BIN.prev" "$(dirname "$BIN")/.picache.update" "$UNIT_DIR"/picache*.prev
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
	say "  configuration   $CONF_DIR  (NAS credentials in $CRED_DIR)"
	say "  data            $DATA_DIR  (configuration database, logs, keys)"
	say "  local cache     /var/cache/picache"
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
	[ "$assume_yes" -eq 0 ] || return 0
	# In a subshell: a failed redirection of the special built-in `:` would
	# end the whole script at once (POSIX), without the message below.
	if ! (: </dev/tty) 2>/dev/null; then
		die "--purge deletes the configuration and all data; run it in a terminal or add --yes"
	fi
	cat >/dev/tty <<EOF
--purge stops PiCache, unmounts the NAS shares it mounted and deletes
$CONF_DIR, $DATA_DIR (database, logs, keys and backups), the local cache
and the picache user. Directories outside the default paths are kept.
EOF
	printf 'Type "purge" to continue: ' >/dev/tty
	answer=""
	read -r answer </dev/tty || true
	[ "$answer" = purge ] || die "cancelled; nothing was changed"
}

# do_purge deletes what --uninstall keeps. Only the default paths are
# deleted: a custom PICACHE_DATA_DIR, PICACHE_CACHE_DIR or PICACHE_MOUNT_ROOT
# may point to a directory with other data, and a mount point may be a
# volume or a NAS share, so those are listed instead.
do_purge() {
	cache_dir=$(env_path PICACHE_CACHE_DIR "$DEFAULT_CACHE_DIR")
	# NAS mount units written by the host-apply helper.
	prefix=$(systemd-escape --path "$MOUNT_ROOT" 2>/dev/null) || prefix=srv-picache
	for f in /etc/systemd/system/"$prefix"-*.mount; do
		[ -e "$f" ] || continue
		systemctl disable --now "$(basename "$f")" >/dev/null 2>&1 || true
		rm -f "$f"
	done
	systemctl daemon-reload
	if mounted_below "$MOUNT_ROOT" || mounted_below "$cache_dir" || mounted_below "$DATA_DIR"; then
		die "something is still mounted below $MOUNT_ROOT, $cache_dir or $DATA_DIR (see findmnt);
unmount it and run --uninstall --purge again. Nothing else was deleted."
	fi
	kept=""
	for pair in "$DATA_DIR:$DEFAULT_DATA_DIR" "$cache_dir:$DEFAULT_CACHE_DIR"; do
		dir=${pair%%:*}
		default=${pair#*:}
		[ -e "$dir" ] || continue
		if [ "$dir" != "$default" ] || is_mountpoint "$dir"; then
			kept="$kept
  $dir"
			continue
		fi
		rm -rf -- "$dir"
	done
	rm -rf -- "$CONF_DIR"
	if [ -d "$MOUNT_ROOT" ]; then
		# Empty mount points only: never delete files below the mount root.
		for d in "$MOUNT_ROOT"/*; do
			if [ -d "$d" ]; then rmdir -- "$d" 2>/dev/null || true; fi
		done
		if [ "$MOUNT_ROOT" != "$DEFAULT_MOUNT_ROOT" ] || ! rmdir -- "$MOUNT_ROOT" 2>/dev/null; then
			kept="$kept
  $MOUNT_ROOT"
		fi
	fi
	if entry=$(getent passwd picache); then
		uid=$(printf '%s' "$entry" | cut -d: -f3)
		if [ "$uid" -gt 0 ] && [ "$uid" -le "$(sys_id_max UID)" ]; then
			userdel picache
		else
			warn "the account picache is not a system account (uid $uid); it was kept"
		fi
	fi
	gid=$(getent group picache | cut -d: -f3)
	if [ -n "$gid" ] && [ "$gid" -gt 0 ] && [ "$gid" -le "$(sys_id_max GID)" ]; then
		groupdel picache 2>/dev/null || true
	fi
	say "Deleted: the configuration, the data and cache in their default paths, and the picache user."
	if [ -n "$kept" ]; then
		say "Kept (custom paths or mount points; delete them yourself if no longer needed):$kept"
	fi
}

# --- main ------------------------------------------------------------------

binary=""
with_host_apply=0
without_updater=0
with_dhcp=0
without_dhcp=0
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
if [ "$uninstall" -eq 1 ]; then
	if [ "$purge" -eq 1 ]; then
		confirm_purge
	fi
	do_uninstall
	if [ "$purge" -eq 1 ]; then
		do_purge
	fi
	exit 0
fi

[ -n "$binary" ] || {
	usage >&2
	die "--binary PATH is required"
}
[ -f "$UNIT_SRC/picache.service" ] || die "unit files not found in $UNIT_SRC; run install.sh from the PiCache source tree"
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
