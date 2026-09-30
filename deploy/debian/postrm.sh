
# --- deploy/debian/postrm.sh: the postrm of the Debian package -------------
#
# remove: lists what stays (like install.sh --uninstall). purge: lists what
# it deletes, then runs install.sh's purge in its package mode (default
# paths only, never a mount point; never asks and never fails: whatever it
# cannot or may not delete is kept and listed) and purges the units' enable
# state. After a switch to install.sh (its binary or unit exists, the test of
# the preinst's refusal) the purge of the removed package deletes nothing of
# that installation and only forgets the package's unit state. Never
# prompts.

ME=picache.postrm
BIN=/usr/bin/picache
UNIT_DIR=/usr/lib/systemd/system
umask 022

PKG_UNITS="picache.service picache-storage.path picache-storage.service picache-shared-mounts.service"

# pkg_paths reads the data directory and the mount root like read_paths,
# an invalid value as the default (removing the package never fails on it).
pkg_paths() {
	DATA_DIR=$(purge_path_of PICACHE_DATA_DIR "$DEFAULT_DATA_DIR")
	MOUNT_ROOT=$(purge_path_of PICACHE_MOUNT_ROOT "$DEFAULT_MOUNT_ROOT")
	DATA_DIR=${DATA_DIR:-$DEFAULT_DATA_DIR}
	MOUNT_ROOT=${MOUNT_ROOT:-$DEFAULT_MOUNT_ROOT}
}

postrm_remove() {
	if [ -d /run/systemd/system ]; then
		systemctl daemon-reload || true
	fi
	pkg_paths
	say "PiCache was removed. Kept (apt purge picache deletes the default paths):"
	print_kept
}

# forget_units drops deb-systemd-helper's record of the package's units
# without `deb-systemd-helper purge`, which also deletes the links it
# recorded: install.sh's units have the same names, so after a switch those
# links enable install.sh's PiCache.
forget_units() {
	state=/var/lib/systemd/deb-systemd-helper-enabled
	for unit in $PKG_UNITS; do
		if [ -f "$state/$unit.dsh-also" ]; then
			while IFS= read -r link; do
				case $link in /etc/systemd/system/?*) rm -f -- "$state/${link#/etc/systemd/system/}" ;; esac
			done <"$state/$unit.dsh-also"
			rm -f -- "$state/$unit.dsh-also"
		fi
		rm -f -- "/var/lib/systemd/deb-systemd-helper-masked/$unit"
	done
}

postrm_purge() {
	if [ -e /usr/local/bin/picache ] || [ -L /usr/local/bin/picache ] || [ -e /usr/local/lib/systemd/system/picache.service ]; then
		forget_units
		say "PiCache is installed with install.sh here: its configuration and data were kept (the purge of the"
		say "removed package deletes nothing of that installation)."
		return 0
	fi
	pkg_paths
	say "Purging PiCache: deleting $CONF_DIR, the data, the local cache and the log files in their default"
	say "paths ($DEFAULT_DATA_DIR, $DEFAULT_CACHE_DIR, $DEFAULT_LOG_DIR), the empty mount points below"
	say "$DEFAULT_MOUNT_ROOT, the NAS mount units of the host-apply helper and the account picache (unless kept"
	say "files belong to it)."
	(do_purge package) || warn "the purge did not finish; delete what is left yourself"
	if command -v deb-systemd-helper >/dev/null 2>&1; then
		for unit in $PKG_UNITS; do
			deb-systemd-helper purge "$unit" >/dev/null 2>&1 || true
			deb-systemd-helper unmask "$unit" >/dev/null 2>&1 || true
		done
	fi
}

case ${1:-} in
remove) postrm_remove || true ;;
purge) postrm_purge || true ;;
upgrade | failed-upgrade | abort-install | abort-upgrade | disappear) ;;
esac
exit 0
