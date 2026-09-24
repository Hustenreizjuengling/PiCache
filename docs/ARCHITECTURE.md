# PiCache architecture

PiCache is a single Go binary with an embedded web UI. It combines:

- a **filtering DNS server**, the central DNS for a home or lab network, with parity to the Pi-hole and AdGuard Home features that matter;
- a **LanCache-compatible download cache**: DNS overrides, an HTTP slice cache on :80, and SNI pass-through on :443;
- one **web UI and REST API** to operate both, including NAS-backed cache storage.

This document is the binding specification for the implementation. Where it says MUST, the code must do exactly that. The numbers and behaviours below were verified against upstream source (lancachenet/monolithic, uklans/cache-domains, pi-hole/FTL, AdGuardHome, Linux kernel, systemd, moby) in September 2026 and then hardened by an adversarial design review.

Priorities, in this order: **security, simplicity, correctness, performance, features.**

---

## 1. Goals and non-goals

**Goals**
- One process, one config database, one UI. No nginx, BIND, dnsmasq or cron underneath.
- Deployable on Debian 12/13 bare metal, Proxmox LXC (unprivileged) and Docker, with clearly identified persistent data.
- Secure defaults: no open resolver, no open proxy, no open TLS relay, unprivileged runtime, authenticated UI, strict CSP.
- LanCache-compatible behaviour (Steam, Epic, Battle.net, Riot, Xbox/WSUS, PlayStation, Nintendo, …) including the lancache heartbeat and prefill-tool compatibility.
- First-class observability: who requested what, what is cached, how long it stays, how much bandwidth was saved.
- Runs well on a Raspberry Pi 4 or a 1–2 GB LXC.

**Non-goals (for now)**: DHCP server, DoH/DoT/DoQ *serving* to clients, local DNSSEC validation, blocked-services catalogue, safe-search enforcement, TLS interception of HTTPS downloads (never), in-process NAS mounts (the service never holds `CAP_SYS_ADMIN`), per-service retention, online game-name lookups, nginx cache-disk import. The data model must not prevent adding these later.

---

## 2. Runtime overview

```
                    ┌──────────────────────── picache (one process) ─────────────────────────┐
 clients ──:53/udp,tcp──▶ dnsserver ──▶ filter ──▶ upstream (DoH/DoT/UDP, cache) ──▶ Internet DNS
                    │        │  ▲ clients (identity, groups)      └─▶ router resolver (LAN names)│
                    │        └─ lancache overrides (services) → answers with cache IP         │
 clients ──:80/http─────▶ proxy ──▶ cachestore (slices on local disk / NAS) ──▶ CDN over HTTP │
 clients ──:443/tls─────▶ sni (pass-through, allowlisted SNI only) ────────────▶ CDN :443     │
 admin ──:8080/:8443────▶ api + web UI (auth, CSRF, CSP) ──▶ all components                   │
                    │  logs.db (query log, cache log, stats) · picache.db (config) · index.db │
                    └──────────────────────────────────────────────────────────────────────────┘
      root helper (optional, systemd path unit): `picache storage apply-pending` → .mount units
```

The proxy and SNI server resolve CDN hostnames through `upstream.Resolver.LookupIP`, which **bypasses** local overrides, filtering and local records. This avoids resolution loops.

### Listeners (bootstrap config, restart required to change)

| Env | Default | Purpose | Bind failure |
|---|---|---|---|
| `PICACHE_DNS_LISTEN` | `:53` | DNS over UDP and TCP (comma-separated list) | fatal |
| `PICACHE_CACHE_LISTEN` | `:80` | LanCache HTTP cache | logged, health warning, LanCache overrides disabled |
| `PICACHE_SNI_LISTEN` | `:443` | SNI pass-through (`off` disables) | logged, health warning |
| `PICACHE_WEB_LISTEN` | `:8080` | Web UI + API over HTTP | fatal only if no web listener at all |
| `PICACHE_WEB_TLS_LISTEN` | `:8443` | Web UI + API over HTTPS (self-signed unless a cert is given; `off` disables) | as above |

All listeners are bound **before** privileges are dropped (section 6.2). TCP listeners for DNS, :80 and :443 are wrapped with `netutil.LimitListener`: connections from clients outside the ACL are closed at accept; concurrent connections are capped per client (/32, /64) and in total (DNS 32/1024, cache and SNI 256/4096).

---

## 3. Persistent data

Everything persistent lives in exactly two places plus optional NAS mounts.

| Path (bare metal / LXC) | Docker | Contents | Backup? |
|---|---|---|---|
| `/var/lib/picache` (`PICACHE_DATA_DIR`) | `/data` | `picache.db` (configuration: settings, users, lists, rules, clients, groups, local records, services, storage targets with sealed NAS passwords, audit log); `logs.db` (query log, cache events, sessions, statistics, evictions, seen clients); `cache-index/<store-id>.db`; `lists/`; `cache-domains/`; `tls/`; `keys/master.key` (0600); `instance-id`; `setup-token` (until setup is done); `backups/` (automatic pre-upgrade copies, newest 3); `storage-requests/` (mount requests for the root helper); `picache.db.before-restore` (after a restore) | `picache.db` (UI download or file copy while stopped); everything else is rebuildable. `keys/master.key` separately if stored NAS passwords should survive a move to another machine. |
| `/var/cache/picache` (`PICACHE_CACHE_DIR`) | `/cache` | The built-in **local** cache store (slice files). Large. | No |
| `/srv/picache/<id>` (`PICACHE_MOUNT_ROOT`) | `/srv/picache` (bind, `rslave`) | NAS cache stores. The only place outside the cache dir where stores may live (the only NAS path writable inside the sandbox). | No |
| `/etc/picache/picache.env` | environment | Bootstrap settings only. Read by systemd **and by every CLI command**. | Yes |
| `/etc/picache/credentials/<id>.cred` | – | NAS credentials written by the root helper (0600, root). | – |

Rules:
- SQLite databases MUST live on local disk. PiCache refuses to open a DB on NFS/CIFS.
- Only slice files may live on a NAS.
- A broken `logs.db` is moved aside (`logs.db.broken-<ts>`) and recreated; if that fails, logging is disabled but DNS keeps running. A broken cache index is moved aside and rebuilt from the self-describing slice headers.
- UI backups never contain sessions; sealed NAS passwords only on explicit opt-in. After a restore, all sessions and API tokens are revoked.

---

## 4. Package layout and dependency rules

