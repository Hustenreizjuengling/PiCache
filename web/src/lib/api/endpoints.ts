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
  changePassword: (body: { currentPassword: string; newPassword: string }, o?: ReqOpts) =>
    http.post<void>('/auth/password', body, o),
  sessions: (o?: ReqOpts) => http.get<T.SessionInfo[]>('/auth/sessions', o),
  revokeSession: (id: string, o?: ReqOpts) => http.del(`/auth/sessions/${seg(id)}`, o),
  totpBegin: (o?: ReqOpts) => http.post<T.TotpBegin>('/auth/totp/begin', undefined, o),
  totpConfirm: (code: string, o?: ReqOpts) => http.post<void>('/auth/totp/confirm', { code }, o),
  totpDisable: (password: string, o?: ReqOpts) => http.post<void>('/auth/totp/disable', { password }, o),
}

const tokens = {
  list: (o?: ReqOpts) => http.get<T.TokenInfo[]>('/tokens', o),
  create: (body: { name: string; scope: T.Scope; expiresInDays?: number }, o?: ReqOpts) =>
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
  restore: (file: Blob, o?: ReqOpts) =>
    http.post<T.RestoreResult>('/system/restore', undefined, { ...o, raw: file, timeoutMs: 600_000 }),
  /** 202; the process exits and systemd/Docker restarts it (poll /auth/status until it answers again). */
  restart: (o?: ReqOpts) => http.post<T.RestartResult>('/system/restart', undefined, o),
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

// ---------------------------------------------------------------- lancache services & sni

const lancache = {
  /** Domains are trimmed to the first 50 per service; use service(id) for all. */
  services: (o?: ReqOpts) => http.get<T.LanCacheService[]>('/lancache/services', o),
  service: (id: string, o?: ReqOpts) => http.get<T.LanCacheService>(`/lancache/services/${seg(id)}`, o),
  setEnabled: (id: string, enabled: boolean, o?: ReqOpts) =>
    http.put<T.LanCacheService>(`/lancache/services/${seg(id)}/enabled`, { enabled }, o),
  setExtraDomains: (id: string, extraDomains: string[], o?: ReqOpts) =>
    http.put<T.LanCacheService>(`/lancache/services/${seg(id)}/domains`, { extraDomains }, o),
  createService: (s: T.LanCacheServiceInput, o?: ReqOpts) => http.post<T.LanCacheService>('/lancache/services', s, o),
  updateService: (id: string, s: T.LanCacheServiceInput, o?: ReqOpts) =>
    http.put<T.LanCacheService>(`/lancache/services/${seg(id)}`, s, o),
  deleteService: (id: string, o?: ReqOpts) => http.del(`/lancache/services/${seg(id)}`, o),
  source: (o?: ReqOpts) => http.get<T.SourceStatus>('/lancache/source', o),
  refreshSource: (o?: ReqOpts) =>
    http.post<T.SourceStatus>('/lancache/source/refresh', undefined, { ...o, timeoutMs: LONG }),
  /** Sets a display label for a content group ("" removes it). */
  setLabel: (groupKey: string, label: string, o?: ReqOpts) => http.put<void>('/lancache/labels', { groupKey, label }, o),
  sni: (o?: ReqOpts) => http.get<T.SniStats>('/lancache/sni', o),
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

const storage = {
  capabilities: (o?: ReqOpts) => http.get<T.StorageCapabilities>('/storage/capabilities', o),
  targets: (o?: ReqOpts) => http.get<T.StorageTargetWithStatus[]>('/storage/targets', o),
  target: (id: string, o?: ReqOpts) => http.get<T.StorageTargetWithStatus>(`/storage/targets/${seg(id)}`, o),
  create: (t: T.StorageTargetInput, o?: ReqOpts) => http.post<T.StorageTarget>('/storage/targets', t, o),
  update: (id: string, t: T.StorageTargetInput, o?: ReqOpts) =>
    http.put<T.StorageTarget>(`/storage/targets/${seg(id)}`, t, o),
  remove: (id: string, o?: ReqOpts) => http.del(`/storage/targets/${seg(id)}`, o),
  test: (id: string, o?: ReqOpts) =>
    http.post<T.StorageTestResult>(`/storage/targets/${seg(id)}/test`, undefined, { ...o, timeoutMs: 150_000 }),
  /** Queues a host-apply mount (503 if the root helper is not installed). */
  apply: (id: string, o?: ReqOpts) => http.post<T.StorageStatus>(`/storage/targets/${seg(id)}/apply`, undefined, o),
  init: (id: string, adopt: boolean, o?: ReqOpts) =>
    http.post<T.StorageInitResult>(`/storage/targets/${seg(id)}/init`, { adopt }, { ...o, timeoutMs: 60_000 }),
  /** 409 "not initialised" → call init first. */
  activate: (id: string, o?: ReqOpts) =>
    http.post<T.StoreState>(`/storage/targets/${seg(id)}/activate`, undefined, { ...o, timeoutMs: 60_000 }),
  snippets: (id: string, o?: ReqOpts) => http.get<T.StorageSnippets>(`/storage/targets/${seg(id)}/snippets`, o),
}

// ---------------------------------------------------------------- logs & statistics

const logs = {
  /** Cursor page of the query log (default range 1h, newest first). */
  queries: (q: T.QueryLogQuery = {}, o?: ReqOpts) =>
    http.get<T.Page<T.QueryEvent>>('/logs/queries', { ...o, query: { ...q } }),
}

function rangeQuery(r: T.RangeArg): T.TimeQuery {
  return typeof r === 'string' ? { range: r } : { range: r.range, from: r.from, to: r.to }
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
  top: (kind: T.TopKind, range: T.RangeArg, limit = 10, o?: ReqOpts) =>
    http.get<T.TopItem[]>('/stats/top', { ...o, query: { kind, ...rangeQuery(range), limit } }),
  services: (range: T.RangeArg, o?: ReqOpts) =>
    http.get<T.ServiceStat[]>('/stats/services', { ...o, query: { ...rangeQuery(range) } }),
  clients: (range: T.RangeArg, o?: ReqOpts) =>
    http.get<T.ClientStat[]>('/stats/clients', { ...o, query: { ...rangeQuery(range) } }),
}

/** The complete typed API. */
export const api = {
  auth,
  tokens,
  system,
  settings,
  dns,
  upstreams,
  clients,
  groups,
  filter,
  lancache,
  cache,
  storage,
  logs,
  stats,
}
