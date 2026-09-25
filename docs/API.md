# PiCache REST API (v1)

Base path `/api/v1`. JSON only (`Content-Type: application/json` for request bodies; unknown members are rejected). Timestamps in entities are RFC 3339 (UTC); time series use unix seconds. Types in `code` refer to Go types (`package.Type`) whose JSON field names are authoritative.

**Auth.** Browser: session cookie set by login/setup (`HttpOnly`, `SameSite=Strict`, `Path=/`): `__Host-picache_session` (always `Secure`) when the request arrived over TLS, `picache_session` over plain HTTP (`Secure` only with `PICACHE_WEB_SECURE_COOKIES=true`, for a TLS-terminating reverse proxy). Both names are accepted. Login and setup also set a device cookie, named the same way (`__Host-picache_device` over TLS, `picache_device` otherwise; `HttpOnly`, `SameSite=Strict`, 180 days, sealed with the master key, renewed at every sign-in, kept on logout). It is not a credential: a later sign-in with the same username from that browser is throttled by the device (5 failures → 15 min) instead of the username delay. Automation: `Authorization: Bearer <token>` (API tokens, scope `read` or `admin`). Permissions:
- **P** public
- **R** any authenticated principal (read or admin)
- **A** admin (browser session or admin token)
- **S** interactive admin browser session only (never API tokens): account security and restore

**Password confirmation.** Creating an API token, starting TOTP enrolment, changing the password, disabling TOTP and restoring a backup ask for the current password again. Wrong passwords are throttled per client and per session (each: 5 failures → 15 min lockout) and by the global attempt limit (429 while blocked), but not by the username delay of sign-ins, and are audited as `auth.login_failed`.

All state-changing requests are protected by Go's `CrossOriginProtection` and audited.

**Errors.** `{"error":{"code":"invalid|not_found|conflict|forbidden|unavailable|unauthorized|too_many_requests|internal|misdirected","message":"…","field":"dns.upstreams[1]"}}` with the matching HTTP status.

**Lists.** Paged lists return `listing.Page[T]` = `{"items":[…],"total":N,"next":"cursor"}`. Offset lists take `limit`/`offset`; cursor lists take `limit`/`cursor`. Time ranges: `from`/`to` (RFC 3339 or unix seconds) or `range=15m|1h|6h|24h|7d|30d|90d`.

**Streams.** SSE endpoints send `event: <name>` + `data: <json>`, a `: ping` comment every 15 s, re-validate the session every 15 s and end after 1 h (the browser reconnects). At most 16 concurrent streams (429 beyond).

Ownership column = the `internal/api/routes_*.go` file that implements the endpoint.

**Breaking changes in v0.2.0 (download cache names).** API clients written for v0.1.x have to switch to the new names; the old ones are not accepted as aliases.
- Settings: the download cache section of `settings.All` is `downloadCache` (`GET`/`PUT /settings`, `GET /settings/defaults`, `PATCH /settings/downloadCache`), and validation errors name the field `downloadCache.<member>` (e.g. `downloadCache.cacheIpv4[0]`).
- Routes: the download cache routes are under `/download-cache/…` with the same sub-paths as before (11 routes, see below).
- The DNS query status of answers with the cache address is `override` (query log, `status` filters, live stream); the DNS series key of `/stats/dns` is `override`, and the summary field is `logs.Summary.dnsDownloadCache`.
- JSON members: `/system/overview` `downloadCacheEnabled`; `clients.Client`/`clients.ClientInput` `downloadCacheBypass`.
- The health check is `download_cache`, and the audit actions of the download cache are `download_cache.*` (entries written before the upgrade keep their old action).
- Stored data is migrated automatically at the first start of v0.2.0, and a restored backup of v0.1.x at the start that applies it: the settings section (settings schema v2; an existing `downloadCache` section wins), the client bypass column (clients schema v2) and the query log and DNS rollups in logs.db (logs schema v2). Backups made by v0.2.0 are refused by v0.1.x (newer schema).

---

