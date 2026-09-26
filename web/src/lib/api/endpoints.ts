// One typed function per endpoint of docs/API.md. Every function takes an
// optional trailing ReqOpts ({signal}) for cancellation.

import { apiUrl, http, seg, type ReqOpts } from './client'
import type * as T from './types'

const LONG = 330_000 // endpoints that wait up to 5 min (refresh, source)

// ---------------------------------------------------------------- auth & account

const auth = {
  /** Public: setup/login state, UI language and setup hints. */
  status: (o?: ReqOpts) => http.get<T.AuthStatus>('/auth/status', { ...o, allowUnauthorized: true }),
  setup: (body: { setupToken: string; username: string; password: string }, o?: ReqOpts) =>
    http.post<T.User>('/auth/setup', body, { ...o, allowUnauthorized: true }),
  /** 401 with field "totp" means: ask for the authenticator code and retry with `totp`. */
  login: (body: { username: string; password: string; totp?: string }, o?: ReqOpts) =>
    http.post<T.User>('/auth/login', body, { ...o, allowUnauthorized: true }),
  logout: (o?: ReqOpts) => http.post<void>('/auth/logout', undefined, { ...o, allowUnauthorized: true }),
  me: (o?: ReqOpts) => http.get<T.User>('/auth/me', o),
  /** Signs out the other sessions and, unless keepTokens, revokes the API tokens. */
  changePassword: (body: { currentPassword: string; newPassword: string; keepTokens?: boolean }, o?: ReqOpts) =>
    http.post<void>('/auth/password', body, o),
  sessions: (o?: ReqOpts) => http.get<T.SessionInfo[]>('/auth/sessions', o),
  revokeSession: (id: string, o?: ReqOpts) => http.del(`/auth/sessions/${seg(id)}`, o),
  /** 400 with field "currentPassword" for a missing or wrong password. */
  totpBegin: (currentPassword: string, o?: ReqOpts) =>
    http.post<T.TotpBegin>('/auth/totp/begin', { currentPassword }, o),
  /** Signs out the other sessions. */
  totpConfirm: (code: string, o?: ReqOpts) => http.post<void>('/auth/totp/confirm', { code }, o),
  totpDisable: (password: string, o?: ReqOpts) => http.post<void>('/auth/totp/disable', { password }, o),
}

const tokens = {
  list: (o?: ReqOpts) => http.get<T.TokenInfo[]>('/tokens', o),
  /** 400 with field "currentPassword" for a missing or wrong password. */
  create: (body: { name: string; scope: T.Scope; expiresInDays?: number; currentPassword: string }, o?: ReqOpts) =>
    http.post<T.CreatedToken>('/tokens', body, o),
  remove: (id: number, o?: ReqOpts) => http.del(`/tokens/${seg(id)}`, o),
}

// ---------------------------------------------------------------- system

