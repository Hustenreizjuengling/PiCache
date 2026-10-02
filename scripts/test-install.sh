#!/bin/sh
# Smoke test for deploy/install.sh. It installs, re-installs, checks the
# summary (the setup token and the next steps only until the setup is
# done, the versions of an update), enables
# host-apply, checks the update helper (default, custom data directory,
# --without-updater), the nightly marker (--nightly), the unit files
# (CAP_NET_RAW, capset, adjtimex, no ProtectClock, the log directory, the
# helper's writable paths), the DHCP settings (PICACHE_DHCP spellings, the removal of
# the pre-0.8.0 drop-in and marker, the markers that replace the old opt-in,
# the 0.7.0 env comment, --with-dhcp, --without-dhcp), hits a port-53
# conflict, checks the systemd version (the refusal below 247),
# `systemd-analyze verify` of the installed units, the firewall hints
# (firewalld and ufw stand-ins), the SELinux relabel (selinuxenabled and
# restorecon stand-ins), the refusal next to the Debian package (a
# dpkg-query stand-in), the refusal of a downgrade (PICACHE_ALLOW_DOWNGRADE),
# Raspberry Pi OS (ID=raspbian) as Debian, the purge
# (the default paths with /var/log/picache and the account deleted, or the
# account kept and locked for a custom PICACHE_DATA_DIR) and
# get-picache.sh's CA-bundle and OpenSSL checks on the distribution's own
# files, and uninstalls PiCache in a throwaway container. systemd does not
# run there: systemctl and journalctl are stand-ins that record their
# arguments, so PiCache itself is never started.
#
#   scripts/test-install.sh bin/picache-linux-amd64 [debian:13|fedora:latest|archlinux:latest|opensuse/tumbleweed:latest]
#
# Needs Docker and network access (the package manager of the image
# installs iproute2, netcat, systemd and the other tools first).
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
	-v "$root/scripts/get-picache.sh:/src/scripts/get-picache.sh:ro" \
	"$image" sh -s <<'EOF'
set -eu
fail() { echo "FAIL: $*" >&2; exit 1; }
# check_mode PATH MODE OWNER:GROUP
check_mode() {
	got=$(stat -c '%a %U:%G' "$1") || fail "$1 is missing"
	[ "$got" = "$2 $3" ] || fail "$1: got '$got', want '$2 $3'"
}
starts() { grep -c '^restart picache.service' /tmp/systemctl.log || true; }

# The tools the installer uses, per image family (the installer itself never
# runs a package manager), and the CA bundle get-picache.sh must find.
id=$(sed -n 's/^ID=//p' /etc/os-release | tr -d '"')
case $id in
debian | ubuntu)
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq >/dev/null
	apt-get install -y -qq iproute2 netcat-openbsd systemd passwd procps openssl ca-certificates >/dev/null
	bundle=/etc/ssl/certs/ca-certificates.crt
	;;
fedora)
	dnf -y -q install iproute netcat systemd shadow-utils util-linux findutils procps-ng gawk openssl ca-certificates >/dev/null
	bundle=/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem
	;;
arch)
	pacman -Syu --noconfirm --needed iproute2 openbsd-netcat systemd shadow util-linux findutils procps-ng gawk openssl ca-certificates >/dev/null
	bundle=/etc/ssl/certs/ca-certificates.crt
	;;
opensuse-tumbleweed | opensuse-leap)
	zypper --non-interactive --quiet install iproute2 netcat-openbsd systemd shadow util-linux findutils procps gawk openssl ca-certificates >/dev/null
	bundle=/etc/ssl/ca-bundle.pem
	;;
*) fail "no prelude for $id" ;;
esac

# systemd stand-ins (earlier in PATH than the real ones)
mkdir -p /run/systemd/system /usr/local/sbin
cat >/usr/local/sbin/systemctl <<'STUB'
#!/bin/sh
echo "$*" >>/tmp/systemctl.log
# restart: the database and the unit's LogsDirectory=.
case $1 in
--version) echo "systemd ${STANDIN_SYSTEMD:-257} (${STANDIN_SYSTEMD:-257}-stand-in)" ;;
restart) mkdir -p /var/lib/picache && touch /var/lib/picache/picache.db && install -d -o picache -g picache -m 0750 /var/log/picache ;;
esac
exit 0
STUB
printf '#!/bin/sh\nexit 0\n' >/usr/local/sbin/journalctl
chmod 0755 /usr/local/sbin/systemctl /usr/local/sbin/journalctl
: >/tmp/systemctl.log
cp /src/picache /tmp/picache
chmod 0644 /tmp/picache # the installer must make it executable itself

