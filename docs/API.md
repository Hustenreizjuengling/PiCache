# PiCache REST API (v1)

Base path `/api/v1`. JSON only (`Content-Type: application/json` for request bodies; unknown members are rejected). Timestamps in entities are RFC 3339 (UTC); time series use unix seconds. Types in `code` refer to Go types (`package.Type`) whose JSON field names are authoritative.

**Auth.** Browser: session cookie `picache_session` (set by login/setup). Automation: `Authorization: Bearer <token>` (API tokens, scope `read` or `admin`). Permissions:
- **P** public
- **R** any authenticated principal (read or admin)
- **A** admin (browser session or admin token)
- **S** interactive admin browser session only (never API tokens): account security

All state-changing requests are protected by Go's `CrossOriginProtection` and audited.

**Errors.** `{"error":{"code":"invalid|not_found|conflict|forbidden|unavailable|unauthorized|too_many_requests|internal|misdirected","message":"…","field":"dns.upstreams[1]"}}` with the matching HTTP status.

**Lists.** Paged lists return `listing.Page[T]` = `{"items":[…],"total":N,"next":"cursor"}`. Offset lists take `limit`/`offset`; cursor lists take `limit`/`cursor`. Time ranges: `from`/`to` (RFC 3339 or unix seconds) or `range=15m|1h|6h|24h|7d|30d|90d`.

**Streams.** SSE endpoints send `event: <name>` + `data: <json>`, a `: ping` comment every 15 s, re-validate the session every 15 s and end after 1 h (the browser reconnects). At most 16 concurrent streams (429 beyond).

Ownership column = the `internal/api/routes_*.go` file that implements the endpoint.

---

## Auth & account — `routes_auth.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/auth/status` | P | – | `{setupRequired:bool, authenticated:bool, user?:auth.User, scope?:string, tokenAuth:bool, language:string, setupHints?:[string]}` (setupHints: where to find the token: log, `picache setup-token`, `docker exec … picache setup-token`) |
| POST `/auth/setup` | P | `{setupToken, username, password}` | 200 `auth.User` + cookie. 403 if setup already done or token wrong. Password ≥ 10 chars. |
| POST `/auth/login` | P | `{username, password, totp?}` | 200 `auth.User` + cookie; 401 `unauthorized` (wrong credentials); 401 with `field:"totp"` when a TOTP code is required or wrong; 429 throttled |
| POST `/auth/logout` | R | – | 204, clears cookie |
| GET `/auth/me` | R | – | `auth.User` |
| POST `/auth/password` | S | `{currentPassword, newPassword}` | 204 (other sessions revoked) |
| GET `/auth/sessions` | S | – | `[]auth.SessionInfo` |
| DELETE `/auth/sessions/{id}` | S | – | 204 |
| POST `/auth/totp/begin` | S | – | `{secret, uri}` (render the URI as a QR code client-side) |
| POST `/auth/totp/confirm` | S | `{code}` | 204 |
| POST `/auth/totp/disable` | S | `{password}` | 204 |
| GET `/tokens` | S | – | `[]auth.TokenInfo` |
| POST `/tokens` | S | `{name, scope:"read"|"admin", expiresInDays?:int}` | 201 `{token:string, info:auth.TokenInfo}` (token shown once; audit with `info` only) |
| DELETE `/tokens/{id}` | S | – | 204 |

