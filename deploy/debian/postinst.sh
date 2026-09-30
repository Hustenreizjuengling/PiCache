
# --- deploy/debian/postinst.sh: the postinst of the Debian package ---------
#
# configure: checks that the binary runs here, creates the account, the
# configuration (never overwritten) and the mount root, enables
# picache.service (and the host-apply units only when their root-owned
# marker exists), then starts PiCache on a fresh install or restarts it on
# an upgrade and waits for it to become healthy (neither while a local
# policy-rc.d forbids it). A failed start never fails the package: the
# steps to fix it are printed. The update helper is never
# installed (the package is updated with apt). Never prompts; the setup
# token is never printed (apt writes this output to /var/log/apt/term.log).

ME=picache.postinst
BIN=/usr/bin/picache
UNIT_DIR=/usr/lib/systemd/system
umask 022

HEALTH_WAIT=90 # seconds an upgrade waits for `picache healthcheck`

systemd_running() { [ -d /run/systemd/system ]; }

# policy_denies ACTION reports whether the local policy-rc.d (the policy of
# invoke-rc.d and deb-systemd-invoke, e.g. in container images) forbids
# ACTION for picache.service now (exit status 101): deb-systemd-invoke then
# skips it and still succeeds.
policy_denies() {
	policy=${DPKG_ROOT:-}/usr/sbin/policy-rc.d
	[ -x "$policy" ] || return 1
	rc=0
	"$policy" picache.service "$1" >/dev/null 2>&1 || rc=$?
	[ "$rc" -eq 101 ]
}

# root_file PATH succeeds for a regular file (not a link) owned by root.
root_file() { [ -f "$1" ] && [ ! -L "$1" ] && [ "$(stat -c %u "$1")" = 0 ]; }

# enable_unit UNIT enables a unit (without systemd running, through
# deb-systemd-helper: it starts at the next boot).
enable_unit() {
	if systemd_running; then
		systemctl enable "$1" >/dev/null
	else
		deb-systemd-helper enable "$1" >/dev/null
	fi
}

# pkg_host_apply enables the host-apply units of the package, but only
# when /etc/picache/host-apply.enabled already exists as a root-owned file
# (the admin's opt-in, docs/DEPLOYMENT.md "Debian package").
pkg_host_apply() {
	host_apply_active=0
	root_file "$HOST_APPLY_MARKER" || return 0
	if is_unprivileged_container; then
		warn "this is an unprivileged container: the kernel refuses SMB/NFS mounts here, so the host-apply helper stays off."
		return 0
	fi
	install -d -m 0700 -o root -g root "$CRED_DIR"
	write_helper_dropins
	if command -v systemd-detect-virt >/dev/null 2>&1 && systemd-detect-virt --container --quiet; then
		case $(findmnt -no PROPAGATION / 2>/dev/null) in
		shared*) ;;
		*)
			enable_unit "$SHARED_MOUNTS_UNIT"
			if systemd_running; then deb-systemd-invoke start "$SHARED_MOUNTS_UNIT" >/dev/null 2>&1 || true; fi
			;;
		esac
	fi
	enable_unit picache-storage.path
	if systemd_running; then
		systemctl reset-failed picache-storage.path picache-storage.service >/dev/null 2>&1 || true
		deb-systemd-invoke restart picache-storage.path >/dev/null 2>&1 || true
	fi
	host_apply_active=1
	mount_helper_hint
	say "host-apply helper enabled (picache-storage.path)"
}

# pkg_start_failed prints why PiCache did not start (never fails).
pkg_start_failed() {
	warn "PiCache did not start. Last log lines (without the setup token):"
	journal_tail
	if [ "$(systemctl show -p ExecMainStatus --value picache.service 2>/dev/null)" = 226 ]; then
		say "Exit status 226 (NAMESPACE): in a Proxmox LXC, enable the container's
nesting feature (pct set <ctid> --features nesting=1) and restart it." >&2
	fi
	say "Fix it, then: systemctl start picache" >&2
}

# pkg_start starts PiCache on a fresh install and reports whether it runs.
pkg_start() {
	systemctl reset-failed picache.service >/dev/null 2>&1 || true
	if ! deb-systemd-invoke start picache.service; then
		return 1
	fi
	sleep 3
	i=0
	while [ "$i" -lt 15 ] && [ ! -e "$DATA_DIR/picache.db" ]; do
		sleep 1
		i=$((i + 1))
	done
	systemctl is-active --quiet picache.service
}

