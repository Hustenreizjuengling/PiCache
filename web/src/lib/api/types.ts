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
  downloadCacheEnabled: boolean
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

/**
 * How an update is installed (docs/ARCHITECTURE.md 14.4): `helper` = from the
 * web UI through the root helper, `docker` = pull the new image, `manual` =
 * `sudo picache update` on the host.
 */
export type UpdateMode = 'helper' | 'docker' | 'manual'

/** State of the last update run (status.json of the root helper). */
export type UpdateState = 'running' | 'succeeded' | 'failed' | 'rolled-back'

/** Step of an update run, in this order; `rollback` only when the new version did not come up. */
export type UpdateStep = 'download' | 'verify' | 'install' | 'restart' | 'health' | 'rollback' | 'done'

/** The newest eligible release found by the last check. */
export interface UpdateRelease {
  version: string
  publishedAt: Timestamp
  /** Release page on GitHub. */
  url: string
  /** Release notes (Markdown, at most 64 KiB). Untrusted: rendered as text only. */
  notes: string
  prerelease: boolean
}

/** Progress or result of the last update run. */
export interface UpdateRun {
  state: UpdateState
  step: UpdateStep
  /** Target version. */
  version: string
  /** Version before the update. */
  from: string
  startedAt: Timestamp
  finishedAt?: Timestamp
  message?: string
}

/** GET /system/update, POST /system/update/check */
export interface UpdateInfo {
  current: VersionInfo
  currentIsDevBuild: boolean
  mode: UpdateMode
  checkEnabled: boolean
  includePrereleases: boolean
  latest?: UpdateRelease
  updateAvailable: boolean
  /** Time of the last check (successful or not). */
  checkedAt?: Timestamp
  /** Why the last check failed (no network, private repository, …). */
  checkError?: string
  status?: UpdateRun
  /** Commands shown for the modes `manual` (cli) and `docker`. */
  commands: { cli: string; docker?: string }
}

/** POST /system/update/apply */
export interface UpdateQueued {
  queued: boolean
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
  /** Also trust every network this machine is connected to (public prefixes too; rebuilt every minute). */
  trustConnectedNetworks: boolean
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
  /** Answer forwarded AAAA queries with no records (networks with broken IPv6). Excludes dns64.enabled. */
  disableAAAA: boolean
  /** Synthesise AAAA records from A records for NAT64 networks (RFC 6147). */
  dns64: Dns64Settings
}