echo "== get-picache.sh finds the CA bundle and OpenSSL 3 of $id"
sed '$d' /src/scripts/get-picache.sh >/tmp/get-lib.sh
got=$(sh -c '. /tmp/get-lib.sh; ca_bundle')
# The distribution's bundle, or another name of the same file (Fedora has
# the Debian and the classic RHEL name as links).
[ -s "$bundle" ] || fail "test setup: no $bundle"
[ -n "$got" ] && { [ "$got" = "$bundle" ] || cmp -s "$got" "$bundle"; } || fail "ca_bundle: got '$got', want '$bundle'"
sh -c '. /tmp/get-lib.sh; check_openssl' || fail "check_openssl refused $(openssl version)"

echo "== systemd older than 247 is refused"
out=$(STANDIN_SYSTEMD=246 sh /src/deploy/install.sh --binary /tmp/picache 2>&1) && fail "installed with systemd 246"
echo "$out" | grep -q "systemd 246 is too old: PiCache's sandbox needs systemd 247 or later (ProtectProc=invisible)" ||
	fail "no systemd message: $out"
[ ! -e /usr/local/bin/picache ] || fail "installed files with systemd 246"

echo "== Raspberry Pi OS (ID=raspbian) is Debian"
sed '$d' /src/deploy/install.sh >/tmp/install-lib.sh
cp /etc/os-release /tmp/os-release.orig
rm -f /etc/os-release
cat >/etc/os-release <<'OSR'
PRETTY_NAME="Raspbian GNU/Linux 12 (bookworm)"
ID=raspbian
ID_LIKE=debian
VERSION_ID="12"
OSR
out=$(sh -c '. /tmp/install-lib.sh; check_os; os_family' 2>&1)
[ "$out" = debian ] || fail "raspbian 12: $out"
sed -i 's/^VERSION_ID=.*/VERSION_ID="11"/' /etc/os-release
out=$(sh -c '. /tmp/install-lib.sh; check_os' 2>&1)
echo "$out" | grep -q 'raspbian 11 is untested' || fail "raspbian 11 was not warned about: $out"
cat /tmp/os-release.orig >/etc/os-release

echo "== the Debian package is refused"
printf '#!/bin/sh\nprintf "install ok installed"\n' >/usr/local/sbin/dpkg-query
chmod 0755 /usr/local/sbin/dpkg-query
for args in "--binary /tmp/picache" "--uninstall"; do
	# shellcheck disable=SC2086
	out=$(sh /src/deploy/install.sh $args 2>&1) && fail "install.sh $args ran next to the package"
	echo "$out" | grep -q 'PiCache is installed as a Debian package here: update it with apt' || fail "$args: $out"
done
printf '#!/bin/sh\nprintf "deinstall ok config-files"\n' >/usr/local/sbin/dpkg-query
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null 2>&1 || fail "refused with only the package's configuration files left"
sh /src/deploy/install.sh --uninstall >/dev/null 2>&1
rm -f /usr/local/sbin/dpkg-query
: >/tmp/systemctl.log

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
grep -qx "SystemCallFilter=adjtimex" "$unit" || fail "unit lacks adjtimex"
[ "$(grep -n 'SystemCallFilter=~@privileged' "$unit" | cut -d: -f1)" -lt "$(grep -n '^SystemCallFilter=adjtimex' "$unit" | cut -d: -f1)" ] ||
	fail "adjtimex is not allowed after ~@privileged"
if grep -q '^ProtectClock=' "$unit"; then fail "ProtectClock would refuse adjtimex"; fi
grep -qx "LogsDirectory=picache" "$unit" && grep -qx "LogsDirectoryMode=0750" "$unit" || fail "unit lacks the log directory"
grep -q '^#PICACHE_NTP_LISTEN=:123' /etc/picache/picache.env || fail "env template lacks the NTP example"
grep -q '^#PICACHE_LOG_FILE=/var/log/picache/picache.log' /etc/picache/picache.env || fail "env template lacks the log file example"
[ ! -e /etc/picache/nightly.enabled ] || fail "nightly marker written without --nightly"
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
out=$(systemd-analyze verify /usr/local/lib/systemd/system/picache*.service /usr/local/lib/systemd/system/picache*.path 2>&1 || true)
if echo "$out" | grep -E 'Unknown key|Unknown lvalue|Unknown section'; then fail "systemd-analyze verify: $out"; fi