## Auth & account — `routes_auth.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/auth/status` | P | – | `{setupRequired:bool, authenticated:bool, user?:auth.User, scope?:string, tokenAuth:bool, language:string, setupHints?:[string], httpsPort:int}` (setupHints: where to find the token: log, `picache setup-token`, `docker exec -u 65532:65532 … /picache setup-token`; httpsPort: port of the bound HTTPS listener, 0 if none; the sign-in and setup pages link to it when opened over HTTP) |
| POST `/auth/setup` | P | `{setupToken, username, password}` | 200 `auth.User` + session and device cookies. 403 if the token is wrong, or at once if setup is already done (such a call uses no share of the global attempt limit but counts as a failed attempt of the client). Password ≥ 10 chars. |
| POST `/auth/login` | P | `{username, password, totp?}` | 200 `auth.User` + session and device cookies; 401 `unauthorized` (wrong credentials); 401 with `field:"totp"` when a TOTP code is required or wrong; 429 throttled (client locked out for 15 min after 5 failures; username delayed by up to 30 s per attempt from the 5th failure, unless the browser sends a device cookie issued for this username, which is locked for 15 min after 5 failures instead; global limit 10 attempts/s) |
| POST `/auth/logout` | R | – | 204, clears the session cookie (the device cookie stays) |
| GET `/auth/me` | R | – | `auth.User` |
| POST `/auth/password` | S | `{currentPassword, newPassword, keepTokens?:bool}` | 204. Other sessions are revoked, and all API tokens of the user unless `keepTokens:true`. 400 with `field:"currentPassword"` for a wrong password |
| GET `/auth/sessions` | S | – | `[]auth.SessionInfo` |
| DELETE `/auth/sessions/{id}` | S | – | 204 |
| POST `/auth/totp/begin` | S | `{currentPassword}` | `{secret, uri}` (render the URI as a QR code client-side); 400 with `field:"currentPassword"` for a missing or wrong password; 409 if TOTP is already on |
| POST `/auth/totp/confirm` | S | `{code}` | 204; the user's other sessions are revoked (they were created without a code) |
| POST `/auth/totp/disable` | S | `{password}` | 204 |
| GET `/tokens` | S | – | `[]auth.TokenInfo` |
| POST `/tokens` | S | `{name, scope:"read"|"admin", expiresInDays?:int, currentPassword}` | 201 `{token:string, info:auth.TokenInfo}` (token shown once; audit with `info` only); 400 with `field:"currentPassword"` for a missing or wrong password |
| DELETE `/tokens/{id}` | S | – | 204 |

