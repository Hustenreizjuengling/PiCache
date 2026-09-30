
# --- deploy/debian/prerm.sh: the prerm of the Debian package ---------------
#
# remove: stops and disables PiCache and its host-apply units. upgrade:
# nothing (DNS keeps running while the new files are unpacked; the postinst
# restarts PiCache), except for an older package without
# PICACHE_ALLOW_DOWNGRADE=1: refused here (exit 1) with the safe order.
# That refusal is not the last word: dpkg then runs the older package's
# prerm with failed-upgrade (which succeeds) and goes on to the older
# package's preinst, which refuses the downgrade too (every package since
# 0.16.0 does), so the package is not replaced. This message comes first
# and says so, because the preinst of a version before 1.0.0 offers
# PICACHE_ALLOW_DOWNGRADE=1 as an alternative to putting the database copy
# back. No change here can stop dpkg earlier: the older package's scripts
# decide the failed-upgrade step. Never prompts.

ME=picache.prerm
BIN=/usr/bin/picache
UNIT_DIR=/usr/lib/systemd/system
umask 022

PKG_UNITS="picache.service picache-storage.path picache-storage.service picache-shared-mounts.service"

# refuse_older NEW refuses the upgrade to the older package version NEW:
# the installed version may have migrated the database, which NEW then
# refuses, and a version before 1.0.0 started on a newer database copies it
# and prunes the copies. So the copy goes back first, then the package.
refuse_older() {
	[ -n "${1:-}" ] && [ "${PICACHE_ALLOW_DOWNGRADE:-}" != 1 ] || return 0
	old=$(dpkg-query -W -f='${Version}' picache 2>/dev/null) || return 0
	[ -n "$old" ] && dpkg --compare-versions "$1" lt "$old" || return 0
	tag=$(printf 'v%s' "$1" | tr '~' '-')
	data=$(purge_path_of PICACHE_DATA_DIR "$DEFAULT_DATA_DIR")
	data=${data:-$DEFAULT_DATA_DIR}
	arch=${DPKG_MAINTSCRIPT_ARCH:-$(dpkg --print-architecture)}
	die "picache $1 is older than the installed $old. $old may have migrated the database, which $1 then refuses: PiCache would not start. To go back anyway (docs/DEPLOYMENT.md \"Going back to an earlier version\"): unless the upgrade notes say that $1 opens this database, first stop PiCache (sudo systemctl stop picache) and put the copy made before the upgrade ($data/backups/picache-$tag-*.db) in place as $data/picache.db, deleting picache.db-wal and picache.db-shm; then run: sudo PICACHE_ALLOW_DOWNGRADE=1 apt install ./picache_${tag#v}_$arch.deb. dpkg now tries the older package's own scripts, which refuse it as well; a package before 1.0.0 then suggests PICACHE_ALLOW_DOWNGRADE=1 as an alternative: follow the order given here instead."
}

case ${1:-} in
remove)
	for unit in $PKG_UNITS; do
		if [ -d /run/systemd/system ]; then
			deb-systemd-invoke stop "$unit" >/dev/null 2>&1 || true
			systemctl disable "$unit" >/dev/null 2>&1 || true
		else
			deb-systemd-helper disable "$unit" >/dev/null 2>&1 || true
		fi
	done
	;;
upgrade) refuse_older "${2:-}" ;;
deconfigure | failed-upgrade) ;;
esac
exit 0