## System — `routes_system.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/system/info` | R | – | `{version:version.Info, startedAt, uptimeSec, instanceId, listeners:api.ListenerInfo, dataDir, cacheDir, mountRoot, masterKeySource, memory:{allocBytes,sysBytes,limitBytes,numGC}, goroutines}` |
| GET `/system/health` | R | – | `api.Health` |
| GET `/system/overview` | R | – | Top-bar/overview status in one call: `{blocking:dnsserver.BlockingStatus, dns:dnsserver.Stats, cacheIps:dnsserver.CacheIPStatus, router:dnsserver.RouterStatus, lancacheEnabled:bool, servicesReady:bool, store:api.StoreState, proxy:proxy.Stats, sni:sni.Stats, filter:filter.Stats, upstreams:[]upstream.UpstreamStat, clockGuard:bool, health:{ok:bool, warnings:int, failures:int}}` |
| GET `/system/audit` | A | `?search&limit&offset` | `listing.Page[auth.AuditEntry]` |
| GET `/system/backup` | A | `?includeSecrets=true` (sealed NAS passwords; useless without the master key) | `application/octet-stream` download `picache-backup-<date>.db` (sessions removed) |
| POST `/system/restore` | A | raw body (`application/octet-stream`, ≤ 512 MiB) | 202 `{staged:true, message:"Restart PiCache to apply"}`; 400 for invalid or newer-version backups |
| POST `/system/restart` | A | – | 202; the process exits with code 75 and is restarted by systemd/Docker |
| GET `/metrics` (no `/api/v1` prefix) | A token | – | Prometheus text (404 unless `web.metricsEnabled`) |

## Settings — `routes_settings.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/settings` | R | – | `settings.All` |
| PUT `/settings` | A | `settings.All` (full document) | `settings.All`; 400 with `field` on validation errors |
| PATCH `/settings/{section}` | A | one section object (`dns`, `filter`, `lancache`, `cache`, `logs`, `web`) | `settings.All` |
| GET `/settings/defaults` | R | – | `settings.All` (defaults, for "reset" buttons) |

Changes to `cache.activeStoreId` via these endpoints are rejected (use `POST /storage/targets/{id}/activate`).

## DNS: blocking, lookup, records, forwarders, clients, groups — `routes_dns.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/dns/blocking` | R | – | `dnsserver.BlockingStatus` |
| POST `/dns/blocking` | A | `{enabled:bool, pauseSeconds?:int}` (enabled=false + pauseSeconds>0 = timed pause) | `dnsserver.BlockingStatus` |
| POST `/dns/lookup` | R | `dnsserver.LookupRequest` | `dnsserver.LookupResult` |
| GET `/dns/stats` | R | – | `dnsserver.Stats` |
| GET `/dns/cache-ips` | R | – | `dnsserver.CacheIPStatus` |
| GET `/dns/router` | R | – | `dnsserver.RouterStatus` |
| GET `/dns/records` | R | – | `[]dnsserver.Record` |
| POST `/dns/records` | A | `dnsserver.RecordInput` | 201 `dnsserver.Record` |
| PUT `/dns/records/{id}` | A | `dnsserver.RecordInput` | `dnsserver.Record` |
| DELETE `/dns/records/{id}` | A | – | 204 |
| GET `/dns/forwarders` | R | – | `[]dnsserver.Forwarder` |
| POST `/dns/forwarders` | A | `dnsserver.ForwarderInput` | 201 |
| PUT `/dns/forwarders/{id}` | A | `dnsserver.ForwarderInput` | 200 |
| DELETE `/dns/forwarders/{id}` | A | – | 204 |
| GET `/clients` | R | – | `[]clients.Client` |
| POST `/clients` | A | `clients.ClientInput` | 201 `clients.Client` |
| PUT `/clients/{id}` | A | `clients.ClientInput` | 200 |
| DELETE `/clients/{id}` | A | – | 204 |
| GET `/clients/known` | R | `?within=30d` | `[]clients.Known` |
| GET `/groups` | R | – | `[]clients.Group` |
| POST `/groups` | A | `clients.GroupInput` | 201 |
| PUT `/groups/{id}` | A | `clients.GroupInput` | 200 |
| DELETE `/groups/{id}` | A | – | 204 (403 for group 1) |

## Upstreams — `routes_upstream.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/dns/upstreams` | R | – | `{upstreams:[]upstream.UpstreamStat, cache:upstream.CacheStat, clockGuard:bool}` |
| POST `/dns/upstreams/test` | A | `{upstream:string}` | `upstream.TestResult` |
| POST `/dns/cache/flush` | A | – | 204 |

