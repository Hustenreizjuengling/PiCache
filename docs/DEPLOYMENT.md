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
- PiCache needs these ports: **53/udp+tcp** (DNS), **80/tcp** (LanCache HTTP
  cache), **443/tcp** (LanCache HTTPS pass-through), **8080/tcp** and
  **8443/tcp** (web UI over HTTP and HTTPS). See [Port conflicts](#port-conflicts).
- Never expose these ports to the Internet (no port forwarding). PiCache
  answers only private networks by default.

Contents: [Build](#build) · [Bare metal](#bare-metal-and-vms-debian-1213) ·
[LXC](#proxmox-lxc) · [Docker](#docker) · [First-run setup](#first-run-setup) ·
[Port conflicts](#port-conflicts) · [Persistent data](#persistent-data) ·
[Backup and restore](#backup-and-restore) · [Upgrade](#upgrade) ·
[Uninstall](#uninstall) · [Cache storage](#cache-storage) ·
[Environment variables](#environment-variables) · [CLI](#cli-reference) ·
[Troubleshooting](#troubleshooting)

---

## Build

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
of the UI; the API still works.

The Docker image builds everything itself (see [Docker](#docker)).

---

## Bare metal and VMs (Debian 12/13)

Copy the binary and the `deploy/` directory of the same version to the
machine, then run the installer as root:

```sh
sudo sh deploy/install.sh --binary ./picache-linux-amd64
# optional: let the web UI mount SMB/NFS shares through a root helper
sudo sh deploy/install.sh --binary ./picache-linux-amd64 --with-host-apply
```

The installer is idempotent; running it again upgrades an existing
installation. It never downloads anything. It:

1. requires systemd, warns (and continues) on systems other than Debian
   12/13, and checks that the binary runs on this machine;
2. creates the system user and group `picache`;
3. installs the binary to `/usr/local/bin/picache` and the unit
   `picache.service` to `/usr/local/lib/systemd/system/`;
4. creates `/etc/picache/` (`0750 root:picache`) and, if missing,
   `/etc/picache/picache.env` (`0640 root:picache`, all settings commented
   out); an existing file is kept;
5. creates `/srv/picache` (`0750 root:picache`), the only place for NAS
   mounts. It is owned by root so that the unprivileged service cannot swap a
   mount point for a symbolic link;
6. with `--with-host-apply`: installs `picache-storage.path` and
   `picache-storage.service`, creates `/etc/picache/credentials` (`0700 root`)
   and `/etc/picache/host-apply.enabled`, and tells you whether `cifs-utils`
   or `nfs-common` are missing (install them with `apt install`). Later runs
   keep the helper up to date as long as that marker file exists. In an
   unprivileged container the helper is skipped, because it cannot mount
   anything there;
7. checks ports 53, 80, 443, 8080 and 8443 for other programs. If port 53 is
   taken it prints the fix and does **not** start PiCache
   ([Port 53 conflicts](#port-53-conflicts)); it never reconfigures
   systemd-resolved or other services;
8. enables and (re)starts `picache.service` and prints the web UI address and
   the setup-token command.

`/var/lib/picache` and `/var/cache/picache` are created by systemd
(`StateDirectory=`/`CacheDirectory=`) on the first start.

### The systemd sandbox

`deploy/systemd/picache.service` runs PiCache as `picache` with only
`CAP_NET_BIND_SERVICE` (to bind ports 53, 80 and 443), `NoNewPrivileges=yes`,
`ProtectSystem=strict` and a system-call filter. The service can write only to
`/var/lib/picache`, `/var/cache/picache` and `/srv/picache`. If you change
`PICACHE_DATA_DIR`, `PICACHE_CACHE_DIR` or `PICACHE_MOUNT_ROOT`, add the new
paths with a drop-in (`systemctl edit picache`, `ReadWritePaths=`). Put all
local changes into drop-ins; the installer overwrites the unit file on
upgrades.

`Restart=always` also restarts PiCache after **Restart** in the web UI (the
process exits with code 75).

### Bootstrap configuration

`/etc/picache/picache.env` holds the few settings that are needed before the
database is opened: listeners, paths, logging and the optional admin
provisioning ([Environment variables](#environment-variables)). systemd reads
it, and so does every `picache` CLI command. Everything else (upstreams,
blocklists, clients, LanCache, retention, …) is configured in the web UI and
stored in the database. After editing the file run
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
cd picache/deploy/docker
docker compose up -d --build
docker exec -u 65532:65532 picache /picache setup-token
```

Then open `http://<host LAN IP>:8080/`.

`deploy/docker/docker-compose.yml` uses **host networking**, so PiCache sees
real client addresses (IPv4 and IPv6) and MAC addresses and can detect the
host's LAN IP for LanCache answers. The image (`deploy/docker/Dockerfile`) is
distroless (no shell) and contains only `/picache`. The container:

- starts as root only to bind ports 53, 80 and 443 (Docker gives non-root
  users no ambient capabilities), then drops to `PICACHE_RUN_AS=65532:65532`
  before it touches any file, and checks that it cannot regain root;
- runs with `cap_drop: [ALL]`, `cap_add: [NET_BIND_SERVICE, SETUID, SETGID]`,
  `no-new-privileges`, a read-only root filesystem and a small `/tmp` tmpfs;
- keeps its data in the named volumes `picache-data` (`/data`) and
  `picache-cache` (`/cache`), which inherit the owner 65532 from the image;
- reports health with `picache healthcheck` (web UI and DNS on loopback).

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
has limits: you **must** set the cache IPv4 address (`lancache.cacheIpv4`) to
the host's LAN IP, because PiCache cannot detect it; traffic that passes
docker-proxy (IPv6, loopback, hairpin) appears to come from the Docker
gateway; clients cannot be identified by MAC; and the router resolver must be
set explicitly. Use host networking whenever you can.

Docker Desktop (macOS/Windows) is not a deployment target: containers run in
a VM, so PiCache cannot see real client addresses, and bind-mount
propagation does not work.

---

## First-run setup

1. Open the web UI: `http://<ip>:8080/` or `https://<ip>:8443/` (self-signed
   certificate; see `PICACHE_WEB_TLS_CERT` for your own).
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
5. LanCache is **off** by default. When you enable it, the UI shows the cache
   IP it will answer with, the store path, its filesystem and free space, and
   warnings (SD card, Docker bridge mode). Check them first, and move the
   cache to a suitable disk ([Cache storage](#cache-storage)) if needed.

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

**Other DNS servers** (dnsmasq, bind9, unbound, Pi-hole, AdGuard Home,
libvirt's dnsmasq on `virbr0`): stop and disable them, or use fix A with
addresses they do not use.

**Docker:** with host networking the same fixes apply. In bridge mode, publish
DNS on the host's LAN address instead of all addresses:
`"192.168.1.5:53:53/udp"` and `"192.168.1.5:53:53/tcp"`.

### Other ports

A clash on 80, 443, 8080 or 8443 does not stop PiCache. The listener is
skipped, the error is logged and **System → Health & about** shows it.
Without the :80 listener, LanCache DNS overrides stay off. At least one of
the two web listeners must work. Free the port, or move the listener, for
example `PICACHE_WEB_LISTEN=:8081`, or `PICACHE_WEB_LISTEN=off` to serve the
UI over HTTPS only. The LanCache ports cannot be moved in practice, because
game clients always connect to 80 and 443.

---

## Persistent data

Everything persistent lives in exactly two places plus optional NAS mounts.

| Path (bare metal / LXC) | Docker | Contents | Backup? |
|---|---|---|---|
| `/var/lib/picache` (`PICACHE_DATA_DIR`) | `/data` | `picache.db` (configuration: settings, users, lists, rules, clients, groups, local records, services, storage targets with sealed NAS passwords, audit log); `logs.db` (query log, cache events, sessions, statistics, evictions, seen clients); `cache-index/<store-id>.db`; `lists/`; `cache-domains/`; `tls/`; `keys/master.key` (0600); `instance-id`; `setup-token` (until setup is done); `backups/` (automatic pre-upgrade copies, newest 3); `storage-requests/` (mount requests for the root helper); `picache.db.before-restore` (after a restore) | `picache.db` (UI download or file copy while stopped); everything else is rebuildable. `keys/master.key` separately if stored NAS passwords should survive a move to another machine. |
| `/var/cache/picache` (`PICACHE_CACHE_DIR`) | `/cache` | The built-in **local** cache store (slice files). Large. | No |
| `/srv/picache/<id>` (`PICACHE_MOUNT_ROOT`) | `/srv/picache` (bind, `rslave`) | NAS cache stores. The only place outside the cache dir where stores may live (the only NAS path writable inside the sandbox). | No |
| `/etc/picache/picache.env` | environment | Bootstrap settings only. Read by systemd **and by every CLI command**. | Yes |
| `/etc/picache/credentials/<id>.cred` | – | NAS credentials written by the root helper (0600, root). | – |

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
`keys/master.key` separately and safely if stored NAS passwords and TOTP
secrets should keep working on another machine. Without it, re-enter the NAS
passwords and sign in with `picache reset-password` (which disables TOTP).

### In the web UI

**System → Backup & restore**:

- **Download** gives a consistent copy (`picache-backup-<date>.db`). Sessions
  are always removed. Sealed NAS passwords are included only if you opt in,
  and they are useless without the master key.
- **Restore** accepts a backup of up to 512 MiB, checks its integrity and
  schema version (backups from a newer PiCache are refused) and stages it.
  The next restart applies it. All sessions and API tokens are then revoked.
  The previous database is kept as `picache.db.before-restore`. If PiCache
  cannot start with the restored database, it puts the previous one back
  automatically (the failed file is kept as
  `picache.db.failed-restore-<timestamp>`).

Automated backups use an **admin** API token:

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
Docker). Then start PiCache. Sessions survive a file restore; use the UI
restore if they should be revoked.

**Moving to a new machine:** install PiCache there, stop it, copy
`picache.db` (and `keys/master.key`) into the data directory with the right
owner, and start it. The cache can be copied too, or it simply fills again.
A NAS store is adopted by initialising the target with *adopt* in
**Cache → Storage**.

---

## Upgrade

PiCache migrates its databases automatically. When a new version starts for
the first time, it saves a copy of `picache.db` to
`<data>/backups/picache-<previous version>-<timestamp>.db` and keeps the
newest three. Development builds (version `dev`) skip this.

- **Bare metal / LXC:** run the installer of the new version with the new
  binary. It replaces binary and units and restarts the service. Your
  `picache.env` is kept.

  ```sh
  sudo sh deploy/install.sh --binary ./picache-linux-amd64
  ```

- **Docker:** `git pull`, then `docker compose up -d --build` in
  `deploy/docker`.

**Rollback:** stop PiCache, install the previous binary, copy the matching
file from `<data>/backups/` over `picache.db` (as for a file restore), and
start it. An older binary cannot be expected to open a database that a newer
version has migrated.

---

## Uninstall

- **Bare metal / LXC:** `sudo sh deploy/install.sh --uninstall` stops and
  disables the units and removes the binary, the unit files and
  `/etc/picache/host-apply.enabled`. Configuration and data are kept. The
  script lists what remains, including NAS mount units written by the
  helper (`/etc/systemd/system/srv-picache-*.mount`). To remove everything:

  ```sh
  # disable and remove NAS mount units first (the script prints the commands)
  sudo rm -rf /var/lib/picache /var/cache/picache /etc/picache
  sudo rmdir /srv/picache        # fails while anything is still mounted there
  sudo userdel picache
  ```

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
- The Pi has no battery-backed clock. Until NTP has set the time, PiCache
  uses plain DNS to its bootstrap servers instead of DoH/DoT (health warning).
  Keep `systemd-timesyncd` enabled.
- Cache hits are limited by the Pi's Gigabit port (about 110 MB/s).

### NAS (SMB/NFS)

Only the slice files go to the NAS; the databases and the index stay local.
PiCache itself never mounts anything and never holds `CAP_SYS_ADMIN`.

| Option | Where it works | How |
|---|---|---|
| **External mount** (mode *external*) | everywhere | Something else mounts the share below `/srv/picache` (host fstab or systemd `.mount`, Proxmox mount point, Docker bind with `rslave`). The UI generates the fstab line, the `.mount` unit, the credentials file template, the compose bind and the Proxmox commands for each target. The snippets never contain the password. |
| **Host-apply** (mode *host-apply*) | bare metal, VMs, privileged LXC with systemd | Install with `--with-host-apply`. The UI queues a request. `picache-storage.path` starts the root helper (`picache storage apply-pending`), which re-validates the target, writes `/etc/picache/credentials/<id>.cred` and a `.mount` unit for `/srv/picache/<id>`, and starts it. You can also run `sudo picache storage apply <id>` yourself. |
| **Proxmox LXC** | unprivileged containers | The host mounts the share and passes it in with `pct set <ctid> -mpN`; see [deploy/lxc/README.md](../deploy/lxc/README.md). |
| **Docker** | Docker hosts | The host mounts the share below `/srv/picache` (fstab, `nofail`); the compose file binds `/srv/picache` with `propagation: rslave`. |

Recommendations:

- Give the server as an **IP address**. PiCache requires it, and it avoids a
  boot-time dependency on PiCache's own DNS.
- Use a dedicated share and a NAS account that can access only that share.
  SMB 3.1.1 is the default (optionally encrypted with *seal*). NFS uses
  version 4.2 with `softerr`. NFS relies on the client's address, so allow
  only PiCache's host in the export.
- The generated mounts are soft and have timeouts, so a hanging NAS cannot
  freeze PiCache. Use the same options for your own mounts.
- Hits cannot be faster than the NAS: about 110 MB/s over 1 GbE, which can be
  slower than your Internet connection.
- Docker named volumes of type `cifs`/`nfs` are not recommended. The
  container, and with it DNS, fails to start when the NAS is down, and
  `docker volume inspect` shows the SMB password.

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
| `PICACHE_DATA_DIR` | `/var/lib/picache` · `/data` | Databases, keys, lists, TLS certificate. Must be a local disk. |
| `PICACHE_CACHE_DIR` | `/var/cache/picache` · `/cache` | The built-in local cache store (target `local`). |
| `PICACHE_MOUNT_ROOT` | `/srv/picache` | The only directory below which other storage targets may live. |
| `PICACHE_DNS_LISTEN` | `:53` | DNS over UDP and TCP. Required; a bind failure stops PiCache. |
| `PICACHE_CACHE_LISTEN` | `:80` | LanCache HTTP cache. If it is off or cannot bind, LanCache DNS overrides stay inactive. |
| `PICACHE_SNI_LISTEN` | `:443` | LanCache HTTPS (SNI) pass-through. |
| `PICACHE_WEB_LISTEN` | `:8080` | Web UI and API over HTTP. |
| `PICACHE_WEB_TLS_LISTEN` | `:8443` | Web UI and API over HTTPS. At least one web listener is required. |
| `PICACHE_WEB_TLS_CERT` | – | PEM certificate for the HTTPS listener. Without it, PiCache creates and renews a self-signed certificate in `<data>/tls/`. |
| `PICACHE_WEB_TLS_KEY` | – | PEM private key; set together with the certificate. Both files must be readable by the service user. |
| `PICACHE_WEB_HOSTS` | – | Comma-separated extra host names allowed for the web UI (DNS-rebinding protection), e.g. a reverse-proxy name. Can also be set in the web settings. |
| `PICACHE_RUN_AS` | – · `65532:65532` | Numeric non-root `uid:gid`. When PiCache starts as root it binds the listeners and then switches to this user before touching files. Linux only. Not needed with the systemd unit. |
| `PICACHE_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `PICACHE_LOG_FORMAT` | `text` | `text` or `json` (logs go to stderr / the journal). |
| `PICACHE_ADMIN_USER` | `admin` | User name for password provisioning. |
| `PICACHE_ADMIN_PASSWORD_FILE` | – | File with the password (at least 10 characters; a trailing newline is ignored) for the first admin. Used only while no user exists. |
| `PICACHE_ADMIN_PASSWORD` | – | Same as a plain variable (discouraged, logged as a warning: visible to other processes and in `docker inspect`). The `_FILE` variant wins. |
| `PICACHE_MASTER_KEY_FILE` | `<data>/keys/master.key` | Master key for stored secrets: 32 raw bytes or 64 hex characters. Created with mode 0600 if missing. A systemd credential `picache-master-key` (`$CREDENTIALS_DIRECTORY`) or the Docker secret `/run/secrets/picache_master_key` takes precedence. The Docker secret is read after the privilege drop, so it must be readable by 65532. |
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
| `picache healthcheck [url]` | Exit 0 if the local web endpoint answers `/healthz` with `ok` and the DNS listener resolves `localhost`. It uses the first address of `PICACHE_WEB_LISTEN` (or of `PICACHE_WEB_TLS_LISTEN` if HTTP is off) and of `PICACHE_DNS_LISTEN`, with wildcard addresses replaced by `127.0.0.1`. With a URL, only that URL is checked. Used by the Docker `HEALTHCHECK`. |
| `picache reset-password [user]` | Set a new password for `user` (default `admin`), read from stdin (at least 10 characters). The input is not hidden; you can redirect it from a file. Disables TOTP and signs out all sessions. Run it as root or as the service user. As root it switches to the owner of the data directory first. |
| `picache setup-token` | Print the first-run setup token (until setup is done). Needs read access to the data directory: `sudo` on bare metal, `-u 65532:65532` in Docker. |
| `picache storage apply <id> [--password-stdin]` | Root only. Validate the storage target, write `/etc/picache/credentials/<id>.cred` and a systemd `.mount` unit for `/srv/picache/<id>`, then `systemctl daemon-reload` and `enable --now`. The NAS password is decrypted from the database with the master key, or read from stdin with `--password-stdin`. |
| `picache storage apply-pending` | Root only. Process the mount requests the web UI queued in `<data>/storage-requests/`. Run by `picache-storage.service`. |
| `picache help` | Print the usage. |

Exit codes: `0` success, `1` error or unhealthy, `2` usage or configuration
error, `75` restart requested from the web UI (systemd and Docker restart the
process).

---

## Troubleshooting

- **Health:** **System → Health & about** lists every check with a hint:
  listeners, upstreams (including the clock guard), blocklists, rate limiting, cache-domains,
  LanCache IP, SNI, cache storage, logs and free space on the data disk.
- **Logs:** `journalctl -u picache -f` (bare metal/LXC),
  `docker compose logs -f picache` (Docker). Set `PICACHE_LOG_LEVEL=debug`
  for more detail.
- **`bind … permission denied`:** PiCache was started without
  `CAP_NET_BIND_SERVICE`. Use the systemd unit or the compose file from
  `deploy/`.
- **`… is owned by … but PiCache runs as …` (Docker):** a bind-mounted
  directory has the wrong owner; `chown -R 65532:65532` it on the host.
- **`status=226/NAMESPACE` (LXC):** enable the container's nesting feature.
- **No LanCache answers:** LanCache must be enabled, the cache-domains list
  loaded, the :80 listener bound and a private cache IPv4 address known, and
  the client must not be in a group that bypasses LanCache. The health page
  names the missing piece.
- **Forgotten password:** `sudo picache reset-password admin`.
