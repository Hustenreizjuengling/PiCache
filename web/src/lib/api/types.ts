// TypeScript mirrors of the Go JSON types referenced by docs/API.md. The Go
// structs (json tags) are authoritative. Conventions:
//   - time.Time → RFC 3339 string; `omitzero`/`omitempty` members are optional.
//   - int/int64/uint64/float64 → number (64-bit counters stay far below 2^53).
//   - time series use unix seconds.

/** An RFC 3339 timestamp (UTC). */
export type Timestamp = string

// ---------------------------------------------------------------- errors

/** Error codes of the `{"error":{…}}` body. `network` and `aborted` are client-side. */
export type ErrorCode =
  | 'invalid'
  | 'not_found'
  | 'conflict'
  | 'forbidden'
  | 'unavailable'
  | 'unauthorized'
  | 'too_many_requests'
  | 'internal'
  | 'misdirected'
  | 'network'
  | 'aborted'

// ---------------------------------------------------------------- listing

/** listing.Page[T]: offset lists report total (-1 if unknown); cursor lists set next. */
export interface Page<T> {
  items: T[]
  total: number
  next?: string
}

/** Time range presets accepted by `range=` (docs/API.md "Lists"). */
export type RangePreset = '15m' | '1h' | '6h' | '24h' | '7d' | '30d' | '90d'

/** Either a preset range or explicit bounds (RFC 3339 or unix seconds). */
export interface TimeQuery {
  range?: RangePreset
  from?: string | number
  to?: string | number
}

/** Statistics endpoints take a preset ('24h') or explicit bounds ({ from, to }). */
export type RangeArg = RangePreset | TimeQuery

// ---------------------------------------------------------------- version / system

/** version.Info */
export interface VersionInfo {
  version: string
  commit: string
  date: string
  goVersion: string
  os: string
  arch: string
}

/** api.ListenerInfo: bound addresses and bind errors by role (dns-udp, dns-tcp, cache, sni, web, web-tls). */
export interface ListenerInfo {
  bound: Record<string, string[]>
  failed?: Record<string, string>
}

/** GET /system/info */
export interface SystemInfo {
  version: VersionInfo
  startedAt: Timestamp
  uptimeSec: number
  instanceId: string
  listeners: ListenerInfo
  dataDir: string
  cacheDir: string
  mountRoot: string
  masterKeySource: string
  memory: { allocBytes: number; sysBytes: number; limitBytes: number; numGC: number }
  goroutines: number
}

export type HealthStatus = 'ok' | 'warn' | 'fail'

/** api.HealthCheck */
export interface HealthCheck {
  name: string
  status: HealthStatus
  message?: string
  hint?: string
}

/** api.Health */
export interface Health {
  ok: boolean
  checks: HealthCheck[]
  checkedAt: Timestamp
}

/** api.StoreState: the active cache store. */
export interface StoreState {
  targetId: string
  storeId: string
  online: boolean
  passThrough: boolean
  reason?: string
  hint?: string
  usage?: StoreUsage
  sliceSize: number
  totalBytes: number
  freeBytes: number
  minFreeBytes: number
  /** Configured size limit (settings cache.maxSizeBytes; 0 = none). Absent from older servers. */
  maxSizeBytes?: number
  lowSpace: boolean
  full: boolean
  sdCard: boolean
}

/** api.VerifyState */
export interface VerifyState {
  running: boolean
  repair: boolean
  startedAt?: Timestamp
  finishedAt?: Timestamp
  progress: VerifyProgress
  error?: string
}

/** GET /system/overview: top bar and overview status in one call. */
export interface SystemOverview {
  blocking: BlockingStatus
  dns: DnsStats
  cacheIps: CacheIPStatus
  router: RouterStatus
  lancacheEnabled: boolean
  servicesReady: boolean
  store: StoreState
  proxy: ProxyStats
  sni: SniStats
  filter: FilterStats
  upstreams: UpstreamStat[]
  clockGuard: boolean
  health: { ok: boolean; warnings: number; failures: number }
}