/** settings.DNS64: `prefix` must be a /96 network (400 field "dns.dns64.prefix"). */
export interface Dns64Settings {
  enabled: boolean
  prefix: string
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

/** settings.DownloadCache */
export interface DownloadCacheSettings {
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

/** settings.Updates */
export interface UpdatesSettings {
  /** Check GitHub for a new release every day. */
  checkEnabled: boolean
  /** Offer pre-releases (vX.Y.Z-rc.N) too. */
  includePrereleases: boolean
}

/** How often scheduled backups run. */
export type BackupSchedule = 'daily' | 'weekly'

/** settings.Backups (scheduled backups) */
export interface BackupsSettings {
  enabled: boolean
  schedule: BackupSchedule
  /** "HH:MM", local time of the host. */
  time: string
  /** Weekly only: 0 = Sunday … 6 = Saturday. */
  weekday: number
  /** Scheduled backups kept (1–90); older ones are deleted after a successful run. */
  keep: number
  /** "local" (`<data>/backups/scheduled/`) or a storage target id (`<store root>/picache-backups/`). */
  destination: string
  /** Keep the sealed NAS and notification secrets (useless without the master key). */
  includeSecrets: boolean
}

/** settings.DHCPIPv6: IPv6 DNS announcements of the DHCP server. */
export interface DhcpIpv6Settings {
  /** Router advertisements with RDNSS/DNSSL only (router lifetime 0, no prefixes). */
  routerAdvertisements: boolean
  /** Stateless DHCPv6: answers information requests with the DNS server and domain. */
  dhcpv6: boolean
}

/**
 * settings.DHCP (PATCH /settings/dhcp; field errors "dhcp.<member>" and
 * "dhcp.ipv6.<member>"). Interface and range are checked against the live
 * interface only while `enabled` is true.
 */
export interface DhcpSettings {
  enabled: boolean
  /** A non-virtual interface with exactly one RFC 1918 IPv4 address (the subnet served). */
  interface: string
  rangeStart: string
  rangeEnd: string
  /** 300..604800 (default 86400). */
  leaseSeconds: number
  /** "" = the IPv4 default gateway. */
  router: string
  /** "" = PiCache's own address on the interface. */
  dnsServer: string
  /** "" = dns.localDomain. */
  domain: string
  /** Lease host names become DNS names. */
  registerHostnames: boolean
  /** Serve although another DHCP server answers. */
  ignoreOtherServers: boolean
  ipv6: DhcpIpv6Settings
}

/** settings.All */
export interface Settings {
  dns: DnsSettings
  filter: FilterSettings
  downloadCache: DownloadCacheSettings
  cache: CacheSettings
  logs: LogsSettings
  web: WebSettings
  updates: UpdatesSettings
  backups: BackupsSettings
  dhcp: DhcpSettings
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
  | 'override'
  | 'blocked-list'
  | 'blocked-rule'
  | 'blocked-regex'
  | 'blocked-cname'
  | 'blocked-special'
  | 'blocked-schedule'
  | 'blocked-service'
  | 'refused'
  | 'error'

/** Every blocked-* status (for "blocked" filters and links). */
export const BLOCKED_STATUSES: readonly QueryStatus[] = [
  'blocked-list',
  'blocked-rule',
  'blocked-regex',
  'blocked-cname',
  'blocked-special',
  'blocked-schedule',
  'blocked-service',
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
  /** Address-detection warning (public or Docker-bridge address, …), also while the download cache is off. Absent from older servers. */
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
  downloadCacheBypass: boolean
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
  downloadCacheBypass: boolean
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

// ---------------------------------------------------------------- parental controls

export type ServiceCategory = 'video' | 'social' | 'messaging' | 'gaming' | 'music' | 'ai'

/** parental.Service: a service of the built-in catalogue (domains match with subdomains). */
export interface ParentalService {
  id: string
  name: string
  category: ServiceCategory
  domains: string[]
}

/** What a schedule blocks while it is active. */
export type ScheduleBlock = 'all' | 'services'

/** parental.Schedule. Times are "HH:MM" host local time; end < start ends the next day. */
export interface ParentalSchedule {
  /** 8 hex digits; "" for a new schedule (the server assigns one). */
  id: string
  name: string
  enabled: boolean
  /** 0 = Sunday … 6 = Saturday, sorted ascending. */
  days: number[]
  start: string
  end: string
  block: ScheduleBlock
  /** Service ids (block = services); empty for block = all. */
  services: string[]
}

/** block = block all internet now; allow = lift the group's restrictions. */
export type OverrideMode = 'block' | 'allow'

export interface ParentalOverride {
  mode: OverrideMode
  until: Timestamp
}

/** The next start or end of a schedule within 7 days. */
export interface ParentalNext {
  time: Timestamp
  scheduleId: string
  name: string
  /** true: the schedule starts then; false: it ends. */
  starts: boolean
}

/** parental.GroupState: what applies right now (computed at request time). */
export interface ParentalGroupState {
  blockAll: boolean
  reason?: 'override' | 'schedule'
  /** Name of the active block-all schedule. */
  schedule?: string
  /** When the current block-all ends. */
  until?: Timestamp
  /** Services blocked right now (always blocked + active service schedules). */
  blockedServices: string[]
  /** An allow override is active. */
  lifted: boolean
  liftedUntil?: Timestamp
  next?: ParentalNext
  /** Host time zone that schedule times refer to ("CEST", "UTC"). */
  timeZone: string
  /** Its current offset from UTC in minutes (120 for CEST). */
  utcOffsetMinutes: number
}

/** parental.GroupControls (GET/PUT /parental/groups/{id}). */
export interface GroupControls {
  groupId: number
  groupName: string
  groupEnabled: boolean
  clientCount: number
  blockedServices: string[]
  schedules: ParentalSchedule[]
  override?: ParentalOverride
  state: ParentalGroupState
  updatedAt?: Timestamp
}

/** PUT /parental/groups/{id} */
export interface GroupControlsInput {
  blockedServices: string[]
  schedules: ParentalSchedule[]
}

/** PUT /parental/groups/{id}/override: either minutes (1..10080) or until (≤ 7 days ahead). */
export type OverrideInput = { mode: OverrideMode; minutes: number } | { mode: OverrideMode; until: Timestamp }

// ---------------------------------------------------------------- network check

export type NetworkCheckStatus = 'ok' | 'info' | 'warn'

/** router-forwarding / container-nat */
export interface RouterShareData {
  routerQueries: number
  totalQueries: number
  share: number
  routerAddresses: string[]
}

export interface Ipv6DnsData {
  lanHasIPv6: boolean
  ipv6Queries: number
  ipv6Clients: number
  ula: string[]
  global: string[]
  /** This machine has no ULA/GUA and ignores router advertisements (Linux accept_ra = 0). */
  hostIgnoresRA: boolean
}

export interface Ipv6AddressData {
  ula: string[]
  global: string[]
}

export interface RefusedSource {
  address: string
  count: number
  last: Timestamp
  /** Inside a network this machine is connected to. */
  onLink: boolean
}

export interface RefusedData {
  /** Newest first, at most 20. */
  sources: RefusedSource[]
  since: Timestamp
  /** The DNS setting trustConnectedNetworks is on. */
  trustConnectedNetworks: boolean
}

export interface DevicesData {
  total: number
  active: number
  inactive: number
  never: number
}

/** One check of GET /network/check; the server computes status and numbers, the UI writes the texts. */
export type NetworkCheckItem =
  | { id: 'router-forwarding' | 'container-nat'; status: NetworkCheckStatus; data: RouterShareData }
  | { id: 'ipv6-dns'; status: NetworkCheckStatus; data: Ipv6DnsData }
  | { id: 'ipv6-address'; status: NetworkCheckStatus; data: Ipv6AddressData }
  | { id: 'refused'; status: NetworkCheckStatus; data: RefusedData }
  | { id: 'devices'; status: NetworkCheckStatus; data: DevicesData }

export type NetworkCheckId = NetworkCheckItem['id']

export type DeviceStatus = 'active' | 'inactive' | 'never'

/** A device of the neighbour table (grouped by MAC; router and this machine left out). */
export interface NetworkDevice {
  mac: string
  /** IPv4 first, then ULA, global and link-local. */
  ips: string[]
  /** Configured client name, else PTR or seen host name. */
  name?: string
  clientId?: number
  lastQuery?: Timestamp
  queries24h: number
  status: DeviceStatus
}

export type RouterKind = 'fritzbox' | 'generic' | 'unknown'

export interface NetworkRouter {
  ipv4?: string
  ipv6: string[]
  mac?: string
  name?: string
  kind: RouterKind
}

export interface NetworkSelf {
  ipv4: string[]
  ula: string[]
  global: string[]
  /** The DNS listener serves IPv6. */
  dnsIpv6: boolean
}

export interface NetworkScanState {
  running: boolean
  startedAt?: Timestamp
  finishedAt?: Timestamp
  addresses?: number
}

/** GET /network/check (cached for up to 30 s). */
export interface NetworkCheck {
  checkedAt: Timestamp
  mode: 'host' | 'bridge'
  /** false: the query log is off, only recently seen addresses count. */
  statsAvailable: boolean
  router?: NetworkRouter
  self: NetworkSelf
  queries24h: { total: number; ipv4: number; ipv6: number; fromRouter: number }
  checks: NetworkCheckItem[]
  devices: NetworkDevice[]
  scan: NetworkScanState
  /** PiCache's own DHCP server (DNS → DHCP). */
  dhcp?: { serving: boolean; interface?: string; routerAdvertisements: boolean }
}

/** 202 of POST /network/scan */
export interface NetworkScanStarted {
  started: boolean
  addresses: number
}

// ---------------------------------------------------------------- dhcp server

/** unavailable: PICACHE_DHCP off, bridge mode, non-Linux or sockets failed; blocked: enabled, but a blocker applies. */
export type DhcpState = 'unavailable' | 'off' | 'blocked' | 'serving' | 'error'

/** Why an enabled DHCP server does not serve (docs/ARCHITECTURE.md §18). */
export type DhcpBlocker = 'dynamic-address' | 'other-server' | 'no-interface' | 'range'

/** The served interface: PiCache's own IPv4 address on it. */
export interface DhcpInterfaceState {
  name: string
  mac: string
  ipv4: string
  prefixLen: number
  /** The address came from a DHCP client (finite lifetime). */
  dynamic: boolean
}

export interface DhcpPool {
  start: string
  end: string
  /** Usable addresses in the range. */
  size: number
  used: number
  static: number
}

/** Another DHCP server: answered a probe, or a device's request named it. */
export interface DhcpOtherServer {
  address: string
  serverId: string
  source: 'probe' | 'request'
  lastSeen: Timestamp
}

export interface DhcpCounters {
  received: number
  offers: number
  acks: number
  naks: number
  declines: number
  releases: number
  informs: number
  dropped: number
}

export type DhcpRaState = 'off' | 'blocked' | 'sending' | 'error'

/** Why IPv6 announcements wait: no stable ULA, no usable interface, no raw ICMPv6 socket (RAs) or no UDP port 547 (DHCPv6). */
export type DhcpIpv6Blocker = 'no-ula' | 'no-interface' | 'no-raw-socket' | 'no-socket' | (string & {})

export interface DhcpRaStatus {
  enabled: boolean
  /** false without the raw ICMPv6 socket (CAP_NET_RAW). */
  available: boolean
  reason?: string
  state: DhcpRaState
  blockers: DhcpIpv6Blocker[]
  /** The announced ULA. */
  address?: string
  lastSent?: Timestamp
  sent: number
  solicitations: number
  error?: string
}

export interface Dhcpv6Status {
  enabled: boolean
  state: 'off' | 'blocked' | 'serving' | 'error'
  /** While blocked. */
  blockers?: DhcpIpv6Blocker[]
  replies: number
  /** Other message types and malformed packets. */
  ignored?: number
  error?: string
}

/** GET /dhcp */
export interface DhcpStatus {
  available: boolean
  /** Why the DHCP server is unavailable. */
  reason?: string
  state: DhcpState
  blockers: DhcpBlocker[]
  error?: string
  interface?: DhcpInterfaceState
  pool?: DhcpPool
  router?: string
  dnsServer?: string
  domain?: string
  otherServers: DhcpOtherServer[]
  lastProbe?: { time: Timestamp; servers: number }
  counters: DhcpCounters
  ipv6: {
    routerAdvertisements: DhcpRaStatus
    dhcpv6: Dhcpv6Status
  }
}

export interface DhcpIpv6Address {
  address: string
  kind: 'ula' | 'global' | 'link-local'
  temporary: boolean
  deprecated: boolean
}

/** An entry of GET /dhcp/interfaces. */
export interface DhcpInterface {
  name: string
  mac: string
  /** CIDR notation, e.g. "192.168.178.10/24". */
  ipv4: string[]
  ipv6: DhcpIpv6Address[]
  /** The IPv4 address came from a DHCP client. */
  dynamic4: boolean
  /** Bridges, veth, tunnels and the like: not offered for serving. */
  virtual: boolean
}

export interface DhcpProbeServer {
  address: string
  serverId: string
  /** The address it offered. */
  offer?: string
  mac?: string
}

/** POST /dhcp/probe (429 within 10 s of the last probe, 503 when unavailable). */
export interface DhcpProbeResult {
  servers: DhcpProbeServer[]
  durationMs: number
}

/** GET /dhcp/leases: active and recently expired leases, newest first. */
export interface DhcpLease {
  mac: string
  ip: string
  hostname?: string
  clientId?: string
  expires: Timestamp
  active: boolean
  static: boolean
  /** Name of the configured client this device belongs to. */
  clientName?: string
  /** The DNS name the lease answers (with registerHostnames). */
  dnsName?: string
  /** Another active lease holds the host name: this one gets no DNS name. */
  nameConflict?: boolean
}

export interface DhcpStaticLease {
  mac: string
  ip: string
  hostname?: string
  comment?: string
  createdAt: Timestamp
  updatedAt: Timestamp
  /** A device uses it right now. */
  active: boolean
}

/** POST /dhcp/static (PUT /dhcp/static/{mac} ignores mac). 400 with field mac, ip, hostname or comment. */
export interface DhcpStaticLeaseInput {
  mac: string
  ip: string
  hostname?: string
  comment?: string
}

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

// ---------------------------------------------------------------- download cache services / sni

/** services.Service */
export interface DownloadCacheService {
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
export interface DownloadCacheServiceInput {
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

// ---------------------------------------------------------------- storage speed test

/** State of a speed test run; one runs at a time, globally. */
export type BenchmarkState = 'running' | 'done' | 'failed' | 'cancelled'

/** Phase of a speed test run, in this order (`slices` is skipped with a note when not possible). */
export type BenchmarkPhase = 'prepare' | 'write' | 'read' | 'slices' | 'metadata' | 'cleanup' | 'done'

/** Sizes offered for a speed test (MiB). */
export type BenchmarkSize = 64 | 256 | 1024

/** storage.Throughput */
export interface BenchmarkThroughput {
  bytes: number
  seconds: number
  bytesPerSec: number
}

/** storage.BenchmarkResult: bytes and bytes/s (the UI shows decimal MB/s). */
export interface BenchmarkResult {
  targetId: string
  fsType: string
  storeRoot: string
  testedAt: Timestamp
  /** Bytes actually written (less than requested when the write budget ran out). */
  sizeBytes: number
  /** Free space before the test. */
  freeBytes: number
  /** Sequential 1 MiB blocks; the time includes the final fsync. */
  write: BenchmarkThroughput
  /** Sequential read of the test file after dropping it from the page cache (if possible). */
  read: BenchmarkThroughput & { cacheDropped: boolean }
  /** Reads of real cached slices (the hit path); only for the active store with cached content. */
  slices?: BenchmarkThroughput & { count: number; p50Ms: number; p95Ms: number }
  /** Create 4 KiB + fsync + rename + delete, per operation. */
  metadata: { ops: number; p50Ms: number; p95Ms: number; maxMs: number }
  /** User-facing remarks (skipped phase, budget reached, page cache not dropped, …). */
  notes: string[]
}

/** storage.BenchmarkRun */
export interface BenchmarkRun {
  state: BenchmarkState
  targetId: string
  sizeMiB: number
  phase: BenchmarkPhase
  /** 0..1 over the whole run. */
  progress: number
  startedAt: Timestamp
  finishedAt?: Timestamp
  /** User-facing, for `failed`. */
  error?: string
  /** Set when done; partial when cancelled or failed after a phase finished. */
  result?: BenchmarkResult
}

/** GET /storage/benchmark (in memory only: empty after a restart). */
export interface BenchmarkStatus {
  /** The running or most recent run (kept until the next one starts). */
  run?: BenchmarkRun
  /** The last completed result per target id. */
  last: Record<string, BenchmarkResult>
}

// ---------------------------------------------------------------- notifications

/** Delivery format of a notification channel. */
export type NotifyKind = 'webhook' | 'ntfy' | 'gotify'

/** Severity of an event; a channel gets events at or above its minimum. */
export type NotifySeverity = 'info' | 'warning' | 'error'

/** notify.Channel (the secret is never returned) */
export interface NotifyChannel {
  /** 32 hex characters. */
  id: string
  name: string
  kind: NotifyKind
  /** http(s) URL; may contain a query string (shown without it). */
  url: string
  /** A secret is stored (webhook: Authorization header, ntfy: access token, gotify: app token). */
  hasSecret: boolean
  enabled: boolean
  minSeverity: NotifySeverity
  /** Event keys; empty = all events. */
  events: string[]
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** notify.ChannelInput: secret undefined (absent) = keep, "" = remove, a value = replace. */
export interface NotifyChannelInput {
  name: string
  kind: NotifyKind
  url: string
  secret?: string | null
  enabled: boolean
  minSeverity: NotifySeverity
  events: string[]
}

/** POST /notifications/channels/{id}/test (sent once, synchronously, 10 s timeout) */
export interface NotifyTestResult {
  ok: boolean
  error?: string
  /** HTTP status of the receiver, if it answered. */
  status?: number
  durationMs: number
}

/** GET /notifications/events: an event PiCache can notify about. */
export interface NotifyEvent {
  key: string
  /** Default severity. */
  severity: NotifySeverity
  title: string
  description: string
}

/** GET /notifications/log: one delivery attempt (in memory, the last 200). */
export interface NotifyDelivery {
  time: Timestamp
  channelId: string
  channelName: string
  event: string
  severity: NotifySeverity
  title: string
  ok: boolean
  error?: string
  /** 1..3 */
  attempt: number
}

// ---------------------------------------------------------------- scheduled backups

/** The last scheduled (or "run now") backup. */
export interface ScheduledBackupRun {
  time: Timestamp
  ok: boolean
  error?: string
  /** File name (picache-backup-<instanceId>-<YYYYMMDDTHHMMSSZ>.db). */
  file?: string
  sizeBytes?: number
  /** "local" or a storage target id. */
  destination: string
}

/** A stored scheduled backup in the current destination. */
export interface ScheduledBackupFile {
  name: string
  sizeBytes: number
  time: Timestamp
}

/** GET /system/backups/scheduled */
export interface ScheduledBackups {
  /** The `backups` settings section, as in /settings. */
  settings: BackupsSettings
  last?: ScheduledBackupRun
  /** Next scheduled run (absent while scheduled backups are off). */
  next?: Timestamp
  /** A backup is being written right now (scheduled or started by hand). */
  running: boolean
  /** Abbreviation of the host time zone that settings.time refers to ("CEST", "UTC"). */
  timeZone?: string
  /** Directory of the current destination as PiCache sees it ("" when the storage target is unknown). */
  destinationPath?: string
  /** Why `files` is empty although there may be backups (e.g. the storage target is offline). */
  filesError?: string
  /** Newest first, of the current destination. */
  files: ScheduledBackupFile[]
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
  dnsDownloadCache: number
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
export type DnsSeriesKey = 'allowed' | 'cached' | 'override' | 'blocked' | 'other'
/** Cache series keys (bytes per step). */
export type CacheSeriesKey = 'hit' | 'wan' | 'sni'

/** logs.Series: values aligned on timestamps (unix seconds, bucket start). */
export interface Series<K extends string = string> {
  step: number
  timestamps: number[]
  values: Partial<Record<K, number[]>>
}

export type TopKind = 'domains' | 'blocked' | 'clients' | 'cache-clients' | 'content' | 'upstreams'

/**
 * Opt-in grouping of client statistics (/stats/clients, /stats/top for
 * clients and cache-clients): "device" merges the addresses of one device
 * (a configured client, else one MAC address, else one IP address).
 */
export type StatsGrouping = 'device'

/** logs.TopItem */
export interface TopItem {
  /** Grouped by device: the device's most active address. */
  key: string
  /** Grouped by device: the device name. */
  label?: string
  count: number
  bytes?: number
  extra?: string
  /** Grouped by device (clients, cache-clients): every address of the device in the range. */
  addresses?: string[]
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
  /** Grouped by device: the most recently active address. */
  clientIp: string
  /** Configured name, else the device's host name. */
  clientName?: string
  /** The configured client the address (or device) belongs to. */
  clientId?: number
  mac?: string
  /** Grouped by device: all its addresses in the range, most recent first; else [clientIp]. */
  addresses: string[]
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
  /** An address or part of a name; several values (e.g. all addresses of a device) match any of them. */
  client?: string | string[]
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
