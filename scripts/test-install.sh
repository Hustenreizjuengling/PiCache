#!/bin/sh
# Smoke test for deploy/install.sh. It installs, re-installs, enables
# host-apply, checks the update helper (default, custom data directory,
# --without-updater), the unit files (CAP_NET_RAW, capset, the helper's
# writable paths), the DHCP settings (PICACHE_DHCP spellings, the removal of
# the pre-0.8.0 drop-in and marker, the markers that replace the old opt-in,
# the 0.7.0 env comment, --with-dhcp, --without-dhcp), hits a port-53
# conflict and uninstalls PiCache in a throwaway Debian container. systemd
# does not run there: systemctl and journalctl are stand-ins that record
# their arguments, so PiCache itself is never started.
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
unit=/usr/local/lib/systemd/system/picache.service
grep -qx "AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW" "$unit" || fail "unit lacks the ambient CAP_NET_RAW"
grep -qx "CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_NET_RAW" "$unit" || fail "unit lacks the bounding set"
grep -qx "SystemCallFilter=capset" "$unit" || fail "unit lacks capset"
[ "$(grep -n 'SystemCallFilter=~@privileged' "$unit" | cut -d: -f1)" -lt "$(grep -n '^SystemCallFilter=capset' "$unit" | cut -d: -f1)" ] ||
	fail "capset is not allowed after ~@privileged"
grep -qx "CPUWeight=200" "$unit" && grep -qx "IOWeight=200" "$unit" || fail "unit lacks the weights"
grep -qx "ReadWritePaths=/usr/local/bin -/var/lib/picache -/usr/local/lib/systemd/system" \
	/usr/local/lib/systemd/system/picache-update.service || fail "update helper cannot write the unit directory"
grep -q '^#PICACHE_DHCP=off' /etc/picache/picache.env || fail "env template lacks the DHCP opt-out example"
if grep -q '^PICACHE_DHCP=' /etc/picache/picache.env; then fail "PICACHE_DHCP set on a new install"; fi
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

echo "== DHCP: pre-0.8.0 drop-in and marker removed, PICACHE_DHCP normalised, --with-dhcp, --without-dhcp"
dropin=/etc/systemd/system/picache.service.d/60-dhcp.conf
env=/etc/picache/picache.env
data=/var/lib/picache
[ ! -e "$data/dhcp.sockets" ] && [ ! -e "$data/dhcp.ra" ] || fail "DHCP markers written without the old opt-in"
mkdir -p "$(dirname "$dropin")"
printf '[Service]\nAmbientCapabilities=CAP_NET_BIND_SERVICE CAP_NET_RAW\n' >"$dropin"
touch /etc/picache/dhcp.enabled
echo 'PICACHE_DHCP=on' >>"$env"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ ! -e "$dropin" ] || fail "the old drop-in was kept"
[ ! -e /etc/systemd/system/picache.service.d ] || fail "the empty drop-in directory was kept"
[ ! -e /etc/picache/dhcp.enabled ] || fail "the old marker was kept"
if grep -q '^PICACHE_DHCP=' "$env"; then fail "PICACHE_DHCP=on was kept"; fi
grep -q '^PICACHE_LOG_LEVEL=debug' "$env" || fail "other settings lost"
check_mode "$env" 640 root:picache
# The removed opt-in becomes the markers, so the first start of the new
# version still opens the DHCP ports and the raw socket.
for m in dhcp.sockets dhcp.ra; do
	check_mode "$data/$m" 640 picache:picache
	[ -f "$data/$m" ] && [ ! -s "$data/$m" ] || fail "$m is not an empty regular file"
done
for v in on '"yes"' 1 TRUE t; do
	sed -i '/^PICACHE_DHCP=/d' "$env"
	echo "PICACHE_DHCP=$v" >>"$env"
	sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
	if grep -q '^PICACHE_DHCP=' "$env"; then fail "PICACHE_DHCP=$v was kept"; fi
done
# A symbolic link in a marker's place is left alone (never followed).
rm -f "$data/dhcp.sockets" "$data/dhcp.ra"
ln -s /etc/passwd "$data/dhcp.ra"
echo 'PICACHE_DHCP=on' >>"$env"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ -L "$data/dhcp.ra" ] || fail "a symbolic link in a marker's place was replaced"
check_mode /etc/passwd 644 root:root
check_mode "$data/dhcp.sockets" 640 picache:picache
rm -f "$data/dhcp.sockets" "$data/dhcp.ra"
# --without-dhcp: the opt-out, no markers.
echo 'PICACHE_DHCP=on' >>"$env"
sh /src/deploy/install.sh --binary /tmp/picache --without-dhcp >/dev/null
[ ! -e "$data/dhcp.sockets" ] && [ ! -e "$data/dhcp.ra" ] || fail "DHCP markers written with --without-dhcp"
for v in off "'no'" 0 False; do
	sed -i '/^PICACHE_DHCP=/d' "$env"
	echo "PICACHE_DHCP=$v" >>"$env"
	sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
	[ "$(grep '^PICACHE_DHCP=' "$env")" = PICACHE_DHCP=off ] || fail "PICACHE_DHCP=$v not rewritten as off: $(grep '^PICACHE_DHCP=' "$env")"
