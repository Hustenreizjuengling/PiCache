#!/bin/sh
# Builds the Debian package of one architecture for `make dist`
# (docs/DEPLOYMENT.md "Debian package", docs/ARCHITECTURE.md 14.1):
#
#   SOURCE_DATE_EPOCH=… scripts/build-deb.sh DIST TAG DEBARCH MAINTAINER
#
# It writes DIST/picache_<TAG without v>_<DEBARCH>.deb from a staging tree
# below DIST (removed afterwards) with the binary of that architecture from
# DIST (amd64, arm64, armhf ← armv6, i386 ← 386, riscv64): /usr/bin/picache,
# the units of deploy/systemd with /usr/local/bin/picache replaced by
# /usr/bin/picache (and nothing else) in /usr/lib/systemd/system (never the
# update helper's), the package marker /usr/lib/picache/packaged and the
# license texts; every file and directory has the time SOURCE_DATE_EPOCH and
# root as its owner, so the same inputs give the same package. The
# maintainer scripts are deploy/install.sh without its first and last line
# followed by deploy/debian/<name>.sh. Needs dpkg-deb.
set -eu
umask 022

[ $# -eq 4 ] || {
	echo "usage: $0 DIST TAG DEBARCH MAINTAINER" >&2
	exit 2
}
dist=$1
tag=$2
arch=$3
maintainer=$4
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

die() {
	printf 'build-deb.sh: %s\n' "$*" >&2
	exit 1
}

case ${SOURCE_DATE_EPOCH:-} in '' | *[!0-9]*) die "SOURCE_DATE_EPOCH must be set" ;; esac
command -v dpkg-deb >/dev/null 2>&1 || die "needs dpkg-deb (package dpkg)"
version=$(sh "$root/scripts/deb-version.sh" "$tag")

# The binary of each Debian architecture: armhf takes the GOARM=6 build,
# which runs on ARMv6 Raspberry Pi OS (armhf too) and on ARMv7.
case $arch in
amd64 | arm64 | riscv64) bin=picache-linux-$arch ;;
armhf) bin=picache-linux-armv6 ;;
i386) bin=picache-linux-386 ;;
*) die "no binary for the Debian architecture $arch" ;;
esac
[ -f "$dist/$bin" ] || die "$dist/$bin is missing"

units="picache.service picache-storage.service picache-storage.path picache-shared-mounts.service"
stage=$dist/deb-$arch
rm -rf "$stage"
trap 'rm -rf "$stage"' EXIT

install -d -m 0755 "$stage/DEBIAN" "$stage/usr/bin" "$stage/usr/lib/systemd/system" "$stage/usr/lib/picache" \
	"$stage/usr/share/doc/picache"
install -m 0755 "$dist/$bin" "$stage/usr/bin/picache"
for u in $units; do
	sed 's#/usr/local/bin/picache#/usr/bin/picache#g' "$root/deploy/systemd/$u" >"$stage/usr/lib/systemd/system/$u"
	chmod 0644 "$stage/usr/lib/systemd/system/$u"
done
printf 'deb\n' >"$stage/usr/lib/picache/packaged"
chmod 0644 "$stage/usr/lib/picache/packaged"
install -m 0644 "$root/LICENSE" "$stage/usr/share/doc/picache/copyright"
install -m 0644 "$root/LICENSE" "$root/THIRD_PARTY_NOTICES.md" "$stage/usr/share/doc/picache/"

# The maintainer scripts share install.sh's functions: install.sh without
# its first line (#!/bin/sh) and its last (main "$@"), then the fragment.
last=$(tail -n 1 "$root/deploy/install.sh")
[ "$last" = 'main "$@"' ] || die "the last line of deploy/install.sh must be exactly: main \"\$@\""
for name in preinst postinst prerm postrm; do
	out=$stage/DEBIAN/$name
	{
		echo '#!/bin/sh'
		echo "# The $name of the picache package: generated from deploy/install.sh and"
		echo "# deploy/debian/$name.sh by make dist; do not edit."
		sed '1d;$d' "$root/deploy/install.sh"
		cat "$root/deploy/debian/$name.sh"
	} >"$out"
	chmod 0755 "$out"
	sh -n "$out" || die "DEBIAN/$name has a syntax error"
done

(cd "$stage" && find usr -type f | LC_ALL=C sort | xargs md5sum) >"$stage/DEBIAN/md5sums"
chmod 0644 "$stage/DEBIAN/md5sums"
size=$(du -sk --apparent-size "$stage/usr" | cut -f1)
sed -e "s/@VERSION@/$version/" -e "s/@ARCH@/$arch/" -e "s/@INSTALLED_SIZE@/$size/" \
	-e "s|@MAINTAINER@|$maintainer|" "$root/deploy/debian/control.in" >"$stage/DEBIAN/control"
chmod 0644 "$stage/DEBIAN/control"
find "$stage" -exec touch -h -d "@$SOURCE_DATE_EPOCH" {} +

file=picache_${tag#v}_$arch.deb
rm -f "$dist/$file"
SOURCE_DATE_EPOCH=$SOURCE_DATE_EPOCH dpkg-deb --root-owner-group -Zxz --build "$stage" "$dist/$file" >/dev/null
touch -d "@$SOURCE_DATE_EPOCH" "$dist/$file"
echo "built $dist/$file ($version, $bin)"