const system = {
  info: (o?: ReqOpts) => http.get<T.SystemInfo>('/system/info', o),
  health: (o?: ReqOpts) => http.get<T.Health>('/system/health', o),
  overview: (o?: ReqOpts) => http.get<T.SystemOverview>('/system/overview', o),
  audit: (q: { search?: string; limit?: number; offset?: number } = {}, o?: ReqOpts) =>
    http.get<T.Page<T.AuditEntry>>('/system/audit', { ...o, query: q }),
  /** URL for a plain <a href download> (the browser sends the session cookie). */
  backupUrl: (includeSecrets = false) => apiUrl('/system/backup', { includeSecrets: includeSecrets || undefined }),
  /**
   * Needs the current password (header X-PiCache-Password, percent-encoded
   * because header values cannot carry arbitrary Unicode); 401 with field
   * "password" when it is missing or wrong.
   */
  restore: (file: Blob, password: string, o?: ReqOpts) =>
    http.post<T.RestoreResult>('/system/restore', undefined, {
      ...o,
      raw: file,
      timeoutMs: 600_000,
      headers: { 'X-PiCache-Password': encodeURIComponent(password) },
    }),
  /** 202; the process exits and systemd/Docker restarts it (poll /auth/status until it answers again). */
  restart: (o?: ReqOpts) => http.post<T.RestartResult>('/system/restart', undefined, o),
  /** Running version, the last check result, the install mode and the progress of an update. */
  update: (o?: ReqOpts) => http.get<T.UpdateInfo>('/system/update', o),
  /** Checks GitHub now (at most once per 30 s; faster calls return the last result). */
  checkUpdate: (o?: ReqOpts) =>
    http.post<T.UpdateInfo>('/system/update/check', undefined, { ...o, timeoutMs: 60_000 }),
  /**
   * Queues the update to `version` (the available version of the last check)
   * for the root helper. 400 with field "currentPassword" for a missing or
   * wrong password; 409 when the mode is not `helper`, an update is running or
   * the version is not the available one.
   */
  applyUpdate: (body: { version: string; currentPassword: string }, o?: ReqOpts) =>
    http.post<T.UpdateQueued>('/system/update/apply', body, o),
  /** The host sample of the last health evaluation (every 60 s). */
  host: (o?: ReqOpts) => http.get<T.HostInfo>('/system/host', o),
  /** Sizes of picache.db, logs.db and the cache indexes. */
  databases: (o?: ReqOpts) => http.get<T.DatabaseInfo>('/system/databases', o),
  /** Application log (admins): records newest first (400 with field level, component or limit). */
  log: (q: { level?: T.LogLevelFilter; component?: string; limit?: number } = {}, o?: ReqOpts) =>
    http.get<T.SystemLog>('/system/log', { ...o, query: q }),
  /** Turns on a more verbose level for a while (replaces an active one). */
  setLogLevel: (body: T.LogLevelInput, o?: ReqOpts) => http.put<T.LogLevelState>('/system/log/level', body, o),
  /** Ends the temporary level at once (also without one). */
  clearLogLevel: (o?: ReqOpts) => http.del('/system/log/level', o),
  /** Warning history, newest lastTime first (viewers never see security.* entries). */
  events: (q: T.HistoryQuery = {}, o?: ReqOpts) => http.get<T.Page<T.HistoryEvent>>('/system/events', { ...o, query: { ...q } }),
  /** Idempotent; 404 for an unknown id. */
  ackEvent: (id: number, o?: ReqOpts) => http.post<T.HistoryEvent>(`/system/events/${seg(id)}/ack`, undefined, o),
  ackAllEvents: (o?: ReqOpts) => http.post<{ acknowledged: number }>('/system/events/ack-all', undefined, o),
  /**
   * A zip of redacted diagnostics (built in memory within 30 s). 400 with
   * field "currentPassword" for a missing or wrong password; 429 while
   * another bundle is built or after too many wrong passwords.
   */
  supportBundle: (body: T.SupportBundleInput, o?: ReqOpts) =>
    http.postFile('/system/support-bundle', body, { ...o, timeoutMs: 90_000 }),
}

/** Scheduled backups (settings: PATCH /settings/backups). */
const backups = {
  /** Settings, the last run, the next run and the stored files of the current destination. */
  scheduled: (o?: ReqOpts) => http.get<T.ScheduledBackups>('/system/backups/scheduled', o),
  /** 202 {started:true}; 409 while a backup is running. */
  run: (o?: ReqOpts) => http.post<{ started: boolean }>('/system/backups/scheduled/run', undefined, o),
  /** URL for a plain <a href download> of a stored scheduled backup. */
  fileUrl: (name: string) => apiUrl(`/system/backups/scheduled/files/${seg(name)}`),
  removeFile: (name: string, o?: ReqOpts) => http.del(`/system/backups/scheduled/files/${seg(name)}`, o),
}

/**
 * Accounts (admin sessions only). Every change needs the caller's password
 * (400 with field "currentPassword"); 409 keeps at least one admin and at
 * most 32 accounts. Changing your own role or deleting yourself ends your
 * sessions (the server clears the cookies).
 */
const users = {
  /** Oldest first. */
  list: (o?: ReqOpts) => http.get<T.User[]>('/system/users', o),
  /** 400 with field username, password or role; 409 for a name that exists (any case). */
  create: (body: T.UserCreate, o?: ReqOpts) => http.post<T.User>('/system/users', body, o),
  /** Another user's password signs them out and revokes their API tokens; disableTotp signs them out. */
  update: (id: number, body: T.UserUpdate, o?: ReqOpts) => http.put<T.User>(`/system/users/${seg(id)}`, body, o),
  remove: (id: number, currentPassword: string, o?: ReqOpts) =>
    http.del(`/system/users/${seg(id)}`, { ...o, body: { currentPassword } }),
}

