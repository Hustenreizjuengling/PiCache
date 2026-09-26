# Changelog

All notable changes to PiCache are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.8.0] - 2026-09-26

### Changed

- **The DHCP server needs no installation option any more.** It is
  available on every Linux installation and switched on in the web UI
  (**DNS → DHCP**) when needed; `install.sh --with-dhcp` and
  `PICACHE_DHCP=on` are no longer needed. **Nothing is held while DHCP is
  off**: PiCache opens UDP ports 67 and 547 only while the DHCP server is
  switched on (a search for other DHCP servers opens port 67 for its 5
  seconds) and never binds 546; the passive detection of other DHCP
  servers (a device asking another server) therefore runs only while it is
  on. PiCache remembers what to open at the next start in two empty files
  in the data directory (`dhcp.sockets`, `dhcp.ra`).
- `PICACHE_DHCP` has three states: unset (the default: the DHCP server can
  be switched on in the web UI), `off` (it cannot; for hosts that run
  another DHCP server) and the old `on` (still accepted: everything opens at
  start and PiCache closes what is not needed). **`install.sh
  --without-dhcp` now writes `PICACHE_DHCP=off`** instead of removing the
  DHCP support; `--with-dhcp` removes that opt-out again. Every installer
  run removes an old `PICACHE_DHCP=on` (creating the two DHCP markers in
  its place, so the first start of 0.8.0 still opens the DHCP ports and
  the raw socket for router advertisements), the drop-in
  `/etc/systemd/system/picache.service.d/60-dhcp.conf` and
  `/etc/picache/dhcp.enabled`, and rewrites any off spelling as
  `PICACHE_DHCP=off`; a value it does not know is reported (PiCache refuses
  to start with it). It also replaces the DHCP comment of a 0.7.0
  `picache.env`, which still described the old install option. A leftover `60-dhcp.conf` on an installation updated
  only from the web UI is harmless (it holds the same settings as the new
  unit) and goes with the next installer run.
- **Docker needs one restart after switching DHCP on:** the container opens
  its DHCP ports only at start (the switch to 65532 clears every
  capability), and the page offers the restart. The compose file now has
  `NET_RAW` in `cap_add` (used only at start for the router
  advertisements); an existing compose file keeps working for DHCPv4 and
  DHCPv6, and the page says what to add for router advertisements.
- **Router advertisements need one restart after switching them on**: the
  raw socket can only be opened at start. The systemd unit now grants
  `CAP_NET_RAW` for it (with `capset` allowed) on every installation;
  PiCache drops it on every thread right after every start and now
  **refuses to run** if that fails (before, it only closed the socket and
  warned). If the drop cannot be verified, the raw socket is closed and the
  health check `dhcp` fails.
- **Updates also install the unit files** of the release (only from the
  signed `picache-deploy.tar.gz`, only PiCache's own units that are
  installed, never drop-ins; the old ones kept as `<unit>.prev`), where the
  update helper may write them; if that fails, only the program is updated
  and the message says so. **Existing installations run the one-line
  installer once** to get the new units (`CAP_NET_RAW` for router
  advertisements, the resource weights and an update helper that may
  replace unit files); from then on updates from the web UI and the CLI keep
  them current (`sudo picache update` does so from its first update after
  0.8.0 anyway). The update to this version itself is installed without its
  units. `install.sh --uninstall` removes the `.prev` copies.
- `picache.service` has `CPUWeight=200` and `IOWeight=200`: under CPU or
  disk contention PiCache gets twice the share of a service with the default
  weight (a mild bias; no effect where the cgroup controller is not
  delegated or the I/O scheduler ignores weights).
- Two devices with the same host name: the second now gets its generated
  name (below) instead of no DNS name.

### Added

- DHCP reservations: import and export (CSV with a guard against
  spreadsheet formulas, hosts format, and `mac ip [hostname]` lines; with a
  preview, all or nothing, optionally replacing all reservations), a lease
  time per device, and matching by client identifier (option 61) for
  devices that change their MAC address (as forgeable as the MAC: a
  convenience, not a security control). API: `GET /dhcp/static/export`,
  `POST /dhcp/static/import`; reservations have `clientId` and
  `leaseSeconds`.
- Generated DNS names for leases without a usable host name, such as
  `192-168-178-23.lan` (`dhcp.generateNames`, on by default; DNS answers
  only). Devices can no longer take such names, `wpad` or `localhost`.
- DHCP options NTP servers, interface MTU, WPAD URL (sent only when a
  device asks; changes of the WPAD URL are audited with the value) and
  extra search domains (`dhcp.options`).
