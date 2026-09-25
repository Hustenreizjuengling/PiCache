# Changelog

All notable changes to PiCache are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

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

[Unreleased]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Hustenreizjuengling/PiCache/releases/tag/v0.1.0