/** The HTTPS certificate of the web UI. */
const tls = {
  status: (o?: ReqOpts) => http.get<T.TlsStatus>('/system/tls', o),
  /**
   * Uploads a certificate chain (PEM, leaf first) and its unencrypted key;
   * only over HTTPS. 400 with field certPem, keyPem, currentPassword or body;
   * 409 without an HTTPS listener or while PICACHE_WEB_TLS_CERT is set.
   */
  upload: (body: { certPem: string; keyPem: string; currentPassword: string }, o?: ReqOpts) =>
    http.put<T.TlsStatus>('/system/tls', body, o),
  /** Deletes the uploaded certificate (404 without one); PiCache serves the next source. */
  remove: (o?: ReqOpts) => http.del<T.TlsStatus>('/system/tls', o),
  /** Creates (or replaces) the local CA and a new certificate from it. */
  createLocalCa: (currentPassword: string, o?: ReqOpts) =>
    http.post<T.TlsStatus>('/system/tls/local-ca', { currentPassword }, o),
  /** URL for a plain <a href download> of the local CA certificate (public). */
  caUrl: () => apiUrl('/system/tls/ca.crt'),
}

// ---------------------------------------------------------------- notifications

const notifications = {
  channels: {
    list: (o?: ReqOpts) => http.get<T.NotifyChannel[]>('/notifications/channels', o),
    /** 400 with field name, kind, url, minSeverity, events or secret (gotify without a token). */
    create: (c: T.NotifyChannelInput, o?: ReqOpts) => http.post<T.NotifyChannel>('/notifications/channels', c, o),
    update: (id: string, c: T.NotifyChannelInput, o?: ReqOpts) =>
      http.put<T.NotifyChannel>(`/notifications/channels/${seg(id)}`, c, o),
    remove: (id: string, o?: ReqOpts) => http.del(`/notifications/channels/${seg(id)}`, o),
    /** Sends a test message now (ignores the channel's filters; the server waits up to 10 s). */
    test: (id: string, o?: ReqOpts) =>
      http.post<T.NotifyTestResult>(`/notifications/channels/${seg(id)}/test`, undefined, { ...o, timeoutMs: 30_000 }),
  },
  /** Events PiCache can notify about, with titles and descriptions. */
  events: (o?: ReqOpts) => http.get<T.NotifyEvent[]>('/notifications/events', o),
  /** The last delivery attempts, newest first (limit ≤ 200). */
  log: (limit?: number, o?: ReqOpts) => http.get<T.NotifyDelivery[]>('/notifications/log', { ...o, query: { limit } }),
}

// ---------------------------------------------------------------- settings

const settings = {
  get: (o?: ReqOpts) => http.get<T.Settings>('/settings', o),
  put: (all: T.Settings, o?: ReqOpts) => http.put<T.Settings>('/settings', all, o),
  /**
   * Updates one section: members sent replace the current ones (arrays are
   * replaced), omitted members keep their values. Returns the complete settings;
   * validation errors name the member in `field` (e.g. "dns.upstreams[1]").
   */
  patch: <S extends T.SettingsSection>(section: S, value: Partial<T.Settings[S]>, o?: ReqOpts) =>
    http.patch<T.Settings>(`/settings/${seg(section)}`, value, o),
  defaults: (o?: ReqOpts) => http.get<T.Settings>('/settings/defaults', o),
}

// ---------------------------------------------------------------- dns

const dns = {
  blocking: (o?: ReqOpts) => http.get<T.BlockingStatus>('/dns/blocking', o),
  /** enabled=false with pauseSeconds > 0 pauses; enabled=false alone disables until re-enabled. */
  setBlocking: (enabled: boolean, pauseSeconds?: number, o?: ReqOpts) =>
    http.post<T.BlockingStatus>('/dns/blocking', pauseSeconds ? { enabled, pauseSeconds } : { enabled }, o),
  lookup: (req: T.LookupRequest, o?: ReqOpts) => http.post<T.LookupResult>('/dns/lookup', req, o),
  stats: (o?: ReqOpts) => http.get<T.DnsStats>('/dns/stats', o),
  cacheIps: (o?: ReqOpts) => http.get<T.CacheIPStatus>('/dns/cache-ips', o),
  router: (o?: ReqOpts) => http.get<T.RouterStatus>('/dns/router', o),
  records: {
    list: (o?: ReqOpts) => http.get<T.DnsRecord[]>('/dns/records', o),
    create: (r: T.DnsRecordInput, o?: ReqOpts) => http.post<T.DnsRecord>('/dns/records', r, o),
    update: (id: number, r: T.DnsRecordInput, o?: ReqOpts) => http.put<T.DnsRecord>(`/dns/records/${seg(id)}`, r, o),
    remove: (id: number, o?: ReqOpts) => http.del(`/dns/records/${seg(id)}`, o),
  },
  forwarders: {
    list: (o?: ReqOpts) => http.get<T.Forwarder[]>('/dns/forwarders', o),
    create: (f: T.ForwarderInput, o?: ReqOpts) => http.post<T.Forwarder>('/dns/forwarders', f, o),
    update: (id: number, f: T.ForwarderInput, o?: ReqOpts) =>
      http.put<T.Forwarder>(`/dns/forwarders/${seg(id)}`, f, o),
    remove: (id: number, o?: ReqOpts) => http.del(`/dns/forwarders/${seg(id)}`, o),
    /** Checks (dryRun) or applies `[/domain/…/]target …` lines; per-line errors in the 200 answer, nothing written if there is any. */
    import: (req: T.ForwarderImportRequest, o?: ReqOpts) =>
      http.post<T.ForwarderImportResult>('/dns/forwarders/import', req, o),
  },
  /** dns.blockedClients one entry at a time (400 field "client"/"entry" also for a lockout; 409 when the list is full). */
  blockedClients: {
    /** With `device`, an IP address whose MAC address is known is stored as that MAC. Idempotent: `added` false when an entry already matches. */
    add: (client: string, device = false, o?: ReqOpts) =>
      http.post<T.BlockClientResult>('/dns/blocked-clients', device ? { client, device } : { client }, o),
    /** 404 when the entry is not stored. */
    remove: (entry: string, o?: ReqOpts) =>
      http.del<{ blockedClients: string[] }>('/dns/blocked-clients', { ...o, query: { entry } }),
  },
}