- **Only reserved devices** (`dhcp.onlyReserved`): devices without a
  reservation get no answer from PiCache, so it can serve known devices next
  to another DHCP server.
- **End all leases** (`DELETE /dhcp/leases`) and **Reset DHCP**
  (`POST /dhcp/reset`: settings back to their defaults, every reservation
  and lease deleted; detections of other servers are kept).
- A log of the last 200 DHCP exchanges (`GET /dhcp/log`) and **rapid
  commit** (option 80; `dhcp.rapidCommit`, new and **off by default**, used
  only while no other DHCP server counts).
- PiCache looks for other routers and DHCPv6 servers that announce their own
  DNS server while it announces itself over IPv6 (a router solicitation and
  a relayed DHCPv6 information request, and every router advertisement seen
  while its own are on) and warns about them (page, health check `dhcp`,
  notification). The network check's `ipv6-dns` data carries the DNS
  servers the default router announces (`routerRdnss`).
- `GET /dhcp` has `reasonCode` (`opt-out`, `not-linux`, `bridge`, `socket`,
  `restart-required`), `markerError` and `deployment`; the router
  advertisements have a `reasonCode` too, and `ipv6.otherAnnouncers` (with
  `ownDns`, the announced DNS servers that are PiCache's own) and
  `ipv6.lastSearch`.

## [0.7.0] - 2026-09-25

### Added

- Optional DHCP server (**DNS → DHCP**), for routers that cannot hand out
  another DNS server: IPv4 addresses from a range on one chosen interface,
  with the router, PiCache as DNS server and the local domain; static
  leases; the devices' host names become DNS names (`laptop.lan` and the
  reverse name), shown in the query log and client lists. Off by default:
  it needs `install.sh --with-dhcp` (Docker: `PICACHE_DHCP` and host
  networking) and serves only when PiCache's own address is static and no
  other DHCP server answers (PiCache looks for one before it serves and
  every 10 minutes, and notices devices that ask another one). New API
  routes under `/dhcp`, the settings section `dhcp` and the health check
  `dhcp`.
- IPv6 DNS announcements of the DHCP server: router advertisements that
  carry only PiCache's ULA as DNS server and the domain (router lifetime 0,
  no prefixes: PiCache never becomes a router) and stateless DHCPv6. The
  raw socket needs `CAP_NET_RAW` at start (`install.sh --with-dhcp`, Docker
  `cap_add: [NET_RAW]`); PiCache drops it right afterwards on all threads
  and checks that it is gone.
