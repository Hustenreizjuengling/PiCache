# PiCache architecture

PiCache is a single Go binary with an embedded web UI. It combines:

- a **filtering DNS server**, the central DNS for a home or lab network, with parity to the Pi-hole and AdGuard Home features that matter;
- a **LanCache-compatible download cache**: DNS overrides, an HTTP slice cache on :80, and SNI pass-through on :443;
- one **web UI and REST API** to operate both, including NAS-backed cache storage.

This document is the binding specification for the implementation. Where it says MUST, the code must do exactly that. Research notes with primary sources are summarised inline; the numbers and behaviours below were verified against upstream source (lancachenet/monolithic, uklans/cache-domains, pi-hole/FTL, AdGuardHome, Linux kernel, systemd, moby) in September 2026.

Priorities, in this order: **security, simplicity, correctness, performance, features.**

---

## 1. Goals and non-goals

**Goals**
- One process, one config database, one UI. No nginx, BIND, dnsmasq or cron underneath.
- Deployable on Debian 12/13 bare metal, Proxmox LXC (unprivileged) and Docker, with clearly identified persistent data.
- Secure defaults: no open resolver, no open proxy, no open TLS relay, unprivileged runtime, authenticated UI, strict CSP.
- LanCache-compatible behaviour (Steam, Epic, Battle.net, Riot, Xbox/WSUS, PlayStation, Nintendo, …) including the lancache heartbeat and prefill-tool compatibility.
- First-class observability: who requested what, what is cached, how long it stays, how much bandwidth was saved.

**Non-goals (for now)**: DHCP server, DoH/DoT/DoQ *serving* to clients, local DNSSEC validation, blocked-services catalogue, safe-search enforcement, TLS interception of HTTPS downloads (never), userspace SMB/NFS clients, nginx cache-disk import. The data model must not prevent adding these later.

---

## 2. Runtime overview

```
                    ┌──────────────────────── picache (one process) ─────────────────────────┐
 clients ──:53/udp,tcp──▶ dnsserver ──▶ filter ──▶ upstream (DoH/DoT/UDP, cache) ──▶ Internet DNS
                    │        │  ▲ clients (identity, groups)                                  │
                    │        └─ lancache overrides (services) → answers with cache IP         │
 clients ──:80/http─────▶ proxy ──▶ cachestore (slices on local disk / NAS) ──▶ CDN over HTTP │
 clients ──:443/tls─────▶ sni (pass-through, allowlisted SNI only) ────────────▶ CDN :443     │
 admin ──:8080/:8443────▶ api + web UI (auth, CSRF, CSP) ──▶ all components                   │
                    │  logs.db (query log, cache log, stats) · picache.db (config) · index.db │
                    └──────────────────────────────────────────────────────────────────────────┘
```

The proxy and SNI server resolve CDN hostnames through `upstream.Resolver.LookupIP`, which **bypasses** local overrides, filtering and local records. This avoids resolution loops (the cache would otherwise resolve CDN hosts to itself).

### Listeners (bootstrap config, restart required to change)

| Env | Default | Purpose |
|---|---|---|
| `PICACHE_DNS_LISTEN` | `:53` | DNS over UDP and TCP (comma-separated list of addresses) |
| `PICACHE_CACHE_LISTEN` | `:80` | LanCache HTTP cache |
| `PICACHE_SNI_LISTEN` | `:443` | SNI pass-through for overridden domains (empty = disabled) |
| `PICACHE_WEB_LISTEN` | `:8080` | Web UI + API over HTTP |
| `PICACHE_WEB_TLS_LISTEN` | `:8443` | Web UI + API over HTTPS (self-signed unless a cert is given; empty = disabled) |

All listeners are bound **before** privileges are dropped (section 6).

---

## 3. Persistent data

Everything persistent lives in exactly two places plus optional NAS mounts. Nothing else is written.

| Path (bare metal / LXC) | Docker | Contents | Backup? |
|---|---|---|---|
| `/var/lib/picache` (`PICACHE_DATA_DIR`) | `/data` | `picache.db` (all configuration: settings, users, lists, rules, clients, groups, local records, services, storage targets with encrypted secrets, audit log); `logs.db` (query log, cache request log, download sessions, statistics, evictions); `cache-index/<store-id>.db` (cache index per store); `lists/` (downloaded blocklists); `cache-domains/` (downloaded cache-domains snapshot); `tls/` (self-signed web cert); `keys/master.key` (secret-encryption key, 0600) | `picache.db` yes (via UI export or file copy while stopped); the rest is rebuildable |
| `/var/cache/picache` (`PICACHE_CACHE_DIR`) | `/cache` | The default **local** cache store (slice files). Large. | No (re-downloadable) |
| `/srv/picache/<name>` | `/srv/picache` (bind, `rslave`) | Conventional mountpoints for NAS cache stores (SMB/NFS), mounted by the host or by PiCache | No |
| `/etc/picache/picache.env` | environment | Bootstrap settings only (listen addresses, paths, run-as). All runtime settings live in `picache.db` and are edited in the UI. | Yes |

Rules:
- SQLite databases MUST live on local disk (WAL does not work on network filesystems). PiCache refuses to open a DB on NFS/CIFS (statfs magic check).
- Only slice files may live on a NAS.
- The master key is never included in UI exports. Exports contain encrypted secrets only if the user opts in, and then they are useless without the key.

---

## 4. Package layout and dependency rules

