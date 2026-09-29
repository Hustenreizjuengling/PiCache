#!/bin/sh
# Unit test of scripts/get-picache.sh without Docker or network: it sources
# the script without its last line (which must be exactly main "$@", so no
# test hook is needed) with shell-function stand-ins for uname and openssl
# and a stand-in dpkg-query early in PATH (a name with "-" cannot be a
# portable shell function), and checks the architecture table, the refusal
# of OpenSSL 1.1 and the refusal next to the Debian package.
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

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo PASS