## Filtering — `routes_filter.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/filter/lists` | R | – | `[]filter.List` |
| POST `/filter/lists` | A | `filter.ListInput` | 201 `filter.List` (download starts in background) |
| PUT `/filter/lists/{id}` | A | `filter.ListInput` | `filter.List` |
| DELETE `/filter/lists/{id}` | A | – | 204 |
| POST `/filter/lists/{id}/refresh` | A | – | `filter.List` (waits up to 5 min; `extendDeadlines`) |
| POST `/filter/lists/refresh` | A | – | 202 `{started:true}` (all lists, background) |
| GET `/filter/catalog` | R | – | `[]filter.CatalogEntry` |
| GET `/filter/rules` | R | `?action&type&search` | `[]filter.Rule` |
| POST `/filter/rules` | A | `filter.RuleInput` | 201 `filter.Rule` |
| PUT `/filter/rules/{id}` | A | `filter.RuleInput` | `filter.Rule` |
| DELETE `/filter/rules/{id}` | A | – | 204 |
| GET `/filter/stats` | R | – | `filter.Stats` |
| POST `/filter/explain` | R | `{domain, clientIp?}` | `{domain, groupIds:[int], matches:[]filter.Match, decision:{action, name, source, kind}}` |

## LanCache services & SNI — `routes_lancache.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/lancache/services` | R | – | `[]services.Service` (`domains` trimmed to the first 50 in the list view) |
| GET `/lancache/services/{id}` | R | – | `services.Service` (full domains) |
| PUT `/lancache/services/{id}/enabled` | A | `{enabled:bool}` | `services.Service` |
| PUT `/lancache/services/{id}/domains` | A | `{extraDomains:[string]}` | `services.Service` (400 with the offending pattern) |
| POST `/lancache/services` | A | `services.ServiceInput` | 201 (custom service) |
| PUT `/lancache/services/{id}` | A | `services.ServiceInput` | 200 (custom only) |
| DELETE `/lancache/services/{id}` | A | – | 204 (custom only) |
| GET `/lancache/source` | R | – | `services.SourceStatus` |
| POST `/lancache/source/refresh` | A | – | `services.SourceStatus` (waits up to 5 min; `extendDeadlines`) |
| PUT `/lancache/labels` | A | `{groupKey, label}` ("" removes) | 204 |
| GET `/lancache/sni` | R | – | `sni.Stats` |

## Cache store (library, maintenance) — `routes_cache.go`