```
cmd/picache/                 CLI: serve (default), version, healthcheck, reset-password, setup-token, storage apply
internal/version/            build info (ldflags)
internal/config/             bootstrap config from env/flags, derived paths
internal/db/                 SQLite (modernc) open with writer/reader pools, per-component migrations
internal/apperr/             typed errors shared by domain packages and the API (NotFound, Invalid, Conflict, …)
internal/settings/           typed runtime settings (one JSON document in picache.db), validation, pub/sub
internal/secrets/            master key + AEAD seal/open for stored secrets
internal/netutil/            ACL, private/public IP classification, SSRF-safe dialer, rate limiter, host normalisation, local addresses
internal/auth/               users, argon2id, sessions, API tokens, TOTP, login throttling, setup token, audit log
internal/clients/            clients, groups, identity resolution (IP/CIDR/MAC), ARP/neighbour table, client names
internal/dns/upstream/       upstream transports (UDP/TCP/DoT/DoH), modes, response cache, serve-stale, bypass LookupIP
internal/dns/filter/         blocklists (fetch, parse, compile), custom rules, matcher, explain
internal/dns/server/         DNS listeners + request pipeline, local records, conditional forwarders, pause
internal/lancache/services/  cache-domains source, service registry & matcher, custom services, content grouping, labels
internal/lancache/store/     slice store on disk + index DB + eviction + verify/rebuild   (package cachestore)
internal/lancache/proxy/     HTTP cache proxy (:80)
internal/lancache/sni/       TLS SNI pass-through (:443)
internal/storage/            storage targets (local/SMB/NFS), capability detection, mount guard, in-process mounts, host-apply, snippets
internal/logs/               logs.db: query log, cache events, sessions, rollups, evictions, live subscriptions
internal/api/                REST API + SSE, middleware, one routes_<domain>.go file per domain
internal/webui/              go:embed of the built frontend (internal/webui/dist)
internal/app/                wiring, lifecycle, privilege drop, store switching, jobs
web/                         Svelte 5 + Vite SPA (builds into internal/webui/dist)
deploy/                      docker/, systemd/, lxc/, install.sh
docs/                        this file, API.md, DEPLOYMENT.md, SECURITY.md
```

Dependency rules (enforced by review):
- `config`, `db`, `apperr`, `settings`, `secrets`, `netutil`, `version` import no other internal package except `settings` → none, `netutil` → `settings`, `db` → none.
- Domain packages (`auth`, `clients`, `upstream`, `filter`, `dnsserver`, `services`, `cachestore`, `proxy`, `sni`, `storage`, `logs`) may import the foundation packages above and each other only along these edges: `dnsserver` → {`upstream`, `filter`, `clients`, `services`, `logs`}; `proxy` → {`upstream`, `services`, `cachestore`, `clients`, `logs`}; `sni` → {`upstream`, `services`, `logs`}; `filter` → none of the domain packages; `services` → `upstream` (only for LookupIP-based HTTP fetching).
- `api` imports domain packages; domain packages never import `api`.
- `app` imports everything and is imported only by `cmd`.

Third-party dependencies are limited to: `github.com/miekg/dns v1.1.73`, `modernc.org/sqlite v1.59.0` (with `modernc.org/libc v1.75.7` pinned exactly; never bump libc alone), `golang.org/x/{crypto,sys,sync,net,time}`. Adding any other module needs an explicit decision.

---

## 5. Conventions

- Go 1.27. `CGO_ENABLED=0`. Pure Go, static binary.
- Logging: `log/slog`, structured, component attribute `slog.String("component", "filter")`. No `fmt.Println` in library code. Never log secrets, passwords, tokens, full query strings of CDN URLs (they may carry auth tokens).
- JSON: `encoding/json/v2` (GA in Go 1.27) for the API and stored JSON. Field names are lowerCamelCase. API decoding rejects unknown members.
- Time: stored as INTEGER unix **milliseconds** (UTC) in SQLite. API entities use RFC 3339 strings; time series use unix **seconds**.
- IDs: SQLite `INTEGER PRIMARY KEY` for rows; service IDs are the cache-domains names (`steam`, `epicgames`, …); storage target and store IDs are random 16-byte hex strings, except the built-in target `local`.
- Errors: domain packages return `apperr` errors for user-facing conditions (`apperr.NotFound`, `apperr.Invalid(field, msg)`, `apperr.Conflict`, `apperr.Forbidden`, `apperr.Unavailable`). The API maps them to 404/400/409/403/503. Everything else is a 500 with a generic message; details go to the log.
- Contexts: every blocking call takes a `context.Context`. Background loops stop when the context passed to `Start(ctx)` is cancelled.
- Concurrency: hot-path read structures are immutable snapshots behind `atomic.Pointer` and swapped on change (filter matcher, service matcher, ACL, client index).
- OS-specific code lives in `*_linux.go` with a portable `*_other.go` fallback, so `go build ./...` and `go test ./...` work on Windows and macOS for development. Release targets: linux/amd64, linux/arm64, linux/arm (v7).
- Tests: table-driven unit tests per package; `testing/synctest` for time-dependent logic; `httptest` for HTTP. No network access in unit tests.
- SQL: parameterised statements only. Never build SQL from user input by string concatenation (sort columns come from an allowlist).

---

## 6. Security model

### 6.1 Threats and controls

