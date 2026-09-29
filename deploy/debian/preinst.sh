
# --- deploy/debian/preinst.sh: the preinst of the Debian package -----------
#
# Refuses (exit 1, nothing unpacked): an installation over one of
# install.sh, a login account named picache (check_user), and a downgrade
# unless PICACHE_ALLOW_DOWNGRADE=1. Never prompts.

ME=picache.preinst
BIN=/usr/bin/picache
UNIT_DIR=/usr/lib/systemd/system
umask 022

GET_PICACHE_URL=https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh

# deb_tag VERSION prints the release tag of a package version
# (0.16.0~rc.1 → v0.16.0-rc.1).
deb_tag() { printf 'v%s' "$1" | tr '~' '-'; }

refuse_install_sh() {
	if [ -e /usr/local/bin/picache ] || [ -L /usr/local/bin/picache ] || [ -e /usr/local/lib/systemd/system/picache.service ]; then
		die "PiCache is installed with install.sh here. Migrate: curl -fsSL $GET_PICACHE_URL | sudo sh -s -- --uninstall (keeps /etc/picache and /var/lib/picache), then apt install this package again"
	fi
}

# refuse_downgrade OLD NEW: a package older than the installed one may not
# open a database the newer one migrated.
refuse_downgrade() {
	[ -n "$1" ] && [ -n "$2" ] || return 0
	dpkg --compare-versions "$2" lt "$1" || return 0
	[ "${PICACHE_ALLOW_DOWNGRADE:-}" != 1 ] || return 0
	data=$(purge_path_of PICACHE_DATA_DIR "$DEFAULT_DATA_DIR")
	arch=${DPKG_MAINTSCRIPT_ARCH:-$(dpkg --print-architecture)}
	file=picache_$(deb_tag "$2" | sed 's/^v//')_$arch.deb
	die "picache $2 is older than the installed $1. $1 may have migrated the database, which $2 then refuses. Restore the copy made before that upgrade (${data:-$DEFAULT_DATA_DIR}/backups/picache-$(deb_tag "$2")-*.db, DEPLOYMENT \"Going back to an earlier version\") or run: sudo PICACHE_ALLOW_DOWNGRADE=1 apt install ./$file"
}

case ${1:-} in
install | upgrade)
	refuse_install_sh
	check_user
	if [ "$1" = upgrade ]; then
		refuse_downgrade "${2:-}" "${3:-}"
	fi
	;;
abort-upgrade) ;;
esac
exit 0
