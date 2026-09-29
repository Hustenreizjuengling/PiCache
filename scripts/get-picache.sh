#!/bin/sh
# Installs, upgrades or removes PiCache from a signed GitHub release (Linux
# with systemd 247 or later: bare metal, VM, Proxmox LXC; supported are the
# families Debian/Ubuntu, Fedora/RHEL, Arch and openSUSE). Run it as root:
#
#   curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh
#   ... | sudo sh -s -- --version v0.1.0 --with-host-apply
#   ... | sudo sh -s -- --uninstall [--purge]
#
# It downloads SHA256SUMS and its signature from the release, verifies the
# signature with the release key below (docs/release-key.pem) and the
# checksums of every other file, and only then runs the release's
# deploy/install.sh. To read it before running it, download it first:
#
#   curl -fsSLO https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh
#   less get-picache.sh && sudo sh get-picache.sh
#
# It needs OpenSSL 3 (Ed25519) and a CA bundle; when they are missing it
# installs them with apt-get, dnf or zypper (on Arch it prints the pacman
# command). It refuses to touch an installation of the Debian package
# (update that with apt).
#
# Everything happens inside main, which is called on the last line, so a
# download that breaks off runs nothing.
set -eu

REPO=Hustenreizjuengling/PiCache
# The trust anchor: the Ed25519 public key that signs every release.
RELEASE_KEY='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAZyR3aUYRK9l9vOA0WP7bTmE1C7LT3nI6IsFYEnqWHj0=
-----END PUBLIC KEY-----'

say() { printf '%s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() {
	printf 'get-picache.sh: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
usage: get-picache.sh [--version vX.Y.Z] [--with-host-apply] [--without-updater]
                      [--with-dhcp | --without-dhcp]
       get-picache.sh --uninstall [--purge] [--yes] [--version vX.Y.Z]

  --version vX.Y.Z    install this release instead of the newest one
  --with-host-apply   also install the root helper for NAS mounts from the web UI
  --without-updater   do not install the update helper (updates only with
                      `sudo picache update`)
  --with-dhcp         allow the DHCP server again after --without-dhcp (it is
                      switched on in the web UI, which works by default)
  --without-dhcp      prevent the DHCP server (PICACHE_DHCP=off): hosts that
                      run another DHCP server
  --uninstall         stop and remove PiCache; configuration and data are kept
  --purge             with --uninstall: also delete the configuration, the
                      data, the local cache and the picache user
  --yes               do not ask before --purge

Environment: PICACHE_RELEASE_BASE replaces
https://github.com/Hustenreizjuengling/PiCache/releases (a mirror or a local
copy with the same layout); the signature is checked all the same.
EOF
}

# fetch URL FILE downloads over HTTPS only (curl or wget).
fetch() {
	case $1 in
	https://* | http://127.0.0.1[:/]* | http://localhost[:/]*) ;;
	*) die "refusing to download from a non-HTTPS URL: $1" ;;
	esac
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL --proto '=https,http' --retry 3 -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is needed"
	fi
}

# arch prints the release name of this machine's architecture.
arch() {
	m=$(uname -m)
	case $m in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	armv7* | armv8l | armhf) echo armv7 ;;
	armv6l) echo armv6 ;;
	i386 | i486 | i586 | i686 | x86) echo 386 ;;
	riscv64) echo riscv64 ;;
	*) die "unsupported architecture $m; PiCache has builds for amd64, arm64, armv7, armv6, 386 and riscv64" ;;
	esac
}

# CA_BUNDLES are the CA bundles of the supported families: Debian, Ubuntu
# and Arch; Fedora and RHEL (the classic name, then the file it points to,
# which newer Fedora releases keep alone); openSUSE.
CA_BUNDLES="/etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/ssl/ca-bundle.pem"

# ca_bundle prints the first CA bundle that exists ("" if none).
ca_bundle() {
	for f in $CA_BUNDLES; do
		if [ -s "$f" ]; then
			printf '%s' "$f"
			return 0
		fi
	done
}

# check_openssl dies unless openssl is version 3 or later: the signature of
# SHA256SUMS is Ed25519, which OpenSSL 1.1 cannot verify with -rawin.
check_openssl() {
	v=$(openssl version 2>/dev/null) || v=""
	major=""
	case $v in
	"OpenSSL "*)
		major=${v#OpenSSL }
		major=${major%%.*}
		;;
	esac
	case $major in
	'' | *[!0-9]*) die "needs OpenSSL 3 for the Ed25519 signature check (found: ${v:-none})" ;;
	esac
	[ "$major" -ge 3 ] || die "needs OpenSSL 3 for the Ed25519 signature check (found: $v)"
}

# check_not_packaged dies when the Debian package installed PiCache here:
# such a host is updated and removed with apt.
check_not_packaged() {
	st=""
	if command -v dpkg-query >/dev/null 2>&1; then
		st=$(dpkg-query -W -f='${Status}' picache 2>/dev/null) || st=""
	fi
	case $st in
	"" | *" not-installed" | *" config-files") [ ! -e /usr/lib/picache/packaged ] && [ ! -L /usr/lib/picache/packaged ] && return 0 ;;
	esac
	die "PiCache is installed as a Debian package here: update it with apt (docs/DEPLOYMENT.md \"Debian package\") or remove it with apt remove picache"
}