| Threat | Control |
|---|---|
| Open DNS resolver / amplification | DNS ACL: only loopback, RFC 1918, ULA `fc00::/7`, link-local, CGNAT `100.64/10`, directly connected subnets and user CIDRs. Others: UDP dropped, TCP REFUSED. `ANY` → NOTIMP. CHAOS class, `version.bind`, `id.server`, `hostname.bind` → REFUSED. Per-client token-bucket rate limit (default 50 qps, burst 200) with exempt CIDRs; UDP excess dropped, TCP REFUSED. EDNS buffer capped at 1232. |
| Cache poisoning | Random IDs and source ports (miekg/dns client), question section verified on every upstream reply, in-flight dedup (singleflight), DoH/DoT upstreams by default. |
| Private reverse-DNS leak | PTR/SOA/NS for RFC 6303 private zones never go to public upstreams; they go to configured local PTR resolvers or get NXDOMAIN. |
| Open HTTP proxy / SSRF via :80 | Proxy only serves hosts of known LanCache services (enabled → cached, disabled → pass-through uncached); everything else 403. Upstream IPs that are private, loopback, link-local, multicast, unspecified, CGNAT, documentation ranges or one of this machine's addresses are refused unless explicitly allowed. Redirect hops are re-checked. Host header port is ignored; upstream is always :80 (or the scheme/port of a verified redirect). Loop detection via `X-LanCache-Processed-By`. Client ACL same as DNS. |
| Open TLS relay via :443 | SNI must match an enabled service domain; no SNI → close; same SSRF rules; client ACL; per-client connection limit; idle timeouts. |
| Web UI takeover on first start | Setup requires a one-time setup token printed to the log and written to `<data>/setup-token` (0600), or the admin password is provisioned via `PICACHE_ADMIN_PASSWORD(_FILE)`. |
| Password guessing | argon2id (m=19456 KiB, t=2, p=1, PHC string, rehash on parameter change); per-IP backoff (5 failures → 15 min lockout) and a global limit; max 2 concurrent hash computations; optional TOTP. |
| Session theft / CSRF | 256-bit random session tokens, stored as SHA-256 hash; cookie `picache_session`: HttpOnly, SameSite=Strict, Secure on HTTPS, Path=/; idle timeout 60 min, absolute 7 days; `http.CrossOriginProtection` on all state-changing requests; JSON-only request bodies. |
| DNS rebinding against the UI | Host allowlist middleware: IP literals, `localhost`, the OS hostname (+ `.<localDomain>`), the configured server names and `PICACHE_WEB_HOSTS`; anything else → 421. |
| XSS / clickjacking | CSP `default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; manifest-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'`; `X-Frame-Options: DENY`; `X-Content-Type-Options: nosniff`; `Referrer-Policy: no-referrer`; `Cross-Origin-Opener-Policy: same-origin`; `Cross-Origin-Resource-Policy: same-origin`. The UI never renders HTML from data (Svelte text bindings only, no `{@html}`). |
| Secret leakage | NAS passwords are write-only in the API, sealed with XChaCha20-Poly1305/AES-GCM under the master key with AAD binding to the record; never logged; passed to the kernel via `fsconfig`, never via argv/env. API tokens and sessions stored hashed. |
| Privilege | Runtime is unprivileged (section 6.2). `CAP_SYS_ADMIN` only for opt-in in-process mounts. |
| Metrics / status endpoints | `/metrics` disabled by default and requires an API token when enabled. `/healthz` returns only `ok`. No unauthenticated status pages. |
| Dependency risk | Minimal dependency set (section 4); `govulncheck` in CI. |

### 6.2 Privilege model per deployment

- **Bare metal / LXC (systemd)**: runs as user `picache` with `AmbientCapabilities=CAP_NET_BIND_SERVICE`, `NoNewPrivileges=yes`, `ProtectSystem=strict` and the full hardening set in `deploy/systemd/picache.service`. In-process NAS mounts require the opt-in drop-in `50-managed-mounts.conf` (adds `CAP_SYS_ADMIN` and the mount syscalls). In unprivileged LXC in-process mounts are impossible (kernel requires init-userns `CAP_SYS_ADMIN`); the host mounts the share and bind-mounts it into the container.
- **Docker**: the image starts as root, binds all listeners, then **drops to `PICACHE_RUN_AS` (default `65532:65532`)** with `setgroups/setgid/setuid` before serving. Compose runs with `cap_drop: [ALL]`, `cap_add: [NET_BIND_SERVICE, SETUID, SETGID]`, `security_opt: [no-new-privileges:true]`, `read_only: true`, host networking. After the drop the process holds no capabilities.
- If started as root without `PICACHE_RUN_AS`, PiCache logs a prominent warning (allowed for the opt-in in-process mount case).

---

## 7. DNS

### 7.1 Request pipeline (exact order)

1. **Parse/validate**: exactly one question (else FORMERR); class must be IN (CHAOS → REFUSED); opcode QUERY (else NOTIMP).
2. **ACL** (section 6.1). Not allowed: UDP → drop silently, TCP → REFUSED.
3. **Rate limit** per client IP. Exceeded: UDP → drop, TCP → REFUSED. Not logged individually (counted).
4. **Hardening**: qtype ANY → NOTIMP (if `refuseAny`, default true).
5. **Identify client** (`clients.Registry.Identify`) → identity with enabled group IDs.
6. **Special-use names** (answered locally, never forwarded, exempt from blocking):
   - `localhost` and `*.localhost` → A 127.0.0.1 / AAAA ::1.
   - `test`, `invalid`, `onion`, `home.arpa`, `internal`, `local` and the configured local domain and their subdomains → NXDOMAIN unless a local record or conditional forwarder covers them.
   - `resolver.arpa` and subdomains → NODATA.
   - Server names (`serverNames`, default `picache` + `picache.<localDomain>`) → this server's address(es).
   - PTR/SOA/NS for RFC 6303 private reverse zones → local PTR upstreams if configured, else PTR for this server's own addresses, else NXDOMAIN.
7. **Local records** (A, AAAA, CNAME, TXT, plus auto-generated PTR for A/AAAA): exact name beats `*.` wildcard (wildcard = subdomains only, AdGuard semantics). CNAME targets are resolved through the full pipeline (minus filtering of the chain). Authoritative answer, TTL from record (default 300). Exempt from blocking.
8. **LanCache override** (`services.Registry.MatchDNS`), if LanCache is enabled, the client is not a passthrough client and the client identity has no `lanCacheBypass`:
   - A → the cache IPv4 address(es) (`lancache.cacheIpv4`, else auto-detected primary LAN IPv4), TTL `lancache.dnsTtl` (default 60), rotated round-robin.
   - AAAA → configured cache IPv6 address(es), else NOERROR/NODATA with SOA.
   - HTTPS (65) and SVCB (64) → NODATA (prevents `ipv4hint`/`ipv6hint` bypass).
   - Any other type → NODATA.
   - Logged with status `lancache`.
9. **Special domains** (skipped if the name is allowlisted for the client):
   - `use-application-dns.net` A/AAAA → NXDOMAIN (`filter.blockMozillaCanary`, default on).
   - `mask.icloud.com`, `mask-h2.icloud.com` → NXDOMAIN, all qtypes (`filter.blockIcloudPrivateRelay`, default on).