echo "== firewall hints (firewalld, ufw); the firewall is never changed"
cat >/usr/local/sbin/firewall-cmd <<'STUB'
#!/bin/sh
echo "$*" >>/tmp/firewall.log
case $1 in
--state) echo running ;;
--get-zone-of-interface=*) echo home ;;
esac
STUB
chmod 0755 /usr/local/sbin/firewall-cmd
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)
echo "$out" | grep -q 'firewalld is active' || fail "no firewalld hint: $out"
echo "$out" | grep -qF "    firewall-cmd --permanent --zone=home --add-rich-rule='rule family=\"ipv4\" source address=\"<LAN-CIDR>\" port port=\"53\" protocol=\"udp\" accept'" ||
	fail "no rich rule for 53/udp: $out"
echo "$out" | grep -qF "    # firewall-cmd --permanent --zone=home --add-rich-rule='rule family=\"ipv4\" source address=\"<LAN-CIDR>\" port port=\"853\" protocol=\"tcp\" accept'" ||
	fail "no commented DoT rule: $out"
if grep -q -- '--add\|--permanent\|--reload' /tmp/firewall.log; then fail "install.sh changed the firewall: $(cat /tmp/firewall.log)"; fi
rm -f /usr/local/sbin/firewall-cmd
printf '#!/bin/sh\necho "$*" >>/tmp/ufw.log\n[ "$1" = status ] && echo "Status: active"\nexit 0\n' >/usr/local/sbin/ufw
chmod 0755 /usr/local/sbin/ufw
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)
echo "$out" | grep -q '^    ufw allow from <LAN-CIDR> to any port 53 proto udp$' || fail "no ufw rule: $out"
echo "$out" | grep -q '^    # ufw allow from <LAN-CIDR> to any port 123 proto udp$' || fail "no commented NTP rule: $out"
if grep -v '^status' /tmp/ufw.log | grep -q .; then fail "install.sh changed ufw: $(cat /tmp/ufw.log)"; fi
rm -f /usr/local/sbin/ufw

echo "== SELinux: restorecon relabels the installed files"
printf '#!/bin/sh\nexit 0\n' >/usr/local/sbin/selinuxenabled
printf '#!/bin/sh\necho "$*" >>/tmp/restorecon.log\n' >/usr/local/sbin/restorecon
chmod 0755 /usr/local/sbin/selinuxenabled /usr/local/sbin/restorecon
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
grep -q '^-RF /usr/local/bin/picache /etc/picache .*/usr/local/lib/systemd/system/picache.service' /tmp/restorecon.log ||
	fail "restorecon not called for the files: $(cat /tmp/restorecon.log)"
grep -qx -- '-F /usr/local/lib/systemd/system' /tmp/restorecon.log || fail "restorecon not called for the unit directory"
rm -f /usr/local/sbin/selinuxenabled /usr/local/sbin/restorecon /tmp/restorecon.log
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ ! -e /tmp/restorecon.log ] || fail "restorecon ran without SELinux"
: >/tmp/systemctl.log
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null

echo "== re-install keeps the configuration"
echo 'PICACHE_LOG_LEVEL=debug' >>/etc/picache/picache.env
sh /src/deploy/install.sh --binary /tmp/picache
grep -q '^PICACHE_LOG_LEVEL=debug' /etc/picache/picache.env || fail "env file overwritten"

