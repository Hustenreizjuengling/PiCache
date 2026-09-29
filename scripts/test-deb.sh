#!/bin/sh
# Test of the Debian package (docs/DEPLOYMENT.md "Debian package") in a
# throwaway container: fresh install (files, modes, units, marker, account,
# no update helper, no token in the output, not even from the journal),
# `picache update` refused, a reinstall that keeps the configuration, an
# upgrade (restart, healthy), a failed restart (the rollback steps, exit 0),
# a downgrade refused and then allowed with PICACHE_ALLOW_DOWNGRADE=1, a
# failed start and a port-53 conflict on a fresh install (the package stays
# installed), a policy-rc.d that forbids services (neither started nor
# restarted, and no failure reported), remove and purge (/var/log/picache
# deleted too; the account kept and locked for a custom PICACHE_DATA_DIR and
# for a PICACHE_CACHE_DIR that is not a plain absolute path; a mount below
# /srv/picache kept and listed), the refusals between the package,
# install.sh and get-picache.sh and of a login account named picache,
# scripts/deb-version.sh against dpkg's version order and
# `systemd-analyze verify` of the installed units.
#
#   make dist VERSION=v0.0.0-ci && scripts/test-deb.sh dist [debian:13]
#
# systemd does not run in the container: systemctl, journalctl,
# deb-systemd-invoke and deb-systemd-helper are stand-ins that record their
# arguments; `systemctl start|restart picache.service` starts the real
# /usr/bin/picache serve (as picache after binding, PICACHE_RUN_AS) unless
# /tmp/fail-start exists. Needs Docker and network access (apt-get).
set -eu

[ $# -ge 1 ] || {
	echo "usage: $0 DIST [IMAGE]" >&2
	exit 2
}
dist=$(CDPATH='' cd -- "$1" && pwd)
image=${2:-debian:13}
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
set -- "$dist"/picache_*_amd64.deb
[ -f "$1" ] || {
	echo "no picache_*_amd64.deb in $dist" >&2
	exit 2
}

for mode in main mount; do
	echo "=== $image: $mode"
	extra=""
	# A tmpfs below the mount root, for the purge that must keep it.
	[ "$mode" = mount ] && extra="--tmpfs=/srv/picache/vol:mode=0755"
	# shellcheck disable=SC2086 # $extra is one option or nothing
	docker run --rm -i $extra \
		-v "$dist:/dist:ro" \
		-v "$root/deploy:/src/deploy:ro" \
		-v "$root/scripts:/src/scripts:ro" \
		"$image" sh -s -- "$mode" <<'EOF'
set -eu
mode=$1
fail() { echo "FAIL: $*" >&2; exit 1; }
check_mode() {
	got=$(stat -c '%a %U:%G' "$1") || fail "$1 is missing"
	[ "$got" = "$2 $3" ] || fail "$1: got '$got', want '$2 $3'"
}
status() { dpkg-query -W -f='${Status}' picache 2>/dev/null || true; }

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq iproute2 netcat-openbsd systemd init-system-helpers dnsutils >/dev/null

deb=$(ls /dist/picache_*_amd64.deb)
version=$(dpkg-deb -f "$deb" Version)

# systemd stand-ins in /usr/sbin: before the real ones (in /usr/bin) in the
# PATH of the shell and in the one apt gives dpkg (DPkg::Path).
mkdir -p /run/systemd/system
cat >/usr/sbin/systemctl <<'STUB'
#!/bin/sh
echo "$*" >>/tmp/systemctl.log
pidf=/tmp/picache.pid
running() { [ -f "$pidf" ] && kill -0 "$(cat "$pidf")" 2>/dev/null; }
stop() {
	if running; then kill "$(cat "$pidf")"; sleep 1; fi
	rm -f "$pidf"
}
case "$1 $*" in
--version*) echo "systemd 257 (257-stand-in)"; exit 0 ;;
"start start picache.service" | "restart restart picache.service")
	[ ! -e /tmp/fail-start ] || exit 1
	stop
	# StateDirectory=, CacheDirectory=, LogsDirectory= of the unit.
	install -d -o picache -g picache -m 0750 /var/lib/picache /var/cache/picache /var/log/picache
	PICACHE_RUN_AS=$(id -u picache):$(id -g picache) nohup /usr/bin/picache serve >>/tmp/picache.log 2>&1 &
	echo $! >"$pidf"
	exit 0
	;;