- The network check says when PiCache hands out addresses itself (the
  router's IPv4 DNS steps then do not apply) and when it announces itself
  as IPv6 DNS server.
- `install.sh --with-dhcp` / `--without-dhcp` (also through
  `get-picache.sh`).

## [0.6.0] - 2026-09-25

### Added

- IPv6 parity: a client configured by its IPv4 address or a network also
  covers the device's IPv6 addresses on the same network, privacy addresses
  included (PiCache learns them from the device's MAC address in the
  neighbour table; for a new address it resolves the MAC before answering
  its first query, so parental controls cannot be bypassed over IPv6). An IPv6
  address without a name of its own shows the name of the device's IPv4
  address.
- Statistics per device: the overview's top clients and **Clients &
  groups** can show one row per device with all its addresses instead of one
  row per address. API: `GET /stats/clients` and `GET /stats/top` take
  `group=device`; client statistics carry `clientId`, `mac` and `addresses`;
  the query log and its live stream take repeated `client` values.
- **Allow every network this machine is connected to**
  (`dns.trustConnectedNetworks`, off by default): devices with a public IPv6
  address of the LAN may use PiCache, and a new prefix from the provider is
  followed within a minute. The network check offers it for refused
  devices of the local network and notes when PiCache's machine ignores
  IPv6 router advertisements.
- **Do not answer IPv6 addresses (AAAA)** (`dns.disableAAAA`) for networks
  whose IPv6 does not work, and **DNS64** (`dns.dns64`) for IPv6-only
  networks with a NAT64 gateway.
- The router resolver works over IPv6 when there is no IPv4 default gateway,
  and every address of the router is protected from forwarding loops and
  exempt from the rate limit.
- The default bootstrap servers include the IPv6 addresses of Quad9 and
  Cloudflare; an unchanged default list gets them with the update.

### Fixed

- Server-name answers no longer include deprecated or tentative IPv6
  addresses, prefer stable addresses over temporary ones and unique local
  addresses over global ones, and follow address changes within a minute.
- NODATA answers of the blocking modes `null` (other query types) and
  `custom_ip` (no blocking address of the queried family) carry the
  synthetic SOA like the other negative answers.
- The rate-limit help text: devices are limited per address; public IPv6
  addresses per /64. The access settings no longer say that the connected
  networks may always use PiCache (only private ones may).
- Client names are looked up through a router resolver with an IPv6 address
  too (the lookups were never sent).

## [0.5.0] - 2026-09-25

### Added

- Parental controls: **DNS → Parental controls** restricts the devices of a
  client group. Services from a built-in catalogue (YouTube, TikTok,
  Instagram, WhatsApp, Discord, Roblox, Fortnite, Steam, Netflix, ChatGPT and
  more) can be blocked always; up to 10 weekly schedules per group block all
  internet (a bedtime, also overnight) or selected services (homework time);
  "Block internet now" and "Lift restrictions" work for a set time. The
  page shows each group's state and weekly plan and tests a domain for a
  device. Parental controls apply even while blocking is paused, a user
  allow rule lets a name through, and blocked queries appear in the query
  log with the new statuses `blocked-schedule` and `blocked-service` and the
  group as reason. API: `/parental/services`, `/parental/groups`.
- Network check: **DNS → Network check** compares the devices in the
  network (the kernel's neighbour table) with PiCache's DNS queries and
  finds a router that forwards all queries or announces itself as IPv6 DNS
  server, missing IPv6 addresses of PiCache, sources refused by the access
  list, and devices that do not use PiCache (named as the router knows
  them), with the steps to fix it for a
  FRITZ!Box and other routers. An optional scan (admins) makes switched-on
  devices show up. The new health check `network` warns while most queries
  come from the router. API: `/network/check`, `/network/scan`.

## [0.4.0] - 2026-09-25

### Added

- Notifications: **System → Notifications** sends messages through ntfy,
  Gotify or a webhook (for example Home Assistant) when a health check fails
  or warns (and recovers), the cache storage goes offline (and back), an
  update is available, installed or fails, a scheduled backup fails or
  succeeds, or sign-ins are locked out. Per channel: minimum severity, event
  filter, test button; secrets are sealed with the master key and
  write-only; a delivery log shows the last attempts.
- Scheduled backups: daily or weekly at a set time, keeping the newest N
  files, to the data directory or to a storage target such as the NAS;
  missed runs are made up after a restart; run now, download and delete in
  **System → Backup & restore**. The time is that of the host, and the page
  shows its time zone (a Docker container uses UTC unless `TZ` is set).

## [0.3.1] - 2026-09-25

### Fixed

- NAS mounts through the root helper: a missing `mount.nfs` (package
  `nfs-common`) or `mount.cifs` (`cifs-utils`) is now reported with the
  package to install, instead of the kernel's misleading "Server address does
  not match proto= option" for NFS. The installer also says that NFS 4 does
  not need `rpcbind`.
- The storage page's hint for NFS permission errors names the settings of
  common NAS systems (TrueNAS Mapall User/Group, Synology Squash).

## [0.3.0] - 2026-09-25

### Added

- Storage speed test: **Cache → Storage → Test speed** measures write,
  read, cache-read (the path of a cache hit) and file-operation speed of a
  storage target, compares it with network speeds and keeps the last result
  per target.

### Documentation

- How to put the NAS on a separate storage network (no PiCache setting
  needed) and when that helps.

## [0.2.0] - 2026-09-25

Update from v0.1.0 on **System → Updates** or with `sudo picache update`
(Docker: `docker compose pull && docker compose up -d`). Settings, query
log and statistics are migrated automatically at the first start; only
API clients and scripts need the new names below.

### Changed

- **Breaking:** the download cache has new technical names. Update API
  clients and scripts that use the old ones.
  - Its API routes are now under `/api/v1/download-cache/...` (the
    sub-paths are unchanged).
  - Its settings section is now `downloadCache`, for example
    `downloadCache.cacheIpv4` and `PATCH /api/v1/settings/downloadCache`.
    Stored settings and restored backups are migrated automatically.
  - DNS queries answered with the cache address have the status
    `override`, also in the statistics series. The query log and the
    statistics are migrated automatically.
  - The related JSON fields are `downloadCacheEnabled`,
    `downloadCacheBypass` and `dnsDownloadCache`.
  - Its health check is `download_cache`, and new audit log entries use
    `download_cache.*` (existing entries keep their names).
  - Its Go package is `internal/dlcache`.
- Unchanged: the names that Steam and prefill tools rely on, that is the
  hostname Steam uses to discover a download cache, the heartbeat path that
  prefill tools probe and the response header they check.

## [0.1.0] - 2026-09-25

First release.

### Added

- Filtering DNS server: blocklists in hosts, domain, adblock (ABP) and regex
  formats with a built-in catalogue (HaGeZi Multi NORMAL by default), your
  own allow and block rules, groups for clients by IP, CIDR or MAC (clients
  get the lists and rules of their groups), five blocking modes, a timed
  pause, CNAME inspection, and blocking of the Firefox DoH canary and iCloud
  Private Relay.
- Local DNS records (A, AAAA, CNAME, TXT, wildcards, automatic PTR),
  conditional forwarding, and the router as resolver for local names and
  reverse lookups.
- Upstreams over DNS-over-HTTPS, DNS-over-TLS, UDP and TCP in load-balance,
  parallel or strict mode, with a response cache, serve-stale and a clock
  guard for devices without a real-time clock.
- Protection against open-resolver abuse: private networks only by default,
  per-client rate limits, `ANY` and CHAOS queries refused, private reverse
  zones never forwarded to public upstreams.
- Download cache: DNS answers that send downloads to the cache, from the
  cache-domains lists
  ([uklans/cache-domains](https://github.com/uklans/cache-domains)) plus
  custom services and hosts, an HTTP slice cache on port 80 (range requests,
  merged concurrent downloads, read-ahead), SNI pass-through on port 443,
  Steam's cache discovery, and the heartbeat path and response header that
  prefill tools use.
- Content grouping of cached downloads (Steam depots, Blizzard products,
  Epic, Riot, Xbox packages, Windows KBs, PlayStation titles, …) with your
  own labels, pinning, retention by inactivity and size, background verify
  and rebuild.
- Cache storage on local disks or SMB/NFS shares, with a mount guard,
  generated mount snippets and an optional root helper that mounts shares on
  request of the web UI.
- Web UI in English and German with overview, query log, live streams,
  statistics, filtering, clients and groups, local DNS, downloads, library,
  services, storage, settings, audit log, backup and restore, and health
  checks.
- One admin account created with a one-time setup token, optional TOTP
  two-factor authentication, API tokens with `read` or `admin` scope, and
  optional Prometheus metrics.
- REST API under `/api/v1` with Server-Sent Events for the live streams.
- Deployment: static binaries for linux/amd64, linux/arm64 and linux/arm
  (v7), a Debian 12/13 installer with hardened systemd units, a Proxmox LXC
  guide, and a distroless Docker image that drops root after binding its
  ports. The installer and the image put `LICENSE` and
  `THIRD_PARTY_NOTICES.md` into `/usr/share/doc/picache/`.
- CI: web UI type check and build, Go vet and tests on Linux, Windows and
  macOS, govulncheck, release builds, an installer smoke test and a
  multi-arch Docker build.
- Updates: **System → Updates** shows the installed version and the newest
  release with its notes. PiCache asks the GitHub API once a day whether a
  new release is out (can be turned off; pre-releases only on request) and
  never installs anything by itself. An admin installs an update with the
  password; a root helper (`picache-update.path` and `.service`, installed
  by `install.sh` unless `--without-updater`) downloads the release, checks
  its signature and checksum, replaces the binary, restarts PiCache and
  rolls back to the previous binary and database if the new version fails
  its health check. The web UI never offers a downgrade.
- `picache update`: `--check` (exit code 10 when an update is available),
  install the newest release or `--version vX.Y.Z` with the same checks and
  rollback, `--prerelease`, `--allow-downgrade`, and offline installs with
  `--from <dir>`.
- Signed releases: pushing a tag `vX.Y.Z` builds the static binaries,
  `picache-deploy.tar.gz` (installer, units, compose files, license texts)
  and `SHA256SUMS` with `make dist`, signs `SHA256SUMS` with the project's
  Ed25519 release key (`docs/release-key.pem`, also built into PiCache) and
  publishes a GitHub release with the matching section of this file as
  notes. Tags with a hyphen become pre-releases.
- One-line installer: `curl -fsSL https://github.com/Hustenreizjuengling/PiCache/releases/latest/download/get-picache.sh | sudo sh`
  installs the newest release after checking its signature, upgrades an
  existing installation when run again, and removes PiCache with
  `--uninstall` (`--purge` also deletes the configuration, the data, the
  local cache and the picache user).
- Container images for linux/amd64, linux/arm64 and linux/arm/v7 at
  `ghcr.io/hustenreizjuengling/picache`, tagged `X.Y.Z`, `X.Y` and `latest`.
  Docker installations update with
  `docker compose pull && docker compose up -d`.

[Unreleased]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.8.0...HEAD
[0.8.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Hustenreizjuengling/PiCache/releases/tag/v0.1.0