## System — `routes_system.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/system/info` | R | – | `{version:version.Info, startedAt, uptimeSec, instanceId, listeners:api.ListenerInfo, dataDir, cacheDir, mountRoot, masterKeySource, memory:{allocBytes,sysBytes,limitBytes,numGC}, goroutines}` |
| GET `/system/health` | R | – | `api.Health` (check names: `listeners`, `upstreams`, `blocklists`, `dns-rate-limit`, `cache-domains`, `download_cache`, `sni`, `cache-store`, `logs`, `data-disk`, `network`; `network` warns while most DNS queries come from the router or the container network's gateway, see `GET /network/check`) |
| GET `/system/overview` | R | – | Top-bar/overview status in one call: `{blocking:dnsserver.BlockingStatus, dns:dnsserver.Stats, cacheIps:dnsserver.CacheIPStatus, router:dnsserver.RouterStatus, downloadCacheEnabled:bool, servicesReady:bool, store:api.StoreState, proxy:proxy.Stats, sni:sni.Stats, filter:filter.Stats, upstreams:[]upstream.UpstreamStat, clockGuard:bool, health:{ok:bool, warnings:int, failures:int}}` |
| GET `/system/audit` | A | `?search&limit&offset` | `listing.Page[auth.AuditEntry]` |
| GET `/system/backup` | A | `?includeSecrets=true` (sealed NAS passwords and notification secrets; useless without the master key) | `application/octet-stream` download `picache-backup-<date>.db`. Never contains accounts: users (password hashes, TOTP secrets), sessions and API tokens are removed; the audit log stays |
| POST `/system/restore` | S | raw body (`application/octet-stream`, ≤ 512 MiB); header `X-PiCache-Password`: the current password, percent-encoded as UTF-8 (JavaScript `encodeURIComponent`; ASCII passwords without `%` can be sent as they are) | 202 `{staged:true, message:"Restart PiCache to apply"}`; 401 with `field:"password"` for a missing or wrong password (nothing is staged; the session stays valid); 429 throttled; 400 for invalid or newer-version backups and for uploads with triggers, views, virtual tables, generated columns, tables or indexes the running PiCache does not have, indexes defined differently, or changed account tables. On the next start the restore keeps the running instance's accounts, API tokens and audit log and ends all sessions |
| POST `/system/restart` | A | – | 202; the process exits with code 75 and is restarted by systemd/Docker |
| GET `/system/update` | R | – | `{current:version.Info, currentIsDevBuild:bool, mode:"helper"|"docker"|"manual", checkEnabled:bool, includePrereleases:bool, latest?:{version, publishedAt, url, notes, prerelease:bool}, updateAvailable:bool, checkedAt?, checkError?:string, status?:{state, step, version, from, startedAt, finishedAt?, message?}, commands:{cli:string, docker?:string}}` (ARCHITECTURE 14). `update.Overview`. `latest` is the newest eligible release of the last check (kept when a later check fails; left out while it is a pre-release and pre-releases are off); `checkedAt`/`checkError` describe the last check. `status` (`update.Status`, left out if no update was ever queued) is the last run of the root helper: `state` one of `running`, `succeeded`, `failed`, `rolled-back`; `step` one of `download`, `verify`, `install`, `restart`, `health`, `rollback`, `done` (the failing step for `failed`, `rollback` for `rolled-back`); every run has a new `startedAt`; a request the helper has not claimed yet is `running`/`download` with a waiting `message` and becomes `failed` after 3 minutes. `commands.cli` is `sudo picache update --version <latest>` when an update is available, else `sudo picache update`; `commands.docker` only in mode `docker`. 503 if the process has no updater |
| POST `/system/update/check` | A | – | same as GET, after checking GitHub now (at most once per 30 s; faster calls return the last result) |
| POST `/system/update/apply` | S | `{version, currentPassword}` | 202 `{queued:true}`; 400 with `field:"currentPassword"` for a missing or wrong password (checked first, throttled like restore, audited as `auth.login_failed`; 429 while throttled); 409 if the mode is not `helper`, an update is already running, or `version` is not the available version of the last check. Audited as `system.update_queued` (target: the version, details `{from}`). The request only queues the update; follow it with `GET /system/update` |
| GET `/metrics` (no `/api/v1` prefix) | A token | – | Prometheus text (404 unless `web.metricsEnabled`) |

## Scheduled backups — `routes_backups.go`

Backups of `picache.db` at a local time of day (ARCHITECTURE 15.2), configured with `PATCH /settings/backups`. Same content as `GET /system/backup` (never accounts; sealed secrets only with `includeSecrets`). 503 if the process has no scheduler.

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/system/backups/scheduled` | R | – | `api.ScheduledBackupsOverview` `{settings:settings.Backups, last?:{time, ok:bool, error?, file?, sizeBytes?, destination}, next?, running:bool, timeZone:string, destinationPath:string, filesError?:string, files:[{name, sizeBytes, time}]}`. `last`: the last run (scheduled, catch-up or run now; kept across restarts), `time` = its start, `destination` = `local` or the target id at that time. `next`: the next run while `enabled` (a pending catch-up run if earlier). `running`: a run is in progress. `timeZone`: abbreviation of the host time zone that `settings.time` refers to (`CEST`, `UTC`; a container without `TZ` uses UTC). A run in the same second as the previous one is stamped one second later, so file names stay unique. `destinationPath`: the directory of the current destination as PiCache sees it (`""` for an unknown target). `files`: this installation's backups in the current destination, newest first, `time` from the file name (UTC); empty with `filesError` when the destination cannot be read (e.g. `the storage target "NAS" is not available: <reason>`, or no answer within 5 s) |
| POST `/system/backups/scheduled/run` | A | – | 202 `{started:true}`: runs in the background with the current settings (also while scheduled backups are disabled); follow it with GET. 409 while a run is going. Audited as `system.backup_scheduled_run` |
| GET `/system/backups/scheduled/files/{name}` | A | – | the file as `application/octet-stream` with `Content-Disposition: attachment; filename=<name>` and `Content-Length`. `name` must be `picache-backup-<instanceId>-<YYYYMMDDTHHMMSSZ>.db` of this installation (400 `field:"name"` otherwise), a regular file in the current destination (404 otherwise; 503 while the destination is offline). Audited as `system.backup_download` (target: the name) |
| DELETE `/system/backups/scheduled/files/{name}` | A | – | 204; same name rules. Audited as `system.backup_delete` (target: the name) |

## Notifications — `routes_notify.go`

Notification channels and the delivery log (ARCHITECTURE 15.1). Channel URLs may carry access tokens (webhook ids, `?auth=`), so channels and the log need admin rights; the event list does not. 503 if the process has no notifier (except `/notifications/events`).

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/notifications/channels` | A | – | `[]notify.Channel` (oldest first) |
| POST `/notifications/channels` | A | `notify.ChannelInput` | 201 `notify.Channel`. 400 with `field` `name`, `kind`, `url`, `minSeverity`, `events` or `secret` (Gotify without a token, invalid characters, too long); 409 with 10 channels. Audited as `notifications.channel.create` (target: id; the channel with the URL without its query string, never the secret) |
| PUT `/notifications/channels/{id}` | A | `notify.ChannelInput` (all members; an omitted `events` means all events, an omitted `enabled` false) | `notify.Channel`; 400 `field:"id"` for a malformed id, 404 unknown. 400 `field:"secret"` when a stored secret would be kept although `kind` or the URL's scheme, host or port changed: enter it again. Audited as `notifications.channel.update` (details: the channel and `secretChanged`) |
| DELETE `/notifications/channels/{id}` | A | – | 204; queued messages of the channel are dropped. Audited as `notifications.channel.delete` |
| POST `/notifications/channels/{id}/test` | A | – | `notify.TestResult` `{ok:bool, error?:string, status?:int, durationMs:int}`: sends the event `notify.test` once, synchronously (10 s), also to a disabled channel and regardless of its filters; `status` is the HTTP status if the server answered, `error` a text without the URL (e.g. `HTTP 401 Unauthorized`, `no answer within 10 seconds`, `HTTP 302 Found: redirects are not followed; use the final URL`). 429 while 2 other tests run. Logged in the delivery log and audited as `notifications.channel.test` (details `{ok, status}`) |
| GET `/notifications/events` | R | – | `[]notify.EventInfo` `[{key, severity, title, description}]`: the selectable events with their default severity (12, in this order: `health.failed`, `health.warning`, `health.recovered`, `storage.offline`, `storage.online`, `update.available`, `update.installed`, `update.failed`, `backup.failed`, `backup.succeeded`, `security.lockout`, `notify.test`) |
| GET `/notifications/log` | A | `?limit` (1–200, default 200) | `[]notify.LogEntry` `[{time, channelId, channelName, event, severity, title, ok:bool, error?, attempt}]`, newest first, the last 200 attempts since the start (memory only). `event` may also be `notify.dropped`: the summary sent after the rate limit dropped messages |

- `notify.Channel` = `{id (32 hex), name, kind:"webhook"|"ntfy"|"gotify", url, hasSecret:bool, enabled:bool, minSeverity:"info"|"warning"|"error", events:[string] (empty = all), createdAt, updatedAt}`. The secret is never returned.
- `notify.ChannelInput` = `{name, kind, url, secret?:string|null, enabled, minSeverity, events}`: `secret` absent or `null` keeps the stored secret, `""` removes it, a value replaces it (webhook: the whole `Authorization` header value, e.g. `Bearer abc`; ntfy: the access token; gotify: the application token, required). `minSeverity` `""` means `warning`. `name` 1–64 characters; `url` http/https, ≤ 2048 characters, printable ASCII, no user name or password, no fragment, not a link-local, multicast or unspecified IP address (private and loopback addresses are allowed); `events` known keys, duplicates removed.
- Delivery (ARCHITECTURE 15.1): up to 3 attempts (10 s and 60 s apart), at most 20 messages per channel in 10 minutes (then one `notify.dropped` summary), no redirects. Formats: webhook JSON `{event, severity, title, message, time, instance, hostname, version}`; ntfy text with `Title`, `Priority` (3/4/5) and `Tags: <event>,<severity>`; Gotify `POST <url>/message` `{title, message, priority}` (4/6/8).

## Settings — `routes_settings.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/settings` | R | – | `settings.All` |
| PUT `/settings` | A | `settings.All` (full document) | `settings.All`; 400 with `field` on validation errors |
| PATCH `/settings/{section}` | A | one section object (`dns`, `filter`, `downloadCache`, `cache`, `logs`, `web`, `updates`, `backups`) | `settings.All` |
| GET `/settings/defaults` | R | – | `settings.All` (defaults, for "reset" buttons) |

Changes to `cache.activeStoreId` via these endpoints are rejected (use `POST /storage/targets/{id}/activate`).

Section `updates` = `settings.Updates` `{checkEnabled:bool, includePrereleases:bool}` (defaults `true`, `false`): the daily release check and whether pre-releases are offered (ARCHITECTURE 14.3). Changing it starts a check (at most once per 30 s).

Section `backups` = `settings.Backups` `{enabled:bool, schedule:"daily"|"weekly", time:"HH:MM", weekday:0..6, keep:1..90, destination:"local"|<storage target id>, includeSecrets:bool}` (defaults `false`, `daily`, `03:30`, `0` = Sunday, `7`, `local`, `false`): scheduled backups (ARCHITECTURE 15.2). `time` is the local time of the host, exactly two digits each (`00:00`–`23:59`); `weekday` is used for `weekly` only; `destination` `local` is `<data>/backups/scheduled`, a storage target id (32 hex, not the built-in cache target) is `<its store root>/picache-backups`. 400 with `field` `backups.schedule`, `backups.time`, `backups.weekday`, `backups.keep` or `backups.destination` (malformed, or a changed destination that is not an existing storage target). `schedule` and `destination` are normalised to lower case.

## DNS: blocking, lookup, records, forwarders, clients, groups — `routes_dns.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/dns/blocking` | R | – | `dnsserver.BlockingStatus` |
| POST `/dns/blocking` | A | `{enabled:bool, pauseSeconds?:int}` (enabled=false + pauseSeconds>0 = timed pause) | `dnsserver.BlockingStatus` |
| POST `/dns/lookup` | R | `dnsserver.LookupRequest` | `dnsserver.LookupResult` |
| GET `/dns/stats` | R | – | `dnsserver.Stats` |
| GET `/dns/cache-ips` | R | – | `dnsserver.CacheIPStatus` (`warning` carries the address-detection warning and the would-be addresses are filled even while the download cache is disabled) |
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

## Parental controls — `routes_parental.go`

Blocked services, schedules and a manual override per client group (ARCHITECTURE 16). They apply in the DNS pipeline at step 7a, also while blocking is paused. 503 if the process has no parental engine (except `/parental/services`).

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/parental/services` | R | – | `[]parental.Service` `[{id, name, category, domains:[string]}]`: the built-in catalogue, sorted by category (`video`, `social`, `messaging`, `gaming`, `music`, `ai`), then by name; `domains` are subtree matches |
| GET `/parental/groups` | R | – | `[]parental.GroupControls` for all groups (id order), each with its `state` now |
| GET `/parental/groups/{id}` | R | – | `parental.GroupControls`; 400 `field:"id"` for a malformed id, 404 for an unknown group |
| PUT `/parental/groups/{id}` | A | `parental.Config` `{blockedServices:[id], schedules:[parental.Schedule]}` (exactly these two members; the override is kept) | `parental.GroupControls`. 400 with `field` `blockedServices` (unknown id, more than 64), `schedules` (more than 10), `schedules[<i>].name` (empty, more than 40 characters, control characters), `schedules[<i>].days` (none, outside 0–6), `schedules[<i>].start`, `schedules[<i>].end` (not `HH:MM`; `end` equal to `start`), `schedules[<i>].block` (not `all`/`services`), `schedules[<i>].services` (unknown id; none or more than 64 for `services`; any for `all`); 404 for an unknown group. Audited as `parental.update` (target: the group id, details: the stored configuration) |
| PUT `/parental/groups/{id}/override` | A | `{mode:"block"|"allow", minutes:int}` (1–10080) or `{mode, until:time}` (in the future, at most 7 days ahead) | `parental.GroupControls`. `block` blocks all internet for the group until then, `allow` lifts its blocked services and schedules (not those of the client's other groups); replaces an existing override. 400 with `field` `override.mode`, `minutes` (missing, out of range, or both `minutes` and `until`) or `override.until`; 404 for an unknown group. Audited as `parental.override` (details `{mode, until}`) |
| DELETE `/parental/groups/{id}/override` | A | – | `parental.GroupControls` (also without an override); 404 for an unknown group. Audited as `parental.override_clear` |

- `parental.GroupControls` = `{groupId, groupName, groupEnabled:bool, clientCount:int, blockedServices:[id], schedules:[parental.Schedule], override?:{mode, until}, state:parental.GroupState, updatedAt?}`. `override` is present only while it is active (an expired one is ignored and removed with the next write); `updatedAt` is absent for a group that was never configured. A disabled group never applies (`groupEnabled`).
- `parental.Schedule` = `{id, name, enabled:bool, days:[int], start:"HH:MM", end:"HH:MM", block:"all"|"services", services:[id]}`: `id` is 8 hex characters, assigned by the server for new schedules (absent or `""`) and kept on update (a malformed or duplicate id gets a new one); `days` 0 = Sunday … 6 = Saturday, stored sorted and unique; `end` before `start` means until `end` the next day (Friday 21:00–07:00 ends Saturday 07:00); times are the host's local time (a Docker container uses UTC unless `TZ` is set). Service lists are stored sorted and unique.
- `parental.GroupState` = `{blockAll:bool, reason?:"override"|"schedule", schedule?:string, until?, blockedServices:[id], lifted:bool, liftedUntil?, next?:{time, scheduleId, name, starts:bool}, timeZone:string, utcOffsetMinutes:int}`: `blockAll` = all internet is blocked now, by the override or by the enabled block-all schedule named in `schedule`; `until` = when that ends (windows that follow each other count as one block); `blockedServices` = the services blocked now (always blocked plus active service schedules; empty while `lifted`); `lifted`/`liftedUntil` = an `allow` override is active; `next` = the next start (`starts:true`) or end of an enabled schedule within 7 days; `timeZone`/`utcOffsetMinutes` = the host time zone that schedule times refer to (`CEST`, `120`), so the UI can show the plan on the host's clock.
- In the query log, parental blocks have the status `blocked-schedule` (a block-all schedule or a block override) or `blocked-service`, and the reason names the group: `Kids: Bedtime`, `Kids: blocked by hand`, `Kids: YouTube`, `Kids: YouTube (Homework time)`. `POST /dns/lookup` shows the step (`parental: blocked by …` or `parental: no restriction`).

## Network check — `routes_network.go`

Whether the devices of the LAN use PiCache (ARCHITECTURE 17). 503 if the process has no network check.

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/network/check` | R | – | `api.NetworkCheck` (below); computed at most every 30 s (every 2 s while a scan runs); starting or finishing a scan invalidates the cached check |
| POST `/network/scan` | A | – | 202 `{started:true, addresses:int}`: sends one empty UDP datagram to port 9 of each host address of this machine's private IPv4 subnets (/24 or smaller in full, larger ones only the /24 around this machine; at most 512; ≤ 200 per second) so that devices appear in the neighbour table; done 3 s after the last packet (follow `scan` in GET). 409 while a scan runs, 429 within 60 s after the previous start, 503 in a container bridge network, on systems other than Linux or without a private IPv4 subnet. Audited as `network.scan` (details `{addresses}`) |

- `api.NetworkCheck` = `{checkedAt, mode:"host"|"bridge", statsAvailable:bool, router?:{ipv4?, ipv6:[addr], mac?, name?, kind:"fritzbox"|"generic"|"unknown"}, self:{ipv4:[addr], ula:[addr], global:[addr], dnsIpv6:bool}, queries24h:{total, ipv4, ipv6, fromRouter}, checks:[{id, status:"ok"|"info"|"warn", data}], devices:[api.NetworkDevice], scan:{running:bool, startedAt?, finishedAt?, addresses?}}`. `mode` `bridge`: PiCache runs in a container bridge network (the router is not visible, `devices` is empty). `statsAvailable` false: the query counts come from the in-memory client activity (logs.db unavailable or client addresses anonymised). `router.ipv6`: the IPv6 default gateway and every neighbour address with the router's MAC; `router.name`: the gateway's PTR name. `self`: this machine's addresses without loopback, link-local and virtual bridges; `dnsIpv6`: a DNS listener serves IPv6. `queries24h` leaves out loopback and this machine's addresses. The response always has `router` (kind `unknown` without a gateway).
- `checks`, in this order: `router-forwarding` (in bridge mode `container-nat`, the bridge gateway being the router) `{routerQueries, totalQueries, share (0–1), routerAddresses:[addr] (most queries first)}`: with at least 200 queries, warn from 80 % from the router, info from 20 %; `ipv6-dns` `{lanHasIPv6:bool, ipv6Queries, ipv6Clients, ula:[addr], global:[addr]}`: warn when the LAN has IPv6 but no LAN device (the router not counted) asked over IPv6 in 24 h; `ipv6-address` `{ula, global}`: warn when the LAN has IPv6 and PiCache has no IPv6 address, info with global addresses only, ok with a ULA; `refused` `{sources:[{address, count, last}] (newest first, ≤ 20), since}`: warn when the DNS ACL dropped queries since the start; `devices` `{total, active, inactive, never}` (not in bridge mode): info when devices did not query in 24 h.
- `api.NetworkDevice` = `{mac, ips:[addr] (IPv4, then ULA, global, link-local), name?:string (configured client name, else the PTR name), clientId?:int, lastQuery?, queries24h:int, status:"active"|"inactive"|"never"}`: the neighbour table grouped by MAC without the router and this machine, sorted never, inactive, active, then by address; at most 1024.

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

## Download cache services & SNI — `routes_downloadcache.go`

The download services come from the cache-domains lists (uklans/cache-domains) plus custom services; the download cache answers their names in DNS with the cache address (status `override`).

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/download-cache/services` | R | – | `[]services.Service` (`domains` trimmed to the first 50 in the list view) |
| GET `/download-cache/services/{id}` | R | – | `services.Service` (full domains) |
| PUT `/download-cache/services/{id}/enabled` | A | `{enabled:bool}` | `services.Service` |
| PUT `/download-cache/services/{id}/domains` | A | `{extraDomains:[string]}` | `services.Service` (400 with the offending pattern) |
| POST `/download-cache/services` | A | `services.ServiceInput` | 201 (custom service) |
| PUT `/download-cache/services/{id}` | A | `services.ServiceInput` | 200 (custom only) |
| DELETE `/download-cache/services/{id}` | A | – | 204 (custom only) |
| GET `/download-cache/source` | R | – | `services.SourceStatus` |
| POST `/download-cache/source/refresh` | A | – | `services.SourceStatus` (waits up to 5 min; `extendDeadlines`) |
| PUT `/download-cache/labels` | A | `{groupKey, label}` ("" removes) | 204 |
| GET `/download-cache/sni` | R | – | `sni.Stats` |

## Cache store (library, maintenance) — `routes_cache.go`

All return 503 `unavailable` when no store is online (except `/cache/state`).

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/cache/state` | R | – | `api.StoreState` |
| GET `/cache/services` | R | – | `[]cachestore.ServiceUsage` |
| GET `/cache/groups` | R | `?service&search&sort&desc&limit&offset` | `listing.Page[GroupView]`, `GroupView = cachestore.GroupUsage + {label:string, userLabel:bool, clients:int}` (`userLabel`: the label was set by a user); `hits`/`bytesServed` count only bytes served from the cache; search also matches labels (`services.SearchLabels` → `GroupQuery.SearchKeys`); client counts via `logs.GroupClientCounts` |
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
| DELETE `/storage/targets/{id}` | A | – | 204 (not local, not active); 409 while it is the destination of scheduled backups (`backups.destination`) |
| POST `/storage/targets/{id}/test` | A | – | `storage.TestResult` (`extendDeadlines` 2 min) |
| POST `/storage/targets/{id}/apply` | A | – | `storage.Status` with `applyState:"queued"` (503 if the root helper is not installed) |
| POST `/storage/targets/{id}/init` | A | `{adopt:bool}` | `storage.InitResult` |
| POST `/storage/targets/{id}/activate` | A | – | `api.StoreState` (409 `not initialised` → call init first) |
| GET `/storage/targets/{id}/snippets` | A | – | `storage.Snippets` (never contains the password) |
| POST `/storage/targets/{id}/benchmark` | A | `{sizeMiB?}` — 64, 256 or 1024; default 256 (the body may be empty) | 202 `storage.BenchmarkRun` (`state:"running"`). 400 `field:"sizeMiB"` for other sizes; 400 `not enough free space: the test needs <size + 1 GiB>, <free> are free`; 404 unknown target; 409 `a speed test is already running`, the target is being initialised or tested, or `the storage target is not available: <reason>` (the functional test's fresh check). `extendDeadlines` 2 min. Audited as `storage.benchmark` (details `{sizeMiB}`) |
| GET `/storage/benchmark` | R | – | `storage.BenchmarkOverview` = `{run?: BenchmarkRun, last: {[targetId]: BenchmarkResult}}`: `run` is the running or most recent run (kept until the next one starts), `last` the last completed result per target. In memory only (empty after a restart) |
| DELETE `/storage/benchmark` | A | – | 204; cancels a running speed test (no-op if none) and waits up to 5 s for it to stop. The run ends `cancelled`; its partial result stays in `run.result`. Audited as `storage.benchmark_cancel` when a run was cancelled |

Speed test (one run at a time, in the background, not tied to the request; docs/ARCHITECTURE.md 10.6). Sizes are bytes, speeds bytes/s (the UI shows decimal MB/s):
- `BenchmarkRun {state: running|done|failed|cancelled, targetId, sizeMiB, phase: prepare|write|read|slices|metadata|cleanup|done, progress (0..1 over the whole run), startedAt, finishedAt?, error? (failed, user-facing), result? (done; partial after a cancel or failure once a phase finished)}`
- `BenchmarkResult {targetId, fsType ("" if unknown), storeRoot, testedAt, sizeBytes (written; less than requested when the write budget ran out), freeBytes (before the test; 0 if unknown), write: Throughput (1 MiB blocks, time includes the final fsync), read: Throughput + {cacheDropped} (after dropping the file from the page cache), slices?: Throughput + {count, p50Ms, p95Ms} (cached slices of the active store; seconds = sum of the per-slice times open → close), metadata: {ops, p50Ms, p95Ms, maxMs} (create 4 KiB + fsync + rename + delete), notes: [string]}`
- `Throughput {bytes, seconds, bytesPerSec}`

## Logs, statistics, downloads, streams — `routes_logs.go`

| Method & path | P | Request | Response |
|---|---|---|---|
| GET `/logs/queries` | R | `?from&to&range&client&domain&status(multi)&qtype&upstream&cursor&limit` (default range 1h; `status`: `forwarded`, `cached`, `stale`, `local`, `special`, `override`, `blocked-list`, `blocked-rule`, `blocked-regex`, `blocked-cname`, `blocked-special`, `blocked-schedule`, `blocked-service`, `refused`, `error`, or a series class; the class `blocked` covers every `blocked-*` status) | `logs.QueryPage` |
| GET `/stats/summary` | R | `?range` (default 24h) | `logs.Summary` (`topFrom`: the hour-aligned start that top lists and `activeClients` actually cover) |
| GET `/stats/dns` | R | `?range&step` (step in seconds; default so there are ≤ 300 points, never finer than the rollup: 60 s for ranges ≤ 48 h, 3600 s beyond; > 1500 points → 400) | `logs.Series` (keys `allowed`, `cached`, `override`, `blocked`, `other`) |
| GET `/stats/cache` | R | `?range&step&service` | `logs.Series` |
| GET `/stats/top` | R | `?kind=domains|blocked|clients|cache-clients|content|upstreams&range&limit` | `[]logs.TopItem` |
| GET `/stats/services` | R | `?range` | `[]logs.ServiceStat` |
| GET `/stats/clients` | R | `?range` | `[]logs.ClientStat` |
| GET `/cache/downloads` | R | `?from&to&range&client&service&group&search&active&limit&offset` | `listing.Page[logs.Download]` |
| GET `/cache/requests` | R | `?from&to&range&client&service&search&status&cursor&limit` | `listing.Page[logs.CacheEvent]` |
| GET `/cache/sni-events` | R | same | `listing.Page[logs.SNIEvent]` |
| GET `/cache/evictions` | R | same (`status` = reason) | `listing.Page[logs.EvictionEvent]` |
| GET `/stream/queries` | R | `?client&status` (server-side filter) | SSE `event: query`, data `logs.QueryEvent` (`id` is 0 in the live feed; `seq` is a positive per-process sequence for keying rows) |
| GET `/stream/cache` | R | – | SSE `event: request`, data `logs.CacheEvent` (`seq` as above) |

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
- Never return secrets (NAS password, notification secrets, token secrets except once at creation, TOTP secret except at begin).
- Validate path IDs with `pathID(r, "id")`; parse ranges with `qRange(r, 24*time.Hour)`.