"stop stop picache.service") stop; exit 0 ;;
"is-active is-active --quiet picache.service") running; exit $? ;;
esac
exit 0
STUB
# The journal holds the setup token until the setup is done; the scripts
# must not print that line (apt keeps their output).
cat >/usr/sbin/journalctl <<'STUB'
#!/bin/sh
echo "stand-in journal of $*"
echo "level=WARN msg=\"first-run setup required\" setupToken=STANDINSETUPTOKEN file=/var/lib/picache/setup-token"
STUB
printf '#!/bin/sh\necho "invoke $*" >>/tmp/systemctl.log\nexec systemctl "$@"\n' >/usr/sbin/deb-systemd-invoke
printf '#!/bin/sh\necho "helper $*" >>/tmp/systemctl.log\nexit 0\n' >/usr/sbin/deb-systemd-helper
chmod 0755 /usr/sbin/systemctl /usr/sbin/journalctl /usr/sbin/deb-systemd-invoke /usr/sbin/deb-systemd-helper
: >/tmp/systemctl.log
# Docker's Debian images forbid service starts (policy-rc.d exits 101); a
# host has no such policy (a case below puts one back).
rm -f /usr/sbin/policy-rc.d

# repack VERSION FILE: the package with another version.
repack() {
	rm -rf /tmp/repack
	dpkg-deb -R "$deb" /tmp/repack
	sed -i "s/^Version: .*/Version: $1/" /tmp/repack/DEBIAN/control
	dpkg-deb --root-owner-group -b /tmp/repack "$2" >/dev/null
}
install_deb() { apt-get install -y -qq --allow-downgrades "$@" 2>&1; }
wait_healthy() {
	i=0
	until /usr/bin/picache healthcheck >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -lt 60 ] || fail "PiCache did not become healthy: $(tail -n 20 /tmp/picache.log)"
		sleep 1
	done
}

if [ "$mode" = mount ]; then
	echo "== purge with a volume mounted below /srv/picache"
	findmnt /srv/picache/vol >/dev/null || fail "test setup: no mount below /srv/picache"
	install_deb "$deb" >/tmp/out || { cat /tmp/out; fail "install"; }
	wait_healthy
	apt-get remove -y -qq picache >/dev/null
	out=$(apt-get purge -y picache 2>&1) || { echo "$out"; fail "purge failed"; }
	echo "$out" | tr -d '\r' | grep -qx '  /srv/picache' || fail "the mount root was not listed as kept: $out"
	[ -d /srv/picache/vol ] || fail "the mounted volume was removed"
	[ ! -e /etc/picache ] && [ ! -e /var/lib/picache ] || fail "the default paths were not deleted"
	[ "$(status)" = "" ] || [ "$(status)" = "unknown ok not-installed" ] || fail "status after purge: $(status)"
	echo "PASS ($mode)"
	exit 0
fi

echo "== deb-version.sh follows dpkg's version order"
prev=""
for tag in v0.16.0-rc.1 v0.16.0-rc.2 v0.16.0-rc.10 v0.16.0 v0.16.1-nightly.20261001.1 v0.16.1-nightly.20261001.2 \
	v0.16.1-nightly.20261002.1 v0.16.1-rc.1 v0.16.1; do
	v=$(sh /src/scripts/deb-version.sh "$tag")
	if [ -n "$prev" ]; then
		dpkg --compare-versions "$prev" lt "$v" || fail "$prev is not before $v"
	fi
	prev=$v
done
[ "$(sh /src/scripts/deb-version.sh v0.16.0-rc.1)" = 0.16.0~rc.1 ] || fail "deb-version.sh v0.16.0-rc.1"
if sh /src/scripts/deb-version.sh v0.16.0-beta-2 2>/dev/null; then fail "a pre-release with - was accepted"; fi

echo "== the preinst refuses a login account named picache"
useradd -m -s /bin/bash picache
out=$(install_deb "$deb") && fail "installed next to a login account"
echo "$out" | grep -q "a login account named picache exists" || fail "no login account message: $out"
[ ! -e /usr/bin/picache ] || fail "files were unpacked"
dpkg --purge picache >/dev/null 2>&1 || true
userdel -r picache 2>/dev/null
groupdel picache 2>/dev/null || true

