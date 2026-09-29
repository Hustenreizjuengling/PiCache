#!/bin/sh
# Checks the release assets that `make dist` wrote (the CI dist job and the
# build jobs of release.yml and nightly.yml run it):
#
#   scripts/check-dist.sh DIST TAG
#
#   - SHA256SUMS matches every file it lists;
#   - every binary is a linux build of its architecture (GOARCH, GOARM,
#     GO386=sse2 for 386; go version -m) that holds the version TAG;
#   - every Debian package has the name, version, architecture and binary
#     of its architecture, the units of deploy/systemd with
#     /usr/local/bin/picache replaced by /usr/bin/picache, the package
#     marker and no unit of the update helper;
#   - picache-deploy.tar.gz is smaller than 1 MiB, has fewer than 200
#     entries, only deploy/…, LICENSE and THIRD_PARTY_NOTICES.md, no file
#     larger than 256 KiB and none with a NUL byte in its first 8 KiB (the
#     update helper reads it into memory: docs/ARCHITECTURE.md 14.3).
#
# Needs go, dpkg-deb and GNU tar.
set -eu

[ $# -eq 2 ] || {
	echo "usage: $0 DIST TAG" >&2
	exit 2
}
dist=$(CDPATH='' cd -- "$1" && pwd)
tag=$2
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
fails=0
fail() {
	printf 'check-dist.sh: %s\n' "$*" >&2
	fails=$((fails + 1))
}

# The values of the Makefile (one line each).
make_list() { sed -n "s/^$1 := //p" "$root/Makefile"; }
binaries=$(make_list BINARIES)
deb_arches=$(make_list DEB_ARCHES)
[ -n "$binaries" ] && [ -n "$deb_arches" ] || {
	echo "check-dist.sh: BINARIES or DEB_ARCHES not found in the Makefile" >&2
	exit 2
}

(cd "$dist" && sha256sum --check --strict --quiet SHA256SUMS) || fail "SHA256SUMS does not match"

for b in $binaries; do
	f=$dist/$b
	if [ ! -f "$f" ]; then
		fail "$b is missing"
		continue
	fi
	case ${b#picache-linux-} in
	armv6) want="GOARCH=arm GOARM=6" ;;
	armv7) want="GOARCH=arm GOARM=7" ;;
	386) want="GOARCH=386 GO386=sse2" ;;
	*) want="GOARCH=${b#picache-linux-}" ;;
	esac
	info=$(go version -m "$f") || {
		fail "$b: go version -m failed"
		continue
	}
	for w in GOOS=linux $want; do
		printf '%s\n' "$info" | grep -Eq "^[[:space:]]+build[[:space:]]+$w\$" || fail "$b: not built with $w"
	done
	# -trimpath keeps -ldflags out of the build information, so the version
	# set with -X is looked for in the binary itself.
	grep -aqF -- "$tag" "$f" || fail "$b: does not hold the version $tag"
done

version=$(sh "$root/scripts/deb-version.sh" "$tag")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for a in $deb_arches; do
	deb=$dist/picache_${tag#v}_$a.deb
	if [ ! -f "$deb" ]; then
		fail "picache_${tag#v}_$a.deb is missing"
		continue
	fi
	case $a in
	armhf) bin=picache-linux-armv6 ;;
	i386) bin=picache-linux-386 ;;
	*) bin=picache-linux-$a ;;
	esac
	[ "$(dpkg-deb -f "$deb" Package)" = picache ] || fail "$a: Package is not picache"
	[ "$(dpkg-deb -f "$deb" Version)" = "$version" ] || fail "$a: Version is not $version"
	[ "$(dpkg-deb -f "$deb" Architecture)" = "$a" ] || fail "$a: Architecture is not $a"
	x=$tmp/$a
	mkdir "$x"
	dpkg-deb -x "$deb" "$x"
	cmp -s "$x/usr/bin/picache" "$dist/$bin" || fail "$a: usr/bin/picache is not $bin"
	for u in picache.service picache-storage.service picache-storage.path picache-shared-mounts.service; do
		sed 's#/usr/local/bin/picache#/usr/bin/picache#g' "$root/deploy/systemd/$u" >"$tmp/unit"
		cmp -s "$x/usr/lib/systemd/system/$u" "$tmp/unit" || fail "$a: usr/lib/systemd/system/$u differs from deploy/systemd/$u"
	done
	[ "$(cat "$x/usr/lib/picache/packaged" 2>/dev/null)" = deb ] || fail "$a: the package marker is missing"
	if [ -n "$(find "$x" -name 'picache-update.*' -print)" ]; then
		fail "$a: contains a unit of the update helper"
	fi
	if dpkg-deb -c "$deb" | grep -q '/usr/local/'; then
		fail "$a: installs below /usr/local"
	fi
done

tgz=$dist/picache-deploy.tar.gz
size=$(wc -c <"$tgz")
[ "$size" -lt 1048576 ] || fail "picache-deploy.tar.gz has $size bytes (at most 1 MiB)"
entries=$(tar -tzf "$tgz" | wc -l)
[ "$entries" -lt 200 ] || fail "picache-deploy.tar.gz has $entries entries (fewer than 200)"
tar -tzf "$tgz" | while read -r e; do
	case $e in
	deploy/* | LICENSE | THIRD_PARTY_NOTICES.md) ;;
	*) echo "$e" ;;
	esac
done >"$tmp/foreign"
[ ! -s "$tmp/foreign" ] || fail "picache-deploy.tar.gz holds other files: $(tr '\n' ' ' <"$tmp/foreign")"
mkdir "$tmp/deploy"
tar -xzf "$tgz" -C "$tmp/deploy"
find "$tmp/deploy" -type f -size +256k -print >"$tmp/big"
[ ! -s "$tmp/big" ] || fail "picache-deploy.tar.gz holds files larger than 256 KiB: $(tr '\n' ' ' <"$tmp/big")"
find "$tmp/deploy" -type f | while read -r f; do
	all=$(head -c 8192 "$f" | wc -c)
	text=$(head -c 8192 "$f" | tr -d '\000' | wc -c)
	[ "$all" -eq "$text" ] || echo "${f#"$tmp/deploy/"}"
done >"$tmp/binary"
[ ! -s "$tmp/binary" ] || fail "picache-deploy.tar.gz holds binary files: $(tr '\n' ' ' <"$tmp/binary")"

if [ "$fails" -gt 0 ]; then
	echo "check-dist.sh: $fails problem(s) in $dist" >&2
	exit 1
fi
echo "check-dist.sh: $dist is fine ($tag)"
