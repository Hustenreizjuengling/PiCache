#!/bin/sh
# Smoke test for deploy/install.sh. It installs, re-installs, enables
# host-apply, checks the update helper (default, custom data directory,
# --without-updater), the DHCP support (--with-dhcp, --without-dhcp), hits
# a port-53 conflict and uninstalls PiCache in a throwaway
# Debian container. systemd does not run there: systemctl and journalctl are
# stand-ins that record their arguments, so PiCache itself is never started.
#
#   scripts/test-install.sh bin/picache-linux-amd64 [debian:13]
#
# Needs Docker and network access (apt-get installs iproute2 and netcat).
set -eu

[ $# -ge 1 ] || {
	echo "usage: $0 PICACHE_LINUX_BINARY [IMAGE]" >&2
	exit 2
}
bin=$(CDPATH='' cd -- "$(dirname -- "$1")" && pwd)/$(basename -- "$1")
image=${2:-debian:13}
root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
[ -f "$bin" ] || {
	echo "binary not found: $1" >&2
	exit 2
}

docker run --rm -i \
	-v "$root/deploy:/src/deploy:ro" \
	-v "$root/LICENSE:/src/LICENSE:ro" \
	-v "$root/THIRD_PARTY_NOTICES.md:/src/THIRD_PARTY_NOTICES.md:ro" \
	-v "$bin:/src/picache:ro" \
	"$image" sh -s <<'EOF'
set -eu
fail() { echo "FAIL: $*" >&2; exit 1; }
# check_mode PATH MODE OWNER:GROUP
check_mode() {
	got=$(stat -c '%a %U:%G' "$1") || fail "$1 is missing"
	[ "$got" = "$2 $3" ] || fail "$1: got '$got', want '$2 $3'"
}
starts() { grep -c '^restart picache.service' /tmp/systemctl.log || true; }

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null && apt-get install -y -qq iproute2 netcat-openbsd >/dev/null

# systemd stand-ins
mkdir -p /run/systemd/system
cat >/usr/local/sbin/systemctl <<'STUB'
#!/bin/sh
echo "$*" >>/tmp/systemctl.log
[ "$1" = restart ] && mkdir -p /var/lib/picache && touch /var/lib/picache/picache.db
exit 0
STUB
printf '#!/bin/sh\nexit 0\n' >/usr/local/sbin/journalctl
chmod 0755 /usr/local/sbin/systemctl /usr/local/sbin/journalctl
: >/tmp/systemctl.log
cp /src/picache /tmp/picache
chmod 0644 /tmp/picache # the installer must make it executable itself

echo "== install"
sh /src/deploy/install.sh --binary /tmp/picache
getent passwd picache >/dev/null || fail "user picache missing"
/usr/local/bin/picache version | grep -q '^picache ' || fail "binary not installed"
check_mode /usr/local/bin/picache 755 root:root
check_mode /etc/picache 750 root:picache
check_mode /etc/picache/picache.env 640 root:picache
check_mode /usr/share/doc/picache/LICENSE 644 root:root
check_mode /usr/share/doc/picache/THIRD_PARTY_NOTICES.md 644 root:root
check_mode /srv/picache 750 root:picache
check_mode /usr/local/lib/systemd/system/picache.service 644 root:root
grep -q '^enable picache.service' /tmp/systemctl.log || fail "service not enabled"
[ "$(starts)" = 1 ] || fail "service not started"
[ ! -e /etc/picache/host-apply.enabled ] || fail "host-apply installed without the flag"
check_mode /etc/picache/updater.enabled 644 root:root
check_mode /usr/local/lib/systemd/system/picache-update.path 644 root:root
check_mode /usr/local/lib/systemd/system/picache-update.service 644 root:root
grep -q '^enable --now picache-update.path' /tmp/systemctl.log || fail "update path unit not enabled"
[ ! -e /etc/systemd/system/picache-update.path.d ] || fail "update drop-in written for the default data directory"

echo "== re-install keeps the configuration"
echo 'PICACHE_LOG_LEVEL=debug' >>/etc/picache/picache.env
sh /src/deploy/install.sh --binary /tmp/picache
grep -q '^PICACHE_LOG_LEVEL=debug' /etc/picache/picache.env || fail "env file overwritten"

echo "== host-apply"
sh /src/deploy/install.sh --binary /tmp/picache --with-host-apply
check_mode /etc/picache/credentials 700 root:root
check_mode /etc/picache/host-apply.enabled 644 root:root
check_mode /usr/local/lib/systemd/system/picache-storage.path 644 root:root
check_mode /usr/local/lib/systemd/system/picache-storage.service 644 root:root
grep -q '^enable --now picache-storage.path' /tmp/systemctl.log || fail "path unit not enabled"

echo "== update helper: custom data directory, --without-updater"
mkdir -p /srv/pdata && touch /srv/pdata/picache.db
echo "PICACHE_DATA_DIR=/srv/pdata" >>/etc/picache/picache.env
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
grep -qx "PathExists=/srv/pdata/update-requests/request" /etc/systemd/system/picache-update.path.d/50-picache-paths.conf ||
	fail "update path drop-in missing"
grep -qx "PathExists=" /etc/systemd/system/picache-update.path.d/50-picache-paths.conf || fail "default update paths not dropped"
grep -qx "ReadWritePaths=-/srv/pdata" /etc/systemd/system/picache-update.service.d/50-picache-paths.conf ||
	fail "update service drop-in missing"
