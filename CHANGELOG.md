# Changelog

All notable changes to PiCache are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.1.0] - 2026-09-25

First release.

### Added

- Filtering DNS server: blocklists in hosts, domain, AdGuard/ABP and regex
  formats with a built-in catalogue (HaGeZi Multi NORMAL by default), your
  own allow and block rules, Pi-hole-style groups for clients by IP, CIDR or
  MAC, five blocking modes, a timed pause, CNAME inspection, and blocking of
  the Firefox DoH canary and iCloud Private Relay.
- Local DNS records (A, AAAA, CNAME, TXT, wildcards, automatic PTR),
  conditional forwarding, and the router as resolver for local names and
  reverse lookups.
- Upstreams over DNS-over-HTTPS, DNS-over-TLS, UDP and TCP in load-balance,
  parallel or strict mode, with a response cache, serve-stale and a clock
  guard for devices without a real-time clock.
- Protection against open-resolver abuse: private networks only by default,
  per-client rate limits, `ANY` and CHAOS queries refused, private reverse
  zones never forwarded to public upstreams.
- LanCache-compatible download cache: DNS overrides from
  [uklans/cache-domains](https://github.com/uklans/cache-domains) plus custom
  services and hosts, an HTTP slice cache on port 80 (range requests, merged
  concurrent downloads, read-ahead), SNI pass-through on port 443, heartbeat
  and prefill-tool compatibility.
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

[Unreleased]: https://github.com/Hustenreizjuengling/PiCache/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Hustenreizjuengling/PiCache/releases/tag/v0.1.0