/** POST /system/restart (the process exits and is restarted by systemd/Docker) */
export interface RestartResult {
  restarting: boolean
}

/** POST /system/restore */
export interface RestoreResult {
  staged: boolean
  message: string
}

// ---------------------------------------------------------------- auth

export type Scope = 'admin' | 'read'

/** auth.User */
export interface User {
  id: number
  username: string
  totpEnabled: boolean
  createdAt: Timestamp
  lastLoginAt?: Timestamp
}

/** GET /auth/status */
export interface AuthStatus {
  setupRequired: boolean
  authenticated: boolean
  user?: User
  scope?: Scope
  tokenAuth: boolean
  language: string
  setupHints?: string[]
  /** Port of the bound HTTPS listener (0 if none). Absent from older servers. */
  httpsPort?: number
}

/** auth.SessionInfo */
export interface SessionInfo {
  id: string
  createdAt: Timestamp
  lastSeen: Timestamp
  expiresAt: Timestamp
  ip: string
  userAgent: string
  current: boolean
}

/** auth.TokenInfo (never contains the secret) */
export interface TokenInfo {
  id: number
  name: string
  scope: Scope
  prefix: string
  createdAt: Timestamp
  expiresAt?: Timestamp
  lastUsed?: Timestamp
}

/** POST /tokens: the token secret is shown exactly once. */
export interface CreatedToken {
  token: string
  info: TokenInfo
}

/** auth.AuditEntry */
export interface AuditEntry {
  id: number
  time: Timestamp
  username: string
  ip: string
  action: string
  target: string
  details?: string
}

/** POST /auth/totp/begin: render `uri` as a QR code client-side. */
export interface TotpBegin {
  secret: string
  uri: string
}

// ---------------------------------------------------------------- settings

/** settings.DNS */
export interface DnsSettings {
  upstreams: string[]
  bootstrap: string[]
  upstreamMode: 'load_balance' | 'parallel' | 'strict'
  upstreamTimeoutMs: number
  localPtrUpstreams: string[]
  localDomain: string
  serverNames: string[]
  routerResolver: string
  allowedNetworks: string[]
  allowAllNetworks: boolean
  rateLimitQps: number
  rateLimitBurst: number
  rateLimitExempt: string[]
  refuseAny: boolean
  cacheEnabled: boolean
  cacheSize: number
  cacheMinTtl: number
  cacheMaxTtl: number
  serveStale: boolean
  serveStaleMaxAgeSec: number
  dnssec: boolean
}

export type BlockingMode = 'null' | 'nxdomain' | 'nodata' | 'refused' | 'custom_ip'

/** settings.Filter */
export interface FilterSettings {
  enabled: boolean
  pausedUntil?: Timestamp
  blockingMode: BlockingMode
  blockingIpv4: string
  blockingIpv6: string
  blockedTtl: number
  cnameInspection: boolean
  updateIntervalHours: number
  blockMozillaCanary: boolean
  blockIcloudPrivateRelay: boolean
}

/** settings.LanCache */
export interface LanCacheSettings {
  enabled: boolean
  cacheIpv4: string[]
  cacheIpv6: string[]
  dnsTtl: number
  domainsSource: string
  updateIntervalHours: number
  disabledServices: string[]
  nocacheClients: string[]
  allowPrivateUpstreams: boolean
}

/** settings.Cache */
export interface CacheSettings {
  sliceSizeBytes: number
  maxSizeBytes: number
  minFreeBytes: number
  maxAgeDays: number
  readAheadSlices: number
  maxConcurrentFills: number
  maxFillsPerClient: number
  activeStoreId: string
}

/** settings.Logs */
export interface LogsSettings {
  queryLogEnabled: boolean
  queryLogRetentionHours: number
  cacheLogRetentionHours: number
  sessionRetentionDays: number
  statsRetentionDays: number
  anonymizeClientIps: boolean
  maxDbSizeMiB: number
}