sed -i "/^PICACHE_DATA_DIR=/d" /etc/picache/picache.env
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ ! -e /etc/systemd/system/picache-update.path.d ] || fail "update drop-ins kept for the default data directory"
sh /src/deploy/install.sh --binary /tmp/picache --without-updater >/dev/null
for f in /etc/picache/updater.enabled /usr/local/lib/systemd/system/picache-update.path /usr/local/lib/systemd/system/picache-update.service; do
	[ ! -e "$f" ] || fail "--without-updater left $f"
done
grep -q "^disable --now picache-update.path" /tmp/systemctl.log || fail "update path unit not disabled"
[ -e /etc/picache/host-apply.enabled ] || fail "--without-updater removed host-apply"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ -e /etc/picache/updater.enabled ] || fail "update helper not installed again"

echo "== DHCP support: --with-dhcp, re-run, --without-dhcp"
dropin=/etc/systemd/system/picache.service.d/60-dhcp.conf
sh /src/deploy/install.sh --binary /tmp/picache --with-dhcp >/dev/null
check_mode "$dropin" 644 root:root
check_mode /etc/picache/dhcp.enabled 644 root:root
check_mode /etc/picache/picache.env 640 root:picache
grep -qx "AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW" "$dropin" || fail "drop-in lacks the ambient capabilities"
grep -qx "CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_RAW" "$dropin" || fail "drop-in lacks the bounding set"
grep -qx "SystemCallFilter=capset" "$dropin" || fail "drop-in lacks capset"
grep -qx "PICACHE_DHCP=on" /etc/picache/picache.env || fail "PICACHE_DHCP not set"
grep -q '^PICACHE_LOG_LEVEL=debug' /etc/picache/picache.env || fail "other settings lost"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ -e "$dropin" ] || fail "a plain re-run removed the DHCP support"
[ "$(grep -c '^PICACHE_DHCP=' /etc/picache/picache.env)" = 1 ] || fail "PICACHE_DHCP written twice"
if sh /src/deploy/install.sh --binary /tmp/picache --with-dhcp --without-dhcp 2>/dev/null; then fail "accepted both DHCP flags"; fi
sh /src/deploy/install.sh --binary /tmp/picache --without-dhcp >/dev/null
[ ! -e "$dropin" ] || fail "--without-dhcp left the drop-in"
[ ! -e /etc/picache/dhcp.enabled ] || fail "--without-dhcp left the marker"
if grep -q '^PICACHE_DHCP=' /etc/picache/picache.env; then fail "--without-dhcp left PICACHE_DHCP"; fi
grep -q '^#PICACHE_DHCP=on' /etc/picache/picache.env || fail "the commented example was removed"
check_mode /etc/picache/picache.env 640 root:picache
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ ! -e "$dropin" ] || fail "a plain re-run installed the DHCP support"
sh /src/deploy/install.sh --binary /tmp/picache --with-dhcp >/dev/null # removed by the uninstall below

echo "== rejects a binary that is not PiCache"
printf '#!/bin/sh\necho hello\n' >/tmp/other
if sh /src/deploy/install.sh --binary /tmp/other 2>/dev/null; then fail "accepted a foreign binary"; fi
/usr/local/bin/picache version | grep -q '^picache ' || fail "installed binary was replaced"

echo "== port 53 in use: prints the fix, does not start"
nc -lu 127.0.0.53 53 &
udp_pid=$!
nc -l 127.0.0.53 53 &
tcp_pid=$!
sleep 1
before=$(starts)
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1) || fail "installer failed on a port conflict"
[ "$(echo "$out" | grep -c "127.0.0.53:53")" = 2 ] || fail "expected one UDP and one TCP conflict line"
echo "$out" | grep -q 'DNSStubListener=no' || fail "resolved fix not printed"
[ "$(starts)" = "$before" ] || fail "service started despite the conflict"
echo 'PICACHE_DNS_LISTEN=192.0.2.1:53' >>/etc/picache/picache.env
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null 2>&1 || fail "installer failed with PICACHE_DNS_LISTEN"
[ "$(starts)" -gt "$before" ] || fail "service not started although PICACHE_DNS_LISTEN is set"
kill "$udp_pid" "$tcp_pid"

echo "== uninstall"
touch /usr/local/bin/picache.prev # left by `picache update`
sh /src/deploy/install.sh --uninstall
[ ! -e /usr/local/bin/picache ] || fail "binary left behind"
for u in picache.service picache-storage.service picache-storage.path picache-update.service picache-update.path; do
	[ ! -e "/usr/local/lib/systemd/system/$u" ] || fail "$u left behind"
done
[ ! -e /etc/picache/host-apply.enabled ] || fail "host-apply marker left behind"
[ ! -e /etc/picache/updater.enabled ] || fail "update helper marker left behind"
[ ! -e /etc/picache/dhcp.enabled ] || fail "DHCP marker left behind"
[ ! -e /etc/systemd/system/picache.service.d/60-dhcp.conf ] || fail "DHCP drop-in left behind"
if grep -q '^PICACHE_DHCP=' /etc/picache/picache.env; then fail "PICACHE_DHCP left in the configuration"; fi
[ ! -e /usr/local/bin/picache.prev ] || fail "picache.prev left behind"
[ ! -e /usr/share/doc/picache ] || fail "license texts left behind"
[ -e /etc/picache/picache.env ] || fail "configuration was removed"
echo "PASS"
EOF