# need_tools installs openssl and the CA certificates when they are missing
# (minimal LXC templates): with apt-get, dnf or zypper; on Arch it prints the
# pacman command instead (never -Sy: a partial upgrade). The rest is part of
# every supported system.
need_tools() {
	missing=""
	command -v openssl >/dev/null 2>&1 || missing="$missing openssl"
	[ -n "$(ca_bundle)" ] || missing="$missing ca-certificates"
	if [ -n "$missing" ]; then
		say "installing:$missing"
		if command -v apt-get >/dev/null 2>&1; then
			DEBIAN_FRONTEND=noninteractive apt-get update -qq >/dev/null
			# shellcheck disable=SC2086 # word splitting is intended
			DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends $missing >/dev/null
		elif command -v dnf >/dev/null 2>&1; then
			dnf -y install openssl ca-certificates >/dev/null
		elif command -v zypper >/dev/null 2>&1; then
			zypper --non-interactive install openssl ca-certificates >/dev/null
		elif command -v pacman >/dev/null 2>&1; then
			die "please install them first: pacman -S --needed openssl ca-certificates"
		else
			die "please install:$missing"
		fi
	fi
	check_openssl
	for t in tar gzip sha256sum base64 mktemp; do
		command -v "$t" >/dev/null 2>&1 || die "$t is missing"
	done
}

main() {
	version=""
	uninstall=0
	pass=""
	while [ $# -gt 0 ]; do
		case $1 in
		--version)
			[ $# -ge 2 ] || die "--version needs a value"
			version=$2
			shift 2
			;;
		--version=*)
			version=${1#--version=}
			shift
			;;
		--with-host-apply | --without-updater | --with-dhcp | --without-dhcp | --purge | --yes)
			pass="$pass $1"
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
	if [ -n "$version" ]; then
		printf '%s\n' "$version" | grep -Eqx 'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?' ||
			die "--version must look like v1.2.3 or v1.2.3-rc.1, not \"$version\""
	fi
	if [ "$uninstall" -eq 0 ]; then
		case $pass in *--purge* | *--yes*) die "--purge and --yes only go with --uninstall" ;; esac
	fi

	[ "$(id -u)" -eq 0 ] || die "run as root, for example: curl -fsSL <url> | sudo sh"
	[ -d /run/systemd/system ] || die "systemd is not running here; use the Docker image instead (docs/DEPLOYMENT.md)"
	check_not_packaged
	need_tools
	a=$(arch)

	base=${PICACHE_RELEASE_BASE:-https://github.com/$REPO/releases}
	base=${base%/}
	if [ -n "$version" ]; then
		url=$base/download/$version
	else
		url=$base/latest/download
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM

	say "downloading from $url"
	fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS"
	fetch "$url/SHA256SUMS.sig" "$tmp/SHA256SUMS.sig"
	printf '%s\n' "$RELEASE_KEY" >"$tmp/release-key.pem"
	base64 -d "$tmp/SHA256SUMS.sig" >"$tmp/SHA256SUMS.sig.bin" 2>/dev/null ||
		die "SHA256SUMS.sig is not a valid signature file"
	if ! openssl pkeyutl -verify -pubin -inkey "$tmp/release-key.pem" -rawin \
		-in "$tmp/SHA256SUMS" -sigfile "$tmp/SHA256SUMS.sig.bin" >/dev/null 2>&1; then
		if [ "$(cat /proc/sys/crypto/fips_enabled 2>/dev/null)" = 1 ]; then
			die "the signature check failed in FIPS mode, which may refuse Ed25519: verify the release on another machine (docs/DEPLOYMENT.md) and run install.sh"
		fi
		die "the signature of SHA256SUMS is not valid: the release files are damaged or were not published by PiCache"
	fi
	say "signature of SHA256SUMS verified"

	files="picache-deploy.tar.gz"
	[ "$uninstall" -eq 1 ] || files="$files picache-linux-$a"
	: >"$tmp/want"
	for f in $files; do
		# Text ("  name") or binary ("*name") mode, as sha256sum --check accepts.
		grep -E "^[0-9a-f]{64} [ *]$f\$" "$tmp/SHA256SUMS" >>"$tmp/want" || die "SHA256SUMS lists no $f"
		fetch "$url/$f" "$tmp/$f"
	done
	(cd "$tmp" && sha256sum --check --strict --quiet want) ||
		die "a downloaded file does not match SHA256SUMS"
	say "checksums verified"

	mkdir "$tmp/src"
	tar -xzf "$tmp/picache-deploy.tar.gz" -C "$tmp/src" --no-same-owner
	[ -f "$tmp/src/deploy/install.sh" ] || die "picache-deploy.tar.gz holds no deploy/install.sh"

	if [ "$uninstall" -eq 1 ]; then
		# shellcheck disable=SC2086 # word splitting is intended
		sh "$tmp/src/deploy/install.sh" --uninstall $pass
		return
	fi

	chmod 0755 "$tmp/picache-linux-$a"
	v=$("$tmp/picache-linux-$a" version) || die "the downloaded binary does not run on this machine"
	say "installing $v"
	# shellcheck disable=SC2086 # word splitting is intended
	sh "$tmp/src/deploy/install.sh" --binary "$tmp/picache-linux-$a" $pass
	if token=$(/usr/local/bin/picache setup-token 2>/dev/null) && [ -n "$token" ]; then
		say ""
		say "Setup token (first start only): $token"
	fi
}

main "$@"
