#!/bin/sh
# Unit test of scripts/get-picache.sh without Docker or network: it sources
# the script without its last line (which must be exactly main "$@", so no
# test hook is needed) with shell-function stand-ins for uname and openssl
# and a stand-in dpkg-query early in PATH (a name with "-" cannot be a
# portable shell function), and checks the architecture table, the refusal
# of OpenSSL 1.1, the refusal next to the Debian package, the version
# comparison, the downgrade refusal and the latest release older than an
# installed pre-release (nothing to do), and the noexec message.
#
#   scripts/test-get-picache.sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
script=$root/scripts/get-picache.sh
[ "$(tail -n 1 "$script")" = 'main "$@"' ] || {
	echo "FAIL: the last line of get-picache.sh must be exactly: main \"\$@\"" >&2
	exit 1
}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
sed '$d' "$script" >"$tmp/lib.sh"
# dpkg-query prints $DPKG_STATUS (unset: the package is unknown, exit 1).
mkdir "$tmp/bin"
cat >"$tmp/bin/dpkg-query" <<'STUB'
#!/bin/sh
[ -n "${DPKG_STATUS:-}" ] || exit 1
printf '%s' "$DPKG_STATUS"
STUB
chmod 0755 "$tmp/bin/dpkg-query"

# check NAME EXPECTED-OUTPUT EXPECTED-STATUS SHELL-CODE runs the code in a
# fresh shell with the functions of get-picache.sh (die exits that shell).
failures=0
check() {
	out=$(PATH="$tmp/bin:$PATH" sh -c ". \"$tmp/lib.sh\"; $4" 2>&1) && st=0 || st=$?
	case $out in
	*"$2"*) ;;
	*)
		echo "FAIL: $1: output \"$out\", want it to contain \"$2\"" >&2
		failures=$((failures + 1))
		return 0
		;;
	esac
	if [ "$st" -ne "$3" ]; then
		echo "FAIL: $1: exit status $st, want $3" >&2
		failures=$((failures + 1))
	fi
}