/** settings.Web */
export interface WebSettings {
  sessionIdleMinutes: number
  sessionMaxHours: number
  allowedHosts: string[]
  redirectToHttps: boolean
  metricsEnabled: boolean
  language: '' | 'en' | 'de'
}

/** settings.All */
export interface Settings {
  dns: DnsSettings
  filter: FilterSettings
  lancache: LanCacheSettings
  cache: CacheSettings
  logs: LogsSettings
  web: WebSettings
}

/** Sections accepted by PATCH /settings/{section}. */
export type SettingsSection = keyof Settings

// ---------------------------------------------------------------- dns server

/** Query statuses (docs/ARCHITECTURE.md 7.1). */
export type QueryStatus =
  | 'forwarded'
  | 'cached'
  | 'stale'
  | 'local'
  | 'special'
  | 'lancache'
  | 'blocked-list'
  | 'blocked-rule'
  | 'blocked-regex'
  | 'blocked-cname'
  | 'blocked-special'
  | 'refused'
  | 'error'

/** Every blocked-* status (for "blocked" filters and links). */
export const BLOCKED_STATUSES: readonly QueryStatus[] = [
  'blocked-list',
  'blocked-rule',
  'blocked-regex',
  'blocked-cname',
  'blocked-special',
]

/** dnsserver.BlockingStatus */
export interface BlockingStatus {
  enabled: boolean
  pausedUntil?: Timestamp
  permanent: boolean
}

/** netutil.RateLimited */
export interface RateLimited {
  client: string
  dropped: number
  first: Timestamp
  last: Timestamp
}

/** dnsserver.Stats */
export interface DnsStats {
  queries: number
  qps: number
  refused: number
  rateLimited: number
  inFlight: number
  /** Dropped because too many queries were in flight. */
  overloaded: number
  topRateLimited: RateLimited[]
}

/** dnsserver.CacheIPStatus */
export interface CacheIPStatus {
  ipv4: string[]
  ipv6: string[]
  auto: boolean
  ready: boolean
  reason?: string
  /** Address-detection warning (public or Docker-bridge address, …), also while LanCache is off. Absent from older servers. */
  warning?: string
}

/** dnsserver.RouterStatus */
export interface RouterStatus {
  mode: 'off' | 'auto' | 'manual'
  address: string
  answers: boolean
  domain: string
}

export type RecordType = 'A' | 'AAAA' | 'CNAME' | 'TXT'