echo "== the package refuses an install.sh installation"
install -d /usr/local/bin
touch /usr/local/bin/picache
out=$(install_deb "$deb") && fail "installed over install.sh"
echo "$out" | grep -q "PiCache is installed with install.sh here. Migrate: curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh -s -- --uninstall" ||
	fail "no migration message: $out"
rm -f /usr/local/bin/picache
dpkg --purge picache >/dev/null 2>&1 || true

echo "== fresh install"
out=$(install_deb "$deb") || { echo "$out"; fail "install"; }
echo "$out" | grep -q 'Setup token:   sudo picache setup-token' || fail "the summary lacks the setup token command: $out"
echo "$out" | grep -q 'PiCache is running.' || fail "not started: $out"
[ "$(status)" = "install ok installed" ] || fail "status $(status)"
check_mode /usr/bin/picache 755 root:root
for u in picache.service picache-storage.service picache-storage.path picache-shared-mounts.service; do
	check_mode "/usr/lib/systemd/system/$u" 644 root:root
	sed 's#/usr/local/bin/picache#/usr/bin/picache#g' "/src/deploy/systemd/$u" | cmp -s - "/usr/lib/systemd/system/$u" ||
		fail "$u is not the unit of deploy/systemd with /usr/bin/picache"
done
for u in picache-update.service picache-update.path; do
	[ ! -e "/usr/lib/systemd/system/$u" ] || fail "$u was installed"
done
[ ! -e /etc/picache/updater.enabled ] || fail "the update helper marker was written"
check_mode /usr/lib/picache/packaged 644 root:root
[ "$(cat /usr/lib/picache/packaged)" = deb ] || fail "marker content"
check_mode /usr/share/doc/picache/copyright 644 root:root
check_mode /etc/picache 750 root:picache
check_mode /etc/picache/picache.env 640 root:picache
check_mode /srv/picache 750 root:picache
entry=$(getent passwd picache) || fail "no account"
uid=$(echo "$entry" | cut -d: -f3)
[ "$uid" -lt 1000 ] || fail "not a system account: $entry"
case $entry in */nologin) ;; *) fail "login shell: $entry" ;; esac
grep -q '^enable picache.service' /tmp/systemctl.log || fail "picache.service not enabled"
if grep -q 'picache-storage.path\|picache-update' /tmp/systemctl.log; then fail "host-apply or update units enabled"; fi
wait_healthy
token=$(/usr/bin/picache setup-token 2>/dev/null) || fail "no setup token"
if echo "$out" | grep -qF "$token"; then fail "the setup token was printed"; fi
out=$(systemd-analyze verify /usr/lib/systemd/system/picache*.service /usr/lib/systemd/system/picache*.path 2>&1 || true)
if echo "$out" | grep -E 'Unknown key|Unknown lvalue|Unknown section'; then fail "systemd-analyze verify: $out"; fi

echo "== picache update is refused"
out=$(/usr/bin/picache update --yes 2>&1) && fail "picache update ran"
echo "$out" | grep -q 'PiCache was installed as a Debian package: download picache_<version>_amd64.deb from the release' ||
	fail "no package hint: $out"
out=$(/usr/bin/picache update apply-pending 2>&1) && fail "apply-pending ran"

echo "== install.sh and get-picache.sh refuse the package"
refusal='PiCache is installed as a Debian package here: update it with apt (docs/DEPLOYMENT.md "Debian package") or remove it with apt remove picache'
cp /usr/bin/picache /tmp/picache-bin
for args in "--binary /tmp/picache-bin" "--uninstall" "--uninstall --purge --yes"; do
	# shellcheck disable=SC2086
	out=$(sh /src/deploy/install.sh $args 2>&1) && fail "install.sh $args ran"
	echo "$out" | grep -qF "$refusal" || fail "install.sh $args: $out"
done
out=$(sh /src/scripts/get-picache.sh 2>&1) && fail "get-picache.sh ran"
echo "$out" | grep -qF "$refusal" || fail "get-picache.sh: $out"
[ -e /usr/bin/picache ] && [ -e /usr/lib/systemd/system/picache.service ] || fail "a refusal removed files"