# pkg_restart restarts PiCache after an upgrade and waits up to 90 s (every
# 2 s) for `picache healthcheck`; on failure it prints the rollback steps (a
# .deb upgrade has no automatic rollback).
pkg_restart() {
	old=$1
	healthy=0
	systemctl reset-failed picache.service >/dev/null 2>&1 || true
	if deb-systemd-invoke restart picache.service; then
		waited=0
		while [ "$waited" -lt "$HEALTH_WAIT" ]; do
			if "$BIN" healthcheck >/dev/null 2>&1; then
				healthy=1
				break
			fi
			sleep 2
			waited=$((waited + 2))
		done
	fi
	if [ "$healthy" -eq 1 ]; then
		say "PiCache was restarted and is healthy."
		return 0
	fi
	tag=$(printf 'v%s' "$old" | tr '~' '-')
	arch=${DPKG_MAINTSCRIPT_ARCH:-$(dpkg --print-architecture 2>/dev/null || echo '<arch>')}
	warn "PiCache did not become healthy within $HEALTH_WAIT s after the upgrade from $old.
Last log lines (without the setup token) follow. To go back to $tag (a .deb upgrade has no automatic rollback):
  1. stop PiCache and put the database copy made before the upgrade back
     (before $tag starts: a version before 1.0.0 would copy this database and prune the copies):
       sudo systemctl stop picache
       sudo ls $DATA_DIR/backups/          # picache-$tag-<timestamp>.db
       sudo install -m 0600 -o picache -g picache $DATA_DIR/backups/picache-$tag-<timestamp>.db $DATA_DIR/picache.db
       sudo rm -f $DATA_DIR/picache.db-wal $DATA_DIR/picache.db-shm
  2. install the previous package again (it starts PiCache):
       sudo PICACHE_ALLOW_DOWNGRADE=1 apt install ./picache_${tag#v}_$arch.deb
See docs/DEPLOYMENT.md \"Going back to an earlier version\"."
	journal_tail
}

pkg_summary() {
	arch=${DPKG_MAINTSCRIPT_ARCH:-$(dpkg --print-architecture 2>/dev/null || echo '<arch>')}
	say ""
	say "  Web UI:        http://$host_ip:8080/  (HTTPS: https://$host_ip:8443/, PiCache's local CA)"
	web_listen=$(env_value PICACHE_WEB_LISTEN)
	if [ -n "$web_listen" ]; then
		say "                 (PICACHE_WEB_LISTEN=$web_listen is set; adjust the address)"
	fi
	say "  Setup token:   sudo picache setup-token  (first start only)"
	say "  Updates:       with apt: download picache_<version>_$arch.deb of the next release, verify it"
	say "                 (docs/DEPLOYMENT.md \"Debian package\") and: sudo apt install ./picache_<version>_$arch.deb"
	say "  Removal:       sudo apt remove picache (keeps the configuration and the data),"
	say "                 sudo apt purge picache (deletes them)"
	say "  Logs:          journalctl -u picache -f"
	say "  Configuration: $ENV_FILE (then: systemctl restart picache)"
}

postinst_configure() {
	old=$1
	# 1) The binary must run on this CPU before anything is configured.
	if ! "$BIN" version >/dev/null 2>&1; then
		die "this package's binary does not run on this CPU ($(uname -m)): apt remove picache and use get-picache.sh"
	fi
	# 2) Account, configuration, paths, DHCP leftovers, labels.
	ensure_user
	install -d -m 0750 -o root -g picache "$CONF_DIR"
	write_env_file
	read_paths
	install -d -m 0750 -o root -g picache "$MOUNT_ROOT"
	cleanup_dhcp
	if [ "$legacy_dhcp_on" -eq 1 ]; then
		seed_dhcp_markers
	fi
	selinux_relabel
	if [ -e /etc/systemd/system/picache.service ]; then
		warn "/etc/systemd/system/picache.service exists and overrides the packaged unit;
remove it unless you maintain it on purpose (use drop-ins for local changes)."
	fi
	# 3) Units: picache.service; the host-apply units only with their marker.
	if systemd_running; then
		systemctl daemon-reload
	fi
	enable_unit picache.service
	pkg_host_apply
	host_ip=$(primary_ip)
	# 4) Start or restart.
	if ! systemd_running; then
		say "systemd is not running (a chroot or an image build): PiCache starts at the next boot."
	elif [ -z "$old" ]; then
		check_ports
		dhcp_ports_note
		if [ "$port53_blocked" -eq 1 ]; then
			say ""
			say "PiCache is installed and enabled but not started because port 53 is in use."
			say "After fixing that, run: systemctl start picache"
		elif policy_denies start; then
			say ""
			say "PiCache is installed and enabled but not started: /usr/sbin/policy-rc.d forbids starting services here."
			say "Start it with: systemctl start picache"
		elif pkg_start; then
			say ""
			say "PiCache is running."
		else
			pkg_start_failed
		fi
	elif policy_denies restart; then
		say "PiCache was not restarted: /usr/sbin/policy-rc.d forbids restarting services here."
		say "Restart it to run the new version: systemctl restart picache"
	else
		pkg_restart "$old"
	fi
	# 5) Firewall hints, 6) the summary.
	firewall_hints
	pkg_summary
}

case ${1:-} in
configure)
	postinst_configure "${2:-}"
	;;
abort-upgrade | abort-remove | abort-deconfigure) ;;
esac
exit 0