/** dnsserver.Record */
export interface DnsRecord {
  id: number
  name: string
  type: RecordType
  value: string
  ttl: number
  enabled: boolean
  comment: string
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** dnsserver.RecordInput (ttl 0 → 300) */
export interface DnsRecordInput {
  name: string
  type: RecordType
  value: string
  ttl: number
  enabled: boolean
  comment: string
}

/** dnsserver.Forwarder */
export interface Forwarder {
  id: number
  domain: string
  upstreams: string[]
  enabled: boolean
  comment: string
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** dnsserver.ForwarderInput */
export interface ForwarderInput {
  domain: string
  upstreams: string[]
  enabled: boolean
  comment: string
}

/** dnsserver.LookupRequest */
export interface LookupRequest {
  name: string
  type?: string
  clientIp?: string
}

/** dnsserver.LookupResult */
export interface LookupResult {
  name: string
  type: string
  status: QueryStatus
  rcode: string
  answers: string[]
  reason?: string
  upstream?: string
  durationUs: number
  groupIds: number[]
  steps: string[]
  matches: FilterMatch[]
}

// ---------------------------------------------------------------- upstream

/** upstream.UpstreamStat */
export interface UpstreamStat {
  upstream: string
  queries: number
  errors: number
  avgRttMs: number
  lastError?: string
  lastErrorAt?: Timestamp
  healthy: boolean
}

/** upstream.CacheStat */
export interface UpstreamCacheStat {
  entries: number
  capacity: number
  hits: number
  misses: number
  staleHits: number
}

/** GET /dns/upstreams */
export interface UpstreamsState {
  upstreams: UpstreamStat[]
  cache: UpstreamCacheStat
  clockGuard: boolean
}

/** upstream.TestResult */
export interface UpstreamTestResult {
  upstream: string
  ok: boolean
  rttMs: number
  answer?: string
  error?: string
}

// ---------------------------------------------------------------- clients

/** clients.Group */
export interface ClientGroup {
  id: number
  name: string
  comment: string
  enabled: boolean
  createdAt: Timestamp
  clientCount: number
}

/** clients.GroupInput */
export interface ClientGroupInput {
  name: string
  comment: string
  enabled: boolean
}

/** clients.Client */
export interface Client {
  id: number
  name: string
  identifiers: string[]
  groupIds: number[]
  comment: string
  lanCacheBypass: boolean
  ignoreLogs: boolean
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** clients.ClientInput */
export interface ClientInput {
  name: string
  identifiers: string[]
  groupIds: number[]
  comment: string
  lanCacheBypass: boolean
  ignoreLogs: boolean
}

/** clients.Known: an address seen recently. */
export interface KnownClient {
  ip: string
  mac?: string
  hostname?: string
  clientId?: number
  name?: string
  firstSeen: Timestamp
  lastSeen: Timestamp
  queries: number
}

/** ID of the built-in "Default" group (cannot be deleted). */
export const DEFAULT_GROUP_ID = 1

// ---------------------------------------------------------------- filter

export type ListStatus = 'pending' | 'ok' | 'unchanged' | 'failed-cached' | 'failed-empty'

/** filter.List */
export interface FilterList {
  id: number
  name: string
  url: string
  kind: 'block' | 'allow'
  plainDomains: 'exact' | 'subtree'
  enabled: boolean
  groupIds: number[]
  comment: string
  status: ListStatus
  lastError?: string
  lastUpdated?: Timestamp
  lastChecked?: Timestamp
  lastSuccess?: Timestamp
  entries: number
  invalid: number
  unsupported: number
  sizeBytes: number
  createdAt: Timestamp
}

/** filter.ListInput */
export interface FilterListInput {
  name: string
  url: string
  kind: 'block' | 'allow'
  plainDomains: 'exact' | 'subtree'
  enabled: boolean
  groupIds: number[]
  comment: string
}

export type RuleAction = 'allow' | 'block'
export type RuleType = 'exact' | 'subtree' | 'regex'

/** filter.Rule */
export interface FilterRule {
  id: number
  action: RuleAction
  type: RuleType
  pattern: string
  enabled: boolean
  groupIds: number[]
  comment: string
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** filter.RuleInput */
export interface FilterRuleInput {
  action: RuleAction
  type: RuleType
  pattern: string
  enabled: boolean
  groupIds: number[]
  comment: string
}

/** GET /filter/rules query */
export interface RuleQuery {
  action?: RuleAction
  type?: RuleType
  search?: string
}

/** filter.Match */
export interface FilterMatch {
  action: string
  source: 'list' | 'rule'
  kind: string
  listId?: number
  ruleId?: number
  name: string
  pattern: string
  important?: boolean
  groupIds: number[]
  applies: boolean
  decisive: boolean
}

/** filter.Stats */
export interface FilterStats {
  lists: number
  entries: number
  patterns: number
  /** Patterns beyond the total cap of 20 000. */
  patternsDropped: number
  rules: number
  compiledAt?: Timestamp
  compileMs: number
  memoryBytes: number
  updating: boolean
  failedLists: number
  staleLists: number
}

/** filter.CatalogEntry */
export interface CatalogEntry {
  key: string
  name: string
  description: string
  url: string
  category: 'general' | 'security' | 'privacy' | 'other'
  plainDomains: 'exact' | 'subtree'
  recommended: boolean
}

/** POST /filter/explain */
export interface ExplainResult {
  domain: string
  groupIds: number[]
  matches: FilterMatch[]
  decision: { action: string; name: string; source: string; kind: string }
}

// ---------------------------------------------------------------- lancache services / sni

/** services.Service */
export interface LanCacheService {
  id: string
  name: string
  description: string
  notes?: string
  mixedContent: boolean
  custom: boolean
  enabled: boolean
  domains: string[]
  extraDomains: string[]
  domainCount: number
}

/** services.ServiceInput (custom services) */
export interface LanCacheServiceInput {
  name: string
  description: string
  domains: string[]
}

/** services.SourceStatus */
export interface SourceStatus {
  source: string
  lastFetched?: Timestamp
  lastAttempt?: Timestamp
  error?: string
  serviceCount: number
  domainCount: number
  skipped?: string[]
  ready: boolean
}

/** sni.Stats */
export interface SniStats {
  active: number
  total: number
  refused: number
  bytesUp: number
  bytesDown: number
  listening: boolean
}

// ---------------------------------------------------------------- cache store

/** cachestore.Usage */
export interface StoreUsage {
  storeId: string
  objects: number
  slices: number
  cachedBytes: number
  sliceSize: number
}

/** cachestore.VerifyProgress */
export interface VerifyProgress {
  filesScanned: number
  bytesScanned: number
  added: number
  removed: number
  corrupt: number
}

/** cachestore.EvictResult */
export interface EvictResult {
  objects: number
  bytes: number
  reasons: Record<string, number>
  full: boolean
}

/** cachestore.ServiceUsage */
export interface ServiceUsage {
  service: string
  objects: number
  groups: number
  cachedBytes: number
  bytesServed: number
  lastAccess?: Timestamp
}

/** cachestore.GroupUsage */
export interface GroupUsage {
  service: string
  groupKey: string
  objects: number
  cachedBytes: number
  totalBytes: number
  bytesServed: number
  hits: number
  firstCached: Timestamp
  lastAccess: Timestamp
  expiresAt?: Timestamp
  pinned: boolean
}

/** GroupView = cachestore.GroupUsage + {label, userLabel, clients} */
export interface GroupView extends GroupUsage {
  label: string
  /** The label was set by a user (not the automatic name). Absent from older servers. */
  userLabel?: boolean
  clients: number
}

/** cachestore.Object */
export interface CacheObject {
  id: string
  service: string
  host: string
  path: string
  groupKey: string
  total: number
  sliceSize: number
  contentType: string
  createdAt: Timestamp
  lastAccess: Timestamp
  hits: number
  bytesServed: number
  cachedBytes: number
  sliceCount: number
  slicesTotal: number
  pinned: boolean
  noSlice: boolean
  expiresAt?: Timestamp
}

export type GroupSort = 'bytes' | 'lastAccess' | 'firstCached' | 'served' | 'name'
export type ObjectSort = 'lastAccess' | 'size' | 'created' | 'path'

/** GET /cache/groups query */
export interface GroupQuery {
  service?: string
  search?: string
  sort?: GroupSort
  desc?: boolean
  limit?: number
  offset?: number
}

/** GET /cache/objects query */
export interface ObjectQuery {
  service?: string
  group?: string
  search?: string
  sort?: ObjectSort
  desc?: boolean
  limit?: number
  offset?: number
}

/** GET /cache/groups/detail */
export interface GroupDetail {
  group: GroupView
  clients: GroupClient[]
  objects: Page<CacheObject>
}

/** {bytesFreed} of purge endpoints */
export interface BytesFreed {
  bytesFreed: number
}

// ---------------------------------------------------------------- proxy

/** proxy.Transfer */
export interface Transfer {
  id: string
  clientIp: string
  clientName?: string
  service: string
  host: string
  path: string
  groupKey: string
  label: string
  started: Timestamp
  bytesSent: number
  bytesHit: number
  bytesWan: number
  total: number
  rateBps: number
}

/** proxy.ActiveDownload */
export interface ActiveDownload {
  clientIp: string
  clientName?: string
  service: string
  groupKey: string
  label: string
  started: Timestamp
  lastSeen: Timestamp
  inFlight: number
  bytesSent: number
  bytesHit: number
  bytesWan: number
  rateBps: number
}

/** proxy.Stats */
export interface ProxyStats {
  requests: number
  activeClients: number
  activeFills: number
  bytesHit: number
  bytesWan: number
  refused: number
  errors: number
  passThrough: boolean
  steamHostsRefused?: string[]
}

/** proxy.NoSliceHost */
export interface NoSliceHost {
  host: string
  failures: number
  marked: boolean
  since: Timestamp
}

// ---------------------------------------------------------------- storage

export type StorageKind = 'local' | 'smb' | 'nfs'
export type StorageMode = 'external' | 'host-apply'

/** storage.Target (the password is never returned) */
export interface StorageTarget {
  id: string
  name: string
  kind: StorageKind
  mode: StorageMode
  path: string
  server: string
  share: string
  export: string
  subdir: string
  username: string
  domain: string
  hasPassword: boolean
  smbVersion: string
  smbSeal: boolean
  nfsVersion: string
  nfsNconnect: number
  requireMountpoint: boolean
  storeId: string
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** storage.TargetInput: password undefined = keep, "" = clear. */
export interface StorageTargetInput {
  name: string
  kind: StorageKind
  mode: StorageMode
  path: string
  server: string
  share: string
  export: string
  subdir: string
  username: string
  domain: string
  password?: string
  smbVersion: string
  smbSeal: boolean
  nfsVersion: string
  nfsNconnect: number
  requireMountpoint: boolean
}

/** storage.Status */
export interface StorageStatus {
  online: boolean
  reason?: string
  hint?: string
  mounted: boolean
  fsType?: string
  device?: string
  sdCard: boolean
  sameFsAsData: boolean
  totalBytes: number
  freeBytes: number
  latencyMs: number
  storeRoot: string
  storeId?: string
  checkedAt?: Timestamp
  applyState?: string
  /** A store is set up for the target (false: initialise or adopt). Absent from older servers. */
  initialised?: boolean
  /** The location was found and passed the write test. Absent from older servers. */
  writable?: boolean
}

/** storage.TargetWithStatus (Target members are embedded) */
export interface StorageTargetWithStatus extends StorageTarget {
  status: StorageStatus
  active: boolean
}

/** storage.Capabilities */
export interface StorageCapabilities {
  os: string
  container: string
  initUserNs: boolean
  uidMapOffset: number
  gidMapOffset: number
  uid: number
  gid: number
  systemd: boolean
  filesystems: Record<string, boolean>
  mountHelpers: Record<string, boolean>
  hostApply: boolean
  mountRoot: string
  dockerMode?: string
}

/** storage.TestResult */
export interface StorageTestResult {
  ok: boolean
  steps: string[]
  error?: string
  hint?: string
  status: StorageStatus
}

/** storage.Snippets (never contain the password) */
export interface StorageSnippets {
  credentialsFile?: string
  fstab?: string
  systemdMount?: string
  dockerCompose?: string
  proxmox?: string
  hostApply?: string
  notes?: string[]
}

/** storage.InitResult */
export interface StorageInitResult {
  storeId: string
  adopted: boolean
}

// ---------------------------------------------------------------- logs / stats

/** logs.QueryEvent */
export interface QueryEvent {
  /** Database id (0 for events from the live stream, which are not stored yet). */
  id: number
  /** Sequence number of live-stream events (unique per server run). Absent from older servers. */
  seq?: number
  time: Timestamp
  clientIp: string
  clientName?: string
  qname: string
  qtype: string
  status: QueryStatus
  rcode: string
  reason?: string
  listId?: number
  ruleId?: number
  service?: string
  upstream?: string
  durationUs: number
  answer?: string
  dnssec?: boolean
  protocol: 'udp' | 'tcp'
}

export type CacheStatus = 'HIT' | 'MISS' | 'PARTIAL' | 'BYPASS' | 'PASS' | 'ERROR'

/** logs.CacheEvent */
export interface CacheEvent {
  /** Database id (0 for events from the live stream, which are not stored yet). */
  id: number
  /** Sequence number of live-stream events (unique per server run). Absent from older servers. */
  seq?: number
  time: Timestamp
  clientIp: string
  clientName?: string
  service: string
  host: string
  path: string
  method: string
  status: number
  cacheStatus: CacheStatus
  range?: string
  bytesSent: number
  bytesHit: number
  bytesWan: number
  bytesStored: number
  durationMs: number
  groupKey: string
  label?: string
  userAgent?: string
}

/** logs.SNIEvent */
export interface SniEvent {
  id: number
  time: Timestamp
  clientIp: string
  clientName?: string
  sni: string
  service: string
  bytesUp: number
  bytesDown: number
  durationMs: number
}

export type EvictionReason = 'inactive' | 'size' | 'min-free' | 'manual' | 'corrupt' | 'invalidated'

/** logs.EvictionEvent */
export interface EvictionEvent {
  id: number
  time: Timestamp
  storeId: string
  objectId: string
  service: string
  groupKey: string
  bytes: number
  reason: EvictionReason
}

/** logs.Summary: dashboard totals for a range (from rollups). */
export interface Summary {
  from: Timestamp
  to: Timestamp
  dnsQueries: number
  dnsBlocked: number
  dnsCached: number
  dnsLancache: number
  dnsForwarded: number
  blockedPercent: number
  avgDnsDurationUs: number
  cacheRequests: number
  cacheBytesSent: number
  cacheBytesHit: number
  cacheBytesWan: number
  cacheBytesStored: number
  evictedBytes: number
  byteHitRatio: number
  sniBytes: number
  activeClients: number
  activeDownloads: number
  droppedLogEvents: number
}

/** DNS series keys (disjoint, sum to all queries). */
export type DnsSeriesKey = 'allowed' | 'cached' | 'lancache' | 'blocked' | 'other'
/** Cache series keys (bytes per step). */
export type CacheSeriesKey = 'hit' | 'wan' | 'sni'

/** logs.Series: values aligned on timestamps (unix seconds, bucket start). */
export interface Series<K extends string = string> {
  step: number
  timestamps: number[]
  values: Partial<Record<K, number[]>>
}

export type TopKind = 'domains' | 'blocked' | 'clients' | 'cache-clients' | 'content' | 'upstreams'

/** logs.TopItem */
export interface TopItem {
  key: string
  label?: string
  count: number
  bytes?: number
  extra?: string
}

/** logs.Download: one client downloading one content group. */
export interface Download {
  id: number
  clientIp: string
  clientName?: string
  service: string
  groupKey: string
  label: string
  firstSeen: Timestamp
  lastSeen: Timestamp
  requests: number
  bytesSent: number
  bytesHit: number
  bytesWan: number
  active: boolean
}

/** logs.ServiceStat */
export interface ServiceStat {
  service: string
  requests: number
  bytesSent: number
  bytesHit: number
  bytesWan: number
  sniConnections: number
  sniBytes: number
}

/** logs.ClientStat */
export interface ClientStat {
  clientIp: string
  clientName?: string
  queries: number
  blocked: number
  cacheBytes: number
  cacheHitBytes: number
  lastSeen: Timestamp
}

/** logs.GroupClient */
export interface GroupClient {
  clientIp: string
  clientName?: string
  sessions: number
  bytesSent: number
  lastSeen: Timestamp
}

/** GET /logs/queries filter (default range 1h, newest first). */
export interface QueryLogQuery extends TimeQuery {
  client?: string
  domain?: string
  status?: QueryStatus[]
  qtype?: string
  upstream?: string
  cursor?: string
  limit?: number
}

/** GET /cache/downloads filter */
export interface DownloadQuery extends TimeQuery {
  client?: string
  service?: string
  group?: string
  search?: string
  active?: boolean
  limit?: number
  offset?: number
}

/** GET /cache/requests, /cache/sni-events, /cache/evictions filter */
export interface EventQuery extends TimeQuery {
  client?: string
  service?: string
  search?: string
  status?: string
  cursor?: string
  limit?: number
}