const upstreams = {
  get: (o?: ReqOpts) => http.get<T.UpstreamsState>('/dns/upstreams', o),
  test: (upstream: string, o?: ReqOpts) =>
    http.post<T.UpstreamTestResult>('/dns/upstreams/test', { upstream }, { ...o, timeoutMs: 60_000 }),
  flushCache: (o?: ReqOpts) => http.post<void>('/dns/cache/flush', undefined, o),
}

const clients = {
  list: (o?: ReqOpts) => http.get<T.Client[]>('/clients', o),
  create: (c: T.ClientInput, o?: ReqOpts) => http.post<T.Client>('/clients', c, o),
  update: (id: number, c: T.ClientInput, o?: ReqOpts) => http.put<T.Client>(`/clients/${seg(id)}`, c, o),
  remove: (id: number, o?: ReqOpts) => http.del(`/clients/${seg(id)}`, o),
  /** Addresses seen within `within` (e.g. "30d"). */
  known: (within?: string, o?: ReqOpts) => http.get<T.KnownClient[]>('/clients/known', { ...o, query: { within } }),
}

const groups = {
  list: (o?: ReqOpts) => http.get<T.ClientGroup[]>('/groups', o),
  create: (g: T.ClientGroupInput, o?: ReqOpts) => http.post<T.ClientGroup>('/groups', g, o),
  update: (id: number, g: T.ClientGroupInput, o?: ReqOpts) => http.put<T.ClientGroup>(`/groups/${seg(id)}`, g, o),
  /** 403 for the Default group. */
  remove: (id: number, o?: ReqOpts) => http.del(`/groups/${seg(id)}`, o),
}

// ---------------------------------------------------------------- parental controls & network check

const parental = {
  /** The built-in service catalogue. */
  services: (o?: ReqOpts) => http.get<T.ParentalService[]>('/parental/services', o),
  /** Every group (id order) with its restrictions and current state. */
  groups: (o?: ReqOpts) => http.get<T.GroupControls[]>('/parental/groups', o),
  group: (id: number, o?: ReqOpts) => http.get<T.GroupControls>(`/parental/groups/${seg(id)}`, o),
  /**
   * Replaces blocked services, schedules, safe search and the category
   * switches (list assignments). 400 with field blockedServices, schedules,
   * schedules[<i>].name|days|start|end|block|services or safeSearch.youtube;
   * 409 when a switch's list is an allowlist or the list limit is reached.
   */
  update: (id: number, c: T.GroupControlsInput, o?: ReqOpts) =>
    http.put<T.GroupControls>(`/parental/groups/${seg(id)}`, c, o),
  /** Blocks all internet or lifts the restrictions for a while (400 with field override.mode|override.until|minutes). */
  setOverride: (id: number, body: T.OverrideInput, o?: ReqOpts) =>
    http.put<T.GroupControls>(`/parental/groups/${seg(id)}/override`, body, o),
  /** Ends the override: the plan applies again. */
  clearOverride: (id: number, o?: ReqOpts) => http.del<T.GroupControls>(`/parental/groups/${seg(id)}/override`, o),
  /** Pauses the group's filtering (replaces a pause; 400 with field minutes or pause.until). */
  pause: (id: number, body: T.PauseInput, o?: ReqOpts) =>
    http.put<T.GroupControls>(`/parental/groups/${seg(id)}/pause`, body, o),
  /** Ends the pause (also without one). */
  resume: (id: number, o?: ReqOpts) => http.del<T.GroupControls>(`/parental/groups/${seg(id)}/pause`, o),
}

