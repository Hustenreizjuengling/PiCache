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
```

To read the script before running it, download it first:

```sh
curl -fsSLO https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh
less get-picache.sh
sudo sh get-picache.sh
```

Running it again upgrades an existing installation, unit files included.
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
8. checks ports 53, 80, 443, 8080 and 8443 for other programs. If port 53 is
   taken it prints the fix and does **not** start PiCache
   ([Port 53 conflicts](#port-53-conflicts)); it never reconfigures
   systemd-resolved or other services;
9. enables and (re)starts `picache.service` and prints the web UI address and
   the setup-token command.

`/var/lib/picache` and `/var/cache/picache` are created by systemd
(`StateDirectory=`/`CacheDirectory=`) on the first start.

### The systemd sandbox

`deploy/systemd/picache.service` runs PiCache as `picache` with only
`CAP_NET_BIND_SERVICE` (to bind ports 53, 80 and 443), `NoNewPrivileges=yes`,
`ProtectSystem=strict` and a system-call filter. The service can write only to
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

- starts as root only to bind ports 53, 80 and 443 (Docker gives non-root
  users no ambient capabilities), then drops to `PICACHE_RUN_AS=65532:65532`
  before it opens its data, and checks that it cannot regain root;
- runs with `cap_drop: [ALL]`, `cap_add: [NET_BIND_SERVICE, SETUID, SETGID]`,
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
MAC; and the router resolver must be set explicitly. Use host networking
whenever you can.

Docker Desktop (macOS/Windows) is not a deployment target: containers run in
a VM, so PiCache cannot see real client addresses, and bind-mount
propagation does not work.

---

## First-run setup

1. Open the web UI: `https://<ip>:8443/` (self-signed certificate; see
   `PICACHE_WEB_TLS_CERT` for your own) or `http://<ip>:8080/`. Over plain
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
   file afterwards: every command reads the variable and fails if the file is
   gone.
3. Consider enabling two-factor authentication (**System → Account &
   security**).
4. Point your clients at PiCache: set the DNS server option of your router's
   DHCP server to PiCache's address. If the router instead forwards DNS to
   PiCache, all queries appear to come from the router. Also make sure the
   router does not advertise its own IPv6 DNS server, or clients will bypass
   PiCache over IPv6.
