#!/bin/sh
# Prints the Debian package version of a release tag (docs/DEPLOYMENT.md
# "Debian package"): the tag without its v, the hyphen before the
# pre-release replaced by ~ so that a pre-release sorts before its release:
#
#   scripts/deb-version.sh v0.16.0                      → 0.16.0
#   scripts/deb-version.sh v0.16.0-rc.1                 → 0.16.0~rc.1
#   scripts/deb-version.sh v0.16.1-nightly.20260927.1   → 0.16.1~nightly.20260927.1
#
# A pre-release that itself contains "-" is refused (Debian would sort it
# differently from SemVer); so is anything that is not vX.Y.Z[-pre].
set -eu

[ $# -eq 1 ] || {
	echo "usage: $0 TAG" >&2
	exit 2
}
tag=$1
core=${tag%%-*}
pre=""
case $tag in *-*) pre=${tag#*-} ;; esac
if ! printf '%s\n' "$core" | grep -Eqx 'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'; then
	echo "deb-version.sh: \"$tag\" is not a release version such as v1.2.3 or v1.2.3-rc.1" >&2
	exit 1
fi
case $pre in
*-*)
	echo "deb-version.sh: the pre-release part must not contain '-' (Debian package versions): \"$tag\"" >&2
	exit 1
	;;
*[!0-9A-Za-z.]*)
	echo "deb-version.sh: \"$tag\" is not a release version such as v1.2.3 or v1.2.3-rc.1" >&2
	exit 1
	;;
esac
case $tag in
*-) echo "deb-version.sh: \"$tag\" has an empty pre-release" >&2; exit 1 ;;
esac
if [ -n "$pre" ]; then
	printf '%s~%s\n' "${core#v}" "$pre"
else
	printf '%s\n' "${core#v}"
fi