const network = {
  /** Router set-up checks and the devices of the neighbour table (computed at most every 30 s). */
  check: (o?: ReqOpts) => http.get<T.NetworkCheck>('/network/check', o),
  /** 202; 409 while a scan runs, 429 within 60 s of the last one, 503 where scanning is not possible. */
  scan: (o?: ReqOpts) => http.post<T.NetworkScanStarted>('/network/scan', undefined, o),
}

// ---------------------------------------------------------------- dhcp server

/** The optional DHCP server (settings: PATCH /settings/dhcp). */
const dhcp = {
  status: (o?: ReqOpts) => http.get<T.DhcpStatus>('/dhcp', o),
  /** Interfaces of this machine with their addresses (virtual ones marked). */
  interfaces: (o?: ReqOpts) => http.get<T.DhcpInterface[]>('/dhcp/interfaces', o),
  /** Looks for other DHCP servers for 3 s. 429 within 10 s of the last search, 503 when unavailable. */
  probe: (o?: ReqOpts) => http.post<T.DhcpProbeResult>('/dhcp/probe', undefined, { ...o, timeoutMs: 30_000 }),
  /** Active and recently expired leases, newest first. */
  leases: (o?: ReqOpts) => http.get<T.DhcpLease[]>('/dhcp/leases', o),
  /** Ends a dynamic lease; the device gets a new one when it renews. */
  endLease: (mac: string, o?: ReqOpts) => http.del(`/dhcp/leases/${seg(mac)}`, o),
  /** Ends every lease (also on reserved addresses) and forgets pending offers. */
  endAllLeases: (o?: ReqOpts) => http.del<{ deleted: number }>('/dhcp/leases', o),
  /** Settings section dhcp back to its defaults (switches the server off), every reservation and lease deleted. */
  reset: (o?: ReqOpts) => http.post<void>('/dhcp/reset', undefined, o),
  /** The last handled exchanges, newest first (limit 1–200, default 200). */
  log: (limit?: number, o?: ReqOpts) => http.get<T.DhcpLogEntry[]>('/dhcp/log', { ...o, query: { limit } }),
  static: {
    list: (o?: ReqOpts) => http.get<T.DhcpStaticLease[]>('/dhcp/static', o),
    /** 400 with field mac, ip, hostname, comment, clientId or leaseSeconds. */
    create: (s: T.DhcpStaticLeaseInput, o?: ReqOpts) => http.post<T.DhcpStaticLease>('/dhcp/static', s, o),
    update: (mac: string, s: Omit<T.DhcpStaticLeaseInput, 'mac'>, o?: ReqOpts) =>
      http.put<T.DhcpStaticLease>(`/dhcp/static/${seg(mac)}`, s, o),
    remove: (mac: string, o?: ReqOpts) => http.del(`/dhcp/static/${seg(mac)}`, o),
    /** URL for a plain <a href> download of every reservation (the browser sends the session cookie). */
    exportUrl: (format: 'csv' | 'hosts') => apiUrl('/dhcp/static/export', { format }),
    /** Checks (dryRun) or applies a list; per-line errors in the 200 answer, nothing written if there is any. */
    import: (req: T.DhcpImportRequest, o?: ReqOpts) => http.post<T.DhcpImportResult>('/dhcp/static/import', req, o),
  },
}

// ---------------------------------------------------------------- filtering

const filter = {
  lists: {
    list: (o?: ReqOpts) => http.get<T.FilterList[]>('/filter/lists', o),
    create: (l: T.FilterListInput, o?: ReqOpts) => http.post<T.FilterList>('/filter/lists', l, o),
    update: (id: number, l: T.FilterListInput, o?: ReqOpts) => http.put<T.FilterList>(`/filter/lists/${seg(id)}`, l, o),
    remove: (id: number, o?: ReqOpts) => http.del(`/filter/lists/${seg(id)}`, o),
    /** Downloads one list now (waits up to 5 min). */
    refresh: (id: number, o?: ReqOpts) =>
      http.post<T.FilterList>(`/filter/lists/${seg(id)}/refresh`, undefined, { ...o, timeoutMs: LONG }),
    /** Starts a background refresh of all lists. */
    refreshAll: (o?: ReqOpts) => http.post<{ started: boolean }>('/filter/lists/refresh', undefined, o),
  },
  catalog: (o?: ReqOpts) => http.get<T.CatalogEntry[]>('/filter/catalog', o),
  rules: {
    list: (q: T.RuleQuery = {}, o?: ReqOpts) => http.get<T.FilterRule[]>('/filter/rules', { ...o, query: { ...q } }),
    create: (r: T.FilterRuleInput, o?: ReqOpts) => http.post<T.FilterRule>('/filter/rules', r, o),
    update: (id: number, r: T.FilterRuleInput, o?: ReqOpts) => http.put<T.FilterRule>(`/filter/rules/${seg(id)}`, r, o),
    remove: (id: number, o?: ReqOpts) => http.del(`/filter/rules/${seg(id)}`, o),
  },
  stats: (o?: ReqOpts) => http.get<T.FilterStats>('/filter/stats', o),
  /** "Why is this blocked?" for a domain, optionally as a specific client. */
  explain: (domain: string, clientIp?: string, o?: ReqOpts) =>
    http.post<T.ExplainResult>('/filter/explain', clientIp ? { domain, clientIp } : { domain }, o),
}