```
cmd/picache/                 CLI: serve (default), version, healthcheck, reset-password, setup-token, storage apply|apply-pending
internal/version/            build info (ldflags)
internal/config/             bootstrap config from env/flags, derived paths
internal/db/                 SQLite (modernc) writer/reader pools, per-component migrations, read-only open, schema registry
internal/apperr/             typed user-facing errors (NotFound, Invalid, Conflict, …)
internal/listing/            generic page type
internal/settings/           typed runtime settings (one JSON document in picache.db), validation, pub/sub
internal/secrets/            master key + AEAD seal/open for stored secrets
internal/netutil/            ACL, IP classification, SSRF-safe dialer, rate limiter, LimitListener, host normalisation, gateway/resolv.conf detection
internal/auth/               users, argon2id, sessions, API tokens, TOTP, login throttling, setup token, audit log
internal/clients/            clients, groups, identity resolution (IP/CIDR/MAC), ARP/neighbour table, client names
internal/dns/upstream/       upstream transports (UDP/TCP/DoT/DoH), modes, response cache, serve-stale, bypass LookupIP, clock guard
internal/dns/filter/         blocklists (fetch, parse, compile), custom rules, matcher, explain
internal/dns/server/         DNS listeners + request pipeline, local records, conditional forwarders, router resolver, pause
internal/lancache/services/  cache-domains source, service registry & matcher, custom services, content grouping, labels
internal/lancache/store/     slice store + index DB + eviction + verify/rebuild + store marker   (package cachestore)
internal/lancache/proxy/     HTTP cache proxy (:80)
internal/lancache/sni/       TLS SNI pass-through (:443)
internal/storage/            storage targets, capability detection, mount guard, store init/adopt, host-apply root helper, snippets
internal/logs/               logs.db: query log, cache events, sessions, rollups, evictions, live subscriptions
internal/api/                REST API + SSE, middleware, one routes_<domain>.go file per domain
internal/webui/              go:embed of the built frontend (internal/webui/dist)
internal/app/                wiring, lifecycle, privilege drop, store switching, health, backup/restore
web/                         Svelte 5 + Vite SPA (builds into internal/webui/dist)
deploy/                      docker/, systemd/, lxc/, install.sh
docs/                        this file, API.md, DESIGN.md, DEPLOYMENT.md, SECURITY.md
```

Dependency rules:
- Foundation packages (`version`, `config`, `db`, `apperr`, `listing`, `settings`, `secrets`, `netutil`) import only each other (`settings` → `db`, `apperr`; `netutil` → `settings`).
- Domain packages import foundation packages and each other only along these edges: `dnsserver` → {`upstream`, `filter`, `clients`, `logs`} (types only; collaborators are consumer-side interfaces); `proxy` → {`cachestore`, `clients`, `logs`, `services` (pure functions GroupFor/IsBypassPath/constants only)}; `sni` → {`clients`, `logs`}; `storage` → {`cachestore`} (store marker only). `filter`, `services`, `upstream`, `clients`, `logs`, `auth`, `cachestore` import no other domain package.
- `dnsserver`, `proxy` and `sni` declare **consumer-side interfaces** for their collaborators (see their `Deps`) so they can be tested with fakes.
- `api` imports domain packages; domain packages never import `api`. `app` imports everything and is imported only by `cmd`.

Third-party dependencies are limited to: `github.com/miekg/dns v1.1.73`, `modernc.org/sqlite v1.59.0` (with `modernc.org/libc v1.75.7` pinned exactly; never bump libc alone), `golang.org/x/{crypto,sys,sync,net,time}` (pinned in `internal/deps`). Adding any other module needs an explicit decision.

---

## 5. Conventions

- Go 1.27, `CGO_ENABLED=0`, static binary. Release targets: linux/amd64, linux/arm64, linux/arm (v7).
- Logging: `log/slog`, attribute `component`. Never log secrets, passwords, tokens or CDN query strings. Repeated errors are logged when they change or at most hourly.
- JSON: `encoding/json/v2`; lowerCamelCase field names; API decoding rejects unknown members.
- Time: stored as INTEGER unix **milliseconds** (UTC). API entities use RFC 3339; time series use unix **seconds**.
- IDs: SQLite `INTEGER PRIMARY KEY` for rows; service IDs are the cache-domains names; storage target and store IDs are 32 hex characters (the built-in target is `local`).
- **Table names are prefixed with their component** (`dns_records`, `filter_lists`, `client_groups`, `auth_sessions`, `storage_targets`, `logs_queries`, `proxy_noslice_hosts`, …). Each component migrates only its own tables via `db.Migrate(ctx, "<component>", steps)`; steps are append-only.
- Errors: domain packages return `apperr` errors for user-facing conditions; everything else is a 500 with a generic message.
- Lifecycle: every `Start(ctx)` **blocks** until ctx is done and all goroutines it started have exited; after it returns the component no longer touches its DB. The app runs each Start in a tracked goroutine and waits for them on shutdown before closing databases.
- Concurrency: hot-path structures are immutable snapshots behind `atomic.Pointer`. 64-bit counters use `atomic.Int64`/`atomic.Uint64` (never `atomic.AddInt64` on plain fields: 32-bit ARM alignment).
- OS-specific code lives in `*_linux.go` with portable `*_other.go` fallbacks so `go build ./...` and `go test ./...` work on Windows/macOS.
- Tests: table-driven unit tests per package, `testing/synctest` for time, `httptest` for HTTP; no network access in unit tests.
- SQL: parameterised statements only; sort columns from an allowlist.
- Bounds everywhere: every cache, map, queue, list and upload has an explicit size limit.

---

## 6. Security model

### 6.1 Threats and controls

