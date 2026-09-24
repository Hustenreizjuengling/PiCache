#!/bin/sh
# PiCache installer for Debian 12/13 (bare metal, VM, Proxmox LXC).
#
#   sudo sh deploy/install.sh --binary ./picache-linux-amd64 [--with-host-apply]
#   sudo sh deploy/install.sh --uninstall
#
# Idempotent: run it again with a newer binary to upgrade. It installs only
# the local files you give it (it never downloads anything) and never changes
# systemd-resolved or any other DNS service; it prints the fix instead.
set -eu

BIN=/usr/local/bin/picache
UNIT_DIR=/usr/local/lib/systemd/system
CONF_DIR=/etc/picache
ENV_FILE=$CONF_DIR/picache.env
CRED_DIR=$CONF_DIR/credentials
HOST_APPLY_MARKER=$CONF_DIR/host-apply.enabled
DATA_DIR=/var/lib/picache
MOUNT_ROOT=/srv/picache
UNIT_SRC=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)/systemd

say() { printf '%s\n' "$*"; }
warn() { printf '\nWARNING: %s\n' "$*" >&2; }
die() {
	printf 'install.sh: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
usage: install.sh --binary PATH [--with-host-apply]
       install.sh --uninstall

  --binary PATH       the picache binary to install (for example the
                      picache-linux-arm64 file from `make build-all`)
  --with-host-apply   also install the root helper that lets the web UI
                      mount SMB/NFS shares (bare metal, VMs, privileged LXC)
  --uninstall         stop and remove PiCache; configuration and data are kept
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

ensure_user() {
	if ! getent group picache >/dev/null; then
		groupadd --system picache
	fi
	if ! getent passwd picache >/dev/null; then
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

env_template() {
	cat <<'EOF'
# PiCache bootstrap configuration.
#
# Read by systemd (EnvironmentFile=) and by every `picache` CLI command.
# Everything else (DNS, filtering, LanCache, cache, logs, web) is configured
# in the web UI. Apply changes with: systemctl restart picache
# Format: KEY=value, one per line. Listener values are comma-separated
# host:port lists; "off" disables a listener. All variables are described in
# docs/DEPLOYMENT.md ("Environment variables").

# DNS (mandatory). If port 53 is shared with another service, bind specific
# addresses, for example: PICACHE_DNS_LISTEN=192.168.1.5:53,127.0.0.1:53
#PICACHE_DNS_LISTEN=:53
# LanCache HTTP cache and HTTPS (SNI) pass-through.
#PICACHE_CACHE_LISTEN=:80
#PICACHE_SNI_LISTEN=:443
# Web UI and API over HTTP and HTTPS (self-signed unless a certificate is set).
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
	host_apply_active=1
	missing=""
	command -v mount.cifs >/dev/null 2>&1 || missing="$missing cifs-utils"
	command -v mount.nfs >/dev/null 2>&1 || missing="$missing nfs-common"
	if [ -n "$missing" ]; then
		say "host-apply: mount helpers are missing; install them for the share types you use:"
		say "    apt install$missing"
	fi
	say "host-apply helper installed (picache-storage.path)"
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
Another DNS server (for example dnsmasq, bind9, unbound, Pi-hole or AdGuard
Home) listens on port 53. Stop and disable it, or bind PiCache to addresses
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
	say "  Web UI:        http://$host_ip:8080/  (HTTPS: https://$host_ip:8443/, self-signed)"
	if [ -n "$web_listen" ]; then
		say "                 (PICACHE_WEB_LISTEN=$web_listen is set; adjust the address)"
	fi
	say "  Setup token:   sudo picache setup-token  (first start only)"
	say "  Logs:          journalctl -u picache -f"
	say "  Configuration: $ENV_FILE (then: systemctl restart picache)"
	say ""
	say "Next: finish the setup in the web UI, give this machine a static address and"
	say "point your router's DHCP DNS server option to $host_ip."
}

do_uninstall() {
	for unit in picache-storage.path picache-storage.service picache.service; do
		systemctl disable --now "$unit" >/dev/null 2>&1 || true
	done
	for unit in picache.service picache-storage.service picache-storage.path; do
		rm -f "$UNIT_DIR/$unit"
	done
	rm -f "$HOST_APPLY_MARKER" "$BIN"
	systemctl daemon-reload
	say "PiCache was stopped and removed. Kept, delete them yourself if no longer needed:"
	say "  configuration   $CONF_DIR  (NAS credentials in $CRED_DIR)"
	say "  data            $DATA_DIR  (configuration database, logs, keys)"
	say "  local cache     /var/cache/picache"
	say "  mount root      $MOUNT_ROOT  (unmount NAS shares before deleting anything)"
	say "  system user     picache  (userdel picache)"
	for f in /etc/systemd/system/srv-picache-*.mount; do
		[ -e "$f" ] || continue
		say "  NAS mount unit  $f"
		say "      systemctl disable --now $(basename "$f") && rm $f"
	done
}

# --- main ------------------------------------------------------------------

binary=""
with_host_apply=0
uninstall=0
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
	--uninstall)
		uninstall=1
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

if [ "$uninstall" -eq 1 ]; then
	do_uninstall
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

install -d -m 0750 -o root -g picache "$CONF_DIR"
# Root-owned (group picache may enter): the service must not be able to swap
# a mount point for a symbolic link that the root helper would then use.
install -d -m 0750 -o root -g picache "$MOUNT_ROOT"
write_env_file

install -d -m 0755 "$UNIT_DIR"
install_unit picache.service
if [ -e /etc/systemd/system/picache.service ]; then
	warn "/etc/systemd/system/picache.service exists and overrides the installed unit;
remove it unless you maintain it on purpose (use drop-ins for local changes)."
fi

host_apply_active=0
if [ "$with_host_apply" -eq 1 ] || [ -e "$HOST_APPLY_MARKER" ]; then
	setup_host_apply
fi

systemctl daemon-reload
systemctl enable picache.service >/dev/null
if [ "$host_apply_active" -eq 1 ]; then
	systemctl enable --now picache-storage.path >/dev/null
fi

host_ip=$(primary_ip)
check_ports
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