done
[ ! -e "$data/dhcp.sockets" ] && [ ! -e "$data/dhcp.ra" ] || fail "DHCP markers written without the old opt-in"
check_mode "$env" 640 root:picache
sed -i '/^PICACHE_DHCP=/d' "$env"
echo 'PICACHE_DHCP=maybe' >>"$env"
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1) || fail "installer failed on an invalid PICACHE_DHCP"
echo "$out" | grep -q 'PICACHE_DHCP=maybe is not valid; PiCache refuses to start with it' || fail "no warning for PICACHE_DHCP=maybe"
grep -qx 'PICACHE_DHCP=maybe' "$env" || fail "an invalid PICACHE_DHCP was changed"
sh /src/deploy/install.sh --binary /tmp/picache --without-dhcp >/dev/null
[ "$(grep -c '^PICACHE_DHCP=' "$env")" = 1 ] && grep -qx 'PICACHE_DHCP=off' "$env" || fail "--without-dhcp did not write the opt-out"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
grep -qx 'PICACHE_DHCP=off' "$env" || fail "a plain re-run removed the opt-out"
out=$(sh /src/deploy/install.sh --binary /tmp/picache --with-dhcp 2>&1)
if grep -q '^PICACHE_DHCP=' "$env"; then fail "--with-dhcp left PICACHE_DHCP"; fi
echo "$out" | grep -q 'DHCP server allowed' || fail "--with-dhcp printed nothing"
grep -q '^#PICACHE_DHCP=off' "$env" || fail "the commented example was removed"
if sh /src/deploy/install.sh --binary /tmp/picache --with-dhcp --without-dhcp 2>/dev/null; then fail "accepted both DHCP flags"; fi
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)
if echo "$out" | grep -qi 'dhcp server allowed\|dhcp server prevented\|UDP port 67'; then fail "a plain run printed DHCP messages"; fi
nc -lu 0.0.0.0 67 &
dhcp_pid=$!
sleep 1
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)
echo "$out" | grep -q "UDP port 67 is used by another program (another DHCP server on this host?)" || fail "no note about UDP 67 in use"
kill "$dhcp_pid"
# The DHCP comment of a 0.7.0 env file (PICACHE_DHCP=on as the install
# option) is replaced by the current one; nothing else changes.
awk '$0 == "# DHCP server: switched on in the web UI (DNS -> DHCP); PiCache holds no" {
		print "# DHCP server (DNS -> DHCP in the web UI): set by install.sh --with-dhcp,"
		print "# which also installs the unit drop-in it needs; remove it with"
		print "# install.sh --without-dhcp rather than by hand."
		print "#PICACHE_DHCP=on"
		skip = 3
		next
	}
	skip > 0 { skip--; next }
	{ print }' "$env" >/tmp/env.070
cat /tmp/env.070 >"$env"
grep -qx '#PICACHE_DHCP=on' "$env" || fail "test setup: no 0.7.0 DHCP comment"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
if grep -q 'set by install.sh --with-dhcp\|^#PICACHE_DHCP=on' "$env"; then fail "the 0.7.0 DHCP comment was kept"; fi
[ "$(grep -c '^#PICACHE_DHCP=off$' "$env")" = 1 ] || fail "the DHCP comment was not replaced once"
grep -q '^PICACHE_LOG_LEVEL=debug' "$env" || fail "other settings lost with the DHCP comment"
check_mode "$env" 640 root:picache
echo 'PICACHE_DHCP=on' >>"$env" # the uninstall below normalises it too

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
touch /usr/local/lib/systemd/system/picache.service.prev /usr/local/lib/systemd/system/picache-update.path.prev
sh /src/deploy/install.sh --uninstall
[ ! -e /usr/local/bin/picache ] || fail "binary left behind"
for u in picache.service picache-storage.service picache-storage.path picache-update.service picache-update.path; do
	[ ! -e "/usr/local/lib/systemd/system/$u" ] || fail "$u left behind"
done
[ ! -e /etc/picache/host-apply.enabled ] || fail "host-apply marker left behind"
[ ! -e /etc/picache/updater.enabled ] || fail "update helper marker left behind"
[ ! -e /etc/picache/dhcp.enabled ] || fail "DHCP marker left behind"
[ ! -e /etc/systemd/system/picache.service.d/60-dhcp.conf ] || fail "DHCP drop-in left behind"
if grep -q '^PICACHE_DHCP=' /etc/picache/picache.env; then fail "PICACHE_DHCP=on left in the configuration"; fi
[ ! -e /usr/local/bin/picache.prev ] || fail "picache.prev left behind"
for f in /usr/local/lib/systemd/system/picache*.prev; do
	[ ! -e "$f" ] || fail "$f left behind"
done
[ ! -e /usr/share/doc/picache ] || fail "license texts left behind"
[ -e /etc/picache/picache.env ] || fail "configuration was removed"
echo "PASS"
EOF
