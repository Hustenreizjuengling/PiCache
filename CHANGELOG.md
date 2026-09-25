# Changelog

All notable changes to PiCache are documented in this file. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

PiCache has not had a release yet. Everything so far is listed under
Unreleased.

## [Unreleased]

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

### Fixed

- The Docker health check no longer shows up as a client. `picache
  healthcheck` now asks for `healthcheck.picache.invalid`, which PiCache
  answers locally for queries from its own machine without counting, rate
  limiting or logging them. Before, its `localhost` query every 30 s
  appeared in the query log, the statistics and the client lists.
- Duplicate records in upstream replies (Docker's embedded DNS repeats every
  A and AAAA record) are removed before the reply is cached, answered and
  logged.
- After a restart, the services page showed "Last attempt: Never" next to
  the time of the last update of the domain list. It now shows "Last failed
  attempt" only when the last attempt failed.
- Charts: the last x-axis label is no longer cut off, and the bucket that is
  still filling no longer draws a false drop at the end of live charts.
- Counts use singular and plural forms in English and German, chosen for
  the number as it is shown ("1 query/min" instead of "1 queries/min").
- Truncated table columns (audit log, rules, groups, local records,
  forwarders, clients seen, API tokens) no longer cut targets, details and
  comments to a few characters. The download sessions table uses compact
  dates and fits a 1440 px wide window.
- Downloads: an active session shows the same byte counts as "Downloading
  now", and the list updates as soon as a download starts or ends.
- Filtering: the tab badges count all lists and rules, including disabled
  ones, and the summary says that its pattern count covers only the lists.
- Clients: the "Traffic in" range picker no longer shifts the tabs, the
  range last chosen is kept for links without `?range=`, and the "Seen
  recently" description reads correctly for every range.
- Storage: the Docker bridge warning appears only when the cache address is
  detected automatically, and the note about the space kept free on a disk
  shared with PiCache's data shows the same value as "Kept free".
- A side panel opened from a link no longer shows a focus ring on its close
  button.

[Unreleased]: https://github.com/Hustenreizjuengling/PiCache/commits/main