5. The download cache is **off** by default. When you enable it, the UI
   shows the cache IP it will answer with, the store path, its filesystem and
   free space, and warnings (SD card, Docker bridge mode). Check them first,
   and move the cache to a suitable disk ([Cache storage](#cache-storage)) if
   needed.

If you open the UI through a host name that is not this machine's host name,
local domain or a configured server name, PiCache answers `421 Misdirected
Request` (DNS-rebinding protection). Add the name to `PICACHE_WEB_HOSTS` or
to the allowed hosts in the web settings.

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

  Use the machine's static LAN address. Add its IPv6 address too if clients
  use IPv6 DNS.

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
| `/var/lib/picache` (`PICACHE_DATA_DIR`) | `/data` | `picache.db` (configuration: settings, users, lists, rules, clients, groups, local records, services, storage targets with sealed NAS passwords, audit log); `logs.db` (query log, cache events, sessions, statistics, evictions, seen clients); `cache-index/<store-id>.db`; `lists/`; `cache-domains/`; `tls/`; `keys/master.key` (0600); `instance-id`; `setup-token` (until setup is done); `backups/` (automatic pre-upgrade copies, newest 3); `storage-requests/` (mount requests for the root helper); `update-requests/` (update request and progress of the update helper); `picache.db.before-restore` (after a restore) | `picache.db` (UI download or file copy while stopped); everything else is rebuildable. `keys/master.key` separately if stored NAS passwords should survive a move to another machine. |
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

**Moving to a new machine:** install PiCache there, stop it, copy
`picache.db` (and `keys/master.key`) into the data directory with the right
owner, and start it (this keeps the accounts). Alternatively complete setup
on the new machine and restore a downloaded backup in the UI: the account
created there stays. The cache can be copied too, or it simply fills again.
A NAS store is adopted by initialising the target with *adopt* in
**Cache → Storage**.

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

Only the program is replaced. Unit files and the installer change rarely.
When a release needs them updated, its release notes say so: then also run
the installer of that release ([below](#manual-upgrade-with-the-installer)).

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
replaces only the program: an older version cannot be expected to open a
database that a newer version has migrated, so restore the matching database
copy as described in
[Going back to an earlier version](#going-back-to-an-earlier-version).

### Offline

On a machine without Internet access, put the release files into one
directory with their original names: `SHA256SUMS`, `SHA256SUMS.sig` and the
binary for the machine (`picache-linux-amd64`, `-arm64` or `-armv7`). Then:

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
be expected to open a database that a newer version has migrated. Changes to
the configuration made since the upgrade are lost. With Docker, set the
previous image tag in the compose file and restore the copy from the
`picache-data` volume the same way.

---

## Uninstall

- **Bare metal / LXC:** `sudo sh deploy/install.sh --uninstall` stops and
  disables the units and removes the binary, the unit files (including the
  update helper's), the license texts in `/usr/share/doc/picache/`, the
  installer's drop-ins, `/etc/picache/host-apply.enabled` and
  `/etc/picache/updater.enabled`. Configuration and data are kept. The
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
| `PICACHE_WEB_TLS_CERT` | – | PEM certificate for the HTTPS listener. Without it, PiCache creates and renews a self-signed certificate in `<data>/tls/`. |
| `PICACHE_WEB_TLS_KEY` | – | PEM private key; set together with the certificate. Both files must be readable by the service user. |
| `PICACHE_WEB_HOSTS` | – | Comma-separated extra host names allowed for the web UI (DNS-rebinding protection), e.g. a reverse-proxy name. Can also be set in the web settings. |
| `PICACHE_WEB_SECURE_COOKIES` | `false` | Set to `true` when a TLS-terminating reverse proxy forwards to the plain-HTTP listener: the session and device cookies then get the `Secure` flag although the request reaches PiCache over HTTP. Requests on PiCache's own HTTPS listener always get `Secure` cookies named `__Host-picache_session` and `__Host-picache_device`. With this set, signing in directly over plain HTTP no longer works (browsers drop `Secure` cookies there). |
| `PICACHE_RUN_AS` | – · `65532:65532` | Numeric non-root `uid:gid`. When PiCache starts as root it binds the listeners and then switches to this user before touching files. Linux only. Not needed with the systemd unit. |
| `PICACHE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `PICACHE_LOG_FORMAT` | `text` | `text` or `json` (logs go to stderr / the journal). |
| `PICACHE_ADMIN_USER` | `admin` | User name for password provisioning. |
| `PICACHE_ADMIN_PASSWORD_FILE` | – | File with the password (at least 10 characters; a trailing newline is ignored) for the first admin. Used only while no user exists. |
| `PICACHE_ADMIN_PASSWORD` | – | Same as a plain variable (discouraged, logged as a warning: visible to other processes and in `docker inspect`). The `_FILE` variant wins. |
| `PICACHE_MASTER_KEY_FILE` | `<data>/keys/master.key` | Master key for stored secrets: 32 raw bytes or 64 hex characters. Created with mode 0600 if missing. A systemd credential `picache-master-key` (`$CREDENTIALS_DIRECTORY`) or the Docker secret `/run/secrets/picache_master_key` takes precedence. A systemd credential must also be given to `picache-storage.service` when host-apply mounts SMB shares with a stored password ([Host-apply](#host-apply-root-helper)). The Docker secret is read after the privilege drop, so it must be readable by 65532. |
| `PICACHE_DEV` | `false` | Development mode (relaxed platform checks, verbose errors). Never in production. |
| `PICACHE_ENV_FILE` | `/etc/picache/picache.env` | Env file to read instead of the default. Unlike the default file, it must exist. |
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
| `picache reset-password [user]` | Set a new password for the existing account `user` (default `admin`), read from stdin (at least 10 characters). The input is not hidden; you can redirect it from a file. Disables TOTP for the account, signs out all sessions and revokes all API tokens, then prints exactly what changed. A name that matches no account is refused and the existing names are listed; an account is created only when none exists yet. Run it as root or as the service user. As root it switches to the owner of the data directory first. |
| `picache setup-token` | Print the first-run setup token (until setup is done). Needs read access to the data directory: `sudo` on bare metal, `-u 65532:65532` in Docker. |
| `picache storage apply <id> [--password-stdin]` | Root only. Validate the storage target, write `/etc/picache/credentials/<id>.cred` and a systemd `.mount` unit for `/srv/picache/<id>`, then `systemctl daemon-reload`, `enable` and `start`, or `restart` when the mounted settings are outdated. The NAS password is decrypted from the database with the master key, or read from stdin with `--password-stdin`. |
| `picache storage remove <id>` | Root only. Disable, stop and delete the `.mount` unit of `/srv/picache/<id>`, delete its credentials file and remove the empty mountpoint. Works without the database, for example after the target was deleted. |
| `picache storage apply-pending` | Root only. Process the requests the web UI queued in `<data>/storage-requests/`: mount host-apply targets, remove the mounts of deleted targets and of targets switched to another mode. Run by `picache-storage.service`. |
| `picache update --check` | Print the installed and the newest release and its URL. Exit code `0` up to date, `10` update available, `1` error. Needs no root. |
| `picache update [--version vX.Y.Z] [--prerelease] [--allow-downgrade] [--yes]` | Root only. Show the start of the release notes, ask (unless `--yes`), then download, verify and install the newest release (or the given one) and restart PiCache; roll back if the new version fails its health check ([Updates](#updates)). `--prerelease` also considers pre-releases; `--allow-downgrade` allows an older release. |
| `picache update --from <dir> [--yes]` | Root only. The same from a directory with the release files (`SHA256SUMS`, `SHA256SUMS.sig`, the binary), without network access. |
| `picache update apply-pending` | Root only. Install the version the web UI queued in `<data>/update-requests/`. Run by `picache-update.service`. |
| `picache help` | Print the usage. |

Exit codes: `0` success, `1` error or unhealthy, `2` usage or configuration
error, `75` restart requested from the web UI (systemd and Docker restart the
process).

---

## Troubleshooting

- **Health:** **System → Health & about** lists every check with a hint:
  listeners, upstreams (including the clock guard), blocklists, rate limiting, cache-domains,
  download cache (cache IP), SNI, cache storage, logs and free space on the data disk.
- **Logs:** `journalctl -u picache -f` (bare metal/LXC),
  `docker compose logs -f picache` (Docker). Set `PICACHE_LOG_LEVEL=debug`
  for more detail.
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
- **Forgotten password:** `sudo picache reset-password <user>` with the user name chosen at setup (`admin` by default; an unknown name is refused and the existing names are shown). It also signs out every session and revokes all API tokens, so it is the recovery step after a suspected compromise too.