10. **Filtering** (`filter.Engine.Check`) unless blocking is disabled/paused globally. Blocked → blocking reply (7.3).
11. **Conditional forwarding**: the most specific matching forwarder domain wins (`example.lan` covers the apex and all subdomains; `*.example.lan` subdomains only). Forwarded via `upstream.ResolveVia`.
12. **Forward** via `upstream.Resolve` (cache, serve-stale, in-flight dedup, upstream mode).
13. **Response inspection** (if `filter.cnameInspection`, default on): every CNAME target in the answer is checked with the client's groups; if any hop is blocked (and no earlier hop was allowlisted), the whole answer becomes the blocking reply, status `blocked-cname`.
14. **Reply**: drop upstream OPT, add our own OPT (1232, DO echoed) only if the client sent EDNS; set TC and truncate for UDP when larger than min(client size, 1232) (512 without EDNS); `Msg.Truncate`. Attach EDE 15 "Blocked" with the list name for blocked replies when the client used EDNS.
15. **Log** (async, never blocks the reply): time, client IP, client name, qname, qtype, status, rcode, answer summary, reason (list/rule), upstream, duration µs, cached/stale flag, DNSSEC AD flag, protocol.

Query statuses (stable strings used in DB and API): `forwarded`, `cached`, `stale`, `local`, `special`, `lancache`, `blocked-list`, `blocked-rule`, `blocked-regex`, `blocked-cname`, `blocked-special`, `refused`, `error`.

### 7.2 Filtering semantics

**List formats** (covers ≥99.9 % of StevenBlack, OISD, HaGeZi, AdGuard DNS filter, 1Hosts):
- Preprocess: strip UTF-8 BOM, CRLF, trim. Reject a list whose first non-empty line starts with `<html` or `<!doctype` (case-insensitive) or contains control bytes other than tab/CR/LF. Max list size 256 MiB. Lowercase everything.
- Comments: lines starting with `!`, `#` (but not cosmetic `##`), `;`, `[`; strip inline ` #…`. Skip cosmetic rules (`##`, `#@#`, `#$#`, `#?#`, `#%#`).
- Hosts form `IP host [host…]` where the first token parses as an IP: every host is an **exact** block. Skip junk hosts: `localhost`, `localhost.localdomain`, `local`, `broadcasthost`, `ip6-localhost`, `ip6-loopback`, `ip6-localnet`, `ip6-mcastprefix`, `ip6-allnodes`, `ip6-allrouters`, `ip6-allhosts`, `0.0.0.0`.
- `||d^` → subtree block (d and all subdomains). `@@||d^` (also `@@||d^|`) → subtree allow. `|d^` → exact block.
- Plain `d` → exact block, or subtree if the list's `plainDomains` flag is `subtree` (OISD `domainswild2`, HaGeZi `*-onlydomains`, 1Hosts `domains.wildcards`).
- `*.d` → subtree block of d (apex included; HaGeZi/Blocky semantics).
- `/re/` → Go RE2 regex over the qname (without trailing dot), pattern length ≤ 1024.
- Other ABP shapes (`*` inside, `||x` without `^`, `.x^`, `x^`) → compiled to RE2: `||` → `^(?:[^.]+\.)*`, leading `|` → `^`, `^` or trailing `|` → `$`, `*` → `.*`, no anchor → substring.
- Modifiers: `$important` and `$badfilter` supported; any other `$modifier` → the rule is skipped and counted as unsupported.
- Domains must be valid A-labels (`[a-z0-9._-]`, labels 1–63, total ≤ 253); invalid lines are counted, not fatal.

**Matcher**: per compile, an immutable snapshot with (a) exact set and (b) subtree set, each a sorted `[]uint64` of 64-bit FNV-1a hashes of the domain with a parallel `[]uint32` of source indices (list or rule), plus (c) an ordered slice of compiled regexes. Lookup walks the qname and each parent suffix (full name → exact+subtree, parents → subtree only). Rule text is kept per source for "blocked by …" explanations (store only for user rules and regexes; for list entries report the list, and re-scan the list file on demand for Explain). Memory target: ≤ 16 bytes per list entry.

**Precedence** (first decisive wins):
1. `@@…$important` (allow)
2. `…$important` (block)
3. user exact allow
4. user subtree allow
5. user regex allow
6. list allow (`@@` entries and allow-type lists)
7. user exact deny
8. user subtree deny
9. list block (exact, subtree, hosts, plain, wildcard)
10. user regex deny, list regex/pattern block

**Groups** (Pi-hole semantics): lists and user rules belong to 0..n groups; clients belong to 1..n groups; a list/rule applies to a query iff it shares at least one **enabled** group with the client. Group 1 "Default" always exists and cannot be deleted; unknown clients and clients without groups are in Default. Entries with no group apply to nobody. New lists and rules default to group Default.

**Lists**: subscribed by URL (`http`, `https`; `file://` only under `<data>/lists/local/`), kind `block` or `allow`, `plainDomains` flag, enabled flag, groups, comment. Update every `filter.updateIntervalHours` (default 24; 0 = manual) with ±10 % jitter, conditional GET (ETag / If-Modified-Since), keep the last good copy in `<data>/lists/<id>.txt`; status `ok | unchanged | failed-cached | failed-empty`. Compile off the hot path and swap atomically. List downloads use the bypass resolver and never go through the proxy.

**Default list**: HaGeZi Multi NORMAL (ABP) `https://cdn.jsdelivr.net/gh/hagezi/dns-blocklists@latest/adblock/multi.txt`, enabled. A small embedded catalogue offers more (OISD small/big, StevenBlack, AdGuard DNS filter, 1Hosts Lite, HaGeZi Pro/Pro++/Ultimate, HaGeZi TIF, HaGeZi DoH/VPN bypass).

### 7.3 Blocking replies