echo "== reinstall keeps the configuration"
echo 'PICACHE_LOG_LEVEL=debug' >>/etc/picache/picache.env
install_deb --reinstall "$deb" >/dev/null || fail "reinstall"
grep -q '^PICACHE_LOG_LEVEL=debug' /etc/picache/picache.env || fail "env file overwritten"
check_mode /etc/picache/picache.env 640 root:picache

echo "== upgrade: restart, healthy"
repack 99.0.0 /tmp/up.deb
: >/tmp/systemctl.log
out=$(install_deb /tmp/up.deb) || { echo "$out"; fail "upgrade"; }
grep -q '^invoke restart picache.service' /tmp/systemctl.log || fail "not restarted on upgrade"
echo "$out" | grep -q 'PiCache was restarted and is healthy.' || fail "no health message: $out"

echo "== upgrade with a failed restart: the rollback steps, the package stays installed"
repack 99.0.1 /tmp/up2.deb
touch /tmp/fail-start
out=$(install_deb /tmp/up2.deb) || { echo "$out"; fail "a failed restart failed the upgrade"; }
rm -f /tmp/fail-start
echo "$out" | grep -q 'PICACHE_ALLOW_DOWNGRADE=1 apt install ./picache_99.0.0_amd64.deb' || fail "no rollback steps: $out"
echo "$out" | grep -q '/var/lib/picache/backups/picache-v99.0.0-<timestamp>.db' || fail "no database step: $out"
echo "$out" | grep -q 'stand-in journal of' || fail "no log lines: $out"
if echo "$out" | grep -q STANDINSETUPTOKEN; then fail "the setup token was printed: $out"; fi
[ "$(status)" = "install ok installed" ] || fail "status $(status)"

echo "== downgrade refused, then allowed"
out=$(install_deb "$deb") && fail "downgraded without PICACHE_ALLOW_DOWNGRADE"
echo "$out" | grep -q "picache $version is older than the installed 99.0.1" || fail "no downgrade message: $out"
echo "$out" | grep -q "sudo PICACHE_ALLOW_DOWNGRADE=1 apt install ./picache_" || fail "no downgrade command: $out"
[ "$(dpkg-query -W -f='${Version}' picache)" = 99.0.1 ] || fail "the version changed"
PICACHE_ALLOW_DOWNGRADE=1 install_deb "$deb" >/dev/null || fail "PICACHE_ALLOW_DOWNGRADE=1 was refused"
[ "$(dpkg-query -W -f='${Version}' picache)" = "$version" ] || fail "not downgraded"
wait_healthy

echo "== remove: data and account kept"
: >/tmp/systemctl.log
out=$(apt-get remove -y picache 2>&1) || { echo "$out"; fail "remove"; }
grep -q '^invoke stop picache.service' /tmp/systemctl.log || fail "not stopped"
echo "$out" | grep -q 'configuration   /etc/picache' || fail "kept paths not listed: $out"
[ ! -e /usr/bin/picache ] && [ ! -e /usr/lib/systemd/system/picache.service ] || fail "files left"
[ -e /etc/picache/picache.env ] && [ -d /var/lib/picache ] || fail "data removed"
getent passwd picache >/dev/null || fail "account removed"

echo "== purge: default paths and the account deleted"
out=$(apt-get purge -y picache 2>&1) || { echo "$out"; fail "purge"; }
for p in /etc/picache /var/lib/picache /var/cache/picache /var/log/picache /srv/picache; do
	[ ! -e "$p" ] || fail "$p left after purge: $out"
done
if getent passwd picache >/dev/null; then fail "account left after purge"; fi
if getent group picache >/dev/null; then fail "group left after purge"; fi

echo "== fresh install with a failed start"
touch /tmp/fail-start
out=$(install_deb "$deb") || { echo "$out"; fail "a failed start failed the install"; }
rm -f /tmp/fail-start
echo "$out" | grep -q 'Fix it, then: systemctl start picache' || fail "no fix hint: $out"
echo "$out" | grep -q 'stand-in journal of' || fail "no log lines: $out"
if echo "$out" | grep -q STANDINSETUPTOKEN; then fail "the setup token was printed: $out"; fi
[ "$(status)" = "install ok installed" ] || fail "status $(status)"
apt-get purge -y -qq picache >/dev/null

