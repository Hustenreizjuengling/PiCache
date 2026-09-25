#!/bin/sh
# Smoke test for deploy/install.sh. It installs, re-installs, enables
# host-apply, hits a port-53 conflict and uninstalls PiCache in a throwaway
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
sh /src/deploy/install.sh --uninstall
[ ! -e /usr/local/bin/picache ] || fail "binary left behind"
for u in picache.service picache-storage.service picache-storage.path; do
	[ ! -e "/usr/local/lib/systemd/system/$u" ] || fail "$u left behind"
done
[ ! -e /etc/picache/host-apply.enabled ] || fail "host-apply marker left behind"
[ ! -e /usr/share/doc/picache ] || fail "license texts left behind"
[ -e /etc/picache/picache.env ] || fail "configuration was removed"
echo "PASS"
EOF
