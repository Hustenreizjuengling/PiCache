
# --- deploy/debian/prerm.sh: the prerm of the Debian package ---------------
#
# remove: stops and disables PiCache and its host-apply units. upgrade:
# nothing (DNS keeps running while the new files are unpacked; the postinst
# restarts PiCache). Never prompts.

ME=picache.prerm
BIN=/usr/bin/picache
UNIT_DIR=/usr/lib/systemd/system
umask 022

PKG_UNITS="picache.service picache-storage.path picache-storage.service picache-shared-mounts.service"

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
upgrade | deconfigure | failed-upgrade) ;;
esac
exit 0