echo "== the summary names the setup token until the setup is done"
first_install_summary() {
	echo "$1" | grep -qx 'PiCache is running\.' || fail "$2: no running line: $1"
	echo "$1" | grep -q '^  Setup token:   sudo picache setup-token  (first start only)$' || fail "$2: no setup token line: $1"
	echo "$1" | grep -q '^Next: finish the setup in the web UI' || fail "$2: no next steps: $1"
}
# A fresh installation (the stand-in start creates picache.db).
rm -f /var/lib/picache/picache.db /var/lib/picache/setup-token
first_install_summary "$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)" "fresh install"
# PiCache ran, but the setup is not done: its token file is there.
echo ABCDEFGHIJKLMNOPQRSTUVWXYZ >/var/lib/picache/setup-token
first_install_summary "$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)" "setup not done"
# The setup is done (PiCache deleted the token file): an update.
rm -f /var/lib/picache/setup-token
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)
echo "$out" | grep -qx 'PiCache is updated and running\.' || fail "update: no update line: $out"
if echo "$out" | grep -q 'Setup token\|first start only\|Next: finish the setup\|PiCache is running\.'; then
	fail "update: first-install lines: $out"
fi
echo "$out" | grep -q '^  Web UI:  ' || fail "update: no web UI line: $out"
printf '#!/bin/sh\necho "picache v0.0.0-0 (commit stand-in)"\n' >/usr/local/bin/picache
out=$(sh /src/deploy/install.sh --binary /tmp/picache 2>&1)
new=$(/usr/local/bin/picache version | sed -n '1s/^picache \([^ ]*\).*/\1/p')
echo "$out" | grep -qxF "PiCache was updated from v0.0.0-0 to $new and is running." || fail "update from v0.0.0-0: $out"

echo "== nightly marker"
sh /src/deploy/install.sh --binary /tmp/picache --nightly >/dev/null
check_mode /etc/picache/nightly.enabled 644 root:root
[ -f /etc/picache/nightly.enabled ] && [ ! -L /etc/picache/nightly.enabled ] || fail "nightly marker is not a regular file"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
[ -e /etc/picache/nightly.enabled ] || fail "a re-run without --nightly removed the nightly marker"
# A symbolic link in the marker's place is replaced by a root-owned file.
rm -f /etc/picache/nightly.enabled
ln -s /etc/hostname /etc/picache/nightly.enabled
sh /src/deploy/install.sh --binary /tmp/picache --nightly >/dev/null
[ -f /etc/picache/nightly.enabled ] && [ ! -L /etc/picache/nightly.enabled ] || fail "the link in the marker's place was kept"

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

echo "== a downgrade is refused unless PICACHE_ALLOW_DOWNGRADE=1"
cp /usr/local/bin/picache /tmp/picache.installed
printf '#!/bin/sh\necho "picache v99.1.0 (commit x, built y)"\n' >/tmp/newer
install -m 0755 /tmp/newer /usr/local/bin/picache
printf '#!/bin/sh\necho "picache v99.0.0 (commit x, built y)"\n' >/tmp/older
before=$(starts)
out=$(sh /src/deploy/install.sh --binary /tmp/older 2>&1) && fail "installed an older release"
echo "$out" | grep -qF 'picache v99.0.0 is older than the installed v99.1.0. v99.1.0 may have migrated the database, which v99.0.0 then refuses' ||
	fail "no downgrade message: $out"
echo "$out" | grep -qF 'unless the upgrade notes say that v99.0.0 opens this database, first stop PiCache (sudo systemctl stop picache) and put the copy made before the upgrade (/var/lib/picache/backups/picache-v99.0.0-*.db) in place as /var/lib/picache/picache.db, deleting picache.db-wal and picache.db-shm; then run: sudo PICACHE_ALLOW_DOWNGRADE=1 sh /src/deploy/install.sh --binary /tmp/older' ||
	fail "no downgrade steps: $out"
echo "$out" | grep -q -- '--binary /tmp/older$' || fail "the command names options that were not given: $out"
# The command repeats the options: without --without-updater it would
# install the update helper again.
out=$(sh /src/deploy/install.sh --binary /tmp/older --without-updater --nightly 2>&1) && fail "installed an older release"
echo "$out" | grep -qF 'sudo PICACHE_ALLOW_DOWNGRADE=1 sh /src/deploy/install.sh --binary /tmp/older --without-updater --nightly' ||
	fail "the command lost the options: $out"