# The architecture table (uname -m → release name).
for pair in x86_64:amd64 amd64:amd64 aarch64:arm64 arm64:arm64 armv7l:armv7 armv8l:armv7 armhf:armv7 armv6l:armv6 \
	i386:386 i486:386 i586:386 i686:386 x86:386 riscv64:riscv64; do
	m=${pair%%:*}
	want=${pair#*:}
	check "arch $m" "$want" 0 "uname() { echo $m; }; arch"
done
for m in mips s390x ppc64le armv5tel; do
	check "arch $m" "unsupported architecture $m; PiCache has builds for amd64, arm64, armv7, armv6, 386 and riscv64" 1 \
		"uname() { echo $m; }; arch"
done

# OpenSSL 3 is needed for the Ed25519 check.
check "OpenSSL 3.0" "ok" 0 "openssl() { echo 'OpenSSL 3.0.13 30 Jan 2024 (Library: OpenSSL 3.0.13 30 Jan 2024)'; }; check_openssl; echo ok"
check "OpenSSL 3.5" "ok" 0 "openssl() { echo 'OpenSSL 3.5.1 1 Jul 2025'; }; check_openssl; echo ok"
check "OpenSSL 1.1" "needs OpenSSL 3 for the Ed25519 signature check (found: OpenSSL 1.1.1w  11 Sep 2023)" 1 \
	"openssl() { echo 'OpenSSL 1.1.1w  11 Sep 2023'; }; check_openssl"
check "LibreSSL" "needs OpenSSL 3 for the Ed25519 signature check (found: LibreSSL 3.8.2)" 1 \
	"openssl() { echo 'LibreSSL 3.8.2'; }; check_openssl"
check "no openssl" "needs OpenSSL 3 for the Ed25519 signature check (found: none)" 1 \
	"openssl() { return 127; }; check_openssl"

# The Debian package: refused unless dpkg does not know it (or only its
# configuration files are left).
refusal='PiCache is installed as a Debian package here: update it with apt (docs/DEPLOYMENT.md "Debian package") or remove it with apt remove picache'
check "dpkg: installed" "$refusal" 1 "DPKG_STATUS='install ok installed' check_not_packaged"
check "dpkg: half-configured" "$refusal" 1 "DPKG_STATUS='install ok half-configured' check_not_packaged"
check "dpkg: unpacked" "$refusal" 1 "DPKG_STATUS='install ok unpacked' check_not_packaged"
check "dpkg: config-files" "ok" 0 "DPKG_STATUS='deinstall ok config-files' check_not_packaged; echo ok"
check "dpkg: not-installed" "ok" 0 "DPKG_STATUS='unknown ok not-installed' check_not_packaged; echo ok"
check "dpkg: unknown package" "ok" 0 "check_not_packaged; echo ok"

# SemVer precedence of release versions (as internal/update's Compare).
for pair in v0.16.1:v0.17.0 v0.9.9:v0.10.0 v1.2.3:v1.2.4 v1.9.0:v1.10.0 v0.17.0-rc.1:v0.17.0 v0.17.0-rc.1:v0.17.0-rc.2 \
	v0.17.0-rc.2:v0.17.0-rc.10 v0.17.0-alpha:v0.17.0-alpha.1 v0.17.0-1:v0.17.0-alpha v0.17.1-nightly.20260930.1:v0.17.1 \
	v0.17.1-nightly.20260930.1:v0.17.1-nightly.20261001.1 v0.17.0:v0.17.1-nightly.20260930.1; do
	lo=${pair%%:*}
	hi=${pair#*:}
	check "$lo < $hi" "lt" 0 "if semver_lt $lo $hi; then echo lt; fi"
	check "not $hi < $lo" "ge" 0 "if semver_lt $hi $lo; then echo lt; else echo ge; fi"
done
check "equal" "ge" 0 "if semver_lt v1.2.3 v1.2.3; then echo lt; else echo ge; fi"

# Only release versions are compared (development builds never).
for pair in "picache v0.17.0 (commit abc, built x, go1.27.1, linux/amd64)|v0.17.0" \
	"picache v0.17.0-rc.1 (commit abc)|v0.17.0-rc.1" "picache v0.17.0-3-gabc1234 (commit abc)|" \
	"picache v0.17.0-dirty (commit abc)|" "picache dev (commit none)|" "hello|" \
	"picache v1.1.0-beta-1 (commit abc)|v1.1.0-beta-1" "picache v1.1.0-beta-1-3-gabc1234 (commit abc)|" \
	"picache v1.1.0-beta-1-dirty (commit abc)|" "picache v1.1.0-rc.1-12-g0123456789ab-dirty (commit abc)|"; do
	check "release_version ${pair%%|*}" "[${pair#*|}]" 0 "printf '[%s]' \"\$(release_version '${pair%%|*}')\""
done

# A downgrade is refused unless PICACHE_ALLOW_DOWNGRADE=1; an upgrade, a
# reinstall and development builds are not.
stub() { printf '#!/bin/sh\necho "picache %s (commit x)"\n' "$1" >"$tmp/installed"; chmod 0755 "$tmp/installed"; }
downgrade="INSTALLED_BIN=$tmp/installed ENV_FILE=$tmp/picache.env check_downgrade"
printf 'PICACHE_DATA_DIR="/srv/picache-data"\n' >"$tmp/picache.env"
stub v0.17.0
check "downgrade refused" 'picache v0.16.1 is older than the installed v0.17.0. v0.17.0 may have migrated the database, which v0.16.1 then refuses: PiCache would not start. To go back anyway (docs/DEPLOYMENT.md "Going back to an earlier version"): unless the upgrade notes say that v0.16.1 opens this database, first stop PiCache (sudo systemctl stop picache) and put the copy made before the upgrade (/srv/picache-data/backups/picache-v0.16.1-*.db) in place as /srv/picache-data/picache.db, deleting picache.db-wal and picache.db-shm; then run get-picache.sh again with PICACHE_ALLOW_DOWNGRADE=1 (... | sudo PICACHE_ALLOW_DOWNGRADE=1 sh -s -- --version v0.16.1)' 1 \
	"$downgrade 'picache v0.16.1 (commit y)'"
# The command repeats the options of the run: without --without-updater it
# would install the update helper again.
check "downgrade refused with options" '(... | sudo PICACHE_ALLOW_DOWNGRADE=1 sh -s -- --version v0.16.1 --with-host-apply --without-updater)' 1 \
	"pass=' --with-host-apply --without-updater'; $downgrade 'picache v0.16.1 (commit y)'"
check "downgrade refused, default data directory" '(/var/lib/picache/backups/picache-v0.16.1-*.db) in place as /var/lib/picache/picache.db' 1 \
	"INSTALLED_BIN=$tmp/installed ENV_FILE=$tmp/none check_downgrade 'picache v0.16.1 (commit y)'"
check "downgrade allowed" "ok" 0 "PICACHE_ALLOW_DOWNGRADE=1 $downgrade 'picache v0.16.1 (commit y)'; echo ok"
check "upgrade" "ok" 0 "$downgrade 'picache v0.17.1 (commit y)'; echo ok"
check "reinstall" "ok" 0 "$downgrade 'picache v0.17.0 (commit y)'; echo ok"
check "development build" "ok" 0 "$downgrade 'picache v0.16.1-2-gabc1234 (commit y)'; echo ok"
check "nothing installed" "ok" 0 "INSTALLED_BIN=$tmp/none check_downgrade 'picache v0.1.0 (commit y)'; echo ok"
stub v0.17.0-rc.2
check "pre-release to its release" "ok" 0 "$downgrade 'picache v0.17.0 (commit y)'; echo ok"
check "release candidate downgrade" "is older than the installed v0.17.0-rc.2" 1 "$downgrade 'picache v0.17.0-rc.1 (commit y)'"
# A pre-release with "-" in its identifiers is a release version too.
stub v1.1.0-beta-2
check "pre-release with a hyphen: downgrade" "is older than the installed v1.1.0-beta-2" 1 "$downgrade 'picache v1.1.0-beta-1 (commit y)'"
check "pre-release with a hyphen: upgrade" "ok" 0 "$downgrade 'picache v1.1.0 (commit y)'; echo ok"

# Without --version, a latest release older than the installed one (a
# release candidate newer than the latest stable release) is nothing to do:
# it stops without an error and without the steps to go back. With
# --version (a downgrade asked for, check_downgrade) or
# PICACHE_ALLOW_DOWNGRADE=1 it goes on.
stub v1.0.0-rc.1
latest="INSTALLED_BIN=$tmp/installed ENV_FILE=$tmp/picache.env check_latest"
check "latest older than the installed pre-release" \
	"the installed v1.0.0-rc.1 is newer than the latest release v0.17.0; nothing to do (use --version v1.0.0-rc.1 to reinstall it)" 0 \
	"version=''; $latest 'picache v0.17.0 (commit y)'; echo WENT-ON"
case $(PATH="$tmp/bin:$PATH" sh -c ". \"$tmp/lib.sh\"; version=''; $latest 'picache v0.17.0 (commit y)'; echo WENT-ON" 2>&1) in
*WENT-ON*)
	echo "FAIL: latest older than the installed pre-release: check_latest went on to install" >&2
	failures=$((failures + 1))
	;;
esac
check "latest newer than the installed pre-release" "WENT-ON" 0 "version=''; $latest 'picache v1.0.0 (commit y)'; echo WENT-ON"
check "an older --version is left to check_downgrade" "WENT-ON" 0 "version=v0.17.0; $latest 'picache v0.17.0 (commit y)'; echo WENT-ON"
check "latest older with PICACHE_ALLOW_DOWNGRADE=1" "WENT-ON" 0 \
	"version=''; PICACHE_ALLOW_DOWNGRADE=1 $latest 'picache v0.17.0 (commit y)'; echo WENT-ON"
check "latest older: nothing installed" "WENT-ON" 0 "version=''; INSTALLED_BIN=$tmp/none check_latest 'picache v0.17.0 (commit y)'; echo WENT-ON"

# With --version the downloaded binary must report exactly that release: a
# mirror that serves another signed release (an older one without a fix)
# under download/v0.17.1/ is refused, also on a fresh installation.
check "the release asked for" "ok" 0 "version=v0.17.1; check_version 'picache v0.17.1 (commit y)'; echo ok"
check "another release under the version" "the downloaded binary reports v0.17.0 instead of v0.17.1: the files downloaded for v0.17.1 belong to another release" 1 \
	"version=v0.17.1; check_version 'picache v0.17.0 (commit y)'"
check "a newer release under the version" "reports v0.18.0 instead of v0.17.1" 1 "version=v0.17.1; check_version 'picache v0.18.0 (commit y)'"
check "a development build under the version" "reports no release version instead of v0.17.1" 1 \
	"version=v0.17.1; check_version 'picache v0.17.1-dirty (commit y)'"
check "latest" "ok" 0 "version=''; check_version 'picache v0.16.0 (commit y)'; echo ok"

# A binary that cannot be executed (status 126, as on a noexec /tmp) is
# reported as such, not as the wrong architecture.
printf '#!/bin/sh\necho "picache v0.17.0 (commit x)"\n' >"$tmp/noexec"
chmod 0644 "$tmp/noexec"
check "noexec" "cannot be run in $tmp: its file system is probably mounted noexec. Set TMPDIR" 1 "run_binary $tmp/noexec"
printf '#!/bin/sh\necho "cannot execute binary file" >&2\nexit 2\n' >"$tmp/wrongarch"
chmod 0755 "$tmp/wrongarch"
check "wrong architecture" "the downloaded binary does not run on this machine" 1 "run_binary $tmp/wrongarch"
check "runs" "picache v0.17.0 (commit x)" 0 "chmod 0755 $tmp/noexec; run_binary $tmp/noexec; echo \"\$v\""

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo PASS