| Mode | A | AAAA | Other |
|---|---|---|---|
| `null` (default) | `0.0.0.0` | `::` | NODATA |
| `nxdomain` | NXDOMAIN + SOA | NXDOMAIN + SOA | NXDOMAIN + SOA |
| `nodata` | NODATA + SOA | NODATA + SOA | NODATA + SOA |
| `refused` | REFUSED | REFUSED | REFUSED |
| `custom_ip` | `blockingIpv4` | `blockingIpv6` (or NODATA) | NODATA |

TTL `filter.blockedTtl` (default 10 s). Synthetic SOA: `picache.invalid. hostmaster.picache.invalid. 1 1800 900 604800 <blockedTtl>`.

**Pause**: global blocking pause with optional duration (UI: 30 s, 5 min, 1 h, until re-enabled), persisted in settings (`filter.enabled=false` or `filter.pausedUntil`), auto re-enabled.

### 7.4 Upstreams and cache

- Upstream syntax: `IP[:port]`, `udp://IP[:port]`, `tcp://IP[:port]`, `tls://host[:port]` (DoT, ALPN `dot`, SNI = host), `https://host[:port]/path` (DoH, RFC 8484 POST, ID 0, `application/dns-message`, HTTP/2). Hostnames in DoT/DoH are resolved via the **bootstrap** IP list only.
- Defaults: `https://dns.quad9.net/dns-query`, `https://cloudflare-dns.com/dns-query`; bootstrap `9.9.9.9`, `149.112.112.112`, `1.1.1.1`, `1.0.0.1`.
- Modes: `load_balance` (default; weighted random favouring low EWMA RTT and few recent failures; on error try the next), `parallel` (all at once, first valid wins), `strict` (in order, fail over on error/timeout). Per-attempt timeout 3 s, total 10 s.
- Plain UDP retries over TCP when TC is set. Every reply must match the question (name case-insensitive, type, class) or it is discarded.
- Upstream queries are built fresh: question copied, RD=1, our own OPT (1232, DO=1 if the client set DO or `dns.dnssec`), **no client EDNS options** (no ECS, no cookies) are forwarded.
- Response cache: key (lower qname, qtype, qclass, DO bit, upstream-set id); LRU with `dns.cacheSize` entries (default 10 000); TTL = min RR TTL clamped to `[cacheMinTtl, cacheMaxTtl]` (0 = no clamp); negative caching per RFC 2308 (SOA minimum, capped at 1 h); SERVFAIL cached 5 s. Serve-stale (default on): an entry expired less than `serveStaleMaxAgeSec` (default 3600) ago is answered with TTL 30 and refreshed in the background (at most one refresh per key).
- In-flight dedup: identical (key) upstream queries share one exchange.
- `LookupIP(ctx, host, wantV6)`: resolves via the default upstreams with caching, **never** consults local records, overrides or filtering; used by the proxy, SNI, list and cache-domains downloads.

---

## 8. LanCache

### 8.1 Services and domain lists

- Source: `uklans/cache-domains` (`https://raw.githubusercontent.com/uklans/cache-domains/master/` + `cache_domains.json` + every file in `domain_files`), fetched over verified HTTPS through the bypass resolver, refreshed every `lancache.updateIntervalHours` (default 24), snapshot kept in `<data>/cache-domains/`. Offline start uses the last snapshot; with no snapshot, LanCache stays inactive (DNS unaffected) and the UI says why.
- Schema: `{"cache_domains":[{"name","description","domain_files":[…],"notes"?,"mixed_content"?}]}`.
- Parsing of `.txt`: trim, lowercase, skip empty lines and lines starting with `#`, accept CRLF, strip trailing dot, validate hostname characters. `*.example.com` = any depth below example.com, **not** the apex. Plain entries are exact.
- Services are enabled by default except `test`. Users can disable services, add custom services (name + host list) and add hosts to existing services.
- `lancache.steamcontent.com` MUST be answered whenever `steam` is enabled (it is the only entry in `steam.txt`; Steam then sends all chunk requests to the cache with the real CDN host in `Host`).
- Matching is anchored and case-insensitive (exact map + suffix map walked label by label). Never replicate nginx's unanchored regex matching.

### 8.2 HTTP cache proxy (:80) — behaviour

Order of checks per request:
1. **Client ACL** → 403.
2. **Heartbeat**: `GET|HEAD|OPTIONS /lancache-heartbeat` for **any** Host (including IP literals) → `204` with `X-LanCache-Processed-By: <instanceID>`, `Access-Control-Allow-Origin: *`, `Access-Control-Expose-Headers: *` (+ `Access-Control-Allow-Private-Network: true` on OPTIONS). Checked before Host validation because prefill tools probe `http://<ip>/lancache-heartbeat`. Not logged as a download.
3. **Loop detection**: incoming `X-LanCache-Processed-By` containing our instance ID → 508.
4. **Host**: lowercase, strip port and trailing dot. IP-literal Host → 400 (logged as "IP-based request, not cacheable").
5. **Classify**: User-Agent ending in `Valve/Steam HTTP Client 1.0` → `steam`; else host → service via the matcher. Unknown host → 403. Known host of a **disabled** service → pass-through uncached.
6. **Special paths** (pass-through uncached; rule table in `services`): `/server-status`; `^.+(releaselisting_.*|.version$)`; prefix `/latest64`; `(?i)(authrootstl|pinrulesstl|disallowedcertstl)\.cab$`.
7. **Method**: `GET`/`HEAD` → cache path. Other methods → pass-through uncached (bodies limited, no Range injection).
8. **Cache key**: `service + "\x00" + normalizedPath`, where normalizedPath is the percent-decoded path with `//` merged and dot segments resolved, **without query**. Object ID = first 32 hex chars of SHA-256(key). The upstream request always carries the client's full path **and query**.
9. **Bypass**: `?nocache=<non-empty, not "0">` → skip cache reads, refetch every needed slice and overwrite.
10. **Slicing** with slice size S (store-wide, default 1 MiB; per-service `noSliceServices` → whole-object mode):
    - Slice `i` covers bytes `[i*S, min((i+1)*S, total))`. The upstream request for slice `i` uses `Range: bytes=i*S-(i*S+S-1)`.
    - A valid slice response is `206` with `Content-Range: bytes a-b/total` where `a == i*S`, `b+1 == min(a+S, total)` and `total` is known. Record total, `Content-Type`, `Last-Modified` and other end-to-end headers from the first slice.
    - **Upstream answers `200` (no range support)**: stream the body from offset 0, cut it into S-sized slices, store every **complete** slice, serve the client's range from the stream, stop reading one slice after the client's range end. Count per host; after 3 such responses mark the host `noSlice` (persisted, resettable in the UI) and request it without Range. Never store a full body under a single slice key.
    - A different `total` than recorded → invalidate the object (delete slices) and start over; if the client already received headers with the old length, abort the connection.
    - ETag differences are ignored (Battle.net CDNs differ per edge). ETag is never sent to clients.