grep -q v99.1.0 /usr/local/bin/picache || fail "the installed binary was replaced"
[ ! -e /usr/local/bin/.picache.new ] || fail "the staged binary was left"
[ "$(starts)" = "$before" ] || fail "restarted after refusing the downgrade"
out=$(sh /src/deploy/install.sh --binary /tmp/newer 2>&1) || fail "a reinstall of the same release was refused: $out"
out=$(PICACHE_ALLOW_DOWNGRADE=1 sh /src/deploy/install.sh --binary /tmp/older 2>&1) || fail "PICACHE_ALLOW_DOWNGRADE=1 was refused: $out"
grep -q v99.0.0 /usr/local/bin/picache || fail "not downgraded with PICACHE_ALLOW_DOWNGRADE=1"
echo "$out" | grep -qxF "PiCache went back from v99.1.0 to v99.0.0 and is running." || fail "going back: no went-back line: $out"
# A pre-release with "-" in its identifiers is a release version, not a
# development build.
printf '#!/bin/sh\necho "picache v99.1.0-beta-2 (commit x, built y)"\n' >/tmp/newer
printf '#!/bin/sh\necho "picache v99.1.0-beta-1 (commit x, built y)"\n' >/tmp/older
install -m 0755 /tmp/newer /usr/local/bin/picache
out=$(sh /src/deploy/install.sh --binary /tmp/older 2>&1) && fail "installed an older pre-release"
echo "$out" | grep -qF 'picache v99.1.0-beta-1 is older than the installed v99.1.0-beta-2.' || fail "no pre-release downgrade message: $out"
install -m 0755 /tmp/picache.installed /usr/local/bin/picache

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
[ ! -e /etc/picache/nightly.enabled ] || fail "nightly marker left behind"
[ ! -e /etc/picache/dhcp.enabled ] || fail "DHCP marker left behind"
[ ! -e /etc/systemd/system/picache.service.d/60-dhcp.conf ] || fail "DHCP drop-in left behind"
if grep -q '^PICACHE_DHCP=' /etc/picache/picache.env; then fail "PICACHE_DHCP=on left in the configuration"; fi
[ ! -e /usr/local/bin/picache.prev ] || fail "picache.prev left behind"
for f in /usr/local/lib/systemd/system/picache*.prev; do
	[ ! -e "$f" ] || fail "$f left behind"
done
[ ! -e /usr/share/doc/picache ] || fail "license texts left behind"
[ -e /etc/picache/picache.env ] || fail "configuration was removed"
getent passwd picache >/dev/null || fail "the account was removed without --purge"

echo "== purge: default paths and the account deleted"
[ -d /var/log/picache ] || fail "test setup: no /var/log/picache"
install -o picache -g picache -m 0640 /dev/null /var/log/picache/picache.log
out=$(sh /src/deploy/install.sh --uninstall --purge --yes 2>&1) || { echo "$out"; fail "purge"; }
for p in /etc/picache /var/lib/picache /var/cache/picache /var/log/picache /srv/picache; do
	[ ! -e "$p" ] || fail "$p left after the purge"
done
if getent passwd picache >/dev/null; then fail "the account was kept: $out"; fi
if getent group picache >/dev/null; then fail "the group was kept"; fi

echo "== purge with a custom PICACHE_DATA_DIR: the account is kept and locked"
sh /src/deploy/install.sh --binary /tmp/picache >/dev/null
echo "PICACHE_DATA_DIR=/srv/pdata" >>/etc/picache/picache.env
chown -R picache:picache /srv/pdata
uid=$(id -u picache)
out=$(sh /src/deploy/install.sh --uninstall --purge --yes 2>&1) || { echo "$out"; fail "purge with a custom data directory"; }
echo "$out" | grep -q "kept the account picache (uid $uid) because it owns kept files in /srv/pdata; it is locked" ||
	fail "no locked-account message: $out"
getent passwd picache >/dev/null || fail "the account was deleted"
case $(getent shadow picache | cut -d: -f2) in '!'*) ;; *) fail "the account is not locked" ;; esac
[ "$(getent shadow picache | cut -d: -f8)" = 1 ] || fail "the account does not expire"
[ -e /srv/pdata/picache.db ] || fail "the custom data directory was deleted"
# The default directory next to it is kept (and checked) too.
echo "$out" | grep -qx '  /var/lib/picache' || fail "the default data directory was not kept: $out"
[ ! -e /var/log/picache ] || fail "/var/log/picache left after the purge"
[ ! -e /etc/picache ] || fail "/etc/picache left after the purge"
echo "PASS"
EOF
