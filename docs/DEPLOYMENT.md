# Deploying PiCache

PiCache is one static binary (`picache`) with the web UI built in. It runs on
Linux (amd64, arm64, armv7). There are three supported ways to deploy it:

| Target | When | Guide |
|---|---|---|
| **Debian 12/13 with systemd** (bare metal, VM, Raspberry Pi) | the default; full feature set including NAS host-apply | [Bare metal and VMs](#bare-metal-and-vms-debian-1213) |
| **Proxmox VE, unprivileged LXC** | Proxmox hosts | [deploy/lxc/README.md](../deploy/lxc/README.md) |
| **Docker** (host networking) | hosts that already run Docker | [Docker](#docker) |

Whichever you choose:

- Give the machine a **static IP address** (or a DHCP reservation). It becomes
  your network's DNS server.
- PiCache needs these ports: **53/udp+tcp** (DNS), **80/tcp** (download
  cache over HTTP), **443/tcp** (HTTPS pass-through of the download cache),
  **8080/tcp** and **8443/tcp** (web UI over HTTP and HTTPS). See [Port conflicts](#port-conflicts).
- Never expose these ports to the Internet (no port forwarding). PiCache
  answers only private networks by default.

Contents: [Download](#download) · [Build from source](#build-from-source) ·
[Bare metal](#bare-metal-and-vms-debian-1213) · [LXC](#proxmox-lxc) ·
[Docker](#docker) · [First-run setup](#first-run-setup) ·
[Web access and accounts](#web-access-and-accounts) ·
[HTTPS certificates](#https-certificates) ·
[Behind a reverse proxy](#behind-a-reverse-proxy) ·
[IPv6](#ipv6-and-dual-stack-networks) ·
[Port conflicts](#port-conflicts) · [Persistent data](#persistent-data) ·
[Backup and restore](#backup-and-restore) · [Updates](#updates) ·
[Uninstall](#uninstall) · [Cache storage](#cache-storage) ·
[Environment variables](#environment-variables) · [CLI](#cli-reference) ·
[Troubleshooting](#troubleshooting)

---

## Download

Releases are published on
[GitHub](https://github.com/Hustenreizjuengling/PiCache/releases). Each
release has these files:

| File | Contents |
|---|---|
| `picache-linux-amd64` | static binary for x86-64 |
| `picache-linux-arm64` | static binary for 64-bit ARM (Raspberry Pi OS 64-bit, Debian arm64) |
| `picache-linux-armv7` | static binary for 32-bit ARM (`armhf`) |
| `picache-deploy.tar.gz` | `deploy/` (installer, systemd units, compose files, LXC guide), `LICENSE` and `THIRD_PARTY_NOTICES.md` |
| `SHA256SUMS` | SHA-256 checksums of the four files above |
| `SHA256SUMS.sig` | Ed25519 signature of `SHA256SUMS` made with the PiCache release key |

Download the binary for the machine, `picache-deploy.tar.gz` and both
checksum files of the newest release, check them, and unpack the deploy
files:

```sh
arch=$(dpkg --print-architecture)          # amd64, arm64 or armhf
[ "$arch" = armhf ] && arch=armv7
base=https://github.com/Hustenreizjuengling/PiCache/releases/latest/download
for f in "picache-linux-$arch" picache-deploy.tar.gz SHA256SUMS SHA256SUMS.sig; do
  curl -fLO "$base/$f"
done
sha256sum -c --ignore-missing SHA256SUMS   # every file must say OK
tar -xzf picache-deploy.tar.gz             # deploy/, LICENSE, THIRD_PARTY_NOTICES.md
```

`sha256sum -c` protects against broken downloads. To also make sure the
files come from the PiCache project, check the signature of `SHA256SUMS`
with OpenSSL 3 as described in
[SECURITY.md](SECURITY.md#verifying-a-release-by-hand). Later updates check
the signature by themselves ([Updates](#updates)).

Container images for linux/amd64, linux/arm64 and linux/arm/v7 are published
as `ghcr.io/hustenreizjuengling/picache` ([Docker](#docker)).

## Build from source

You need Go 1.27 and Node.js 22 (for the web UI), plus GNU make:

```sh
git clone https://github.com/hustenreizjuengling/picache.git
cd picache
make               # web UI + bin/picache for this machine
make build-all     # bin/picache-linux-{amd64,arm64,armv7} (run `make web` first)
```

The binary is static (`CGO_ENABLED=0`). You can build on a workstation and
copy it to the target. Use `picache-linux-arm64` for 64-bit Raspberry Pi OS
or Debian arm64, and `picache-linux-armv7` for 32-bit ARM systems. A binary
built without `make web` serves a short "web UI is not built" notice instead
of the UI; the API still works. `make dist VERSION=vX.Y.Z` builds the
release files in `dist/`, as the release workflow does
([CONTRIBUTING.md](../CONTRIBUTING.md#releases)).

The Docker image can also be built from source (see [Docker](#docker)).

---

## Bare metal and VMs (Debian 12/13)

### One-line install

On a Debian 12/13 machine or LXC container with systemd, as root:

```sh
curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh
```

`get-picache.sh` downloads the newest release, verifies the signature of
`SHA256SUMS` with the release key it carries and the checksums of the files
it uses, and then runs the release's installer (described below). It shows
the setup token at the end. Options go after `sh -s --`:

```sh
curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh -s -- --version v0.1.0      # a specific release
curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh -s -- --with-host-apply     # NAS mounts from the web UI
curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh -s -- --without-updater     # no update helper
curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh -s -- --without-dhcp        # prevent PiCache's DHCP server (another one runs on this host)
```

To read the script before running it, download it first:

```sh
curl -fsSLO https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh
less get-picache.sh
sudo sh get-picache.sh
```

Running it again upgrades an existing installation, unit files included.
Installations from before 0.8.0 need that once: their update helper cannot
replace unit files ([Updates](#updates)).
It needs `curl` (or `wget`); `openssl` and `ca-certificates` are installed
with apt when they are missing, as in minimal LXC templates.

### Manual install

You need the binary, the `deploy/` directory and the license texts `LICENSE`
and `THIRD_PARTY_NOTICES.md` of the same version on the machine, with the two
files next to `deploy/`. That is what unpacking `picache-deploy.tar.gz` of a
release gives you ([Download](#download)); from a source tree, copy them.
Then run the installer as root:

```sh
sudo sh deploy/install.sh --binary ./picache-linux-amd64
# optional: let the web UI mount SMB/NFS shares through a root helper
sudo sh deploy/install.sh --binary ./picache-linux-amd64 --with-host-apply
# optional, on a host that runs another DHCP server: prevent PiCache's
# (PICACHE_DHCP=off; --with-dhcp allows it again)
sudo sh deploy/install.sh --binary ./picache-linux-amd64 --without-dhcp
```

The installer is idempotent; running it again upgrades an existing
installation. It never downloads anything. It:

1. requires systemd, warns (and continues) on systems other than Debian
   12/13, and checks that the binary runs on this machine;
2. creates the system user and group `picache`. An existing `picache` login
   account or non-system group (ID above `SYS_UID_MAX`/`SYS_GID_MAX`, by
   default `UID_MIN`/`GID_MIN` − 1) is refused, and so is a system account
   `picache` with a login shell, because the service owns the database and
   the master key. The error shows the fix (rename the login account, or set
   the shell to `nologin`);
3. installs the binary to `/usr/local/bin/picache`, the unit
   `picache.service` to `/usr/local/lib/systemd/system/`, and `LICENSE` and
   `THIRD_PARTY_NOTICES.md` to `/usr/share/doc/picache/` (`0644 root`). If
   they are not next to `deploy/`, it warns and continues without them;
4. creates `/etc/picache/` (`0750 root:picache`) and, if missing,
   `/etc/picache/picache.env` (`0640 root:picache`, all settings commented
   out); an existing file is kept;
5. creates `/srv/picache` (`0750 root:picache`), the only place for NAS
   mounts. It is owned by root so that the unprivileged service cannot swap a
   mount point for a symbolic link;
6. with `--with-host-apply`: installs `picache-storage.path` and
   `picache-storage.service`, creates `/etc/picache/credentials` (`0700 root`)
   and `/etc/picache/host-apply.enabled`, and tells you whether `cifs-utils`
   or `nfs-common` are missing (install them with `apt install`). With a
   custom `PICACHE_DATA_DIR` or `PICACHE_MOUNT_ROOT` in `picache.env` it writes
   drop-ins (`50-picache-paths.conf`) that point the helper units at those
   paths. In a container whose `/` is not a shared mount (privileged LXC) it
   also installs `picache-shared-mounts.service`
   ([Host-apply](#host-apply-root-helper)). Later runs keep the helper up to
   date as long as that marker file exists. In an unprivileged container the
   helper is skipped, because it cannot mount anything there;
7. installs the update helper `picache-update.path` and
   `picache-update.service` and creates `/etc/picache/updater.enabled`, so
   that updates can be installed from the web UI ([Updates](#updates)).
   `--without-updater` skips both and removes a helper that an earlier run
   installed. With a custom `PICACHE_DATA_DIR` it writes
   a path drop-in for the helper, as for host-apply;
8. removes what `--with-dhcp` of versions before 0.8.0 installed (the drop-in
   `/etc/systemd/system/picache.service.d/60-dhcp.conf`, whose settings the
   unit now carries, and `/etc/picache/dhcp.enabled`) and normalises
   `PICACHE_DHCP` in `picache.env`: an on value (`on`, `yes`, `1`, `t`,
   `true`, quoted or not, any case) is removed, since the DHCP server needs
   no installation option any more, and the two DHCP markers are created
   in the data directory in its place (`dhcp.sockets`, `dhcp.ra`), so the
   first start of the new version still opens the DHCP ports and the raw
   socket (PiCache then removes what its settings do not need); an off
   value is written as
   `PICACHE_DHCP=off`; anything else is reported (PiCache refuses to start
   with it) and left alone. `--without-dhcp` writes `PICACHE_DHCP=off` (the
   DHCP server can then not be switched on in the web UI), `--with-dhcp`
   removes it again. Without either it says nothing about DHCP unless
   another program uses UDP port 67 or 547 (another DHCP server on this
   host); see [DHCP server](#dhcp-server);
9. checks ports 53, 80, 443, 8080 and 8443 for other programs. If port 53 is
   taken it prints the fix and does **not** start PiCache
   ([Port 53 conflicts](#port-53-conflicts)); it never reconfigures
   systemd-resolved or other services;
10. enables and (re)starts `picache.service` and prints the web UI address and
   the setup-token command.

`/var/lib/picache` and `/var/cache/picache` are created by systemd
(`StateDirectory=`/`CacheDirectory=`) on the first start.

### The systemd sandbox

`deploy/systemd/picache.service` runs PiCache as `picache` with only
`CAP_NET_BIND_SERVICE` (to bind ports 53, 80 and 443, and the DHCP ports
67 and 547 while the DHCP server is switched on) and `CAP_NET_RAW` (used only
at start to open the raw socket for IPv6 router advertisements while they
are on, then dropped on every thread; PiCache refuses to run if that fails,
and the unit allows the `capset` system call for exactly that),
`NoNewPrivileges=yes`, `ProtectSystem=strict` and a system-call filter.
`CPUWeight=200` and `IOWeight=200` give PiCache twice the CPU and disk
share of a service with the default weight 100 (sshd included) when the
machine is busy: a mild bias, no starvation. Inside the process DNS still
competes with the download cache, and the weights have no effect where the
cgroup controller is not delegated (some containers) or the disk's I/O
scheduler ignores them (`none`, `mq-deadline`). The service can write only to
`/var/lib/picache`, `/var/cache/picache` and `/srv/picache`. If you change
`PICACHE_DATA_DIR`, `PICACHE_CACHE_DIR` or `PICACHE_MOUNT_ROOT`, add the new
paths with a drop-in (`systemctl edit picache`, `ReadWritePaths=`). With
host-apply, also run the installer again: the helper units watch and write
fixed paths, and the installer gives them matching drop-ins. Put all local
changes into drop-ins; the installer overwrites the unit files on upgrades.

`Restart=always` also restarts PiCache after **Restart** in the web UI (the
process exits with code 75).

### Bootstrap configuration

`/etc/picache/picache.env` holds the few settings that are needed before the
database is opened: listeners, paths, logging and the optional admin
provisioning ([Environment variables](#environment-variables)). systemd reads
it, and so does every `picache` CLI command. Everything else (upstreams,
blocklists, clients, download cache, retention, …) is configured in the web
UI and stored in the database. After editing the file run
`sudo systemctl restart picache`.

---

## Proxmox LXC

Run PiCache natively in an **unprivileged Debian 12/13 container** with
`nesting=1` and install it with `deploy/install.sh` as above. NAS shares are
mounted on the Proxmox host and passed in as a bind mount point, because
unprivileged containers cannot mount SMB/NFS. The step-by-step guide,
including the UID offset (host UID = 100000 + container UID), is in
[deploy/lxc/README.md](../deploy/lxc/README.md).

---

## Docker

```sh
git clone https://github.com/hustenreizjuengling/picache.git
cd picache/deploy/docker     # or unpack picache-deploy.tar.gz and cd deploy/docker
docker compose up -d
docker exec -u 65532:65532 picache /picache setup-token
```

Then open `http://<host LAN IP>:8080/`.

`docker compose up -d` pulls the release image
`ghcr.io/hustenreizjuengling/picache:latest` (linux/amd64, linux/arm64 and
linux/arm/v7). To build the image from the source tree instead, run
`docker compose up -d --build` in a clone of the repository. Such an image
reports the version `dev` ([Updates](#updates)).

`deploy/docker/docker-compose.yml` uses **host networking**, so PiCache sees
real client addresses (IPv4 and IPv6) and MAC addresses and can detect the
host's LAN IP for the download cache's DNS answers. The image
(`deploy/docker/Dockerfile`) is distroless (no shell) and contains only
`/picache` and its license texts `LICENSE` and `THIRD_PARTY_NOTICES.md` in
`/usr/share/doc/picache/` (copy them out with
`docker cp picache:/usr/share/doc/picache/ .`). The container:

- starts as root only to bind ports 53, 80 and 443 and, while the DHCP
  server is switched on, the DHCP ports and (with router advertisements on)
  the raw ICMPv6 socket (Docker gives non-root users no ambient
  capabilities), then drops to `PICACHE_RUN_AS=65532:65532` before it opens
  its data, and checks that it cannot regain root;
- runs with `cap_drop: [ALL]`,
  `cap_add: [NET_BIND_SERVICE, SETUID, SETGID, NET_RAW]` (`NET_RAW` only
  opens the raw socket for the IPv6 router advertisements at start; the
  switch to 65532 clears it with every other capability),
  `no-new-privileges`, a read-only root filesystem and a small `/tmp` tmpfs;
- keeps its data in the named volumes `picache-data` (`/data`) and
  `picache-cache` (`/cache`), which inherit the owner 65532 from the image;
- reports health with `picache healthcheck` (web UI and DNS on loopback;
  the DNS probe never shows up in the query log or the statistics).

PiCache never chowns. If you replace a named volume with a bind-mounted host
directory, give it to 65532 first, or PiCache stops with a clear error:

```sh
sudo install -d -o 65532 -g 65532 -m 0750 /mnt/ssd/picache-cache
```

**CLI commands** run inside the container as the runtime user (the root user
of the container has no file-access capabilities):

```sh
docker exec -u 65532:65532 picache /picache setup-token
docker exec -it -u 65532:65532 picache /picache reset-password admin
docker compose logs -f picache
```

**Admin password from a Docker secret** (instead of the setup token): see the
commented `PICACHE_ADMIN_PASSWORD_FILE` and `secrets:` entries in the compose
file. The file is read as root before the privilege drop and without
`CAP_DAC_OVERRIDE`, so on the host it must be owned by root with mode `0400`.
Remove the secret and the variable after the first start. CLI commands run as
65532 cannot read the file and fail while the variable is set. Avoid
`PICACHE_ADMIN_PASSWORD`: plain environment variables are visible in
`docker inspect`.

**Bridge networking** (`docker-compose.bridge.yml`, published ports) works but
has limits: you **must** set the cache IPv4 address
(`downloadCache.cacheIpv4`) to the host's LAN IP, because PiCache cannot
detect it; traffic that passes docker-proxy (IPv6, loopback, hairpin)
appears to come from the Docker gateway; clients cannot be identified by
MAC; the router resolver must be set explicitly; and the DHCP server is not
available (DHCP broadcasts do not cross the bridge). The
[local CA](#the-local-ca) cannot cover the host's LAN IP either: the
container does not know it, so browsers keep warning for
`https://<host IP>:8443` even after you trust the CA. Use host networking
whenever you can.

**DHCP server** (optional, [DHCP server](#dhcp-server)): switch it on under
**DNS → DHCP**; it needs host networking (`docker-compose.yml`). PiCache opens
its DHCP ports (and the raw socket for IPv6 router advertisements, the only
use of `NET_RAW` in `cap_add`) as root at start; the switch to 65532 clears
every capability. So switching DHCP or the router advertisements on needs
one restart of the container, which the page offers (**Restart PiCache**;
`restart: unless-stopped` brings the container back). A compose file from
before 0.8.0 lacks `NET_RAW`: DHCPv4 and DHCPv6 work, the router
advertisements then say so — add `NET_RAW` to `cap_add` and recreate the
container (`docker compose up -d`). `PICACHE_DHCP: "off"` prevents the DHCP
server.

Docker Desktop (macOS/Windows) is not a deployment target: containers run in
a VM, so PiCache cannot see real client addresses, and bind-mount
propagation does not work.

---

## First-run setup

1. Open the web UI: `https://<ip>:8443/` (a certificate of PiCache's own
   local CA, which your browser does not know yet: accept the warning once,
   then trust the CA, see [HTTPS certificates](#https-certificates)) or
   `http://<ip>:8080/`. Over plain
   HTTP the setup token and the password cross the network unencrypted; the
   setup and sign-in pages then show a link to the HTTPS port.
2. Enter the one-time **setup token** and create the admin account (password
   at least 10 characters). The token is written to `<data>/setup-token`
   (mode 0600) and logged at WARN on every start until setup is done:

   | Deployment | Command |
   |---|---|
   | Bare metal | `sudo picache setup-token` or `journalctl -u picache \| grep -i token` |
   | LXC | `pct exec <ctid> -- picache setup-token` |
   | Docker | `docker exec -u 65532:65532 picache /picache setup-token` |

   Alternatively provision the admin at start with
   `PICACHE_ADMIN_PASSWORD_FILE` (and `PICACHE_ADMIN_USER`, default `admin`).
   This only takes effect while no user exists. Remove the variable and the
   file afterwards: `picache serve` reads the variable at every start and
   fails if the file is gone (the maintenance commands such as
   `web-access --reset`, `users` and `reset-password` ignore it).
3. Consider enabling two-factor authentication (**System → Users &
   security**). There you can also add accounts for other people; give
   people who only look the role *viewer* ([Web access and
   accounts](#web-access-and-accounts)).
4. Point your clients at PiCache: set the DNS server option of your router's
   DHCP server to PiCache's address. If the router instead forwards DNS to
   PiCache, all queries appear to come from the router. Also make sure the
   router does not advertise its own IPv6 DNS server, or clients will bypass
   PiCache over IPv6. **DNS → Network check** shows whether it worked
   ([Network check](#network-check), with the steps for a FRITZ!Box).
5. The download cache is **off** by default. When you enable it, the UI
   shows the cache IP it will answer with, the store path, its filesystem and
   free space, and warnings (SD card, Docker bridge mode). Check them first,
   and move the cache to a suitable disk ([Cache storage](#cache-storage)) if
   needed.

A new installation allows the web UI only from this machine, the private
networks (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10,
fc00::/7, link-local), the networks this machine is connected to and the
DNS allowed networks. Other addresses (a VPN with public addresses, an
uptime monitor on the Internet) need an entry under **System → Users &
security → Web access** first ([Web access and accounts](#web-access-and-accounts)).

If you open the UI through a host name that is not this machine's host name,
local domain or a configured server name, PiCache answers `421 Misdirected
Request` (DNS-rebinding protection). Add the name to `PICACHE_WEB_HOSTS` or
to the allowed hosts in the web settings.

---

## Web access and accounts

**Who may open the web UI.** The switch *Allow the web UI only from these
networks* (**System → Users & security → Web access**, setting
`web.restrictToNetworks`) is on for new installations. Allowed are then:
this machine (loopback and its own addresses, always, whatever the
settings say), the private ranges (10.0.0.0/8, 172.16.0.0/12,
192.168.0.0/16, 100.64.0.0/10, fc00::/7, link-local), every network this
machine is connected to (public ones too, so a LAN's global IPv6 prefix
works and follows renumbering within a minute), the DNS allowed networks
(`dns.allowedNetworks`), the web allowed networks (`web.allowedNetworks`,
up to 64 addresses or CIDRs of at least /8 or /32) and the trusted proxies.
Connections from other addresses are closed right after they are accepted;
every request is checked again, so an address you remove is refused from its
next request. Refusals are counted (the Web access panel shows the number)
and logged at WARN once per address and 10 minutes:

```text
refused web UI access (not in the allowed networks; see Users & security > Web access or run `picache web-access --reset`) client=203.0.113.9
```

An installation upgraded from 0.10 keeps the web UI open to every address;
the Web access panel recommends switching the restriction on. PiCache
refuses a change that would lock out the address you are connected from
(the error names the field and the address) and never refuses requests from
the host itself; if you still lock yourself out, see [Locked out of the web
UI](#troubleshooting). This applies to every path, `/healthz` and `/metrics`
included: a monitor or a Prometheus server outside the allowed networks needs
an entry in the web allowed networks.

**Accounts.** Up to 32 accounts, each an *admin* or a *viewer*. Viewers see
the pages read-only (not the audit log, the notification channels, backup
downloads or the storage mount snippets) and change nothing except their own
password, two-factor authentication, sessions and read-only API tokens; an API token of a viewer
is always read-only. Admins manage accounts under **System → Users &
security** (with their own password); an API token can never create
accounts or change roles. A role change signs the account out everywhere; a
demotion also deletes its admin API tokens. At least one admin always
remains. Every account existing before 0.11.0 is an admin. When no admin is
left (for example after editing the database), PiCache logs `no account is
an admin: run `picache reset-password --admin <user>` on the host`.

**Infrastructure as code.** Two bootstrap variables
([Environment variables](#environment-variables)):

- `PICACHE_CONFIG_LOCKED=on` refuses configuration changes from browser
  sessions (the API answers `config_locked`; the UI shows a banner). Your
  automation writes with an admin API token (`PUT /api/v1/settings`, the
  lists, records and so on). Pausing blocking, parental overrides and pauses,
  list refreshes, tests, the update check, restarts and cache verification
  stay possible in the UI; a permanent blocking switch-off does not. This is
  **not** an access control against admins: admin tokens still write, and
  whoever controls the host can unset it.
- `PICACHE_DESTRUCTIVE_API=false` refuses restores, the DHCP reset and lease
  wipe, cache purges and group deletes, storage initialisation and deletion,
  deleting scheduled backups, accounts and the uploaded certificate, for
  sessions and tokens alike.

---

## HTTPS certificates

PiCache serves the HTTPS listener (`:8443`) with, in this order:

1. **Certificate files** named by `PICACHE_WEB_TLS_CERT` and
   `PICACHE_WEB_TLS_KEY` (for example from [Let's Encrypt](#lets-encrypt));
2. an **uploaded** certificate (**System → HTTPS certificate**);
3. a certificate of PiCache's **local CA** (the default);
4. the **self-signed** certificate of versions before 0.11.0, until 30 days
   before it expires; then PiCache switches to a certificate of its local CA.

If the certificate files or the upload cannot be used, PiCache keeps serving
HTTPS with the previous certificate or with its local CA's, and the health
check *HTTPS certificate* fails with the reason. HTTPS is never switched off
because of a certificate problem. The page **System → HTTPS certificate**
shows the certificate, its names and addresses, and which of PiCache's names
it does not cover (browsers warn for those).

### The local CA

At its first start with an HTTPS listener PiCache creates a small
certification authority in `<data>/tls/` and issues its HTTPS certificate
with it (renewed automatically 30 days before it expires). Trust the CA once
on each device and the browser warnings are gone, also after renewals:
download it under **System → HTTPS certificate** (or from
`https://<ip>:8443/api/v1/system/tls/ca.crt`) and import it:

- **Windows:** double-click `picache-ca.crt` → *Install certificate* →
  *Local machine* → *Trusted Root Certification Authorities*.
- **macOS:** open it in Keychain Access, add it to *System*, then set *When
  using this certificate* to *Always Trust*.
- **iOS/iPadOS:** open the file, install the profile (Settings → *Profile
  downloaded*), then enable it under Settings → General → About →
  Certificate Trust Settings.
- **Android:** Settings → Security → Encryption & credentials → Install a
  certificate → CA certificate.
- **Firefox** (its own store): Settings → Privacy & Security → Certificates →
  View Certificates → Authorities → Import.
- **Linux:** `sudo cp picache-ca.crt /usr/local/share/ca-certificates/ &&
  sudo update-ca-certificates` (Debian/Ubuntu).

The CA can sign certificates only for PiCache's own names (`localhost`,
`picache`, the host name and the DNS server names, also with the local
domain) and its own addresses; its name constraints are marked critical, so
devices refuse anything else it might sign (your router, other LAN devices,
public names). When a new server name or a new address appears, the page and
the health check say that the CA does not cover it: *Create a new local CA*
includes it, and every device must then trust the new CA. The CA's key stays
in the data directory so renewals need no new trust. Whoever can read the
data directory can therefore issue certificates for PiCache's names and
addresses: remove the CA from your devices when you retire PiCache or its
data directory was exposed, and create a new CA after a compromise.

**Docker bridge networking:** inside a bridge network PiCache does not know
the host's LAN IP (and leaves the container's own addresses out), so the CA
can never cover `https://<host IP>:8443`; the page does not list that
address as not covered because PiCache cannot see it. Open PiCache by a name
the CA covers instead: add the name to the DNS server names (`dns.serverNames`)
with a local DNS record pointing to the host, then create a new local CA. Or
use host networking, or upload (or point `PICACHE_WEB_TLS_CERT` to) your own
certificate for the host's address.

### Uploading a certificate

Under **System → HTTPS certificate** an admin can paste (or pick) a
certificate with its intermediates and the private key (PEM; PKCS#8, PKCS#1
or SEC1, without a passphrase; RSA 2048–4096 or ECDSA P-256/P-384). The key
is accepted only over HTTPS or from a loopback address on the PiCache host
(for example `http://127.0.0.1:8080`; the host's LAN address over plain HTTP
does not count). PiCache stores
it as `<data>/tls/uploaded.pem` (0600) and serves it at once; names the
certificate does not cover are listed as a warning, never refused. *Delete
uploaded certificate* goes back to the local CA. An upload is not possible
while `PICACHE_WEB_TLS_CERT` is set.

### Let's Encrypt

PiCache has no ACME client of its own. Use acme.sh or lego with the **DNS-01
challenge** (no port 80 or 443 needed on PiCache, works for hosts that are
not reachable from the Internet) and let a deploy hook copy the files to a
place the service can read. PiCache loads renewed files within a minute, no
restart needed.

1. The name, for example `picache.example.com`, must resolve to PiCache in
   your network (a local DNS record, **DNS → Local DNS**) and be listed under
   the web settings' allowed hosts (or `PICACHE_WEB_HOSTS`).
2. Create the directory and a deploy hook (as root):

   ```sh
   sudo install -d -m 0750 -o root -g picache /etc/picache/tls
   sudo tee /usr/local/sbin/picache-deploy-cert >/dev/null <<'EOF'
   #!/bin/sh
   # usage: picache-deploy-cert <fullchain.pem> <privkey.pem>
   set -eu
   d=/etc/picache/tls
   install -m 0640 -o root -g picache "$2" "$d/.privkey.pem.new"
   install -m 0640 -o root -g picache "$1" "$d/.fullchain.pem.new"
   mv -f "$d/.privkey.pem.new" "$d/privkey.pem"      # the key first,
   mv -f "$d/.fullchain.pem.new" "$d/fullchain.pem"  # then the certificate
   systemctl kill -s HUP --kill-whom=main picache 2>/dev/null || true  # optional: load it now
   EOF
   sudo chmod 0755 /usr/local/sbin/picache-deploy-cert
   ```

3. Issue the certificate, for example with acme.sh and your DNS provider's
   API (here Cloudflare):

   ```sh
   acme.sh --issue --dns dns_cf -d picache.example.com
   acme.sh --install-cert -d picache.example.com \
     --fullchain-file /root/picache/fullchain.pem --key-file /root/picache/privkey.pem \
     --reloadcmd "/usr/local/sbin/picache-deploy-cert /root/picache/fullchain.pem /root/picache/privkey.pem"
   ```

   or with lego:

   ```sh
   lego --email you@example.com --dns cloudflare --domains picache.example.com run \
     --run-hook '/usr/local/sbin/picache-deploy-cert "$LEGO_CERT_PATH" "$LEGO_CERT_KEY_PATH"'
   # renewal (e.g. a daily timer): the same with "renew --renew-hook …"
   ```

4. Point PiCache at the copies in `/etc/picache/picache.env` and restart once:

   ```sh
   PICACHE_WEB_TLS_CERT=/etc/picache/tls/fullchain.pem
   PICACHE_WEB_TLS_KEY=/etc/picache/tls/privkey.pem
   ```

The service cannot read the files where the tools keep them
(`/etc/letsencrypt/archive` is 0700, and `~/.acme.sh` is hidden by the
unit's `ProtectHome=yes`), hence the copies. PiCache reads both files every
minute (each at most 1 MiB) and loads them when their content changed; a
half-written pair keeps the previous certificate until the next minute.
`sudo systemctl kill -s HUP --kill-whom=main picache` loads them at once (it
never stops PiCache). **Docker:** bind-mount the directory read-only (e.g.
`/etc/picache/tls:/tls:ro`, files readable by UID/GID 65532), set
`PICACHE_WEB_TLS_CERT=/tls/fullchain.pem` and `PICACHE_WEB_TLS_KEY=/tls/privkey.pem`,
and use `docker kill -s HUP picache` in the hook.

### Minimum TLS version

**System → Users & security → Web access** can require TLS 1.3
(`web.tlsMinVersion`, default 1.2). It applies to the next connection;
older clients can then no longer connect over HTTPS (the HTTP port is not
affected). PiCache refuses the change from a browser that is itself
connected with TLS 1.2.

---

## Behind a reverse proxy

A reverse proxy (Caddy, nginx, Traefik) can terminate TLS with a public
certificate and forward to PiCache's **plain-HTTP listener** (`:8080`). For
PiCache to see the real client (for the web access, sign-in throttling,
sessions and the audit log) and the scheme:

- the proxy sets `X-Forwarded-For` by appending the address it received the
  request from (nginx: `$proxy_add_x_forwarded_for`), and sets
  `X-Forwarded-Proto` itself (never passes it through; nginx: `$scheme`);
- the proxy keeps the `Host` header (nginx: `proxy_set_header Host $host`),
  and the external name is listed under the web settings' allowed hosts;
- the proxy's address (as PiCache sees it) is entered in **System → Users &
  security → Web access → Trusted reverse proxies** (`web.trustedProxies`, exact
  addresses or at least /24 and /64). PiCache reads `X-Forwarded-For` and
  `X-Forwarded-Proto` only from these addresses (never `Forwarded` or
  `X-Real-IP`), right-most entry first;
- the live streams under `/api/v1/stream/` need HTTP/1.1 without buffering
  and a long read timeout (one hour).

With the proxy trusted, `X-Forwarded-Proto: https` makes the session cookie
`Secure` and the HTTPS redirect is not applied, so
`PICACHE_WEB_SECURE_COOKIES` is not needed.

**Caddy** (sets the headers itself; Caddy ignores a client's
`X-Forwarded-For` unless you configure trusted proxies there):

```text
picache.example.com {
	reverse_proxy 192.168.1.10:8080
}
```

**nginx:**

```nginx
server {
    listen 443 ssl;
    server_name picache.example.com;
    ssl_certificate     /etc/ssl/picache/fullchain.pem;
    ssl_certificate_key /etc/ssl/picache/privkey.pem;

    location / {
        proxy_pass http://192.168.1.10:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
    location /api/v1/stream/ {
        proxy_pass http://192.168.1.10:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_http_version 1.1;
        proxy_buffering off;
        proxy_read_timeout 3600s;
    }
}
```

**Traefik** (file provider; Traefik sets `X-Forwarded-For` and
`X-Forwarded-Proto` and keeps the host with `passHostHeader`):

```yaml
http:
  routers:
    picache:
      rule: Host(`picache.example.com`)
      entryPoints: [websecure]
      service: picache
      tls:
        certResolver: letsencrypt
  services:
    picache:
      loadBalancer:
        passHostHeader: true
        servers:
          - url: http://192.168.1.10:8080
```

Pitfalls:

- A proxy on the same host that is **not** in the trusted proxies makes every
  client appear as `127.0.0.1`: loopback is always allowed, so the web
  access restriction no longer applies, and all clients share one sign-in
  throttle. Always list such a proxy (`127.0.0.1` or `::1`); note that
  trusting loopback trusts every process on the host.
- Never list a whole LAN as trusted proxies: every device in it could claim
  any address.
- A proxy that passes a client's `X-Forwarded-For` through without appending
  its peer lets clients choose their address.

---

## Network check

**DNS → Network check** shows whether the devices in your network use
PiCache. It compares the devices your PiCache machine can see (the kernel's
neighbour table: every device that talked on the local network recently)
with the DNS queries of the last 24 hours, and looks for the two router
set-ups that hide devices from PiCache:

- **The router forwards DNS to PiCache.** The router hands out itself as DNS
  server and passes the queries on to PiCache. Everything works, but every
  query comes from the router, so PiCache cannot tell devices apart: no
  per-device statistics, and groups and parental controls cannot work. The
  check warns when at least 80 % of at least 200 queries a day come from the
  router, and the health check `network` warns too.
- **The router announces itself as IPv6 DNS server.** Devices that have IPv6
  then ask the router over IPv6 and bypass PiCache. The check warns when your
  network has IPv6 but no device asked PiCache over IPv6 for a day.

The page shows the steps for your router with PiCache's addresses filled
in. **Scan network** (admins) sends one small, empty UDP packet to each
address of the local network (at most 512 addresses, the /24 around
PiCache's address for larger networks) so that devices that are switched on
show up; it is not needed for normal operation. Devices only appear after
they talked on the network recently, and devices with hard-coded DNS servers
or encrypted DNS never ask PiCache.

If your router cannot hand out another DNS server at all, PiCache can do it
itself: see [DHCP server](#dhcp-server). While PiCache serves DHCP, the page
says so instead of showing the router's IPv4 DNS steps, and it mentions
PiCache's own IPv6 announcements when they are on.

### Router set-up: FRITZ!Box

In the FRITZ!Box interface (labels of the English interface in brackets):

1. **IPv4**: *Heimnetz → Netzwerk → Netzwerkeinstellungen →
   IPv4-Einstellungen* (*Home Network → Network → Network Settings → IPv4
   settings*; in newer FRITZ!OS versions under *IP-Adressen*): set
   **Lokaler DNS-Server** (*Local DNS server*) to PiCache's IPv4 address.
   The FRITZ!Box then hands out PiCache as DNS server over DHCP.
2. **IPv6**: *… → IPv6-Einstellungen* (*IPv6 settings*): turn on
   **Unique Local Addresses (ULA) immer zuweisen** (*Always assign unique
   local addresses (ULA)*), so PiCache gets a stable IPv6 address; under
   **DNSv6-Server im Heimnetz** set **Lokaler DNSv6-Server** (*Local DNSv6
   server*) to PiCache's ULA (the `fd…` address the network check shows) and
   turn on **DNSv6-Server auch über Router Advertisement bekanntgeben
   (RFC 5006)** (*Also announce DNSv6 server via router advertisement
   (RFC 5006)*).
3. **Do not** enter PiCache under *Internet → Zugangsdaten → DNS-Server*
   (*Internet → Account Information → DNS Server*): the FRITZ!Box then
   forwards everything and PiCache sees only the box.

Devices pick up the new settings when they renew their lease or reconnect
to the network (switching Wi-Fi off and on is enough).

### Router set-up: other routers

- Set the **DNS server of the DHCP server** to PiCache's IPv4 address
  instead of the router's own address. Do not set PiCache as the router's
  upstream DNS server, that is forwarding.
- **IPv6**: announce PiCache's ULA (an `fd…` address; give PiCache one if it
  has none) as DNS server through router advertisements (RDNSS) or DHCPv6,
  or turn off the router's own IPv6 DNS announcement.
- **Docker in bridge mode**: every query appears to come from the container
  network's gateway. Use host networking (the provided compose file does).
- **Refused sources**: if the check lists addresses whose queries PiCache
  refused, they are not in an allowed network, typically devices with a
  public (global) IPv6 address. For addresses of a network PiCache's machine
  is connected to, the check offers **Allow the networks this machine is
  connected to** (`dns.trustConnectedNetworks`, see below); for others it
  offers their /64 under **DNS settings → Access → Additional networks**
  (`dns.allowedNetworks`).

---

## IPv6 and dual-stack networks

PiCache answers over IPv4 and IPv6 alike. What to know for networks with
IPv6:

- **Give PiCache a ULA and announce it.** A unique local address (`fd…`)
  stays the same when the provider changes your prefix; global addresses do
  not, and privacy addresses change every day. Announce the ULA as IPv6 DNS
  server (router advertisement or DHCPv6; the FRITZ!Box steps are
  above). If you bind the DNS listener to specific addresses
  (`PICACHE_DNS_LISTEN`), use the ULA, not a global address. PiCache itself
  answers its server names with its stable addresses, ULAs first, and never
  with deprecated or temporary ones while others exist.
- **Which devices may ask.** Loopback, private IPv4, ULA and link-local
  addresses are always allowed. Devices that ask from a public (global)
  IPv6 address of your LAN are refused unless you allow them: turn on
  **DNS settings → Access → Allow every network this machine is connected
  to** (`dns.trustConnectedNetworks`), which follows prefix changes within a
  minute, or add the prefix to the additional networks (it goes stale when
  the prefix changes). Do not use the switch on a cloud server or VPS: its
  connected network can belong to other customers, and PiCache would become
  their resolver.
- **Clients over IPv4 and IPv6.** A client configured by an IPv4 address or
  a CIDR also covers the IPv6 addresses of the same device on the same
  network, privacy addresses included: PiCache learns them from the kernel's
  neighbour table (the device's MAC address) within about a second of the
  first query. A MAC address is still the most robust identifier, and the
  only one that works for devices behind another router. With Docker bridge
  networking PiCache sees no MAC addresses.
- **Names and statistics per device.** An IPv6 address without a name of
  its own gets the name of the device's IPv4 address (for example the
  name the router's DHCP server gave it). The overview and **Clients &
  groups** show one row per device with all its addresses, and link to the
  query log with all of them.
- **Router over IPv6.** With the router resolver on *auto* and no IPv4
  default gateway (an IPv6-only network), PiCache asks the router over IPv6:
  at its ULA or global address if known, else at its link-local address.
  Queries from any address of the router are never sent back to it and are
  not rate limited.
- **Broken IPv6.** If your network announces IPv6 but it does not reach the
  internet, turn on **Do not answer IPv6 addresses (AAAA)**
  (`dns.disableAAAA`): devices then use IPv4. Your local AAAA records and
  PiCache's own names keep working.
- **IPv6-only networks with NAT64.** Turn on **DNS64** (`dns.dns64`) with
  the NAT64 prefix of your gateway (the well-known `64:ff9b::/96` by
  default; only /96 prefixes): names with IPv4 addresses only then get
  synthesised IPv6 addresses through the gateway. DNS64 and "Do not answer
  IPv6 addresses" exclude each other.
- **Bootstrap.** The default bootstrap servers include the IPv6 addresses
  of Quad9 and Cloudflare (`2620:fe::fe`, `2606:4700:4700::1111`), so
  encrypted upstreams also work on IPv6-only hosts; IPv4 is tried first.
  An unchanged default list gets them with the update to 0.6.0.

---

## DNS resolution and protection

### Upstream DNS servers and fallback

By default PiCache sends queries over DNS-over-HTTPS to **Quad9**
(`https://dns.quad9.net/dns-query`), which blocks known malware domains.
Quad9's blocks show in the query log as **Blocked by upstream**
(`blocked-upstream`) and count as blocked. Allow rules cannot lift such a
block (the upstream answered nothing usable); to reach a name Quad9 blocks,
create a conditional forwarder for it to another resolver.

If Quad9 does not answer at all (no reply within 3 seconds per upstream and
at most 7 seconds for all of them, or network errors), PiCache asks the
**fallback** upstream, **Cloudflare's malware-filtering resolver**
(`https://security.cloudflare-dns.com/dns-query`), another operator, so an
outage of one provider is bridged by the other. A reply of any kind (also
an error reply) never switches to the fallback, which gets at most another
2.5 seconds. While a fallback answers, the health check warns "fallback DNS
in use". A network that blocks both providers (for example a firewall that
allows only its own DNS server) needs its own upstreams in **DNS settings →
Upstream DNS servers**, e.g. the router's address.

Installations upgraded from a version before 0.9.0 that still used the old
default list (Quad9 and Cloudflare) get Quad9 and the fallback. An
installation with its own upstream list keeps it and gets no fallback, so
queries never start going to another operator; turn the fallback on in
**DNS settings → Upstream DNS servers → Fallback DNS** if you want one.

- A **local resolver** as upstream (a Docker service such as unbound, a
  NAS, the router) is entered by its **IP address**. Plain DNS upstreams
  may also be given by name (`dns.example.com`, `tcp://dns.example.com:5353`),
  but only public names: PiCache resolves them through the bootstrap
  servers and dials only public addresses.
- **Connect to upstreams over IPv6 first** (`dns.bootstrapPreferIpv6`)
  dials DoT, DoH and named upstreams over IPv6 first, for IPv6-only and
  DS-Lite networks.
- **Client subnet (ECS)** (`dns.ecs`, off by default) sends a part of the
  client's address to the upstreams, so CDNs can pick a nearby server.
  Quad9's default endpoint and Cloudflare ignore it (Quad9's
  `https://dns11.quad9.net/dns-query` uses it). Mode `client` discloses the
  /56 of global IPv6 client addresses, the household's prefix, also for
  queries sent over IPv4.
- **Fastest address** (upstream mode `fastest_addr`) asks the upstreams in
  parallel and puts the address of an answer first that accepts a TCP
  connection fastest. PiCache then connects to addresses of names its
  clients look up (bounded, public addresses only, SECURITY.md).

### DNS rebinding protection

Many routers (the FRITZ!Box among them) block DNS answers that point public
names at addresses of the home network. Once the devices use PiCache, the
router no longer sees these queries, so PiCache does it itself
(`dns.rebindProtection`, on by default): an answer from the upstreams that
points a name at a private, loopback or link-local address is blocked and
logged as **Rebinding blocked** (`blocked-rebind`). Names of the local
domain, local records, DHCP names, conditional forwarders with their own
targets and the router's answers are not affected.

When a legitimate service answers with private addresses:

- add its domain to the allowed domains in **DNS settings → Protection**
  (`dns.rebindAllow`; the query panel of a `blocked-rebind` row offers
  **Allow rebinding for `<name>`**). `plex.direct` (Plex) is allowed by
  default;
- the host names in `web.allowedHosts` (a public name you gave the PiCache
  web UI) are allowed automatically;
- a **local resolver used as default upstream** answers LAN names with LAN
  addresses: allow its domains, or better create a conditional forwarder
  for them (forwarders with their own targets are not checked);
- **DNSBL zones** that a mail server on your network queries through
  PiCache (they answer with 127.0.0.x) must be allowed too; public DNSBLs
  usually refuse queries that come through public resolvers anyway.

### Bare names

An address query (A, AAAA, HTTPS, SVCB, ANY) for a bare name without a dot
(`nas`, `printer`) is answered as
`<name>.<local domain>` (`nas.lan`): from local records, DHCP names, a
conditional forwarder, the router, else "does not exist". It is never sent
to the upstreams, so device names do not leak (`dns.domainNeeded`, on by
default; other query types such as `NS` or `DS` of top-level names still go
upstream, so validating resolvers behind PiCache keep working). `wpad` and
`isatap` come only from local records, never from a device that calls
itself so. A conditional forwarder with the domain `(unqualified)` (**Single-label
names** in the forwarder form) gets the bare names that nothing local
answers.

### Blocked clients

The blocked clients in **DNS settings → Access** (`dns.blockedClients`), and
**Block device** in the query log and **DNS → Clients & groups → Seen
recently**,
drop all DNS queries of an address, a network or a device's MAC address
without an answer. It is a DNS block only: the device can still use the
download cache, and a device that uses another DNS server is not affected.
A MAC entry covers all addresses of a device, but a brand-new address (a
fresh IPv6 privacy address) passes until the neighbour table knows its
MAC. PiCache refuses entries that would block itself, the router, the
container network's gateway or a trusted forwarder, and never drops them
even if such an entry got into the settings (a restored backup, a changed
router).

**Dropped domains** (`dns.droppedDomains`) get no answer at all and are not
logged, which makes troubleshooting harder; over TCP the connection is
closed, and other queries pipelined on it are lost.

### Forwarders that name their clients (EDNS)

When another DNS server forwards its clients' queries to PiCache (a second
router, a dnsmasq instance), PiCache sees only that server. If it adds each
client's address (ECS) and MAC (option 65001), PiCache can identify the
devices behind it: add the forwarder's address to the trusted EDNS forwarders in
**DNS settings → Access** (`dns.ednsClientTrusted`). The forwarder must remove
the options its own clients send and add its own, otherwise any client
behind it can claim to be another device (and get its groups, parental
controls and rules). For dnsmasq:

```
--strip-subnet --strip-mac --add-subnet=32,128 --add-mac
```

Trusted forwarders are exempt from the rate limit (a whole network behind
one address).

---

## DHCP server

PiCache can hand out IPv4 addresses itself and announce itself as IPv6 DNS
server, for routers that cannot hand out another DNS server (many provider
routers). If your router can, prefer that ([Network check](#network-check)):
one DHCP server less to look after. The DHCP server is off by default and
serves only on the interface you choose.

It hands out addresses from a range, with the router (the default gateway),
PiCache as DNS server and your local domain, and registers the devices'
host names in DNS (`laptop.lan`, and the reverse name of the address), so
the query log and client lists show names. Static leases give a device a
fixed address (and name). For IPv6 it sends router advertisements that carry
only the DNS server (PiCache's ULA) and the domain, never a prefix and never
itself as router (router lifetime 0): the router keeps doing addresses and
routing. Optionally it answers stateless DHCPv6 information requests with
the same DNS server. It hands out no IPv6 addresses.

There is nothing to install: the DHCP server is part of every installation
and switched on in the web UI. While it is off PiCache holds no DHCP port.
On a host that runs another DHCP server (dnsmasq, a router distribution)
you can prevent it with `PICACHE_DHCP=off` in `picache.env`
(`install.sh --without-dhcp`; Docker: the container's environment).

### Before you start

1. **Give PiCache a fixed address.** A static IPv4 address configured on
   the machine (or container) itself; a reservation in the router is not
   enough, because the router's DHCP server will be off. PiCache refuses to
   serve while its own address comes from a DHCP client. Examples:
   - Debian with ifupdown (`/etc/network/interfaces`):
     `iface eth0 inet static` with `address 192.168.178.10/24` and
     `gateway 192.168.178.1`;
   - NetworkManager: `nmcli con mod <connection> ipv4.method manual
     ipv4.addresses 192.168.178.10/24 ipv4.gateway 192.168.178.1`;
   - Proxmox LXC: `pct set <ctid> -net0 name=eth0,bridge=vmbr0,ip=192.168.178.10/24,gw=192.168.178.1`.
   Pick an address outside the range PiCache will hand out.
2. **For IPv6 announcements** PiCache needs a stable ULA (`fd…`). On a
   FRITZ!Box turn on *Heimnetz → Netzwerk → Netzwerkeinstellungen →
   IPv6-Einstellungen → Unique Local Addresses (ULA) immer zuweisen*
   (*Home Network → Network → Network Settings → IPv6 settings → Always
   assign unique local addresses (ULA)*). Turn off the router's own IPv6
   DNS announcement where possible, otherwise devices may keep asking the
   router.

### Enabling it

- **Bare metal, VM, LXC:** nothing to do. When you switch the server on,
  PiCache opens UDP ports 67 and 547 at once (the unit keeps
  `CAP_NET_BIND_SERVICE`). The IPv6 router advertisements need a raw socket
  that PiCache can open only at start: switching them on asks for one
  restart (**Restart PiCache** on the page). An installation from before
  0.8.0 has a unit without `CAP_NET_RAW`: run the one-line installer once
  ([Updates](#updates)).
- **Docker:** host networking (`docker-compose.yml`) and `NET_RAW` in
  `cap_add` for the IPv6 router advertisements (both in the shipped compose
  file). The container opens the DHCP ports only at start: after switching
  DHCP (or the router advertisements) on, restart PiCache once with the
  button on the page. Not available with bridge networking.

PiCache remembers what to open at the next start in two empty files in the
data directory (`dhcp.sockets`, `dhcp.ra`); it keeps them itself.

Then open **DNS → DHCP**:

1. Choose the interface (the page shows its address and warns when it is
   dynamic), the range, the lease time, and optionally the router, DNS
   server and domain (defaults: the default gateway, PiCache, the local
   domain). **Advanced options** add NTP servers, the MTU, a WPAD URL (every
   DHCP client that asks uses that proxy configuration) and extra search
   domains, rapid commit (only when PiCache is the only DHCP server) and
   **Hand out addresses only to devices with a reservation** (devices
   without one get no address from PiCache; a convenience, not an access
   control, since MAC addresses and client identifiers can be forged).
2. **Find other DHCP servers**: PiCache asks the network for DHCP servers
   (5 s) and lists the ones that answered. In Docker this search is
   possible only once the DHCP server is switched on and PiCache restarted
   (the container opens the DHCP ports only at start); skip this step
   there: PiCache searches by itself before it hands out addresses.
3. **Switch off the router's DHCP server.** FRITZ!Box: *Heimnetz → Netzwerk
   → Netzwerkeinstellungen → IPv4-Einstellungen* → *DHCP-Server aktivieren*
   off (*Home Network → Network → Network Settings → IPv4 settings →
   Enable the DHCP server*). Other routers: the DHCP server switch of the
   LAN settings.
4. Enable PiCache's DHCP server. It searches for other DHCP servers again
   and starts serving when none answers. Devices move over when their lease
   from the router runs out or when they reconnect (switch Wi-Fi off and
   on); a device keeps its address when it is free and inside PiCache's
   range.

PiCache refuses to serve (status **Blocked**) while its own address is
dynamic, while another DHCP server answered its search within the last 10
minutes or a device asked another DHCP server within the last 24 hours,
while the interface has no single private IPv4 address, or while the range
does not fit the subnet. The page names the reason and what to do. After
switching off the router's DHCP server, click **Search again**: a search
that finds no other server lifts the earlier detections at once. Only if
another server must stay on (for example one that serves other devices
only), **Hand out addresses even when another DHCP server answers**
overrides that check;
two servers handing out addresses in the same network cause address
conflicts. While PiCache serves, a newly seen DHCP server does not stop it,
but raises a health warning (and a notification).

**Reservations** give a device a fixed address, optionally a host name, its
own lease time and a client identifier (option 61) that matches too, for
devices that change their MAC address. They can be exported (CSV or hosts
format) and imported (CSV, hosts lines, or `mac ip [hostname]` lines, with a
preview; **Replace all reservations** deletes the reservations that are
not in the list). **Recent requests** shows the last 200 DHCP exchanges (in
memory).
**End all leases** deletes every lease (devices keep their addresses until
they renew); **Reset DHCP** puts the DHCP settings back to their defaults
and deletes every reservation and lease. Devices without a usable host name
get a generated DNS name such as `192-168-178-23.lan` (**Generate names for
devices without one**, on by default; DNS only).

### IPv6 announcements

Under **DNS → DHCP → IPv6**:

- **Router advertisements** announce PiCache's ULA as DNS server (RDNSS)
  and the domain (DNSSL) with a lifetime of 30 minutes: three at the start,
  then one every 200 to 600 seconds, and one shortly after a device asks
  (a router solicitation). They never make PiCache a router. Switching them
  off, a new address and stopping PiCache send one last announcement that
  withdraws the DNS server.
- **DHCPv6** answers information requests (stateless DHCPv6) with the same
  DNS server and domain; the router advertisements then tell devices to
  ask (the O flag).

They need a stable ULA on the interface and, for the router advertisements,
the raw socket, which PiCache opens only at start (switching them on needs
one restart; Docker needs `NET_RAW`); the page says what is missing. They
are off by default for a reason: while they are on, the raw socket stays
open, and a compromised PiCache could send any IPv6 control message on the
LAN ([SECURITY.md](SECURITY.md#dhcp-server)). Use them only when the router
cannot announce PiCache itself.

While PiCache announces itself over IPv6 it also looks for other routers and
DHCPv6 servers that announce their own DNS server (when the announcements
start and every 10 minutes): router advertisements are seen while PiCache's
own are on, DHCPv6 servers when they answer a relayed request (servers that
do not are seen only through the routers' "offers DHCPv6" flags). A router
that announces its own DNS server raises a warning; the fix is on the router
([Router set-up: FRITZ!Box](#router-set-up-fritzbox)). A router that
announces PiCache's ULA is fine.

### Troubleshooting

- **Unavailable, "PICACHE_DHCP=off is set":** remove the line from
  `/etc/picache/picache.env` (Docker: the container's environment) and
  restart PiCache.
- **Another DHCP server on this host does not start while PiCache's DHCP
  server is on** (or PiCache's says the port is in use): only one program
  can use UDP port 67. Switch one of them off; `ss -ulnp 'sport = :67'`
  shows who holds it. `PICACHE_DHCP=off` prevents switching PiCache's on.
  The health check `dhcp` fails while PiCache's is on and the port is taken.
- **Unavailable, "restart PiCache":** the ports open only at start in this
  installation (Docker). Restart once with the button on the page.
- **Unavailable in Docker bridge networking:** use `docker-compose.yml`
  (host networking) instead of `docker-compose.bridge.yml`.
- **Blocked, "another DHCP server answers":** the router's DHCP server (or
  another one) is still on. Switch it off and search again. A server a
  device asked within the last 24 hours keeps blocking until then; if you
  are sure it is off, use the override once and switch it off again later.
- **Blocked, "comes from a DHCP client":** the machine's address is
  dynamic; configure a static address (above).
- **Devices get no address:** check that a firewall on the host lets UDP
  port 67 in and 68 out (`nft list ruleset`), and look at the counters on
  the page (`dropped` counts malformed and rate-limited packets). Devices
  behind another router or a DHCP relay are not served.
- **No IPv6 announcements:** "no-ula" means PiCache has no stable ULA on
  the interface. "Restart PiCache" means the raw socket opens at the next
  start. **Router advertisements say no-cap-net-raw** (PiCache did not have
  `CAP_NET_RAW` at start): on bare metal, VMs and LXC the unit is older than
  0.8.0 — run the one-line installer once ([One-line install](#one-line-install));
  in Docker add `NET_RAW` to `cap_add` and recreate the container
  (`docker compose up -d`); an unprivileged container may not allow
  `CAP_NET_RAW` at all.
- **Names:** a device's DNS name is its host name plus the domain
  (`laptop.lan`). Two devices with the same host name: the second gets its
  generated name (`192-168-178-23.lan`; the lease list shows this); give it
  a reservation with another name. Local DNS records with the same name win.

---

## Parental controls

**DNS → Parental controls** restricts the devices of a client group (for
example a group "Kids" with the children's phones, tablets and consoles):

- **Blocked services**: apps and sites from a built-in list of 144
  services in 13 categories (video, social networks, messaging, games,
  music, AI, dating, gambling, shopping, VPN and proxy apps, app stores,
  file hosting, news; for example YouTube, TikTok, Instagram, WhatsApp,
  Roblox, Fortnite, Steam, Netflix, ChatGPT, Tinder, bet365, NordVPN) that
  are always blocked for the group.
- **Schedules** (up to 10 per group): on selected days between two times,
  block all internet (a bedtime, for example school nights Sunday to
  Thursday, 21:00 until 07:00 the next day) or selected services (for
  example YouTube and TikTok during homework time).
- **Safe search**: per search engine (Google, Bing, DuckDuckGo, Ecosia,
  Yandex, Pixabay) the restricted mode, and YouTube's restricted mode
  (moderate or strict). PiCache answers the engine's names with its
  restricted host (for example `forcesafesearch.google.com`); the query
  log shows them as *Safe search*.
- **Category switches**: adult content, gambling, dating, piracy and
  encrypted-DNS/VPN bypass. Each switch assigns the group to one catalogue
  list that PiCache downloads (created on first use, visible in
  **Filtering → Blocklists**; switching off removes the group, the list
  stays).
  Unlike blocked services they are not schedulable. Switching one on also
  enables its list again for other groups that have it.
- **Block internet now** and **Lift restrictions** for 30 minutes, 1 or
  2 hours or until a time (at most 7 days), and **End** for either.
- **Pause filtering** of the group (for a while, at most 7 days): the group's
  lists and rules stop for its devices, its security lists (malware,
  phishing) included; the devices' other groups still apply.
- **Test**: a domain and a device show whether and why PiCache blocks it.

A device in several groups gets the restrictions of all its groups; lifting
restrictions affects only the group it is done for. **What stays in
force**: parental controls, safe search and the protection lists (every
list of the categories adult, gambling, dating, piracy and DNS/VPN bypass,
also one you add yourself) stay on while blocking is paused or disabled in
the header and while a group's filtering is paused, so a pause never ends a
bedtime. "Lift restrictions" lifts only the group's blocked services and
schedules; safe search and the category switches stay on. The Default
group applies to every device that is in no other group. Blocked queries
appear in the query log as *Blocked by schedule*, *Blocked service* or,
for a protection list, *Blocked by list*, with the group and the schedule,
service or list as reason. A user allow rule for the group
(**Filtering → Rules**) lets a name through, for example a school website
during bedtime or a site a category list blocks by mistake; it does not
lift safe search.

Things to know:

- Devices must be identified: add them as clients by IP or MAC address
  (**DNS → Clients & groups**, or **Add as client** in the network check).
  Phones and tablets use a private MAC address per Wi-Fi network; keep it
  fixed for your Wi-Fi (the usual default) so that the device keeps its
  identity.
- Schedules use the time of the PiCache host. A Docker container uses UTC
  unless you set `TZ` (for example `TZ: "Europe/Berlin"` in the compose
  file).
- DNS blocking starts when an app looks a name up again, usually within
  minutes; open connections (a running video, a game session) can continue
  until they reconnect.
- Encrypted DNS and VPN apps bypass PiCache (parental controls, safe search
  and the category switches), and so do mobile data and other networks.
  Switch on the bypass category for the group (the list **HaGeZi
  DoH/VPN/TOR/Proxy Bypass**), and on the router, if it can, block outgoing
  DNS (ports 53 and 853) to servers other than PiCache. Since 0.10.0 an
  existing bypass list applies also while blocking is paused; give it the
  category *security* (**Filtering → Blocklists**) for the old behaviour.
  The switch *Bypass services* then shows off for its groups, because the
  list pauses with blocking; switching it on gives the list its category
  back, for all its groups.
- A list meant to block whole top-level domains (`||zip^`, `*.xyz^`) needs
  the category *Abused top-level domains*: in every other list PiCache
  ignores such entries, so a broken or hostile list cannot block all of
  `.com`. The list's details show how many entries it ignores, and the
  health check *blocklists* names own lists that ignore some.
- Memory: the category lists are large (the adult list has about 470 000
  entries). One entry needs about 24 bytes; PiCache warns in the health
  checks when all lists together hold more than 4 000 000 entries, which
  peaks at about 250 MiB during a list update, enough for a 1 GB host
  running PiCache alone.

---

## Port conflicts

### Port 53 conflicts

DNS is mandatory: if PiCache cannot bind port 53 it exits with
`port 53 is in use; is systemd-resolved's stub listener or another DNS server
running?`. Find the other program with `sudo ss -lunp 'sport = :53'`.

**systemd-resolved** (installed by default on some images and cloud
templates; on Debian 13 it is the separate `systemd-resolved` package)
listens on `127.0.0.53` and `127.0.0.54`. Choose one fix:

- **A) Bind PiCache to specific addresses.** Specific addresses do not collide
  with resolved's loopback stub. In `/etc/picache/picache.env`:

  ```sh
  PICACHE_DNS_LISTEN=192.168.1.5:53,127.0.0.1:53
  ```

  Use the machine's static LAN address. Add its IPv6 unique local address
  (`fd…`) too if clients use IPv6 DNS, not a global address: that changes
  with the provider's prefix.

- **B) Turn off the stub listener** and let the host resolve through PiCache:

  ```sh
  sudo mkdir -p /etc/systemd/resolved.conf.d
  printf '[Resolve]\nDNS=127.0.0.1\nDNSStubListener=no\n' | \
    sudo tee /etc/systemd/resolved.conf.d/picache.conf
  sudo mv /etc/resolv.conf /etc/resolv.conf.backup
  sudo ln -s /run/systemd/resolve/resolv.conf /etc/resolv.conf
  sudo systemctl reload-or-restart systemd-resolved
  ```

  `DNS=127.0.0.1` is necessary. Without it the host would keep asking
  `127.0.0.53`, which no longer answers. PiCache does not depend on the
  host's resolver: it reaches its upstreams through their bootstrap IPs.

**Other DNS servers** (dnsmasq, bind9, unbound or another DNS filter; libvirt
also runs a dnsmasq on `virbr0`): stop and disable them, or use fix A with
addresses they do not use.

**Docker:** with host networking the same fixes apply. In bridge mode, publish
DNS on the host's LAN address instead of all addresses:
`"192.168.1.5:53:53/udp"` and `"192.168.1.5:53:53/tcp"`.

### Other ports

A clash on 80, 443, 8080 or 8443 does not stop PiCache. The listener is
skipped, the error is logged and **System → Health & about** shows it.
Without the :80 listener, the download cache's DNS answers stay off. At
least one of the two web listeners must work. Free the port, or move the
listener, for example `PICACHE_WEB_LISTEN=:8081`, or `PICACHE_WEB_LISTEN=off`
to serve the UI over HTTPS only. The download cache ports cannot be moved in
practice, because game clients always connect to 80 and 443.

---

## Persistent data

Everything persistent lives in exactly two places plus optional NAS mounts.

| Path (bare metal / LXC) | Docker | Contents | Backup? |
|---|---|---|---|
| `/var/lib/picache` (`PICACHE_DATA_DIR`) | `/data` | `picache.db` (configuration: settings, users, lists, rules, clients, groups, parental controls, local records, services, storage targets with sealed NAS passwords, audit log); `logs.db` (query log, cache events, sessions, statistics, evictions, seen clients, warning history); `cache-index/<store-id>.db`; `lists/`; `cache-domains/`; `tls/`; `keys/master.key` (0600); `instance-id`; `setup-token` (until setup is done); `backups/` (automatic pre-upgrade copies, newest 3); `storage-requests/` (mount requests for the root helper); `update-requests/` (update request and progress of the update helper); `picache.db.before-restore` (after a restore) | `picache.db` (UI download or file copy while stopped); everything else is rebuildable. `keys/master.key` separately if stored NAS passwords should survive a move to another machine. |
| `/var/cache/picache` (`PICACHE_CACHE_DIR`) | `/cache` | The built-in **local** cache store (slice files). Large. | No |
| `/srv/picache/<id>` (`PICACHE_MOUNT_ROOT`) | `/srv/picache` (bind, `rslave`) | NAS cache stores. The only place outside the cache dir where stores may live (the only NAS path writable inside the sandbox). | No |
| `/etc/picache/picache.env` | environment | Bootstrap settings only. Read by systemd **and by every CLI command**. | Yes |
| `/etc/picache/credentials/<id>.cred` | – | NAS credentials written by the root helper (0600, root), and `<id>.applied`, a fingerprint of the settings that are mounted. Deleted when the target is deleted or leaves host-apply mode. | – |

Rules:

- The databases must live on a local disk. PiCache refuses to open a database
  on NFS or CIFS. Only cache slice files may live on a NAS.
- A broken `logs.db` is moved aside (`logs.db.broken-<timestamp>`) and
  recreated; DNS keeps running. A broken cache index is rebuilt from the
  slice files.
- `keys/master.key` encrypts stored NAS passwords and TOTP secrets. Backups
  never contain it.

---

## Backup and restore

**What to back up:** `picache.db`, which holds the whole configuration.
`logs.db` (history and statistics) is optional. The cache, the cache index,
lists and cache-domains snapshots are rebuilt automatically. Keep
`keys/master.key` separately and safely if stored NAS passwords (and, for
file copies, TOTP secrets) should keep working on another machine. Without
it, re-enter the NAS passwords and, after a file restore, sign in with
`picache reset-password <user>` (which disables TOTP).

### In the web UI

**System → Backup & restore**:

- **Download** gives a consistent copy (`picache-backup-<date>.db`) of the
  configuration and the audit log. Accounts are never included: users with
  their password hashes and TOTP secrets, sessions and API tokens are
  removed. Sealed NAS passwords are included only if you opt in, and they are
  useless without the master key.
- **Restore** needs a browser session and your current password (API tokens
  cannot restore). It accepts a backup of up to 512 MiB, checks its
  integrity and schema version (backups from a newer PiCache are refused),
  refuses databases with triggers, views, virtual tables, generated columns,
  tables or indexes this PiCache does not have, indexes defined differently,
  or altered account tables, and stages it. The next restart checks it again and applies it: the
  configuration comes from the backup, while the accounts, passwords, TOTP,
  API tokens and audit log of the running instance stay. Everyone has to
  sign in again. The previous database is kept as
  `picache.db.before-restore`. If PiCache cannot start with the restored
  database, it puts the previous one back automatically (the failed file is
  kept as `picache.db.failed-restore-<timestamp>`).

### Scheduled backups

**System → Backup & restore → Scheduled backups** writes the same backup as
**Download** on a schedule:

- daily or weekly at a local time (default 03:30), keeping the newest 1–90
  files (default 7). The time is that of the PiCache host; a Docker
  container uses UTC unless you set `TZ` (for example `TZ: "Europe/Berlin"`
  in the compose file). The page shows which time zone applies;
- to the data directory (`<data>/backups/scheduled/`) or to a storage target
  that is online, for example your NAS (`<store root>/picache-backups/`), so
  that a copy survives the loss of the PiCache machine;
- a run missed while PiCache was down is made up within ten minutes after the
  next start;
- **Run now** starts one at once; stored backups can be downloaded and
  deleted there. Only files named
  `picache-backup-<instance>-<time>.db` of this instance are ever deleted.

Like downloads, scheduled backups never contain accounts, and sealed secrets
(NAS passwords, notification tokens) only if you opt in. Keep
`keys/master.key` separately if restored secrets should work on another
machine. A failed run is reported as a notification (`backup.failed`, see
[Notifications](#notifications)).

### With an API token

Automated backups from another machine use an **admin** API token (restores
are done in the web UI):

```sh
curl -fsS -H "Authorization: Bearer $PICACHE_TOKEN" \
  -o "picache-$(date +%F).db" https://192.168.1.5:8443/api/v1/system/backup
```

(Add `--cacert` with PiCache's certificate, or use your own certificate.)

### As files

Stop PiCache first; copying a live SQLite database can produce a broken copy.

```sh
# bare metal / LXC
sudo systemctl stop picache
sudo cp -a /var/lib/picache/picache.db /var/lib/picache/keys/master.key /root/
sudo systemctl start picache

# Docker
docker compose stop picache
docker run --rm -v picache_picache-data:/data:ro -v "$PWD":/backup alpine \
  cp /data/picache.db /data/keys/master.key /backup/
docker compose start picache
```

To restore from files, stop PiCache, delete `picache.db-wal` and
`picache.db-shm`, copy the backup to `picache.db`, and make it owned by the
service user (`chown picache:picache` on bare metal, `65532:65532` in
Docker). Then start PiCache. A file restore replaces everything, including
the accounts, API tokens and sessions of the copy; use the UI restore to keep
the current accounts. PiCache creates no triggers or views; if the file has
any, they are removed at start and a warning is logged.

### Recovering a damaged picache.db

When PiCache reports a damaged configuration database (for example "database
disk image is malformed" at start, after a power cut or a failing SD card),
check it and copy every readable row into a new file:

```sh
sudo systemctl stop picache
sudo picache db check              # integrity, foreign keys, schema versions
sudo picache db salvage --out /var/lib/picache/picache.db.salvaged
```

Read the report. When `salvage` created the file (exit code `0` or `3`),
put it in place. The data directory is readable by the service user only,
so every step runs as root, and the block stops at the first step that
fails (nothing is started on the damaged database then):

```sh
sudo sh -e -c '
  d=/var/lib/picache
  ts=$(date -u +%Y%m%dT%H%M%SZ)
  test -s "$d/picache.db.salvaged"
  for f in picache.db picache.db-wal picache.db-shm; do
    if [ -e "$d/$f" ]; then mv "$d/$f" "$d/$f.damaged-$ts"; fi
  done
  mv "$d/picache.db.salvaged" "$d/picache.db"
  systemctl start picache
'
```

- `picache db check` reads `picache.db` read-only and may run while PiCache
  runs. It prints the first 100 lines of SQLite's integrity check and of the
  foreign key check and compares the schema versions with the program. Exit
  code `0` no problems, `3` problems found, `1` error, `2` usage.
- `picache db salvage --out <file>` refuses while PiCache answers on this
  machine (the checks of `picache healthcheck`; `--force` skips this) and when
  the database was written by another PiCache version ("salvage with the
  PiCache version that wrote it"). It never modifies the source. It creates
  the new file (an existing path is refused, mode 0600, owned like the data
  directory), builds this version's schema in it, copies every table this
  version knows column by column (rows that cannot be read are skipped and
  counted), leaves out unknown tables, triggers and views, and checks the new
  file. It prints what was copied and lost per table. Exit code `0` every row
  copied, `3` rows lost or skipped, `1` error, `2` usage.
- The new file holds the accounts with their password hashes, TOTP secrets,
  sealed secrets and API token hashes: keep it private. If accounts were
  lost, run `sudo picache reset-password --admin` after the start.
- Docker: check while it runs with
  `docker exec -u 65532:65532 <container> /picache db check`; salvage with
  the container stopped:
  `docker run --rm --user 65532:65532 -v <data volume>:/data --entrypoint /picache <image> db salvage --out /data/picache.db.salvaged`,
  then move the files in the volume as above (the same block in
  `docker run --rm -v <data volume>:/data alpine sh -e -c '…'` with
  `d=/data` and without the `systemctl` line) and start the container.
- A backup (**System → Backup & restore**, scheduled backups) is the better
  way back when a recent one exists.

**Moving to a new machine:** install PiCache there, stop it, copy
`picache.db` (and `keys/master.key`) into the data directory with the right
owner, and start it (this keeps the accounts). Alternatively complete setup
on the new machine and restore a downloaded backup in the UI: the account
created there stays. The cache can be copied too, or it simply fills again.
A NAS store is adopted by initialising the target with *adopt* in
**Cache → Storage**.

---

## Logs and privacy

**System → Logs & privacy** sets what PiCache records. A preset sets four
switches: *Full* (query log and statistics, client addresses and domains
kept), *Hide domains* (domain names replaced by `hidden` in the query log;
no top lists of domains), *Anonymous* (additionally client addresses masked
to /16 or /48 and client names dropped) and *Off* (no query log, no DNS
statistics); *Custom* shows the switches. A change applies from then on;
stored rows are not rewritten, so clear the query log or the statistics
afterwards if they must go (**Clear data**; admins, needs
`PICACHE_DESTRUCTIVE_API` on). Per client, **Clients & groups** can exclude
the raw data (query log, cache requests, sessions) and the statistics
separately. *Ignored domains* are answered and filtered as usual but never
logged or counted (for noisy names such as a time server; unlike *dropped
domains*, which get no answer at all).

**Fewer writes on SD cards:** *Write interval* (`logs.flushSeconds`, 5 to
300 seconds, default 5) sets how often PiCache writes the query log and the
statistics counters. A longer interval saves writes, but up to that many
seconds of query log rows and counts (and up to the interval or a minute,
whichever is longer, of the seen-client data) are lost on a power cut, and
the query log, exports and minute-based charts lag by as much. The live
query view is not delayed.

### From the command line

`picache logs tail` follows the query log and `picache logs export` saves it
as NDJSON or CSV, through the API (a **read** API token is enough; create one
under **System → API tokens**):

```sh
export PICACHE_TOKEN=pc_...          # or: --token-file ~/.picache-token (chmod 600)
picache logs tail --status blocked --client 192.168.1.20
picache logs tail --json | jq .qname
picache logs export --format csv --range 7d --rcode NXDOMAIN --out nxdomain.csv
picache logs export --format ndjson --from 2026-09-01T00:00:00Z --to 2026-09-02T00:00:00Z --out - | gzip > sept1.ndjson.gz
```

- The token comes from `PICACHE_TOKEN` in the environment or from
  `--token-file` (a regular file, not a link, at most 4 KiB; a warning when
  other users can read it). It is **never** read from `picache.env` (a
  `PICACHE_TOKEN` line there is ignored with a warning): that file configures
  the service.
- The URL is `--url`, else `PICACHE_URL`, else the local web listener
  (`http://127.0.0.1:8080` by default). The token is sent only to a loopback
  `http` URL, or to an `https` URL whose certificate is verified against the
  system's roots and PiCache's local CA (`<data>/tls/ca.crt`, when readable);
  a loopback `https` URL without a readable CA is not verified (like
  `healthcheck`). Any other URL is refused ("refusing to send the API token
  to …"). No proxy is used and redirects are not followed.
- The local listener is taken from `PICACHE_WEB_LISTEN` only when
  `picache.env` is readable (it is not for other users than root and the
  `picache` group: then `http://127.0.0.1:8080` is used). With another web
  listener, or with **Redirect HTTP to HTTPS** on (the commands then stop
  with "PiCache redirects to …"), pass the address: `--url
  https://127.0.0.1:8443` or `PICACHE_URL`.
- `tail` prints one line per query (local time, client, type, name, status,
  response code, duration) with control characters escaped, reconnects after
  1, 2, 4 … 30 seconds when the stream ends or PiCache is busy or
  restarting, stops with 1 on a revoked token (401/403) and with 0 on Ctrl-C.
- `export` takes the filters of the query log; `--out` must name a new file
  (created with mode 0600; `-` writes to stdout). An export stops after
  1 000 000 rows or 15 minutes; the file then ends with a marker and the
  command says so. One export runs at a time.
- While PiCache is stopped, another local user could open the loopback port
  and receive the token: prefer a read token, and a token file over a
  variable in a shell profile.

### Application log and support bundle

**System → Application log** (admins) shows the last 2000 log lines (the
newest 500 at first, **Show older records** loads the rest) with
level and component filters, and can switch on debug logging for a
component or for everything for 1 to 240 minutes (it ends by itself and at a
restart). Secrets are never shown; while client addresses are anonymised or
domains hidden, the page masks them too (the journal keeps them: keep
`PICACHE_LOG_LEVEL=info`).

**System → Health & about → Support bundle** (admins, password) downloads a
zip for a bug report: version, settings, health, listeners, the network
check, the DHCP state, database sizes, host resources and the application
log, with names, addresses and secrets replaced (SECURITY "Support
bundle"). Review the files before sharing them.

---

## Notifications

**System → Notifications** sends messages when something needs attention,
for example when the cache storage goes offline, a health check fails, an
update is available or installed, a scheduled backup fails, or sign-ins are
locked out after wrong passwords. Problems are reported only after they have
lasted about two minutes, and again when they are over.

| Channel | URL | Secret |
|---|---|---|
| **ntfy** | the topic URL, e.g. `https://ntfy.sh/<long random topic>` or your own server | access token (optional) |
| **Gotify** | the server, e.g. `http://192.168.1.20:8080` | application token (required) |
| **Webhook** | any URL that accepts a JSON `POST`, e.g. a Home Assistant webhook `http://homeassistant.local:8123/api/webhook/<id>` | value of an `Authorization` header (optional) |

- Each channel has a minimum severity (info, warning, error; default
  warning) and optionally a list of events. **Send test** checks a channel at
  once; the delivery log shows the last 200 attempts.
- On ntfy.sh, the topic name is the only protection: use a long random one,
  or your own server with an access token.
- Secrets are stored encrypted with the master key and never shown again;
  backups contain them only if you opt in. Messages never contain passwords,
  tokens or session data.
- A channel may point to an address in your LAN (Home Assistant, a
  self-hosted ntfy or Gotify). PiCache follows no redirects and gives up after
  10 seconds; it retries twice and sends at most 20 messages per channel in
  ten minutes.

The webhook body is:

```json
{"event":"storage.offline","severity":"warning","title":"…","message":"…",
 "time":"2026-09-26T03:30:00Z","instance":"picache-3d8b4c93de6c",
 "hostname":"picache","version":"v0.4.0"}
```

---

## Updates

PiCache looks for new releases by itself, but it installs one only when an
admin starts it. Every release comes with a `SHA256SUMS` file signed with the
PiCache release key, and PiCache installs no file whose checksum and
signature it cannot verify with the keys built into the running binary
([SECURITY.md](SECURITY.md#updates)).

| Deployment | How to update |
|---|---|
| Bare metal, VM, LXC | **Install update** in the web UI, or `sudo picache update` |
| Without Internet access | `sudo picache update --from <dir>` with the release files |
| Docker | `docker compose pull && docker compose up -d` |
| Bare metal, VM, LXC, by hand | the installer of the new release ([below](#manual-upgrade-with-the-installer)), or `get-picache.sh` again ([One-line install](#one-line-install)) |

### Checking for updates

- **System → Updates** shows the installed version, the newest release with
  its release notes and the time of the last check. A dot next to
  **Updates** in the navigation marks an available update.
- With **Check for updates daily** (on by default) PiCache asks the GitHub
  API (`api.github.com`) for the list of releases once a day; the first
  check runs about 5 minutes after the start. **Check now** checks at once.
  Checking never downloads or installs anything. Turn the setting off if
  PiCache must not contact GitHub; the CLI and **Check now** still work.
- Only stable releases are offered, unless **Include pre-releases** is on.
  Pre-releases are the tags with a hyphen, such as `v1.4.0-rc.1`; they are
  tested less.
- Only a release newer than the running version is offered; the web UI never
  offers a downgrade. A development build counts as newer than the release
  it is based on (`v1.2.3-4-gabc1234` is newer than `v1.2.3`); a build
  without a release version (`dev`, a commit hash) is offered every release.
- The check needs the GitHub repository to be public. Otherwise it reports
  that the release information is not reachable.
- Checks and downloads use HTTPS and need the CA certificates of the system
  (Debian package `ca-certificates`, part of every standard installation;
  minimal images may lack it). The Docker image brings its own.

### In the web UI (bare metal, VM, LXC)

The service runs as the unprivileged user `picache` and cannot replace its
own program. As with [host-apply](#host-apply-root-helper), it only queues a
request, and a root helper does the work:

1. **Install update** on **System → Updates** asks for your password (an
   interactive session is required; API tokens cannot start an update). It
   queues a request for exactly the version that the last check found:
   `<data>/update-requests/request` names only that version, never a URL, a
   file or a command.
2. `picache-update.path` starts `picache-update.service`
   (`picache update apply-pending`, root). The helper downloads
   `SHA256SUMS` and `SHA256SUMS.sig` of that release from the PiCache
   repository on GitHub and verifies the signature, downloads the binary for
   this machine and checks its SHA-256, and runs it once to check that it
   reports the expected version.
3. It keeps the installed program as `/usr/local/bin/picache.prev`, replaces
   `/usr/local/bin/picache` in one step (rename) and restarts
   `picache.service`.
4. It waits up to 90 seconds for the new version to pass the health check
   (the checks of `picache healthcheck`: the web UI and DNS). If it does not
   pass, the helper **rolls back**: it stops PiCache, puts `picache.prev`
   back, restores the copy of `picache.db` that the new version made before
   it migrated the database, and starts the previous version again.

The page shows each step and reloads itself when the new version answers.
DNS, the cache and the web UI are unavailable for a few seconds during the
restart. A failed or rolled-back update is reported on the page; the details
are in `journalctl -u picache-update`.

The update also replaces the systemd unit files with those of the release
(checked like the program: taken only from the signed release archive, only
PiCache's own units that are installed, never your drop-ins; the old ones
are kept as `<unit>.prev`), followed by `systemctl daemon-reload`. If that
fails, the old units are put back and only the program is updated; the page
then says "the unit files were not updated (…): run the one-line installer
once". A rollback puts back the units of that update too.

**Once for installations from before 0.8.0:** the update to 0.8.0 is
installed by the old helper, which cannot write unit files. Run the one-line
installer once ([One-line install](#one-line-install), or `install.sh` from
`picache-deploy.tar.gz`) to get the new units (`CAP_NET_RAW` for the IPv6
router advertisements, the resource weights and a helper that may replace
unit files); from then on updates in the web UI and with the CLI keep the
units current. `sudo picache update` (not sandboxed) replaces the units from
its first update after 0.8.0 even without the installer. A drop-in
`/etc/systemd/system/picache.service.d/60-dhcp.conf` left by `--with-dhcp`
of an older version is harmless (the same settings as the new unit) and is
removed by the next installer run.

**The update helper.** The installer sets it up by default
(`picache-update.path`, `picache-update.service` and the marker
`/etc/picache/updater.enabled`). Without the marker, or when PiCache does not
run under systemd, the page shows the command for the host instead of the
button. `--without-updater` installs PiCache without the helper and removes
a helper that an earlier run installed; a later run without the flag adds it
again.
`--uninstall` removes the helper. If you change `PICACHE_DATA_DIR`, run the
installer again: it points the helper at the new request directory with a
drop-in. If a request is not picked up within 3 minutes, the page says so;
check `systemctl status picache-update.path`.

### With the CLI

```sh
picache update --check                 # installed and newest version, release URL (no root needed)
sudo picache update                    # shows the start of the release notes, asks, installs
sudo picache update --yes              # installs without asking
sudo picache update --version v1.2.3   # a specific release
sudo picache update --prerelease       # consider pre-releases too
```

`picache update --check` exits with `0` when PiCache is up to date, `10` when
an update is available and `1` on an error, so monitoring scripts can use it.
Installing exits with `0` when the update is installed (or PiCache is already
up to date), `1` when it failed, was rolled back or declined, and `2` on a
usage error. The CLI and the helper never run at the same time (a lock on
the data directory).
`sudo picache update` runs the same steps as the helper, including the
signature check, the health check and the rollback. It needs no update
helper, so it also works after `--without-updater`.

`--allow-downgrade` lets `picache update` install an older release. It
replaces the program and the unit files: an older version cannot be expected to open a
database that a newer version has migrated, so restore the matching database
copy as described in
[Going back to an earlier version](#going-back-to-an-earlier-version).

### Offline

On a machine without Internet access, put the release files into one
directory with their original names: `SHA256SUMS`, `SHA256SUMS.sig`, the
binary for the machine (`picache-linux-amd64`, `-arm64` or `-armv7`) and,
to update the unit files too, `picache-deploy.tar.gz`. Then:

```sh
sudo picache update --from /path/to/release-files
```

The signature and the checksum are checked exactly as for a download from
GitHub. Without `--version`, PiCache installs the version that the verified
binary reports; it must be a release newer than the installed one (or use
`--allow-downgrade`), and PiCache asks before it installs.

### Docker

A container cannot replace its own program. Pull the new image and recreate
the container in `deploy/docker`:

```sh
docker compose pull && docker compose up -d
```

The volumes keep the data. **System → Updates** shows this command when a
new release is out; the update check runs in the container as well.

The images are `ghcr.io/hustenreizjuengling/picache:<tag>` for linux/amd64,
linux/arm64 and linux/arm/v7:

| Tag | Points to |
|---|---|
| `X.Y.Z` (for example `1.4.2`) | exactly that release; every release, including pre-releases (`1.5.0-rc.1`) |
| `X.Y` (for example `1.4`) | the newest release of that minor version |
| `latest` | the newest stable release |

The compose file uses `latest`. To stay on a minor version, or to go back to
an earlier release, set the tag in its `image:` line. An image built with
`docker compose up -d --build` is updated with `git pull` and the same
command.

### Manual upgrade with the installer

Running the installer of a new release is still supported. It is necessary
when the release notes ask for it (changed unit files), and it also works
with a binary built from source:

```sh
tar -xzf picache-deploy.tar.gz             # of the new release
sudo sh deploy/install.sh --binary ./picache-linux-amd64
```

It replaces the binary, the units and the license texts and restarts the
service. Your `picache.env` is kept.

### Database copies before an upgrade

PiCache migrates its databases automatically. When a new version starts for
the first time, it saves a copy of `picache.db` to
`<data>/backups/picache-<previous version>-<timestamp>.db` before it runs
any migration, and keeps the newest three. If the copy cannot be made (for
example because the disk is full), the new version does not start, so a
database is never migrated without a copy; an update then rolls back. The
rollback of a failed update restores the first copy of that run, only when
the service could be stopped and the copy fits into the free space. The version
comes from the build: releases report their tag, `make` reports
`git describe`. Builds that report the version `dev` skip the copy: a plain
`go build`, and the image that `docker compose up --build` builds, because
the compose files pass no `VERSION` build argument. Before you upgrade such
a build, download a backup (**System → Backup & restore**).

### Going back to an earlier version

After a successful update, the previous program stays in
`/usr/local/bin/picache.prev` until the next update. To go back to it (or to
another binary of an older release), stop PiCache, put the program and the
matching database copy back (as for a file restore) and start it:

```sh
sudo systemctl stop picache
sudo install -m 0755 /usr/local/bin/picache.prev /usr/local/bin/picache
sudo ls /var/lib/picache/backups/          # picache-<version>-<timestamp>.db
sudo install -m 0600 -o picache -g picache \
  /var/lib/picache/backups/picache-v1.2.3-20260925T101500.db /var/lib/picache/picache.db
sudo rm -f /var/lib/picache/picache.db-wal /var/lib/picache/picache.db-shm
sudo systemctl start picache
```

Pick the copy named after the version you go back to. An older binary cannot
be expected to open a database that a newer version has migrated. A version before 0.11.0 refuses the `picache.db` of 0.11.0 (auth schema v2 with roles, settings schema v5) and does not start: go back with the copy 0.11.0 made at its first start (`picache-<old version>-<timestamp>.db`, made before any migration; `picache reset-password` of 0.11.0 run before that start makes it instead); the rollback of the update helper does this itself, Docker users must restore that copy before starting an older image; accounts, web access settings and certificates created with 0.11.0 are then gone (the files in `<data>/tls/` stay; 0.10 serves the current `cert.pem`). A version before 0.9.0 refuses the `picache.db` of 0.9.0 or later (newer schema) and does not start: go back with the copy 0.9.0 made at its first start (`picache-<old version>-<timestamp>.db`); the rollback of the update helper does this itself, Docker users must restore that copy before starting an older image. An older version cannot open the newer `logs.db` either and sets it aside, so the query log and the statistics start fresh after such a downgrade. Changes to
the configuration made since the upgrade are lost. With Docker, set the
previous image tag in the compose file and restore the copy from the
`picache-data` volume the same way.

---

## Uninstall

- **Bare metal / LXC:** `sudo sh deploy/install.sh --uninstall` stops and
  disables the units and removes the binary, the unit files (including the
  update helper's), the license texts in `/usr/share/doc/picache/`, the
  installer's drop-ins, the `<unit>.prev` copies of updates,
  `/etc/picache/host-apply.enabled` and `/etc/picache/updater.enabled`, and
  what `--with-dhcp` of versions before 0.8.0 installed (drop-in,
  `/etc/picache/dhcp.enabled`, `PICACHE_DHCP=on` in `picache.env`;
  `PICACHE_DHCP=off` is kept). Configuration and data are kept. The
  script lists what remains, including NAS mount units written by the helper
  (`/etc/systemd/system/srv-picache-*.mount`) with the commands that remove
  them and their credentials. Before uninstalling you can instead run
  `sudo picache storage remove <id>` for each host-apply target.
  To remove everything, add `--purge`:

  ```sh
  sudo sh deploy/install.sh --uninstall --purge
  # or without a local copy of the installer:
  curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh -s -- --uninstall --purge
  ```

  `--purge` also stops and removes the NAS mount units of the host-apply
  helper and deletes `/etc/picache`, `/var/lib/picache` (with the database
  backups), `/var/cache/picache` and the `picache` user. It deletes only
  these default paths: custom `PICACHE_DATA_DIR`, `PICACHE_CACHE_DIR` or
  `PICACHE_MOUNT_ROOT` directories and mount points (a cache volume, a
  share) are listed instead, and it stops without deleting anything while a
  share is still mounted below them. It asks on the terminal first;
  `--yes` skips the question.

  In LXC you can instead destroy the container, then remove the host's NAS
  fstab entry and credentials file.
- **Docker:** `docker compose down` keeps the volumes. `docker compose down -v`
  also deletes the configuration and the cache.

---

## Cache storage

The active cache store is chosen in **Cache → Storage**. The built-in target
`local` is `PICACHE_CACHE_DIR`. Every other target lives below
`PICACHE_MOUNT_ROOT` (`/srv/picache`). Run the test, initialise the target (or
*adopt* an existing store) and activate it. The mount guard checks the
targets every 30 seconds. If a target is
offline, downloads pass through uncached and the UI says why. DNS never
depends on storage.

### Disks and filesystems

- Prefer SSD or NVMe. Put the cache on its own disk or partition if you can.
- ext4 with the default inode ratio is fine. Never format with
  `-T largefile`/`largefile4`, which leaves too few inodes for 1 MiB slices.
  XFS suits very large caches. On ZFS use a dedicated dataset with
  `recordsize=1M`, `atime=off` and no snapshots.
- Mount with `noatime`. PiCache keeps its own access times in the index.

**A second local disk** on bare metal: mount it below `/srv/picache` (fstab
with `nofail`), make it writable for PiCache and add it as a *local* target
with "require mountpoint":

```sh
sudo mkdir -p /srv/picache/ssd
# /etc/fstab: UUID=<uuid> /srv/picache/ssd ext4 noatime,nofail 0 2
sudo mount /srv/picache/ssd && sudo chown picache:picache /srv/picache/ssd
```

### Raspberry Pi

- Use a Raspberry Pi 4 or 5 with a 64-bit OS (Raspberry Pi OS Lite or Debian
  arm64) and the `linux-arm64` binary.
- **Do not put the cache on the SD card.** SD cards are slow and wear out
  under constant writes, and PiCache warns in the health page when the cache
  is on one. Use a USB 3 SSD. Best of all, boot the Pi from the SSD, so the
  databases and logs are off the SD card too. Otherwise mount the SSD below
  `/srv/picache` as shown above and activate it as a local target.
- Use a power supply that can also feed the SSD.
- If the databases stay on the SD card, a longer *Write interval* under
  **System → Logs & privacy** saves writes ([Logs and privacy](#logs-and-privacy)).
- The Pi has no battery-backed clock. While its clock is still before the
  binary's build date (after a boot, until NTP has set the time), PiCache
  uses plain DNS to its bootstrap servers instead of DoH/DoT (health warning).
  Keep `systemd-timesyncd` enabled.
- Cache hits are limited by the Pi's Gigabit port (about 110 MB/s).

### NAS (SMB/NFS)

Only the slice files go to the NAS; the databases and the index stay local.
PiCache itself never mounts anything and never holds `CAP_SYS_ADMIN`.

| Option | Where it works | How |
|---|---|---|
| **External mount** (mode *external*) | everywhere | Something else mounts the share below `/srv/picache` (host fstab or systemd `.mount`, Proxmox mount point, Docker bind with `rslave`). The UI generates the fstab line, the `.mount` unit, the credentials file template, the compose bind and the Proxmox commands for each target. The snippets never contain the password. |
| **Host-apply** (mode *host-apply*) | bare metal, VMs, privileged LXC with systemd | Install with `--with-host-apply`. The UI queues a request. `picache-storage.path` starts the root helper (`picache storage apply-pending`), which re-validates the target, writes `/etc/picache/credentials/<id>.cred` and a `.mount` unit for `/srv/picache/<id>`, and starts it. It also removes both when the target is deleted or switched to another mode. You can also run `sudo picache storage apply <id>` and `sudo picache storage remove <id>` yourself. See [Host-apply](#host-apply-root-helper). |
| **Proxmox LXC** | unprivileged containers | The host mounts the share and passes it in with `pct set <ctid> -mpN`; see [deploy/lxc/README.md](../deploy/lxc/README.md). |
| **Docker** | Docker hosts | The host mounts the share below `/srv/picache` (fstab, `nofail`); the compose file binds `/srv/picache` with `propagation: rslave`. |

Recommendations:

- Give the server as an **IP address**. PiCache requires it, and it avoids a
  boot-time dependency on PiCache's own DNS.
- Host-apply needs the mount program of the share type on the PiCache
  machine: `cifs-utils` for SMB, `nfs-common` for NFS (the helper refuses
  with that hint when it is missing). NFS 4 does not need the `rpcbind`
  service that `nfs-common` installs; turn it off so nothing listens on
  port 111: `sudo systemctl mask --now rpcbind.service rpcbind.socket`.
- NFS checks permissions by user id: PiCache writes as its service user
  (shown on the storage page). Either make that uid the owner of the export
  on the NAS, or map all users of the export to one NAS account that owns the
  directory (`all_squash,anonuid=…,anongid=…` on Linux; TrueNAS: *Mapall
  User* and *Mapall Group* of the NFS share; Synology: *Squash* → *Map all
  users to admin*). Allow only the PiCache machine's address in the export.
- Use a dedicated share and a NAS account that can access only that share.
  SMB 3.1.1 is the default (optionally encrypted with *seal*). NFS uses
  version 4.2 with `softerr`. NFS relies on the client's address, so allow
  only PiCache's host in the export.
- The generated mounts are soft and have timeouts, so a hanging NAS cannot
  freeze PiCache. Use the same options for your own mounts.
- Hits cannot be faster than the NAS: about 110 MB/s over 1 GbE, which can be
  slower than your Internet connection. The [speed test](#speed-test) shows
  what the NAS really delivers.
- A separate storage network between PiCache and the NAS (a second network
  card or VLAN, for example `192.168.8.0/24`) needs no setting in PiCache:
  give the NAS address in that network as the server, and the host routes
  the traffic over it. It keeps SMB/NFS off the client network (the share is
  then not reachable from it) and helps with fast Internet connections,
  because a miss otherwise sends every byte twice over one network card (to
  the client and to the NAS). For cache hits it makes no difference on a
  full-duplex network.
- Docker named volumes of type `cifs`/`nfs` are not recommended. The
  container, and with it DNS, fails to start when the NAS is down, and
  `docker volume inspect` shows the SMB password.

### Speed test

**Cache → Storage → Test speed** measures a storage target that is online
(admins only; one test at a time):

| Measurement | What it shows |
|---|---|
| Write | sequential writes in 1 MiB blocks, including the final flush: how fast misses can be stored |
| Read | sequential reads of the test file after PiCache dropped it from the host's memory. A NAS may still answer from its own memory |
| Cache reads | reads of up to 128 randomly chosen cached slices (active store only): the path of a cache hit, with the time per slice |
| File operations | create, flush, rename and delete of small files: the latency every stored slice pays |

Choose 64 MiB (quick), 256 MiB (default) or 1 GiB (thorough). The test writes
a temporary file into the target (it needs the test size plus 1 GiB free),
loads the storage for up to two minutes and can be cancelled; downloads keep
working meanwhile, perhaps slower. The test file is always removed. The last
result of each target stays visible until PiCache restarts.

Reading the result: cache hits cannot be faster than the slower of the cache
reads and the network to the clients (1 GbE ≈ 118 MB/s, 2.5 GbE ≈ 295 MB/s,
10 GbE ≈ 1,180 MB/s). If the storage is clearly faster than the network, the
network is the limit; if it is slower, a faster disk, an SSD cache on the NAS
or NFS instead of SMB helps more than anything in PiCache.

### Host-apply (root helper)

With `--with-host-apply` the web UI can mount a share without a shell. The
unprivileged service only writes a request file
(`<data>/storage-requests/<id>`). `picache-storage.path` starts
`picache-storage.service` (`picache storage apply-pending`, root, sandboxed),
which reads the target from the database, validates it again and makes the
host match it:

- **Apply** on a host-apply target writes `/etc/picache/credentials/<id>.cred`
  (SMB with a user) and `/etc/systemd/system/srv-picache-<id>.mount`, runs
  `systemctl daemon-reload` and `enable`, and starts the unit. When the
  settings changed since the share was mounted (server, share or export,
  version, encryption, user, password), the unit is **restarted**, so the new
  settings take effect. A share that is in use cannot be unmounted: to change
  the active cache store's mount, activate another target first.
- **Deleting** a host-apply target, or switching it to *external*, disables,
  stops and deletes the mount unit, deletes the credentials file and removes
  the empty mountpoint. Files on the NAS are not touched. Without the helper
  (`sudo picache storage apply` only) run `sudo picache storage remove <id>`.
- Requests that arrive while the helper runs are handled in the same run.
  Each run writes `<id>.result`, which the UI shows. A failed mount shows the
  mount program's own message (for example
  `mount error(13): Permission denied`); the full log is in
  `journalctl -u srv-picache-<id>.mount` and `journalctl -u picache-storage`.
- If a request is not picked up within 3 minutes, the UI says so. Check
  `systemctl status picache-storage.path`; if it failed, run
  `sudo systemctl reset-failed picache-storage.path` and
  `sudo systemctl start picache-storage.path` (or run the installer again).
- To disable host-apply, delete `/etc/picache/host-apply.enabled` and run
  `sudo systemctl disable --now picache-storage.path`.

**Master key as a systemd credential.** The helper decrypts stored NAS
passwords with the master key. If `picache.service` gets the key as a
credential (`LoadCredentialEncrypted=picache-master-key:…` in a drop-in),
give the helper the same line, or SMB targets with a user fail with
"no master key for the root helper":

```sh
sudo systemctl edit picache-storage
# [Service]
# LoadCredentialEncrypted=picache-master-key:/etc/credstore.encrypted/picache-master-key
```

Alternatively apply such a target by hand:
`sudo picache storage apply <id> --password-stdin`.

**Custom paths.** The helper units watch `/var/lib/picache/storage-requests/`
and may only write below `/srv/picache`. After changing `PICACHE_DATA_DIR`
or `PICACHE_MOUNT_ROOT` in `picache.env`, run the installer again. It writes
`/etc/systemd/system/picache-storage.path.d/50-picache-paths.conf` and
`picache-storage.service.d/50-picache-paths.conf` with the new paths (and
removes them when the defaults are back). Otherwise requests stay unanswered
("not picked up") and new mountpoints cannot be created.

**Containers (privileged LXC).** systemd makes `/` a shared mount on bare
metal and in VMs, but not inside a container. PiCache runs in its own mount
namespace (`ProtectSystem=`), and without shared propagation a share the
helper mounts later never becomes visible there: the UI reports "The root
helper reported this share as mounted, but PiCache does not see a mount
here". The installer therefore installs `picache-shared-mounts.service`
(`mount --make-rshared /` before PiCache starts) in a container whose `/` is
not shared. Check with `findmnt -no PROPAGATION /` (`shared`), and restart
PiCache once after enabling it. Your container's AppArmor profile must allow
CIFS/NFS mounts and propagation changes.

---

## Environment variables

Bootstrap settings are read at start from `PICACHE_*` environment variables.
The `picache` command also reads `/etc/picache/picache.env` (or the file named
by `PICACHE_ENV_FILE`) for every command except `version` and `help`. Lines
are `KEY=value`. `#` comments, an `export ` prefix and quotes are accepted,
only `PICACHE_*` keys are used, and variables already set in the environment
win. Changes need a restart.

Listener variables take a comma-separated list of `host:port` addresses. An
empty host means all addresses. `off`, `none` or `-` disables the listener.

| Variable | Default (Linux · Docker image) | Description |
|---|---|---|
| `PICACHE_DATA_DIR` | `/var/lib/picache` · `/data` | Databases, keys, lists, TLS certificate. Must be a local disk. With host-apply, run the installer again after changing it ([Custom paths](#host-apply-root-helper)). |
| `PICACHE_CACHE_DIR` | `/var/cache/picache` · `/cache` | The built-in local cache store (target `local`). |
| `PICACHE_MOUNT_ROOT` | `/srv/picache` | The only directory below which other storage targets may live. With host-apply, run the installer again after changing it. |
| `PICACHE_DNS_LISTEN` | `:53` | DNS over UDP and TCP. Required; a bind failure stops PiCache. |
| `PICACHE_CACHE_LISTEN` | `:80` | Download cache over HTTP. If it is off or cannot bind, the download cache's DNS answers stay inactive. |
| `PICACHE_SNI_LISTEN` | `:443` | HTTPS (SNI) pass-through of the download cache. |
| `PICACHE_WEB_LISTEN` | `:8080` | Web UI and API over HTTP. |
| `PICACHE_WEB_TLS_LISTEN` | `:8443` | Web UI and API over HTTPS. At least one web listener is required. |
| `PICACHE_WEB_TLS_CERT` | – | PEM certificate (with its intermediates) for the HTTPS listener. PiCache reloads it within a minute when the files change (at once on SIGHUP), keeps the previous certificate while a pair cannot be loaded, and serves its local CA's certificate while none could be loaded yet ([HTTPS certificates](#https-certificates)). Without it, PiCache serves an uploaded certificate or one of its local CA in `<data>/tls/`. |
| `PICACHE_WEB_TLS_KEY` | – | PEM private key; set together with the certificate. Both files must be readable by the service user. |
| `PICACHE_WEB_HOSTS` | – | Comma-separated extra host names allowed for the web UI (DNS-rebinding protection), e.g. a reverse-proxy name. Can also be set in the web settings. |
| `PICACHE_CONFIG_LOCKED` | `off` | `on`: configuration changes from browser sessions are refused (`config_locked`); admin API tokens still write ([Web access and accounts](#web-access-and-accounts)). Not an access control against admins. |
| `PICACHE_DESTRUCTIVE_API` | `on` | `off`: restores, resets, purges and other bulk deletions are refused through the API, for sessions and tokens. |
| `PICACHE_WEB_SECURE_COOKIES` | `false` | Not needed when the proxy is in the trusted proxies ([Behind a reverse proxy](#behind-a-reverse-proxy)). Set to `true` when a TLS-terminating reverse proxy that is not trusted forwards to the plain-HTTP listener: the session and device cookies then get the `Secure` flag although the request reaches PiCache over HTTP. Requests on PiCache's own HTTPS listener always get `Secure` cookies named `__Host-picache_session` and `__Host-picache_device`. With this set, signing in directly over plain HTTP no longer works (browsers drop `Secure` cookies there). |
| `PICACHE_DHCP` | – | Unset: the DHCP server is switched on under DNS → DHCP ([DHCP server](#dhcp-server)); PiCache holds no DHCP port while it is off. `off` (`no`, `0`, `false`): prevents it; the server cannot be switched on (for hosts that run another DHCP server; `install.sh --without-dhcp` sets it). `on` (`yes`, `1`, `true`) is the opt-in of versions before 0.8.0: still accepted (every DHCP socket opens at start and PiCache closes what the settings do not need), and removed by the installer. Anything else: PiCache refuses to start. |
| `PICACHE_RUN_AS` | – · `65532:65532` | Numeric non-root `uid:gid`. When PiCache starts as root it binds the listeners and then switches to this user before touching files. Linux only. Not needed with the systemd unit. |
| `PICACHE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `PICACHE_LOG_FORMAT` | `text` | `text` or `json` (logs go to stderr / the journal). |
| `PICACHE_ADMIN_USER` | `admin` | User name for password provisioning. |
| `PICACHE_ADMIN_PASSWORD_FILE` | – | File with the password (at least 10 characters; a trailing newline is ignored) for the first admin. Used only while no user exists. |
| `PICACHE_ADMIN_PASSWORD` | – | Same as a plain variable (discouraged, logged as a warning: visible to other processes and in `docker inspect`). The `_FILE` variant wins. |
| `PICACHE_MASTER_KEY_FILE` | `<data>/keys/master.key` | Master key for stored secrets: 32 raw bytes or 64 hex characters. Created with mode 0600 if missing. A systemd credential `picache-master-key` (`$CREDENTIALS_DIRECTORY`) or the Docker secret `/run/secrets/picache_master_key` takes precedence. A systemd credential must also be given to `picache-storage.service` when host-apply mounts SMB shares with a stored password ([Host-apply](#host-apply-root-helper)). The Docker secret is read after the privilege drop, so it must be readable by 65532. |
| `PICACHE_DEV` | `false` | Development mode (relaxed platform checks, verbose errors). Never in production. |
| `PICACHE_ENV_FILE` | `/etc/picache/picache.env` | Env file to read instead of the default. Unlike the default file, it must exist. |
| `PICACHE_TOKEN` | – | CLI only (`picache logs tail`, `picache logs export`): the API token. Read from the environment only, never from the env file ([From the command line](#from-the-command-line)). |
| `PICACHE_URL` | the local web listener | CLI only: the PiCache URL for `picache logs tail` and `picache logs export`. |
| `GOMEMLIMIT` | 60 % of the memory limit | Go's soft memory limit. By default PiCache sets 60 % of the cgroup memory limit, or of the RAM. |

`picache serve` also accepts flags that override the environment:
`--data-dir`, `--cache-dir`, `--dns-listen`, `--cache-listen`,
`--sni-listen`, `--web-listen`, `--web-tls-listen`, `--log-level`, `--dev`.

---

## CLI reference

| Command | Description |
|---|---|
| `picache [serve] [flags]` | Run PiCache (the default command). |
| `picache version` | Print version, commit, build date, Go version and platform. |
| `picache healthcheck [url]` | Exit 0 if the local web endpoint answers `/healthz` with `ok` and the DNS listener answers the probe name `healthcheck.picache.invalid` with a loopback address. PiCache answers that name like `localhost` when the query comes from this machine and never counts or logs it. Another DNS server on the port answers it with NXDOMAIN, so the check fails. It uses the first address of `PICACHE_WEB_LISTEN` (or of `PICACHE_WEB_TLS_LISTEN` if HTTP is off) and of `PICACHE_DNS_LISTEN`, with wildcard addresses replaced by `127.0.0.1`. With a URL, only that URL is checked. Used by the Docker `HEALTHCHECK`. |
| `picache reset-password [--admin] [user]` | Set a new password for the existing account `user` (default `admin`; found case-insensitively), read from stdin (at least 10 characters). The input is not hidden; you can redirect it from a file. Disables TOTP for the account, signs out all sessions and revokes all API tokens of all accounts, then prints exactly what changed, including the role. The account keeps its role; `--admin` (before or after the name) makes it an admin, the way back when no admin is left. A name that matches no account is refused and the existing names are listed; an admin account is created only when none exists yet. Run it as root or as the service user. As root it switches to the owner of the data directory first. |
| `picache users` | List the accounts: id, user name, role, two-factor on or off, last sign-in. Read-only; root or the service user. |
| `picache web-access --reset` | Let every address use the web UI again: PiCache switches *Allow the web UI only from these networks* off, clears the trusted proxies, accepts TLS 1.2 again and deletes an uploaded certificate (the allowed networks and everything else stay), audited as `web.access_reset`. The command only creates `<data>/web-access.reset`; the running service applies it within a minute (at once: `sudo systemctl kill -s HUP --kill-whom=main picache`; Docker: `docker kill -s HUP <container>`), or at its next start. Root or the service user (Docker: `docker exec -u 65532:65532 <container> /picache web-access --reset`). |
| `picache setup-token` | Print the first-run setup token (until setup is done). Needs read access to the data directory: `sudo` on bare metal, `-u 65532:65532` in Docker. |
| `picache storage apply <id> [--password-stdin]` | Root only. Validate the storage target, write `/etc/picache/credentials/<id>.cred` and a systemd `.mount` unit for `/srv/picache/<id>`, then `systemctl daemon-reload`, `enable` and `start`, or `restart` when the mounted settings are outdated. The NAS password is decrypted from the database with the master key, or read from stdin with `--password-stdin`. |
| `picache storage remove <id>` | Root only. Disable, stop and delete the `.mount` unit of `/srv/picache/<id>`, delete its credentials file and remove the empty mountpoint. Works without the database, for example after the target was deleted. |
| `picache storage apply-pending` | Root only. Process the requests the web UI queued in `<data>/storage-requests/`: mount host-apply targets, remove the mounts of deleted targets and of targets switched to another mode. Run by `picache-storage.service`. |
| `picache update --check` | Print the installed and the newest release and its URL. Exit code `0` up to date, `10` update available, `1` error. Needs no root. |
| `picache update [--version vX.Y.Z] [--prerelease] [--allow-downgrade] [--yes]` | Root only. Show the start of the release notes, ask (unless `--yes`), then download, verify and install the newest release (or the given one) and restart PiCache; roll back if the new version fails its health check ([Updates](#updates)). `--prerelease` also considers pre-releases; `--allow-downgrade` allows an older release. |
| `picache update --from <dir> [--yes]` | Root only. The same from a directory with the release files (`SHA256SUMS`, `SHA256SUMS.sig`, the binary), without network access. |
| `picache update apply-pending` | Root only. Install the version the web UI queued in `<data>/update-requests/`. Run by `picache-update.service`. |
| `picache logs tail [--client ADDR]... [--status S]... [--json] [--url URL] [--token-file FILE]` | Follow the query log through the API ([From the command line](#from-the-command-line)). Needs an API token (read is enough). |
| `picache logs export --format ndjson\|csv [--range R \| --from T --to T] [--client ADDR]... [--status S]... [--domain D] [--qtype T] [--rcode R]... [--dnssec true\|false] [--upstream U] --out FILE\|- [--url URL] [--token-file FILE]` | Export the query log through the API to a new file (0600) or stdout. |
| `picache db check` | Check `picache.db` read-only (also while PiCache runs): integrity, foreign keys, schema versions. Exit code `3` when problems are found ([Recovering a damaged picache.db](#recovering-a-damaged-picachedb)). Root or the service user. |
| `picache db salvage --out FILE [--force]` | Copy every readable row of a damaged `picache.db` into the new file `FILE` (PiCache stopped; the source is never modified). Exit code `3` when rows were lost or skipped. Root or the service user. |
| `picache help` | Print the usage. |

Exit codes: `0` success, `1` error or unhealthy, `2` usage or configuration
error, `3` problems found or rows lost (`db check`, `db salvage`), `75`
restart requested from the web UI (systemd and Docker restart the process).

---

## Troubleshooting

- **Health:** **System → Health & about** lists every check with a hint:
  listeners, upstreams (including the clock guard), blocklists, rate limiting, cache-domains,
  download cache (cache IP), SNI, cache storage, logs, free space on the data disk
  the DHCP server (when enabled; see [DHCP server](#dhcp-server) for its
  troubleshooting) and the HTTPS certificate (when the HTTPS listener is on;
  see [HTTPS certificates](#https-certificates)).
- **Locked out of the web UI** ("PiCache does not allow the web UI from
  <address>", or HTTPS no longer works after requiring TLS 1.3 or a broken
  upload): run `sudo picache web-access --reset` on the PiCache host
  (Docker: `docker exec -u 65532:65532 picache /picache web-access --reset`;
  LXC: `pct exec <ctid> -- picache web-access --reset`). Within a minute (at
  once after `sudo systemctl kill -s HUP --kill-whom=main picache` or
  `docker kill -s HUP picache`) every address may use the web UI again, no
  proxy is trusted, TLS 1.2 is accepted and an uploaded certificate is
  deleted; then allow your networks again under **System → Users & security
  → Web access**. The HTTPS redirect, the sessions and the allowed hosts are
  not changed. A browser that stored HSTS for a name whose certificate it no
  longer trusts refuses that name: open PiCache by its IP address.
- **Logs:** **System → Application log** shows the last 2000 lines and
  switches on debug logging for a while; `journalctl -u picache -f` (bare
  metal/LXC), `docker compose logs -f picache` (Docker). Set
  `PICACHE_LOG_LEVEL=debug` for more detail at every start. Past warnings
  are listed under **System → Health & about → Warnings**.
- **Damaged configuration database:** `sudo picache db check`, then see
  [Recovering a damaged picache.db](#recovering-a-damaged-picachedb).
- **`bind … permission denied`:** PiCache was started without
  `CAP_NET_BIND_SERVICE`. Use the systemd unit or the compose file from
  `deploy/`.
- **`… is owned by … but PiCache runs as …` (Docker):** a bind-mounted
  directory has the wrong owner; `chown -R 65532:65532` it on the host.
- **`status=226/NAMESPACE` (LXC):** enable the container's nesting feature.
- **NAS mount (host-apply) fails or stays queued:** the UI shows the mount
  program's message; see [Host-apply](#host-apply-root-helper) for the
  helper's logs, the path unit, custom paths and containers.
- **Update check fails:** PiCache must be able to reach `api.github.com`
  (like the blocklist downloads), and the GitHub repository must be public.
  **Check now**
  on **System → Updates** shows the error. An update that failed or was
  rolled back is logged in `journalctl -u picache-update` (web UI) or printed
  by `sudo picache update`; see [Updates](#updates).
- **No download cache answers:** the download cache must be enabled, the
  cache-domains list loaded, the :80 listener bound and a private cache IPv4
  address known, and the client must not be in a group that bypasses the
  download cache. The health page names the missing piece.
- **Forgotten password or no admin left:** `sudo picache reset-password <user>` with the user name chosen at setup (`admin` by default; an unknown name is refused and the existing names are shown; `sudo picache users` lists them); add `--admin` to make the account an admin. It also signs out every session and revokes all API tokens, so it is the recovery step after a suspected compromise too.