| Threat | Control |
|---|---|
| Open DNS resolver / amplification | ACL: loopback, RFC 1918, ULA, link-local, CGNAT and **private** directly connected subnets plus user CIDRs (each at least /8 IPv4, /32 IPv6; `allowAllNetworks` is the explicit dangerous switch). UDP from others is dropped; TCP is closed at accept. `ANY` → NOTIMP; CHAOS class and `version.bind`/`id.server`/`hostname.bind` → REFUSED. Rate limit per client key (/32 IPv4, /64 IPv6), default 50 qps burst 200; loopback, the router resolver, local PTR upstreams and forwarder targets are exempt automatically; the limiter fails open when its table is full. EDNS capped at 1232. |
| Cache poisoning (DNS) | Random IDs and ports, question verified on every upstream reply, in-flight dedup, DoH/DoT upstreams by default, no client EDNS options forwarded. |
| Private reverse-DNS leak | PTR/SOA/NS for RFC 6303 zones and RFC 7793 (100.64/10) never reach public upstreams (7.1 step 6). |
| Open HTTP proxy / SSRF via :80 | Only hosts of known LanCache services are served; the Steam User-Agent only classifies Steam-shaped paths (`/depot/<n>/…`, `/server-status`) with GET/HEAD. Unknown → 403. Upstream addresses must be public unicast and not this machine; link-local (incl. cloud metadata) is always refused; redirect hops are re-checked. Non-canonical paths are never stored (cache key == fetched resource). Per-client fill caps and at most 16 ranges per request prevent WAN amplification; `?nocache` is honoured only from `lancache.nocacheClients`. |
| Open TLS relay via :443 | SNI must match an enabled service; no SNI → close; SSRF rules; ACL at accept; connection caps; idle timeout 5 min, lifetime 24 h. |
| Hostile cache-domains source or NAS content | File names, sizes, counts, service IDs and host patterns validated (8.1); snapshots written through `os.Root`; public-suffix patterns rejected. Slice headers validated and CRC-checked; everything accessed through `os.Root`. |
| Web UI takeover on first start | One-time setup token (log + `<data>/setup-token`, 0600, constant-time compare, atomic first-user creation) or provisioning via `PICACHE_ADMIN_PASSWORD_FILE`. |
| Password guessing | argon2id (m=19456 KiB, t=2, p=1), throttling per client key and per user (5 failures → 15 min), TOTP failures count, global 10 attempts/s, max 2 concurrent hashes; optional TOTP with single-use steps. |
| Session theft / CSRF | 256-bit tokens stored hashed; cookie HttpOnly, SameSite=Strict, Secure on HTTPS; idle 60 min, absolute 7 days; sessions re-read from the DB on every request; `CrossOriginProtection`; JSON-only bodies. |
| Token misuse | API tokens (read/admin) can never manage tokens, passwords, TOTP or sessions (`permSession`). SSE streams re-validate the principal every 15 s and end after 1 h. |
| DNS rebinding against the UI | Host allowlist (IP literals, localhost, hostname + local/search domains, server names, configured hosts) → 421. |
| XSS / clickjacking | Strict CSP (`default-src 'none'; script-src 'self'; …`), `X-Frame-Options: DENY`, `nosniff`, `no-referrer`, COOP/CORP. The UI never renders HTML from data. HSTS only when the HTTPS redirect is enabled; redirects are 307. |
| Secret leakage | NAS passwords sealed (XChaCha20-Poly1305, AAD per record), write-only in the API, never in snippets, logs or audit details (redacted by name), decrypted only by the root helper. |
| Privilege | Runtime is unprivileged and never holds `CAP_SYS_ADMIN` (6.2). |
| Metrics / status | `/metrics` disabled by default, admin token required. `/healthz` returns only `ok`. |
| Resource exhaustion | Bounded caches, queues, subscribers (16 SSE), fill memory (≤ 1 GiB), query timeouts (10 s), log DB size cap, audit retention, connection caps. |

### 6.2 Privilege model per deployment

- **Bare metal / LXC (systemd)**: user `picache`, `AmbientCapabilities=CAP_NET_BIND_SERVICE`, `NoNewPrivileges=yes`, `ProtectSystem=strict`, full hardening (`deploy/systemd/picache.service`). NAS mounts are done by the optional **root helper**: `picache-storage.path` watches `/var/lib/picache/storage-requests/` and starts `picache-storage.service` (`picache storage apply-pending`, root, oneshot), which re-validates every request (`storage.ValidateTarget`), writes `/etc/picache/credentials/<id>.cred` and a `.mount` unit for `/srv/picache/<id>` and starts it. The main service only writes request files. In unprivileged LXC the helper cannot mount CIFS/NFS (kernel rule); the Proxmox host mounts the share and bind-mounts it (snippets in the UI).
- **Docker**: the image starts as root, binds all listeners, then **drops to `PICACHE_RUN_AS` (default `65532:65532`) before touching any file** (`setgroups/setgid/setuid`, verified; regaining root is checked to fail). PiCache never chowns: named volumes inherit ownership from the image (`/data`, `/cache` owned by 65532); bind-mounted directories must be chowned on the host (clear error otherwise). Compose: `cap_drop: [ALL]`, `cap_add: [NET_BIND_SERVICE, SETUID, SETGID]`, `security_opt: [no-new-privileges:true]`, `read_only: true`, host networking. NAS: host fstab + bind with `rslave` (snippets in the UI).

---

## 7. DNS

### 7.1 Request pipeline (exact order)

1. **Parse/validate**: exactly one question (else FORMERR); class IN (CHAOS → REFUSED); opcode QUERY (else NOTIMP).
2. **ACL**: not allowed → UDP drop (TCP already refused at accept).
3. **Rate limit** per client key. Exceeded → UDP drop, TCP REFUSED. First drop per client per hour logged at WARN with a hint; top limited clients are exposed in stats and health.
4. **Hardening**: qtype ANY → NOTIMP (if `refuseAny`).
5. **Identify client** (`Clients.Identify`) → identity with enabled group IDs; `Clients.Seen`.
6. **Special-use names** (answered locally, never forwarded to the default upstreams, exempt from blocking):
   - `localhost` and `*.localhost` → A 127.0.0.1 / AAAA ::1.
   - Server names (`serverNames` + `.<localDomain>`) → this server's addresses (A/AAAA of the interface addresses).
   - `resolver.arpa` and subdomains → NODATA.
   - **Locally served reverse zones** (RFC 6303 §4 incl. ULA/link-local `ip6.arpa`, plus RFC 7793 `64–127.100.in-addr.arpa`) for PTR/SOA/NS, in this order: (1) auto-PTR of enabled local A/AAAA records; (2) PTR for this server's own addresses (`serverNames[0]`); (3) the most specific enabled conditional forwarder; (4) `localPtrUpstreams`; (5) the **router resolver**; (6) NXDOMAIN with the synthetic SOA.
   - `test`, `invalid`, `onion`, `home.arpa`, `internal`, `local`, the local domain and resolv.conf search domains: local records and forwarders first, then (for the local domain, `home.arpa` and search domains) the router resolver; otherwise NXDOMAIN.
   - **Router resolver** (`dns.routerResolver`): `auto` = the IPv4 default gateway (`/proc/net/route`, re-read every 5 min) if it answers a DNS probe; an explicit IP; or off. Loop guard: a query from the router's own address for a name PiCache would forward back to it gets SERVFAIL.