// ---------------------------------------------------------------- download cache services & sni

const downloadCache = {
  /** Domains are trimmed to the first 50 per service; use service(id) for all. */
  services: (o?: ReqOpts) => http.get<T.DownloadCacheService[]>('/download-cache/services', o),
  service: (id: string, o?: ReqOpts) => http.get<T.DownloadCacheService>(`/download-cache/services/${seg(id)}`, o),
  setEnabled: (id: string, enabled: boolean, o?: ReqOpts) =>
    http.put<T.DownloadCacheService>(`/download-cache/services/${seg(id)}/enabled`, { enabled }, o),
  setExtraDomains: (id: string, extraDomains: string[], o?: ReqOpts) =>
    http.put<T.DownloadCacheService>(`/download-cache/services/${seg(id)}/domains`, { extraDomains }, o),
  createService: (s: T.DownloadCacheServiceInput, o?: ReqOpts) => http.post<T.DownloadCacheService>('/download-cache/services', s, o),
  updateService: (id: string, s: T.DownloadCacheServiceInput, o?: ReqOpts) =>
    http.put<T.DownloadCacheService>(`/download-cache/services/${seg(id)}`, s, o),
  deleteService: (id: string, o?: ReqOpts) => http.del(`/download-cache/services/${seg(id)}`, o),
  source: (o?: ReqOpts) => http.get<T.SourceStatus>('/download-cache/source', o),
  refreshSource: (o?: ReqOpts) =>
    http.post<T.SourceStatus>('/download-cache/source/refresh', undefined, { ...o, timeoutMs: LONG }),
  /** Sets a display label for a content group ("" removes it). */
  setLabel: (groupKey: string, label: string, o?: ReqOpts) => http.put<void>('/download-cache/labels', { groupKey, label }, o),
  sni: (o?: ReqOpts) => http.get<T.SniStats>('/download-cache/sni', o),
}

// ---------------------------------------------------------------- cache store, proxy, downloads

const cache = {
  state: (o?: ReqOpts) => http.get<T.StoreState>('/cache/state', o),
  services: (o?: ReqOpts) => http.get<T.ServiceUsage[]>('/cache/services', o),
  groups: (q: T.GroupQuery = {}, o?: ReqOpts) => http.get<T.Page<T.GroupView>>('/cache/groups', { ...o, query: { ...q } }),
  groupDetail: (service: string, key: string, o?: ReqOpts) =>
    http.get<T.GroupDetail>('/cache/groups/detail', { ...o, query: { service, key } }),
  objects: (q: T.ObjectQuery = {}, o?: ReqOpts) =>
    http.get<T.Page<T.CacheObject>>('/cache/objects', { ...o, query: { ...q } }),
  deleteObject: (id: string, o?: ReqOpts) => http.del(`/cache/objects/${seg(id)}`, o),
  pinObject: (id: string, pinned: boolean, o?: ReqOpts) =>
    http.post<void>(`/cache/objects/${seg(id)}/pin`, { pinned }, o),
  deleteGroup: (service: string, key: string, o?: ReqOpts) =>
    http.post<T.BytesFreed>('/cache/groups/delete', { service, key }, o),
  pinGroup: (service: string, key: string, pinned: boolean, o?: ReqOpts) =>
    http.post<void>('/cache/groups/pin', { service, key, pinned }, o),
  purgeService: (service: string, o?: ReqOpts) =>
    http.post<T.BytesFreed>(`/cache/services/${seg(service)}/purge`, undefined, { ...o, timeoutMs: LONG }),
  evict: (o?: ReqOpts) => http.post<T.EvictResult>('/cache/evict', undefined, { ...o, timeoutMs: LONG }),
  verify: (repair: boolean, o?: ReqOpts) => http.post<T.VerifyState>('/cache/verify', { repair }, o),
  verifyState: (o?: ReqOpts) => http.get<T.VerifyState>('/cache/verify', o),

  /** Live downloads per client + content (overview). */
  live: (o?: ReqOpts) => http.get<T.ActiveDownload[]>('/cache/live', o),
  /** Individual live requests. */
  active: (o?: ReqOpts) => http.get<T.Transfer[]>('/cache/active', o),
  proxyStats: (o?: ReqOpts) => http.get<T.ProxyStats>('/cache/proxy/stats', o),
  noSlice: (o?: ReqOpts) => http.get<T.NoSliceHost[]>('/cache/noslice', o),
  resetNoSlice: (host: string, o?: ReqOpts) => http.del(`/cache/noslice/${seg(host)}`, o),

  downloads: (q: T.DownloadQuery = {}, o?: ReqOpts) =>
    http.get<T.Page<T.Download>>('/cache/downloads', { ...o, query: { ...q } }),
  requests: (q: T.EventQuery = {}, o?: ReqOpts) =>
    http.get<T.Page<T.CacheEvent>>('/cache/requests', { ...o, query: { ...q } }),
  sniEvents: (q: T.EventQuery = {}, o?: ReqOpts) =>
    http.get<T.Page<T.SniEvent>>('/cache/sni-events', { ...o, query: { ...q } }),
  /** `status` filters by eviction reason. */
  evictions: (q: T.EventQuery = {}, o?: ReqOpts) =>
    http.get<T.Page<T.EvictionEvent>>('/cache/evictions', { ...o, query: { ...q } }),
}