echo "== a policy-rc.d that forbids services (container images): installed, not started, no failure"
printf '#!/bin/sh\nexit 101\n' >/usr/sbin/policy-rc.d
chmod 0755 /usr/sbin/policy-rc.d
: >/tmp/systemctl.log
out=$(install_deb "$deb") || { echo "$out"; fail "install with policy-rc.d"; }
echo "$out" | grep -q 'not started: /usr/sbin/policy-rc.d forbids starting services here' || fail "no policy-rc.d message: $out"
if echo "$out" | grep -q 'did not start'; then fail "reported as a failed start: $out"; fi
if grep -q 'start picache.service' /tmp/systemctl.log; then fail "started despite policy-rc.d"; fi
[ "$(status)" = "install ok installed" ] || fail "status $(status)"
repack 99.0.2 /tmp/up3.deb
out=$(install_deb /tmp/up3.deb) || { echo "$out"; fail "upgrade with policy-rc.d"; }
echo "$out" | grep -q 'not restarted: /usr/sbin/policy-rc.d forbids restarting services here' || fail "no policy-rc.d message on upgrade: $out"
if echo "$out" | grep -q 'did not become healthy'; then fail "reported as a failed upgrade: $out"; fi
if grep -q 'restart picache.service' /tmp/systemctl.log; then fail "restarted despite policy-rc.d"; fi
rm -f /usr/sbin/policy-rc.d
apt-get purge -y -qq picache >/dev/null

echo "== purge with an unreadable PICACHE_CACHE_DIR: the account is kept and locked"
install_deb "$deb" >/tmp/out || { cat /tmp/out; fail "install"; }
install -d -o picache -g picache -m 0750 "/srv/p cache"
install -o picache -g picache -m 0640 /dev/null "/srv/p cache/slice"
echo 'PICACHE_CACHE_DIR="/srv/p cache"' >>/etc/picache/picache.env
uid=$(id -u picache)
out=$(apt-get purge -y picache 2>&1) || { echo "$out"; fail "purge"; }
# (/var/cache/picache, which systemd creates anyway, is kept and checked too.)
echo "$out" | grep -q "kept the account picache (uid $uid) because .*the purge cannot check the directories of PICACHE_CACHE_DIR; it is locked" ||
	fail "no locked-account message: $out"
echo "$out" | grep -qF '  PICACHE_CACHE_DIR="/srv/p cache"  (not a plain absolute path: not checked)' || fail "not listed: $out"
passwd -S picache | awk '{ exit $2 != "L" }' || fail "account not locked: $(passwd -S picache)"
[ -e "/srv/p cache/slice" ] || fail "the cache was deleted"
[ ! -e /etc/picache ] || fail "/etc/picache left"
userdel picache
groupdel picache 2>/dev/null || true
rm -rf "/srv/p cache" /var/lib/picache /var/cache/picache /var/log/picache

echo "== fresh install with port 53 in use"
nc -lu 127.0.0.53 53 &
udp=$!
sleep 1
: >/tmp/systemctl.log
out=$(install_deb "$deb") || { echo "$out"; fail "a port conflict failed the install"; }
kill "$udp"
echo "$out" | grep -q 'installed and enabled but not started' || fail "no port-53 message: $out"
echo "$out" | grep -q 'DNSStubListener=no' || fail "no resolved fix: $out"
if grep -q 'start picache.service' /tmp/systemctl.log; then fail "started despite the conflict"; fi
[ "$(status)" = "install ok installed" ] || fail "status $(status)"

echo "== purge with a custom PICACHE_DATA_DIR: the account is kept and locked"
install -d -o picache -g picache -m 0750 /srv/pdata
touch /srv/pdata/picache.db && chown picache:picache /srv/pdata/picache.db
echo 'PICACHE_DATA_DIR=/srv/pdata' >>/etc/picache/picache.env
uid=$(id -u picache)
apt-get remove -y -qq picache >/dev/null
out=$(apt-get purge -y picache 2>&1) || { echo "$out"; fail "purge"; }
echo "$out" | grep -q "kept the account picache (uid $uid) because it owns kept files in /srv/pdata; it is locked" ||
	fail "no locked-account message: $out"
getent passwd picache >/dev/null || fail "account deleted"
passwd -S picache | awk '{ exit $2 != "L" }' || fail "account not locked: $(passwd -S picache)"
[ -e /srv/pdata/picache.db ] || fail "custom data deleted"
[ ! -e /etc/picache ] || fail "/etc/picache left"

echo "PASS ($mode)"
EOF
done