7. **Local records** (A, AAAA, CNAME, TXT; auto-PTR): exact name beats `*.` wildcard (subdomains only). If any enabled record matches the name: a CNAME record → answer the CNAME plus the resolved target (max 8 hops, visited set) for every qtype (qtype CNAME → the CNAME only); otherwise, if no record has the requested type → authoritative NOERROR/NODATA with the synthetic SOA. Never forwarded. A CNAME may not share a name with other records. Exempt from blocking.
8. **User block rules before overrides**: if a *user* block rule (`Filter.CheckRules`) applies to the client for the qname and blocking is active, answer the blocking reply (status `blocked-rule`) — this lets a group block Steam even though the name is a LanCache override. List entries never block LanCache names.
9. **LanCache override** (`Services.MatchDNS`) if `lancache.enabled`, `LanCacheReady()` is true (cache listener bound), a valid cache IPv4 is known, and the identity has no `lanCacheBypass`:
   - A → cache IPv4 address(es), TTL `lancache.dnsTtl` (default 60), rotated. Configured addresses must be RFC 1918; auto-detection uses `PrimaryIPv4` only if it is RFC 1918 (else the first RFC 1918 local address; in Docker bridge mode no auto address — the admin must configure the host's LAN IP). Recomputed every 5 min.
   - AAAA → configured ULA address(es), else NOERROR/NODATA with the synthetic SOA (minimum = dnsTtl).
   - HTTPS (65), SVCB (64) and every other type → NODATA.
   - Otherwise (not ready) the query continues normally and the UI/health explain why.
10. **Special domains** (skipped if the name is allowlisted for the client or blocking is disabled/paused): `use-application-dns.net` A/AAAA → NXDOMAIN; `mask.icloud.com`, `mask-h2.icloud.com` → NXDOMAIN (all qtypes).
11. **Filtering** (`Filter.Check`) if blocking is active. Blocked → blocking reply (7.3).
12. **Conditional forwarding**: the most specific matching forwarder (`example.lan` = apex + subdomains, `*.example.lan` = subdomains only) via `ResolveVia`.
13. **Forward** via `Resolve`.
14. **Response inspection** (if `cnameInspection`): skipped when the qname's decision is Allow or blocking is paused. Otherwise every CNAME target in the answer is checked with the client's groups; if any hop is blocked, the whole answer becomes the blocking reply (status `blocked-cname`).
15. **Reply shaping**: drop upstream OPT; add our OPT (1232, DO echoed) only if the client sent EDNS; if the client's DO bit is 0 remove RRSIG/NSEC/NSEC3 from Answer and Ns (unless queried); set AD only if the client set AD or DO; truncate for UDP (min(client size, 1232), 512 without EDNS). EDE 15 "Blocked" with the list/rule name on blocked replies to EDNS clients.
16. **Log** (async, skipped for identities with `ignoreLogs`): time, client, name, qname, qtype, status, rcode, answer summary, reason, upstream, duration, cached/stale, AD, protocol.

Query statuses: `forwarded`, `cached`, `stale`, `local`, `special`, `lancache`, `blocked-list`, `blocked-rule`, `blocked-regex`, `blocked-cname`, `blocked-special`, `refused`, `error`.

### 7.2 Filtering semantics

**List formats** (≥ 99.9 % of StevenBlack, OISD, HaGeZi, AdGuard DNS filter, 1Hosts):
- Preprocess: strip UTF-8 BOM, CRLF, trim. Reject a list whose first non-empty line starts with `<html`/`<!doctype`, or that contains control bytes other than tab/CR/LF. Max 256 MiB, parsed line by line. Lowercase everything.
- Comments: `!`, `#` (not cosmetic `##`), `;`, `[`; strip inline ` #…`. Skip cosmetic rules (`##`, `#@#`, `#$#`, `#?#`, `#%#`).
- Hosts form `IP host [host…]`: every host is an **exact** block. Skip `localhost`, `localhost.localdomain`, `local`, `broadcasthost`, `ip6-localhost`, `ip6-loopback`, `ip6-localnet`, `ip6-mcastprefix`, `ip6-allnodes`, `ip6-allrouters`, `ip6-allhosts`, `0.0.0.0`.
- `||d^` → subtree block. `@@||d^` (also `@@||d^|`) → subtree allow. `|d^` → exact block.
- Plain `d` → exact block, or subtree if the list's `plainDomains` flag is `subtree`.
- `*.d` → subtree block of d (apex included).
- `/re/` → Go RE2 regex (≤ 1024 chars).
- Other ABP shapes (`*` inside, `||x` without `^`, `.x^`, `x^`) → RE2 (`||` → `^(?:[^.]+\.)*`, leading `|` → `^`, `^`/trailing `|` → `$`, `*` → `.*`).
- Modifiers: `$important` and `$badfilter`; any other `$modifier` → rule skipped (counted as unsupported). Patterns (regex + wildcard) capped at 20 000 in total.
- Domains must be valid A-labels; invalid lines are counted, not fatal.

**Matcher**: immutable snapshot with exact and subtree sets (sorted 64-bit FNV-1a hashes + parallel source indices, ≤ 16 bytes per entry), plus compiled patterns (with a literal pre-check). Lookup walks the qname and its parent suffixes.

**Precedence** (first decisive wins; user rules always beat list rules):
1. user exact allow · 2. user subtree allow · 3. user regex allow · 4. user exact deny · 5. user subtree deny · 6. list `@@…$important` · 7. list `…$important` · 8. list allow (`@@` entries, allow-type lists, `@@/re/`) · 9. list block (exact, subtree, hosts, plain, wildcard) · 10. user regex deny · 11. list regex/pattern block.

**Groups** (Pi-hole semantics): lists and rules belong to 0..n groups; clients to 1..n groups; a list/rule applies iff it shares at least one **enabled** group with the client. Group 1 "Default" always exists and cannot be deleted; unknown clients are in Default. Entries with no group apply to nobody. New lists/rules default to Default. Client/group changes never recompile the matcher (group IDs are passed per query); editing a list's/rule's groups swaps only the source→groups table.

**Lists**: `https` URLs (or `http`/`file://` only for private IP literal hosts / `<data>/lists/local/`), kind `block`/`allow`, `plainDomains`, groups, comment. Update every `filter.updateIntervalHours` (default 24; 0 = manual) with ±10 % jitter, conditional GET, last good copy kept; status `ok | unchanged | failed-cached | failed-empty`. Downloads use the bypass resolver, never follow redirects to private addresses, and are parsed only when the content changed. Recompiles are coalesced.

**Default list**: HaGeZi Multi NORMAL (ABP). An embedded catalogue offers OISD small/big, StevenBlack, AdGuard DNS filter, 1Hosts Lite, HaGeZi Pro/Pro++/Ultimate/TIF/DoH-bypass.

### 7.3 Blocking replies

| Mode | A | AAAA | Other |
|---|---|---|---|
| `null` (default) | `0.0.0.0` | `::` | NODATA |
| `nxdomain` | NXDOMAIN + SOA | NXDOMAIN + SOA | NXDOMAIN + SOA |
| `nodata` | NODATA + SOA | NODATA + SOA | NODATA + SOA |
| `refused` | REFUSED | REFUSED | REFUSED |
| `custom_ip` | `blockingIpv4` | `blockingIpv6` (or NODATA) | NODATA |

TTL `filter.blockedTtl` (default 10 s). Synthetic SOA: `picache.invalid. hostmaster.picache.invalid. 1 1800 900 604800 <ttl>`. **Pause**: global, optional duration (30 s, 5 min, 1 h, until re-enabled), persisted, auto re-enabled.

### 7.4 Upstreams and cache

- Upstream syntax: `IP[:port]`, `udp://`, `tcp://`, `tls://host[:port]` (DoT, ALPN `dot`), `https://host[:port]/path` (DoH, RFC 8484 POST, ID 0, HTTP/2). DoT/DoH hostnames are resolved via the bootstrap IPs only.
- Defaults: Quad9 DoH and Cloudflare DoH; bootstrap `9.9.9.9`, `149.112.112.112`, `1.1.1.1`, `1.0.0.1`.
- Modes: `load_balance` (default; weighted by EWMA RTT and recent failures, fail over on error), `parallel`, `strict`. Per attempt 3 s, total `upstreamTimeoutMs`.
- Plain UDP retries over TCP on TC. Replies must match the question or are discarded. Upstream queries are built fresh (RD=1, our OPT 1232, DO=1 if the client set DO or `dns.dnssec`), no client EDNS options.
- Response cache: key (lower qname, qtype, qclass, DO sent upstream, upstream-set id); LRU of `cacheSize` entries; TTL clamp; negative caching per RFC 2308 (≤ 1 h); SERVFAIL 5 s; serve-stale (TTL 30, background refresh, one per key). Replies keep the client's question case and Id; TTLs are decremented by the cached age.
- In-flight dedup: identical queries share one exchange; each waiter gets its own copy.
- **Clock guard**: while the clock is before the binary's build date, encrypted upstreams are skipped and plain DNS to the bootstrap IPs is used (logged once, health warning). Certificate errors alone never trigger this.
- `LookupIP(host, want6)` resolves via the default upstreams with caching and never consults local data.

---

## 8. LanCache

### 8.1 Services and domain lists

- Source: `uklans/cache-domains` (`cache_domains.json` + every file in `domain_files`), fetched over verified HTTPS through the bypass resolver, refreshed every `lancache.updateIntervalHours` (default 24), snapshot in `<data>/cache-domains/` (written through `os.Root`, temp + rename). Offline start uses the last snapshot; without a snapshot, overrides stay inactive and health says why.
- **Untrusted input rules**: `domain_files` names must match `^[A-Za-z0-9][A-Za-z0-9._-]{0,63}\.txt$` and are joined with `url.JoinPath`; `cache_domains.json` ≤ 1 MiB and ≤ 128 services; each `.txt` ≤ 4 MiB and ≤ 50 000 lines; service IDs `^[a-z0-9][a-z0-9_-]{0,31}$`; no redirects to other hosts, never private destinations.
- `.txt` parsing: trim, lowercase, skip empty and `#` lines, accept CRLF, strip trailing dot. `*.example.com` = any depth below, **not** the apex; plain = exact. Every pattern (source, custom, extra) passes `ValidatePattern`: valid A-labels, ≥ 2 labels, not `*`, base not a public suffix. Rejected patterns are listed in the source status.
- LanCache is **off by default**. Enabling it in the UI shows the effective cache IP, the store path, its filesystem, free space and warnings (SD card, Docker bridge). All services are enabled by default except `test`. Users can disable services, add custom services and extra hosts.
- `lancache.steamcontent.com` is answered whenever `steam` is enabled.
- Matching is anchored and case-insensitive (exact map + suffix walk).

### 8.2 HTTP cache proxy (:80)

Order per request:
1. **Client ACL** (also enforced at accept) → 403.
2. **Heartbeat**: `GET|HEAD|OPTIONS /lancache-heartbeat` for any Host (incl. IP literals) → `204` with `X-LanCache-Processed-By: <instanceID>`, `Access-Control-Allow-Origin: *`, `Access-Control-Expose-Headers: *` (+ `Access-Control-Allow-Private-Network: true` on OPTIONS). Not logged as a download.
3. **Loop detection**: incoming `X-LanCache-Processed-By` containing our instance ID → 508.
4. **Host**: lowercase, strip port and trailing dot. IP-literal Host → 400.
5. **Classify** (`Classify(host, ua, path)`): User-Agent ending in `Valve/Steam HTTP Client 1.0` **and** path `^/depot/[0-9]+/` or `/server-status` **and** GET/HEAD → `steam` (Steam sends the real CDN host). Otherwise by host. Unknown → 403 (Steam-UA refusals are counted and shown so the admin can add the host). Known host of a disabled service → pass-through uncached. Residual risk (as in monolithic): a client can poison `/depot/…` keys from its own public host; Steam verifies chunk SHA-1, so the effect is a denial of service, not code execution.
6. **Special paths** (pass-through uncached): `/server-status`; `^.+(releaselisting_.*|.version$)`; prefix `/latest64`; `(?i)(authrootstl|pinrulesstl|disallowedcertstl)\.cab$`.
7. **Method**: `GET`/`HEAD` → cache path. Others → pass-through uncached (bounded bodies, no Range injection, client's Accept-Encoding kept).
8. **Canonical path and key**: if `r.URL.EscapedPath()` contains `//`, a `.`/`..` segment, `%2F`/`%2f`, `%5C`, `%00`, `\`, or percent-encodes an unreserved character, the request is passed through and never stored. Otherwise key = `service + "\x00" + decodedPath` (no query), object ID = first 32 hex chars of SHA-256(key). The upstream request target is the client's escaped form byte for byte (`u := *r.URL; u.Scheme = "http"; u.Host = host`), including the query.
9. **Bypass**: `?nocache=<non-empty, not "0">` from a client in `lancache.nocacheClients` → skip cache reads, refetch and overwrite (logged BYPASS). From others it is ignored.
10. **Slicing** (slice size S from the store marker, default 1 MiB): slice `i` covers `[i·S, min((i+1)·S, total))`; fills request `Range: bytes=i·S-(i·S+S-1)` with `Accept-Encoding: identity` (never the client's).
    - Valid slice response: `206` with `Content-Range: bytes a-b/total`, `a == i·S`, `b+1 == min(a+S, total)`, total known and ≤ 1 TiB (larger objects are passed through), no Content-Encoding other than identity.
    - (a) A `200` with Content-Length ≤ S is the complete object: stored as slice 0, not a range failure. (b) A `200` with Content-Length > S, or a mismatching `206`, is a **range failure**: stream it from its actual start, store complete slices only when total is known and the encoding is identity, count one failure for the host. (c) A `200` without Content-Length is served, never stored. (d) A host is marked `noSlice` after 3 range failures on distinct objects within 24 h (persisted, resettable); a later valid 206 resets it.
    - A response with a non-identity Content-Encoding is streamed uncached (PASS).
    - Upstream `416` for slice i>0 of a recorded object → invalidate and abort; for slice 0 → retry once without Range and pass through uncached; never send 416 to a client that sent no Range.
    - A different total than recorded → the store increments the object generation (old slices discarded); a response whose headers were sent with the old length is aborted.
    - **Stored headers**: every upstream header except Content-Length, Content-Range, Content-Encoding, Transfer-Encoding, Accept-Ranges, ETag, Set-Cookie, Age, Date, Expires, Cache-Control, Pragma, Vary, Alt-Svc, Strict-Transport-Security, Connection, Keep-Alive, Trailer, Upgrade, Via, Server-Timing and names starting with X-Cache, CF-, X-Amz-Cf-, X-LanCache-, X-Upstream-; values with CR/LF/NUL dropped; ≤ 4 KiB.
11. **Serving**: the handler parses Range itself. With an unknown total and an If-Range, suffix or multi-range request it fetches slice 0 first. It writes 200, 206 or 416 (`Content-Range: bytes */total`); multi-range (≤ 16 ascending, non-overlapping ranges, else 200 full) as `206 multipart/byteranges` with a precomputed Content-Length; If-Range (dates vs stored Last-Modified; any ETag form → 200 full); If-Modified-Since → 304; HEAD without body. Cached slices are written with `SliceReader.WriteRange` to the unwrapped `ResponseWriter` so net/http uses sendfile(2). Response headers: stored headers, `Accept-Ranges: bytes`, `X-LanCache-Processed-By`, `X-Upstream-Cache-Status: HIT|MISS|PARTIAL|BYPASS`; never ETag or Set-Cookie. A 60 s write deadline is set before each client write.
12. **Mid-stream failure** of a later slice: retry once, then fetch the remaining bytes of the current range directly (uncached) and keep streaming; abort only if that fails too.
13. **Status handling**: only 200/206 are stored. `301/302/307/308` followed server-side (≤ 5 hops, Range kept, SSRF re-check, `https` with verified TLS); content stored under the original key. `426` → retry once as `https://<host><RequestURI>` (verified TLS, SSRF-guarded), host remembered as https-only for 24 h. `4xx/5xx` passed through unmodified and never stored (403 must pass through for prefill tools). On `404` from one IP, retry once on the next A record.
14. **Fills and collapsing**: in-flight fills live in a `map[sliceKey]*fill` with a sync.Cond-signalled growing buffer; concurrent readers stream from it as it grows (not x/sync/singleflight). Buffers come from a free list of size S. Global slots `min(maxConcurrentFills, 1 GiB / S)`; per client key `maxFillsPerClient` (incl. read-ahead). A demand fill waits ≤ 2 s for a slot, then streams its slice directly uncached (PASS). A slot is released after the buffer was renamed into the store or discarded; store writes wait ≤ 10 s for the NAS I/O semaphore, then the buffer is discarded. Stalled fill (no bytes for 15 s) → a waiting reader may fetch directly. Fills continue after the client disconnects; no new slices are started for a gone client. While the store is full (`StoreFull`), no new fills are stored.
15. **Read-ahead**: `readAheadSlices` (default 2) following uncached slices, started only after the client consumed one full slice of the current range, `TryAcquire` only.
16. **Upstream transport**: HTTP/1.1 keep-alive (`MaxIdleConnsPerHost` 32, `IdleConnTimeout` 90 s, no env proxy, `DisableCompression`), `netutil.SafeDialer` over `LookupIP` (IPv4), connect 10 s, response header 15 s, idle read 60 s. Forward end-to-end client headers except hop-by-hop, `Range`/`If-*`, `Cookie`, `Accept-Encoding` (fills); add `X-LanCache-Processed-By`. No `X-Forwarded-For`.
17. **Accounting**: bytes served from cache vs fetched upstream per slice, bytes stored; one cache event per client request; live transfers and aggregated active downloads (per client + content, 10 s rate window).

### 8.3 SNI pass-through (:443)

Accept (ACL + caps at accept) → read the ClientHello: 5-byte record header (type 0x16, length ≤ 16384), then the full record, reading further 0x16 records until the handshake message is complete (16 KiB, 5 s) → extract SNI without terminating TLS → must match an enabled service (or the Steam trigger) → `LookupIP` → SSRF guard → dial :443 (10 s) → replay the buffered bytes → relay with `io.CopyN(dst, src, 4 MiB)` loops on the raw `*net.TCPConn` pair (splice), refreshing a 5 min idle deadline per chunk; max lifetime 24 h. No SNI or not allowed → close. One event per connection (client, SNI, service, bytes, duration). The UI flags services whose traffic is mostly pass-through (HTTPS bypass, e.g. Epic launcher ≥ 20.0.3).

### 8.4 Content grouping ("what was downloaded")

| Service | Rule | Group key | Label |
|---|---|---|---|
| steam | path `^/depot/(\d+)/(?:chunk/[0-9a-fA-F]{40}|manifest/(\d+)/\d+(?:/\d+)?|[^/]+/)` | `steam:depot:<depot>` | user label → "Steam depot <depot>" |
| blizzard | path `^/(tpr|cortez)/([^/]+)/(config|data|patch)/` | `blizzard:<lower(cdnpath)>` (`configs` → shared) | built-in product map (wow, ovw, fenris, diablo3, hs, sc2, …) |
| epicgames | `^/Builds/Org/(o-[a-z0-9]+)/([0-9a-f]{32})/` or `^/Builds/(.+?)/CloudDir/` | `epic:<org>/<build>` or `epic:<app>` | user label → "Epic item <build[:8]>" / humanised app |
| riot | host `^([a-z0-9-]+)\.(?:secure\.)?dyn\.riotcdn\.net$` | `riot:<prod>` | lol → League of Legends, valorant → VALORANT, ks-foundation → Riot Client |
| xboxlive / wsus | package file name `(.+)_(\d+(?:\.\d+){3})_(x64|x86|arm64|neutral)__([a-z0-9]{13})\.(msixvc|xvc|appx|appxbundle|msix|msixbundle|eappx|eappxbundle)$`; KB `(?i)(?:^|[-_/])kb(\d{6,8})(?:[-_.]|$)`; DO `^/filestreamingservice/files/<guid>`; Office `^/pr/<guid>/Office/Data/<ver>/` | `xbox:<pkg>`, `win:kb<n>`, `win:do`, `win:office:<channel>` | humanised |
| sony | `/((?:CUSA|PPSA)\d{5})_00/` | `psn:<title>` | title id |
| nintendo | `^/c/([csa])/([0-9a-f]{32})` | `nintendo:switch` | "Nintendo eShop content" |
| uplay / origin | `^/uplaypc/downloads/([^/]+)/`, `^/eamaster/s/shift/([^/]+)/` | `ubi:<g>`, `ea:<g>` | humanised |
| wargaming | host `^dl-(wot|wows|wowp)-` | `wg:<g>` | World of Tanks/Warships/Warplanes |
| fallback | — | `<service>:<host>` | "<Service> · <host>" |

Query strings are never stored or displayed. Users can set labels for any group key (e.g. name a Steam depot).

---

## 9. Cache store

### 9.1 On-disk format (per store root)

```
<root>/.picache-store          JSON {"storeId","format":1,"sliceSize","createdAt"} (≤ 4 KiB, validated)
<root>/tmp/                    temp files (same filesystem → atomic rename)
<root>/slices/<h0h1>/<h2h3>/<objectId>.<sliceIndex>
```
Slice file = magic `PCS1` + header length (uint32 LE, ≤ 64 KiB) + JSON header `{"o","i","s","h","p","t","z","c","k","m"}` (object id, index, service, host, path, total, slice size, created ms, **crc32c of data**, headers) + data. Validation on read and verify: `o`/`i` match the file name, `ObjectID(s,p) == o`, `z` == the store's slice size, `t` ≤ 1 TiB, file size == 8 + headerLen + min(z, t − i·z); on rebuild only Content-Type and Last-Modified are taken from `m`. The store marker is created by `cachestore.InitRoot` (via `storage.InitStore`), never implicitly by `Open`. All access goes through `os.Root`; no fsync per slice (temp → close → rename → index).

### 9.2 Index (`<data>/cache-index/<storeId>.db`, local disk)

- Tables: `store_objects` (id, gen, service, host, path, group_key, total, slice_size, headers, created_at, last_access, hits, bytes_served, cached_bytes, slice_count, pinned, no_slice), `store_slices` (object_id, idx, size, crc, created_at; WITHOUT ROWID), `store_groups` (aggregates per service+group, updated in the same transactions), `store_pinned_groups`, `store_meta`. Index `store_objects(last_access) WHERE pinned = 0`.
- **Nothing is loaded at open.** `Head`/`HasSlice` read by primary key through a bounded LRU (65 536 compact entries). Index writes are batched (250 ms or 512 rows); access statistics accumulate in a dirty map flushed every 30 s. Aggregates make `Groups`/`Services`/`Usage` cheap.
- Each object has a **generation**; `SetMeta` increments it for new records or changed totals; `WriteSlice`/`ReadSlice` with a stale generation return `ErrStale`.
- `Close` may run concurrently with anything; afterwards every call returns `ErrClosed` (treated as a miss by the proxy).

### 9.3 Retention and eviction (every 60 s and on low space; serialised)

1. **Inactive expiry**: objects not accessed for `cache.maxAgeDays` (default 365) are deleted unless pinned. `expiresAt = lastAccess + maxAge` is shown in the UI.
2. **Size**: if `cachedBytes > cache.maxSizeBytes` (0 = none) or free space < effective minimum (`min(cache.minFreeBytes, 10 % of the filesystem)`, at least 2 GiB when the store shares the data filesystem), delete least-recently-used unpinned objects until the deficit (+5 %) is covered. Free space is a sample (≤ 30 s old) read once per run.
3. If limits cannot be met (everything left is pinned) the store is **full**: hits are served, new content is streamed uncached, health warns.
4. Every eviction is recorded (reason `inactive|size|min-free|manual|corrupt|invalidated`).

Free space never takes a store offline. Groups can be pinned persistently (new objects of the group inherit the pin).

### 9.4 Verify / rebuild

`Verify(repair)` walks the slice tree via `os.Root`, validates headers, sizes and crc32c, reconciles with the index, deletes corrupt files (repair), reports progress. Runs in the background; the store stays usable.

---

## 10. Storage targets (NAS)

### 10.1 Model

`Target{id, name, kind: local|smb|nfs, mode: external|host-apply, path, server (IP literal), share, export, subdir, username, domain, password (sealed, write-only), smbVersion (3.1.1), smbSeal, nfsVersion (4.2), nfsNconnect, requireMountpoint, storeId}`. Built-in target `local` = `PICACHE_CACHE_DIR`. Exactly one target is active (`cache.activeStoreId`, changed only via the storage API). Every non-built-in path must be below `PICACHE_MOUNT_ROOT` (default `/srv/picache`); host-apply always uses `MountRoot/<id>`. `ValidateTarget` enforces strict allowlists for every field.

### 10.2 Modes

1. **external** (default, always available): something else mounts the share at `path` (host fstab/systemd, Docker bind with `rslave`, Proxmox `mpX`). The UI shows snippets (credentials file template, fstab line, systemd `.mount`, docker-compose bind, Proxmox host fstab + `pct set <ct> -mp0 …` with the UID offset from `/proc/self/uid_map`). Snippets never contain the password.
2. **host-apply** (bare metal / VM / privileged LXC with systemd): the UI queues a request; the root helper (`picache storage apply-pending`, started by `picache-storage.path`) or the admin (`sudo picache storage apply <id> [--password-stdin]`) validates the target again, writes `/etc/picache/credentials/<id>.cred` (0600) and a `.mount` unit for `/srv/picache/<id>` (`nofail`, `x-systemd.mount-timeout=30`, CIFS `vers=3.1.1,uid,gid,file_mode=0640,dir_mode=0750,soft,nosuid,nodev,noexec,noatime`, NFS `vers=4.2,proto=tcp,softerr,timeo=100,retrans=2,nconnect`), runs `systemctl daemon-reload` and `enable --now`, and writes a result file the UI displays. The root helper opens the database read-only and never trusts it.

### 10.3 Initialisation and adoption

A target becomes usable after `InitStore`: an empty root gets a new marker; an existing marker can be **adopted** (e.g. a NAS store that survived a reinstall — the index is then rebuilt by Verify). The built-in local store is initialised automatically when its directory is empty.

### 10.4 Mount guard (before and during use, every 30 s)

Online only if: the path exists; for smb/nfs (or `requireMountpoint`) it is a mountpoint (`/proc/self/mountinfo` or `st_dev` differs from parent); statfs type matches (CIFS `0xff534d42`/SMB2 `0xfe534d42`, NFS `0x6969`; compared as `uint32`); the marker exists with the expected store ID; a write/rename/delete test in `tmp/` succeeds (EROFS → hint about the sandbox). statfs runs with a timeout (single-flight). Offline → the proxy serves pass-through and the UI/health explain why. It never writes into an unmounted underlying directory. DNS never depends on storage. NAS I/O runs behind a semaphore (64 ops).

### 10.5 Capability detection (shown in the UI)

Container type, init user namespace and UID offset, systemd, kernel filesystems (`/proc/filesystems`), mount helpers, whether the root helper is installed (`/etc/picache/host-apply.enabled`), Docker network mode (best effort). `/proc/1/environ` is only searched for `container=` and never returned.

---

## 11. Logs, statistics, sessions (`logs.db`)

- Producers call `Log*` for every event unless the identity has `ignoreLogs`. The logs package anonymises (if enabled) before storage and the live feed; with the query log disabled it still updates rollups.
- Ingestion through bounded channels; one writer batch-inserts every 5 s or 5000 rows; full channels drop and count. Live feeds fan out from the writer (≤ 16 subscribers, buffer 256, drop when slow).
- Raw tables: queries (retention `queryLogRetentionHours`, default 168), cache requests (`cacheLogRetentionHours`, default 48), SNI events, evictions, download sessions (`sessionRetentionDays`, default 90; key client+service+group, new session after a 120 s gap).
- Rollups: per minute (48 h) and per hour (`statsRetentionDays`, default 365) for DNS counts by status class and cache bytes by service; hourly top tables for domains, blocked domains, clients, upstreams (DNS) and clients, content (cache), top 1000 keys per hour and kind. Dashboard queries read rollups only.
- Limits: 10 s query timeout behind a semaphore of 2; searches need ≥ 3 characters; series ≤ 1500 points; `logs.maxDbSizeMiB` (default 2048) enforced by pruning the oldest raw events; raw inserts pause while the data disk has < 1 GiB free.

---

## 12. Web API and UI

- REST under `/api/v1`, JSON only (`docs/API.md`). SSE under `/api/v1/stream/*`.
- Auth: cookie session (UI) or `Authorization: Bearer <token>` (scope `read`/`admin`). Permissions per route: `public`, `read`, `admin`, `session` (interactive admin session only: password, TOTP, tokens, sessions).
- Middleware: recover → security headers → host allowlist → HTTPS redirect (307, if enabled) → CrossOriginProtection → handler. Long handlers lift the server timeouts with `extendDeadlines`.
- `GET /healthz` → `ok`. `GET /metrics` → Prometheus text (admin token, disabled by default). Health checks carry severity (`ok|warn|fail`) and hints, are evaluated every 60 s and every status change is logged once.
- Audit log for every state-changing admin action (secrets redacted by member name).
- Static UI embedded; hashed assets cached for a year, `index.html` never.

### 12.1 Frontend

Svelte 5 (runes) + Vite 8 + TypeScript 6, plain SPA with a hash router, uPlot for charts, no other runtime dependencies. English and German. Design system: `docs/DESIGN.md`.

Information architecture: **Overview** · **DNS** (Query log, Filtering, Clients & groups, Local DNS, DNS settings) · **Cache** (Downloads, Library, Services, Storage, Cache settings) · **System** (Account & security, API tokens, Audit log, Backup & restore, Health & about).

---

## 13. Defaults (single source of truth: `internal/settings/defaults.go`)

| Setting | Default |
|---|---|
| DNS upstreams | Quad9 DoH, Cloudflare DoH; bootstrap 9.9.9.9, 149.112.112.112, 1.1.1.1, 1.0.0.1 |
| Upstream mode | `load_balance`, timeout 10 s |
| DNS cache | on, 10 000 entries, serve-stale 1 h |
| Local domain / router resolver | first resolv.conf search domain (else `lan`) / `auto` (default gateway) |
| Rate limit | 50 qps, burst 200 per client (/32, /64) |
| Blocking | on, mode `null`, blocked TTL 10 s, CNAME inspection on |
| Lists | HaGeZi Multi NORMAL, update every 24 h |
| Special domains | Mozilla canary blocked, iCloud Private Relay blocked |
| LanCache | **off** until enabled in the UI; all services except `test`; DNS TTL 60 s; `nocache` honoured from nobody |
| Slice size | 1 MiB |
| Retention | inactive 365 days; min free `min(10 GiB, 10 %)`; no max size |
| Fills | 64 global (× slice ≤ 1 GiB), 32 per client, read-ahead 2 |
| Logs | query log 7 days, cache log 48 h, sessions 90 days, stats 365 days, logs.db ≤ 2 GiB |
| Web sessions | idle 60 min, absolute 7 days |