// ---------------------------------------------------------------- storage

// The server gives init and activate up to 2 minutes (storageLongOp; init runs
// three bounded steps of up to 30 s each): wait a little longer than that.
const STORAGE_LONG_OP_MS = 150_000

const storage = {
  capabilities: (o?: ReqOpts) => http.get<T.StorageCapabilities>('/storage/capabilities', o),
  targets: (o?: ReqOpts) => http.get<T.StorageTargetWithStatus[]>('/storage/targets', o),
  target: (id: string, o?: ReqOpts) => http.get<T.StorageTargetWithStatus>(`/storage/targets/${seg(id)}`, o),
  create: (t: T.StorageTargetInput, o?: ReqOpts) => http.post<T.StorageTarget>('/storage/targets', t, o),
  update: (id: string, t: T.StorageTargetInput, o?: ReqOpts) =>
    http.put<T.StorageTarget>(`/storage/targets/${seg(id)}`, t, o),
  remove: (id: string, o?: ReqOpts) => http.del(`/storage/targets/${seg(id)}`, o),
  test: (id: string, o?: ReqOpts) =>
    http.post<T.StorageTestResult>(`/storage/targets/${seg(id)}/test`, undefined, { ...o, timeoutMs: STORAGE_LONG_OP_MS }),
  /** Queues a host-apply mount (503 if the root helper is not installed). */
  apply: (id: string, o?: ReqOpts) => http.post<T.StorageStatus>(`/storage/targets/${seg(id)}/apply`, undefined, o),
  init: (id: string, adopt: boolean, o?: ReqOpts) =>
    http.post<T.StorageInitResult>(`/storage/targets/${seg(id)}/init`, { adopt }, { ...o, timeoutMs: STORAGE_LONG_OP_MS }),
  /** 409 "not initialised" → call init first. */
  activate: (id: string, o?: ReqOpts) =>
    http.post<T.StoreState>(`/storage/targets/${seg(id)}/activate`, undefined, { ...o, timeoutMs: STORAGE_LONG_OP_MS }),
  snippets: (id: string, o?: ReqOpts) => http.get<T.StorageSnippets>(`/storage/targets/${seg(id)}/snippets`, o),
  /**
   * Starts a speed test of the target (202 with the running run; default size
   * 256 MiB). It first runs the same fresh check as test(), so it can take as
   * long. 409 when a test is already running, the target is busy or not
   * available; 400 with field "sizeMiB" for other sizes; 400 when there is
   * not enough free space (size + 1 GiB).
   */
  benchmark: (id: string, sizeMiB?: T.BenchmarkSize, o?: ReqOpts) =>
    http.post<T.BenchmarkRun>(`/storage/targets/${seg(id)}/benchmark`, sizeMiB ? { sizeMiB } : {}, {
      ...o,
      timeoutMs: STORAGE_LONG_OP_MS,
    }),
  /** The running or most recent speed test and the last result per target. */
  benchmarkState: (o?: ReqOpts) => http.get<T.BenchmarkStatus>('/storage/benchmark', o),
  /** Cancels a running speed test (no-op if none); its partial result is kept. */
  cancelBenchmark: (o?: ReqOpts) => http.del('/storage/benchmark', o),
}