11. **Serving**: the object is exposed as an `io.ReadSeeker` over its slices (cached slices from disk, missing slices filled on demand) and served with `http.ServeContent`, which gives correct `200`/`206`/multi-range `206 multipart/byteranges`/`416`/HEAD handling. The first needed slice is fetched before headers are written (to learn `total`). Response headers: stored `Content-Type` (default `application/octet-stream`), `Last-Modified`, `Accept-Ranges: bytes`, `X-LanCache-Processed-By: <instanceID>`, `X-Upstream-Cache-Status: HIT|MISS|PARTIAL|BYPASS`. Never `ETag`, never `Set-Cookie`.
12. **Mid-stream failure** of a later slice: retry the slice once; then fetch the remaining bytes of the current range **directly** from upstream (uncached `Range` request) and keep streaming; only if that fails abort the connection. (nginx aborts mid-body here, which causes Steam's "stuck at 99 %".)
13. **Status handling**: only 200/206 are ever stored. `301/302/307/308` are followed server-side (max 5 hops, Range kept, every hop re-checked by the SSRF guard, `https` hops with verified TLS); content is stored under the **original** key. `4xx/5xx` are passed through to the client unmodified and never stored (prefill tools rely on 403 passing through). On `404` from one IP, retry once on the next A record.
14. **Request collapsing**: one fill per (object, slice) at a time (singleflight). The fill writes into an in-memory buffer of size S; concurrent readers stream from the buffer as it grows (no wait for completion). Completed buffers are written to disk as temp file → close → rename, then indexed. Fill concurrency is bounded (`cache.maxConcurrentFills`, default 32) which bounds memory to `maxConcurrentFills × S`. A stalled fill (no bytes for 15 s) lets a waiting reader start a parallel direct fetch. Fills continue after the requesting client disconnects, but no *new* slices are started for a gone client.
15. **Read-ahead**: while slice i is served, start fills for up to `cache.readAheadSlices` (default 2) following uncached slices of the same range.
16. **Upstream transport**: HTTP/1.1 keep-alive pool (`http.Transport` with `MaxIdleConnsPerHost` 32, `IdleConnTimeout` 90 s, no proxy from env, `DisableCompression: true`), dialing through `netutil.SafeDialer` with `upstream.LookupIP` (IPv4 only unless configured), connect timeout 10 s, response-header timeout 15 s, idle read timeout 60 s. Forward the client's end-to-end headers except hop-by-hop headers, `Range`/`If-Range`/`If-*` (replaced for fills), `Cookie`, `Authorization`? (**kept**: some CDNs sign requests), and add `X-LanCache-Processed-By: <instanceID>`. No `X-Forwarded-For` (privacy).
17. **Accounting**: per slice, bytes served from cache vs. fetched from upstream (WAN bytes counted at fetch time). One cache event per client request (see 10.2).

### 8.3 SNI pass-through (:443)

Accept → client ACL → read the TLS ClientHello (timeout 5 s, max 16 KiB) and parse SNI without terminating TLS → SNI must match an **enabled** service domain (or the Steam trigger) → resolve via `LookupIP` → SSRF guard → dial `:443` (10 s) → replay the buffered ClientHello → bidirectional copy (kernel splice via `io.Copy` on `*net.TCPConn`) with idle timeout 5 min and max lifetime 24 h. No SNI or not allowed → close. Per-client concurrent connection cap (default 256). Log one event per connection: client, SNI, service, bytes up/down, duration. The UI flags services whose traffic is mostly pass-through (HTTPS bypass, e.g. Epic launcher ≥ 20.0.3).

### 8.4 Content grouping ("what was downloaded")

Grouping rules live in `services` and turn (service, host, path) into `groupKey`, `label`, `version`:

| Service | Rule | Group key | Label |
|---|---|---|---|
| steam | path `^/depot/(\d+)/(?:chunk/[0-9a-fA-F]{40}|manifest/(\d+)/\d+(?:/\d+)?|[^/]+/)` | `steam:depot:<depot>` | user label → cached app name (optional online lookup, off by default) → "Steam depot <depot>" |
| blizzard | path `^/(tpr|cortez)/([^/]+)/(config|data|patch)/` | `blizzard:<lower(cdnpath)>` (`configs` → shared) | built-in product map (wow, ovw, fenris, diablo3, hs, sc2, …) |
| epicgames | `^/Builds/Org/(o-[a-z0-9]+)/([0-9a-f]{32})/` or `^/Builds/(.+?)/CloudDir/` | `epic:<org>/<build>` or `epic:<app>` | user label → "Epic item <build[:8]>" / humanised app |
| riot | host `^([a-z0-9-]+)\.(?:secure\.)?dyn\.riotcdn\.net$` | `riot:<prod>` | lol → League of Legends, valorant → VALORANT, ks-foundation → Riot Client |
| xboxlive / wsus | package file name `(.+)_(\d+(?:\.\d+){3})_(x64|x86|arm64|neutral)__([a-z0-9]{13})\.(msixvc|xvc|appx|appxbundle|msix|msixbundle|eappx|eappxbundle)$`; KB `(?i)(?:^|[-_/])kb(\d{6,8})(?:[-_.]|$)`; DO `^/filestreamingservice/files/<guid>`; Office `^/pr/<guid>/Office/Data/<ver>/` | `xbox:<pkg>`, `win:kb<n>`, `win:do`, `win:office:<channel>` | humanised |
| sony | `/((?:CUSA|PPSA)\d{5})_00/` | `psn:<title>` | title id |
| nintendo | `^/c/([csa])/([0-9a-f]{32})` | `nintendo:switch` | "Nintendo eShop content" |
| uplay / origin | `^/uplaypc/downloads/([^/]+)/`, `^/eamaster/s/shift/([^/]+)/` | `ubi:<g>`, `ea:<g>` | humanised |
| wargaming | host `^dl-(wot|wows|wowp)-` | `wg:<g>` | World of Tanks/Warships/Warplanes |
| fallback | — | `<service>:<host>` | "<Service> · <host>" |

Query strings are never stored or displayed (they can contain CDN auth tokens).

---

## 9. Cache store

### 9.1 On-disk format (per store root)

```
<root>/.picache-store          JSON {"storeId","format":1,"sliceSize","createdAt"}; written on first attach
<root>/tmp/                    temp files (same filesystem → atomic rename)
<root>/slices/<h0h1>/<h2h3>/<objectId>.<sliceIndex>
```
Each slice file = fixed header + data: magic `PCS1` (4 bytes), header length (uint32 LE), JSON header `{"o":objectId,"i":index,"s":service,"h":host,"p":path,"t":total,"z":sliceSize,"c":createdAtMs,"m":{"Content-Type":…,"Last-Modified":…}}`, then the slice bytes. The header makes the store **self-describing**: the index can be rebuilt by scanning (important for NAS stores that survive a reinstall). Reads use `ReadAt(headerLen+offset)`. A slice whose size on disk does not match the index is treated as missing and deleted.

All file access goes through `os.Root` (Go ≥ 1.25) so paths can never escape the store root (hostile NAS content, symlinks). Directories are created lazily. No fsync per slice; write temp → close (surfaces NFS/SMB write errors) → rename → index commit.

### 9.2 Index (`<data>/cache-index/<storeId>.db`, local disk)

- `objects(id TEXT PRIMARY KEY, service, host, path, group_key, total_size, slice_size, headers JSON, created_at, last_access, hits, bytes_served, cached_bytes, slice_count, pinned, no_slice)`.
- `slices(object_id, idx, size, created_at, PRIMARY KEY(object_id, idx)) WITHOUT ROWID`.
- Hot-path lookups are served from an in-memory map of object metadata + slice presence bitmaps, loaded at open (fast, no full disk scan). `last_access`, `hits`, `bytes_served` are updated in memory and flushed every 30 s.

### 9.3 Retention and eviction (background, every 60 s and on low space)

1. **Inactive expiry**: objects not accessed for `cache.maxAgeDays` (default 365; per-service overrides `cache.serviceMaxAgeDays`) are deleted unless pinned. `expiresAt = lastAccess + maxAge` is shown in the UI.
2. **Size**: if `cachedBytes > cache.maxSizeBytes` (0 = no limit) or statfs free space < `cache.minFreeBytes` (default 10 GiB), delete least-recently-used unpinned objects until 95 % of the limit / min free + 5 %.
3. Every eviction is recorded (time, object, group, service, bytes, reason `inactive|size|min-free|manual|corrupt`).
UI shows per object/group: first cached, last access, expires at, LRU eviction risk (percentile), pinned.

### 9.4 Verify / rebuild

`Verify(repair)` walks the slice tree via `os.Root`, reads headers, reconciles with the index (adds missing objects/slices, removes index entries without files, deletes corrupt files), reporting progress. Runs in the background; the store stays usable.

---

## 10. Storage targets (NAS)

### 10.1 Model

`Target{id, name, kind: local|smb|nfs, mode: external|in-process|host-apply, path, server(IP literal), share, export, subdir, username, domain, password(sealed, write-only), smbVersion(3.1.1), nfsVersion(4.2), nconnect, seal, requireMountpoint}`. Built-in target `local` = `PICACHE_CACHE_DIR`. Exactly one target is active for the cache (`cache.activeStoreId`). Switching targets switches cache stores (each target keeps its own index).

### 10.2 Modes

1. **external** (default, always available): something else mounts the share (host fstab/systemd, Docker bind with `rslave`, Proxmox `mpX`). PiCache only uses `path`. The UI generates snippets (fstab line + credentials file, systemd `.mount` unit, docker-compose bind, Proxmox host fstab + `pct set <ct> -mp0 …` with the UID offset computed from `/proc/self/uid_map`).
2. **host-apply** (bare metal / VM / privileged LXC with systemd): `sudo picache storage apply <id>` (same binary, run as root) writes `/etc/picache/credentials/<id>.cred` (0600), a `.mount` unit for `/srv/picache/<id>` with `nofail` semantics, runs `systemctl daemon-reload` and `systemctl enable --now`. Strict input validation (IP literal, share/export charset allowlist, target under `/srv/picache/`). The service itself stays without `CAP_SYS_ADMIN`.
3. **in-process** (opt-in: `PICACHE_ENABLE_MOUNTS=true` and `CAP_SYS_ADMIN` in the init user namespace): mount with `fsopen/fsconfig/fsmount/move_mount` (no option-string escaping; password via `fsconfig`, never argv/env), fallback to `mount(2)` with doubled commas. Flags `nosuid,nodev,noexec,noatime`. CIFS: `ip=<literal>`, `vers=3.1.1`, `uid/gid`, `file_mode=0640`, `dir_mode=0750` (leading zero is mandatory: the kernel parses base 0), `soft`, optional `seal`. NFS: `addr=<literal>` (mandatory), `vers=4.2`, `proto=tcp`, `softerr`, `timeo=100`, `retrans=2`, `nconnect` 1–16. Unmount `MNT_FORCE` then `MNT_DETACH|UMOUNT_NOFOLLOW`. fs_context error messages are read from the fsopen fd and shown in the UI.

### 10.3 Mount guard (all modes, before and during use)

A store is **online** only if: the path exists; if kind is smb/nfs (or `requireMountpoint`), it is a mountpoint (`/proc/self/mountinfo` or `st_dev` differs from parent); statfs type matches the kind (CIFS `0xff534d42`/SMB2 `0xfe534d42`, NFS `0x6969`; compare as `uint32`); `.picache-store` exists with the expected store ID (written on first attach after confirmation); a write/rename/delete test in `tmp/` succeeds; free space ≥ `minFreeBytes`. Re-checked every 30 s (statfs with timeout, single-flight). When offline: the proxy runs in **pass-through** mode (serves uncached) and the UI raises an alert. It never writes into an unmounted underlying directory. DNS never depends on storage.

NAS I/O runs behind a semaphore (default 64 concurrent ops) so a hung server cannot exhaust OS threads.

### 10.4 Capability detection (shown in the UI)

Container type (`/.dockerenv`, `/run/.containerenv`, `/run/systemd/container`, `/proc/1/environ`), init user namespace (`/proc/self/uid_map` = `0 0 4294967295`) and UID offset, effective capabilities (`/proc/self/status` CapEff bit 21 SYS_ADMIN, bit 10 NET_BIND_SERVICE), NoNewPrivs, AppArmor profile, kernel filesystems (`/proc/filesystems`), presence of `mount.cifs`/`mount.nfs` (host-apply), systemd as PID 1.

---

## 11. Logs, statistics, sessions (`logs.db`)

- Ingestion through bounded channels; batch insert every 1 s or 1000 rows in one transaction; if a channel is full the event is dropped and counted (DNS latency never depends on logging).
- **Query log** `queries(id, ts, client_ip, client_name, qname, qtype, status, rcode, reason, list_id, rule_id, upstream, duration_us, answer, flags)`; indexes on `ts`, `(qname, ts)`, `(client_ip, ts)`. Retention `logs.queryLogRetentionHours` (default 168). Optional client-IP anonymisation (IPv4 /16, IPv6 /48). Query log can be disabled; per-client "ignore".
- **Cache events** `cache_requests(id, ts, client_ip, service, host, path (no query), method, status, cache_status, range, bytes_sent, bytes_hit, bytes_wan, duration_ms, group_key, user_agent)`; retention `logs.cacheLogRetentionHours` (default 168).
- **Download sessions** `downloads(id, client_ip, service, group_key, label, first_ts, last_ts, requests, bytes_sent, bytes_hit, bytes_wan)`: key (client, service, groupKey), a new session starts after a 120 s gap. Retention `logs.sessionRetentionDays` (default 90). This powers "who downloaded what".
- **Pass-through events** `sni_events(ts, client_ip, sni, service, bytes_up, bytes_down, duration_ms)`.
- **Rollups** per minute (kept 48 h) and per hour (kept `logs.statsRetentionDays`, default 365): DNS counts by status; cache bytes hit/wan/sent and requests by service; SNI bytes by service. Dashboard queries read rollups, never full scans. Time ranges default to 24 h; the query log defaults to the last hour and paginates by cursor `(ts,id)`.
- **Evictions** `evictions(ts, store_id, object_id, service, group_key, bytes, reason)`.
- Live feeds: in-process fan-out to subscribers (SSE in the API), dropping events for slow subscribers.

---

## 12. Web API and UI

- REST under `/api/v1`, JSON only, documented in `docs/API.md`. SSE streams under `/api/v1/stream/*`.
- Auth: cookie session (UI) or `Authorization: Bearer <token>` (API tokens with scope `read` or `admin`). Permissions per route: `public` (health, auth status, login, setup), `read`, `admin`.
- Middleware order: recover → request ID → security headers → host allowlist → (HTTPS redirect if enabled) → CrossOriginProtection → body limit (1 MiB) → auth → handler.
- `GET /healthz` → `200 ok` (no auth, no details). `GET /metrics` → Prometheus text, admin token, disabled by default.
- Audit log for every state-changing admin action (who, when, what, target, source IP).
- Static UI served from the embedded `dist` with long-lived caching for hashed assets and `no-cache` for `index.html`.

### 12.1 Frontend

Svelte 5 (runes) + Vite 8 + TypeScript 6, plain SPA with a hash router, uPlot for charts, no other runtime dependencies. Built into `internal/webui/dist` (`//go:embed all:dist`). No external requests (fonts, icons, images all bundled). English and German (auto-detected, switchable).

Information architecture: **Overview** · **DNS** (Query log, Filtering: lists & rules, Clients & groups, Local DNS, DNS settings) · **Cache** (Downloads, Library, Services, Storage, Cache settings) · **System** (Account & security, API tokens, Audit log, Backup, About & health). Global top bar: DNS and blocking status with pause control, cache store status, quick search (domain/client).

Design system: see `docs/DESIGN.md`.

---

## 13. Defaults (single source of truth: `internal/settings/defaults.go`)

| Setting | Default |
|---|---|
| DNS upstreams | Quad9 DoH, Cloudflare DoH |
| Upstream mode | `load_balance` |
| DNS cache | on, 10 000 entries, serve-stale 1 h |
| Rate limit | 50 qps, burst 200 per client |
| Blocking | on, mode `null`, blocked TTL 10 s, CNAME inspection on |
| Lists | HaGeZi Multi NORMAL, update every 24 h |
| Special domains | Mozilla canary blocked, iCloud Private Relay blocked |
| LanCache | on, all services except `test`, DNS TTL 60 s, SNI pass-through on |
| Slice size | 1 MiB |
| Retention | inactive 365 days, min free 10 GiB, no max size |
| Fill concurrency / read-ahead | 32 / 2 slices |
| Query log | on, 7 days; cache log 7 days; sessions 90 days; stats 365 days |
| Web sessions | idle 60 min, absolute 7 days |