All return 503 `unavailable` when no store is online (except `/cache/state`).

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/cache/state` | R | – | `api.StoreState` |
| GET `/cache/services` | R | – | `[]cachestore.ServiceUsage` |
| GET `/cache/groups` | R | `?service&search&sort&desc&limit&offset` | `listing.Page[GroupView]`, `GroupView = cachestore.GroupUsage + {label:string, clients:int}`; search also matches labels (`services.SearchLabels` → `GroupQuery.SearchKeys`); client counts via `logs.GroupClientCounts` |
| GET `/cache/groups/detail` | R | `?service&key` | `{group:GroupView, clients:[]logs.GroupClient, objects:listing.Page[cachestore.Object]}` (exact `GroupQuery.GroupKey`) |
| GET `/cache/objects` | R | `?service&group&search&sort&desc&limit&offset` | `listing.Page[cachestore.Object]` |
| DELETE `/cache/objects/{id}` | A | – | 204 (400 for malformed ids) |
| POST `/cache/objects/{id}/pin` | A | `{pinned:bool}` | 204 |
| POST `/cache/groups/delete` | A | `{service, key}` | `{bytesFreed}` |
| POST `/cache/groups/pin` | A | `{service, key, pinned}` | 204 |
| POST `/cache/services/{service}/purge` | A | – | `{bytesFreed}` |
| POST `/cache/evict` | A | – | `cachestore.EvictResult` |
| POST `/cache/verify` | A | `{repair:bool}` | 202 `api.VerifyState` |
| GET `/cache/verify` | R | – | `api.VerifyState` |

`sort` for groups: `bytes|lastAccess|firstCached|served|name`; objects: `lastAccess|size|created|path`. `ExpiresAt` uses `cache.maxAgeDays`.

## Proxy (live) — `routes_proxy.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/cache/live` | R | – | `[]proxy.ActiveDownload` (per client + content, for the overview) |
| GET `/cache/active` | R | – | `[]proxy.Transfer` (individual requests) |
| GET `/cache/proxy/stats` | R | – | `proxy.Stats` |
| GET `/cache/noslice` | R | – | `[]proxy.NoSliceHost` |
| DELETE `/cache/noslice/{host}` | A | – | 204 |

## Storage — `routes_storage.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/storage/capabilities` | R | – | `storage.Capabilities` |
| GET `/storage/targets` | R | – | `[]storage.TargetWithStatus` |
| GET `/storage/targets/{id}` | R | – | `storage.TargetWithStatus` |
| POST `/storage/targets` | A | `storage.TargetInput` | 201 `storage.Target` |
| PUT `/storage/targets/{id}` | A | `storage.TargetInput` | `storage.Target` |
| DELETE `/storage/targets/{id}` | A | – | 204 (not local, not active) |
| POST `/storage/targets/{id}/test` | A | – | `storage.TestResult` (`extendDeadlines` 2 min) |
| POST `/storage/targets/{id}/apply` | A | – | `storage.Status` with `applyState:"queued"` (503 if the root helper is not installed) |
| POST `/storage/targets/{id}/init` | A | `{adopt:bool}` | `storage.InitResult` |
| POST `/storage/targets/{id}/activate` | A | – | `api.StoreState` (409 `not initialised` → call init first) |
| GET `/storage/targets/{id}/snippets` | A | – | `storage.Snippets` (never contains the password) |

## Logs, statistics, downloads, streams — `routes_logs.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/logs/queries` | R | `?from&to&range&client&domain&status(multi)&qtype&upstream&cursor&limit` (default range 1h) | `logs.QueryPage` |
| GET `/stats/summary` | R | `?range` (default 24h) | `logs.Summary` |
| GET `/stats/dns` | R | `?range&step` (step in seconds; default so there are ≤ 300 points, never finer than the rollup: 60 s for ranges ≤ 48 h, 3600 s beyond; > 1500 points → 400) | `logs.Series` |
| GET `/stats/cache` | R | `?range&step&service` | `logs.Series` |
| GET `/stats/top` | R | `?kind=domains|blocked|clients|cache-clients|content|upstreams&range&limit` | `[]logs.TopItem` |
| GET `/stats/services` | R | `?range` | `[]logs.ServiceStat` |
| GET `/stats/clients` | R | `?range` | `[]logs.ClientStat` |
| GET `/cache/downloads` | R | `?from&to&range&client&service&group&search&active&limit&offset` | `listing.Page[logs.Download]` |
| GET `/cache/requests` | R | `?from&to&range&client&service&search&status&cursor&limit` | `listing.Page[logs.CacheEvent]` |
| GET `/cache/sni-events` | R | same | `listing.Page[logs.SNIEvent]` |
| GET `/cache/evictions` | R | same (`status` = reason) | `listing.Page[logs.EvictionEvent]` |
| GET `/stream/queries` | R | `?client&status` (server-side filter) | SSE `event: query`, data `logs.QueryEvent` |
| GET `/stream/cache` | R | – | SSE `event: request`, data `logs.CacheEvent` |

---

## Non-API endpoints

| Path | Notes |
|---|---|
| `GET /healthz` | `ok` (no auth, no details) |
| `GET /` and static assets | embedded UI (hash router); unknown paths serve `index.html` |

## Conventions for implementers

- Register routes with `s.route("GET /api/v1/…", permRead, handler)`; handlers return `error`. Permission constants: `permPublic`, `permRead`, `permAdmin`, `permSession`.
- Decode with `decode(w, r, &in)`; respond with `ok`, `created`, `noContent` or `writeJSON`.
- Handlers that may take long (list/source refresh, storage test, backup, restore) call `extendDeadlines(w, d)` first; SSE handlers use `sse(w, r, event, ch, alive)` with `alive = func() bool { return s.d.Auth.Valid(ctx, principal(r)) }`.
- Audit every successful state change with `s.audit(r, "<area>.<verb>", target, details)`; details are redacted by member name (password, token, secret, code, totp, setupToken, …). Never pass a token secret.
- Never return secrets (NAS password, token secrets except once at creation, TOTP secret except at begin).
- Validate path IDs with `pathID(r, "id")`; parse ranges with `qRange(r, 24*time.Hour)`.