// ---------------------------------------------------------------- logs & statistics

// DELETE /logs/queries and /stats wait for the log writer (up to 2 minutes).
const CLEAR_MS = 150_000

const logs = {
  /** Cursor page of the query log (default range 1h, newest first); several `client` values match any of them. */
  queries: (q: T.QueryLogQuery = {}, o?: ReqOpts) =>
    http.get<T.Page<T.QueryEvent>>('/logs/queries', { ...o, query: { ...q } }),
  /** URL for a plain <a href> download of the filtered query log (newest first; one export at a time). */
  exportUrl: (format: T.QueryExportFormat, q: T.QueryExportQuery = {}) => apiUrl('/logs/queries/export', { ...q, format }),
  /** Deletes every query-log entry (statistics stay); 503 while logs.db is disabled. */
  clear: (o?: ReqOpts) => http.del<{ deleted: number }>('/logs/queries', { ...o, timeoutMs: CLEAR_MS }),
}

function rangeQuery(r: T.RangeArg): T.TimeQuery {
  return typeof r === 'string' ? { range: r } : { range: r.range, from: r.from, to: r.to }
}

/** Options of the client statistics: `group: 'device'` merges the addresses of one device. */
export interface ClientStatsOpts extends ReqOpts {
  group?: T.StatsGrouping
}

const stats = {
  summary: (range: T.RangeArg = '24h', o?: ReqOpts) =>
    http.get<T.Summary>('/stats/summary', { ...o, query: { ...rangeQuery(range) } }),
  /** step in seconds (optional; the server picks ≤ 300 points, > 1500 points → 400). */
  dns: (range: T.RangeArg, step?: number, o?: ReqOpts) =>
    http.get<T.Series<T.DnsSeriesKey>>('/stats/dns', { ...o, query: { ...rangeQuery(range), step } }),
  /** Bytes per step for hit/wan/sni, optionally for one service. */
  cache: (range: T.RangeArg, step?: number, service?: string, o?: ReqOpts) =>
    http.get<T.Series<T.CacheSeriesKey>>('/stats/cache', { ...o, query: { ...rangeQuery(range), step, service } }),
  /** `group: 'device'` applies to the kinds clients and cache-clients (items then carry `addresses`). */
  top: (kind: T.TopKind, range: T.RangeArg, limit = 10, { group, ...o }: ClientStatsOpts = {}) =>
    http.get<T.TopItem[]>('/stats/top', { ...o, query: { kind, ...rangeQuery(range), limit, group } }),
  services: (range: T.RangeArg, o?: ReqOpts) =>
    http.get<T.ServiceStat[]>('/stats/services', { ...o, query: { ...rangeQuery(range) } }),
  /** Per address, or per device with `group: 'device'`. */
  clients: (range: T.RangeArg, { group, ...o }: ClientStatsOpts = {}) =>
    http.get<T.ClientStat[]>('/stats/clients', { ...o, query: { ...rangeQuery(range), group } }),
  /** Blocked (and safe-search) queries by purpose, counted per hour. */
  purposes: (range: T.RangeArg = '24h', o?: ReqOpts) =>
    http.get<T.PurposeStats>('/stats/purposes', { ...o, query: { ...rangeQuery(range) } }),
  /** Queries by record type, counted per hour. */
  qtypes: (range: T.RangeArg = '24h', o?: ReqOpts) =>
    http.get<T.QTypeStats>('/stats/qtypes', { ...o, query: { ...rangeQuery(range) } }),
  /**
   * Activity of one client per step (≥ 3600 s; default range 7d). `key`: an
   * address, "ip:<address>", "client:<id>" or "mac:<MAC>" (device keys are
   * refused with field "key" while client addresses are anonymised).
   */
  clientSeries: (key: string, range: T.RangeArg, step?: number, o?: ReqOpts) =>
    http.get<T.ClientSeries>(`/stats/clients/${seg(key)}/series`, { ...o, query: { ...rangeQuery(range), step } }),
  /** Deletes the DNS and cache statistics (the query log stays); 503 while logs.db is disabled. */
  reset: (o?: ReqOpts) => http.del<{ deleted: number }>('/stats', { ...o, timeoutMs: CLEAR_MS }),
}

/** The complete typed API. */
export const api = {
  auth,
  tokens,
  system,
  backups,
  users,
  tls,
  notifications,
  settings,
  dns,
  upstreams,
  clients,
  groups,
  parental,
  network,
  dhcp,
  filter,
  downloadCache,
  cache,
  storage,
  logs,
  stats,
}
