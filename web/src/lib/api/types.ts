// TypeScript mirrors of the Go JSON types referenced by docs/API.md. The Go
// structs (json tags) are authoritative. Conventions:
//   - time.Time → RFC 3339 string; `omitzero`/`omitempty` members are optional.
//   - int/int64/uint64/float64 → number (64-bit counters stay far below 2^53).
//   - time series use unix seconds.

import type { Locale } from '../../i18n/locales'

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
  /** 403: the host locks the configuration (PICACHE_CONFIG_LOCKED) and the request came from a session. */
  | 'config_locked'
  | 'network'
  | 'aborted'

// ---------------------------------------------------------------- listing

/** listing.Page[T]: offset lists report total (-1 if unknown); cursor lists set next. */
export interface Page<T> {
  items: T[]
  total: number
  next?: string
  /** The search stopped early (its time budget): `next` continues where it stopped. */
  partial?: boolean
}

/** Time range presets accepted by `range=` (docs/API.md "Lists"). */
export type RangePreset = '15m' | '1h' | '6h' | '24h' | '7d' | '30d' | '90d' | '180d' | '365d'

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

/** api.ListenerInfo: bound addresses and bind errors by role (dns-udp, dns-tcp, cache, sni, web, web-tls, dot, doh, ntp). */
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
  /** Connections and requests refused by the web access control since the start. */
  webRefused: number
  /** The address this request is attributed to (the forwarded client behind a trusted proxy). */
  clientAddress: string
  /** The TCP peer of this request (the proxy when it differs from clientAddress). */
  peerAddress: string
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
  /** Unacknowledged warning and error entries of the warning history the caller may see. */
  events: { unacknowledged: number }
}

/** POST /system/restart (the process exits and is restarted by systemd/Docker) */
export interface RestartResult {
  restarting: boolean
}

/** POST /system/restore */
export interface RestoreResult {
  staged: boolean
  message: string
  /** The sections restored at the next start (every restorable section for a full restore). */
  sections: RestoreSection[]
  /** Set when the restored settings would not let this browser use the web UI after the restart. */
  webAccessWarning?: string
  /** Set when the restored settings turn plain DNS off but no encrypted DNS listener of this host serves them. */
  dnsWarning?: string
}

/**
 * Parts of the configuration (docs/ARCHITECTURE.md 15, section map): what a
 * partial restore replaces and what a follower syncs. `dns-settings` (the
 * syncable members of the dns and filter settings) is synced only; a restore
 * restores it as part of `settings`.
 */
export type ConfigSection =
  | 'settings'
  | 'clients-and-groups'
  | 'lists-and-rules'
  | 'local-dns'
  | 'parental'
  | 'dhcp'
  | 'download-cache'
  | 'notifications'
  | 'storage'
  | 'dns-settings'

/** Sections of POST /system/restore?sections=… */
export type RestoreSection = Exclude<ConfigSection, 'dns-settings'>

/** Sections a follower syncs (settings sync.sections, GET /system/export?sections=…). */
export type SyncSection = 'clients-and-groups' | 'lists-and-rules' | 'local-dns' | 'parental' | 'dns-settings'

/** Restorable sections in display order. */
export const RESTORE_SECTIONS: readonly RestoreSection[] = [
  'settings',
  'clients-and-groups',
  'lists-and-rules',
  'local-dns',
  'parental',
  'dhcp',
  'download-cache',
  'notifications',
  'storage',
]

/** Syncable sections in display order. */
export const SYNC_SECTIONS: readonly SyncSection[] = ['clients-and-groups', 'lists-and-rules', 'local-dns', 'parental', 'dns-settings']

/**
 * Sections that refer to the groups: selecting `clients-and-groups` (which
 * replaces the groups) requires them too, for a restore and a sync alike.
 */
export const GROUP_LINKED_SECTIONS: readonly ('lists-and-rules' | 'local-dns' | 'parental')[] = ['lists-and-rules', 'local-dns', 'parental']

/**
 * How an update is installed (docs/ARCHITECTURE.md 14.4): `helper` = from the
 * web UI through the root helper, `docker` = pull the new image, `manual` =
 * `sudo picache update` on the host, `package` = installed as a Debian package
 * (updated with apt; `package` describes the file).
 */
export type UpdateMode = 'helper' | 'docker' | 'manual' | 'package'

/** GET /system/update in mode `package`: the Debian package of this machine. */
export interface UpdatePackage {
  format: 'deb'
  /** Debian architecture (amd64, arm64, armhf, i386, riscv64). */
  arch: string
  /** "picache_<version without v>_<arch>.deb" of `latest` (only while `latest` is set). */
  file?: string
  /** Its download URL on the release (only while `latest` is set). */
  url?: string
}

/**
 * Which releases are offered: `stable` = releases only, `beta` = also
 * release candidates (-rc.N), `nightly` = also the daily builds of main
 * (-nightly.<date>.<n>, signed with a separate key).
 */
export type UpdateChannel = 'stable' | 'beta' | 'nightly'

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
  /** Derived from channel (channel != "stable"). */
  includePrereleases: boolean
  channel: UpdateChannel
  /** The root helper installs nightly builds on this host (/etc/picache/nightly.enabled exists: install.sh --nightly). */
  nightlyAllowed: boolean
  /** The proxy installs download through (PICACHE_UPDATE_PROXY of the host, scheme://host:port). */
  installProxy?: string
  latest?: UpdateRelease
  updateAvailable: boolean
  /** Time of the last check (successful or not). */
  checkedAt?: Timestamp
  /** Why the last check failed (no network, private repository, …). */
  checkError?: string
  status?: UpdateRun
  /** Commands shown for the modes `manual` (cli) and `docker`. */
  commands: { cli: string; docker?: string }
  /** Mode `package` only. */
  package?: UpdatePackage
}

/** POST /system/update/apply */
export interface UpdateQueued {
  queued: boolean
}

// ---------------------------------------------------------------- auth

/** `sync` tokens read only the configuration export (a follower's credential; created by admins). */
export type Scope = 'admin' | 'read' | 'sync'

/** An account's role: admins manage everything, viewers read and manage only their own account. */
export type Role = 'admin' | 'viewer'

/** auth.User */
export interface User {
  id: number
  username: string
  role: Role
  totpEnabled: boolean
  createdAt: Timestamp
  lastLoginAt?: Timestamp
}

/** GET /auth/status */
export interface AuthStatus {
  setupRequired: boolean
  authenticated: boolean
  user?: User
  /** `read` for viewer sessions and read tokens (and admin tokens of viewers). */
  scope?: Scope
  tokenAuth: boolean
  language: string
  setupHints?: string[]
  /** Port of the bound HTTPS listener (0 if none). Absent from older servers. */
  httpsPort?: number
  /** Sessions cannot change the configuration (PICACHE_CONFIG_LOCKED); false while signed out. */
  configLocked: boolean
  /** Destructive actions are allowed (PICACHE_DESTRUCTIVE_API); false while signed out. */
  destructiveApi: boolean
  /** Sections this follower syncs from its primary (read-only here); [] unless follower, absent while signed out. */
  syncedSections?: SyncSection[]
}

/** POST /system/users */
export interface UserCreate {
  username: string
  password: string
  role: Role
  currentPassword: string
}

/** PUT /system/users/{id}: at least one of role, password, disableTotp (only role for your own account). */
export interface UserUpdate {
  role?: Role
  password?: string
  disableTotp?: boolean
  currentPassword: string
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
  /** The owner (admins list the tokens of every account). */
  userId: number
  username: string
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

// ---------------------------------------------------------------- https certificate

/**
 * Where the served HTTPS certificate comes from, in order of precedence:
 * `files` (PICACHE_WEB_TLS_CERT/KEY), `uploaded` (PUT /system/tls),
 * `local-ca` (issued by PiCache's local CA), `self-signed` (from 0.10 or
 * older, or an emergency certificate); `none` without a TLS listener (the
 * HTTPS web UI, DoT or DoH).
 */
export type TlsSource = 'files' | 'uploaded' | 'local-ca' | 'self-signed' | 'none'

/** Why an upload is not possible (first matching). */
export type TlsUploadReason = 'no-listener' | 'env-override' | 'plain-http'

/** api.CertInfo: the served leaf certificate. */
export interface CertInfo {
  /** RFC 2253 form. */
  subject: string
  issuer: string
  sans: string[]
  notBefore: Timestamp
  notAfter: Timestamp
  /** Upper-case hex pairs joined by ":". */
  fingerprintSha256: string
  /** "ECDSA P-256", "ECDSA P-384", "RSA <bits>", "Ed25519" or "other". */
  keyType: string
  chainLength: number
  selfSigned: boolean
}

/** The local CA (present when ca.crt exists). */
export interface LocalCaInfo {
  subject: string
  notBefore: Timestamp
  notAfter: Timestamp
  fingerprintSha256: string
  /** The name constraints: the only names and addresses its certificates can be valid for. */
  permittedNames: string[]
  permittedAddresses: string[]
  /** PiCache's names or addresses include some outside the constraints: a new CA is needed to cover them. */
  renewalNeeded: boolean
}

/** GET /system/tls (also the answer of PUT, DELETE and POST /system/tls/local-ca). */
export interface TlsStatus {
  /** A TLS listener (web-tls, dot or doh) is bound: the certificate machinery runs. */
  listener: boolean
  source: TlsSource
  /** PICACHE_WEB_TLS_CERT is set. */
  envOverride: boolean
  /** An uploaded certificate is stored (it may be unused while envOverride). */
  uploadStored: boolean
  upload: { allowed: boolean; reason?: TlsUploadReason }
  certificate?: CertInfo
  /** PiCache's names and addresses the served certificate covers / does not cover (names first). */
  hostsCovered: string[]
  hostsNotCovered: string[]
  caAvailable: boolean
  localCa?: LocalCaInfo
  /** The configured certificate (files or upload) cannot be used: a fallback is served. */
  fallback: boolean
  error?: string
  checkedAt?: Timestamp
}

// ---------------------------------------------------------------- settings

/** How upstreams are asked (fastest_addr: like parallel, then the fastest-connecting address first). */
export type UpstreamMode = 'load_balance' | 'parallel' | 'strict' | 'fastest_addr'

/**
 * dns.dnssecMode: `off` asks for DNSSEC data only for devices that ask,
 * `passthrough` asks for it and passes the upstream's AD flag on,
 * `validate` checks the signatures in PiCache (400 field "dns.dnssecMode").
 */
export type DnssecMode = 'off' | 'passthrough' | 'validate'

/** The verdict of an answer PiCache validated (validate mode; absent for answers it did not validate). */
export type DnssecStatus = 'secure' | 'insecure' | 'bogus' | 'indeterminate'

/** Whether an upstream returns usable DNSSEC data (its probe; validate mode). */
export type UpstreamDnssecState = 'capable' | 'no-dnssec' | 'anchor-mismatch' | 'unknown'

/** settings.DNS */
export interface DnsSettings {
  upstreams: string[]
  /** Asked only when every default upstream failed to answer (transport error or timeout); at most 4, [] = off. */
  fallbackUpstreams: string[]
  bootstrap: string[]
  /** Dial the resolved addresses of DoT, DoH and named plain upstreams IPv6 first. */
  bootstrapPreferIpv6: boolean
  upstreamMode: UpstreamMode
  upstreamTimeoutMs: number
  /** Cache lifetime of answers the upstream blocked (10..86400 s). */
  upstreamBlockedTtl: number
  /** EDNS Client Subnet sent to the default upstreams. */
  ecs: EcsSettings
  localPtrUpstreams: string[]
  localDomain: string
  serverNames: string[]
  routerResolver: string
  /** Answer single-label names (A, AAAA, HTTPS, SVCB, ANY) as <name>.<localDomain>, never from the upstreams. */
  domainNeeded: boolean
  /** Extra reverse zones answered like the private ones (IPv4 /8, /16, /24; IPv6 /16../124 in steps of 4; at most 32). */
  privateReverseNetworks: string[]
  allowedNetworks: string[]
  allowAllNetworks: boolean
  /** Also trust every network this machine is connected to (public prefixes too; rebuilt every minute). */
  trustConnectedNetworks: boolean
  /** DNS queries of these IP addresses, networks, MAC addresses or clientid:<ClientID> entries are dropped (at most 256; DNS only). */
  blockedClients: string[]
  /** Forwarders whose EDNS client address (ECS /32, /128) and MAC option (65001) identify the client (at most 16). */
  ednsClientTrusted: string[]
  rateLimitQps: number
  rateLimitBurst: number
  rateLimitExempt: string[]
  /** Public IPv4 sources share a bucket per network of this length (8..32). */
  rateLimitIpv4Prefix: number
  /** Public IPv6 sources share a bucket per network of this length (32..64). */
  rateLimitIpv6Prefix: number
  refuseAny: boolean
  /** Block default-upstream answers with private or loopback addresses (DNS rebinding). */
  rebindProtection: boolean
  /** Domains (with subdomains) exempt from rebind protection (at most 256). */
  rebindAllow: string[]
  /** Answers with an address in one of these (IP or CIDR) become NXDOMAIN (at most 64). */
  bogusNxdomain: string[]
  /** "domain" or "domain:TYPE" (subtree): queries get no answer and are not logged (at most 256). */
  droppedDomains: string[]
  cacheEnabled: boolean
  cacheSize: number
  cacheMinTtl: number
  cacheMaxTtl: number
  serveStale: boolean
  serveStaleMaxAgeSec: number
  dnssecMode: DnssecMode
  /**
   * @deprecated The alias of `dnssecMode !== 'off'`, kept for API clients:
   * send `dnssecMode` (a write that sets both so that they contradict fails).
   */
  dnssec: boolean
  /** Answer forwarded AAAA queries with no records (networks with broken IPv6). Excludes dns64.enabled. */
  disableAAAA: boolean
  /** Synthesise AAAA records from A records for NAT64 networks (RFC 6147). */
  dns64: Dns64Settings
  /** false switches off every local record and its reverse record (DHCP names stay). */
  localRecordsEnabled: boolean
  /** Order of multi-address local answers: the client's own network first, only those, or as stored. */
  localizeRecords: LocalizeRecords
  /**
   * Addresses that server-name answers give (and blocked names with `self`);
   * empty = automatic. At most 8 per family (errors
   * "dns.serverNameAddresses.ipv4[i]", "….ipv6[i]").
   */
  serverNameAddresses: { ipv4: string[]; ipv6: string[] }
  /**
   * Plain DNS (port 53) for other devices. false closes it only while DoT or
   * DoH is serving (error field "dns.plainDns"); this machine keeps it.
   */
  plainDns: boolean
  /** DNS over TLS and DNS over HTTPS for devices (errors "dns.encrypted.<member>"). */
  encrypted: EncryptedDnsSettings
}

/** settings.EncryptedDNS */
export interface EncryptedDnsSettings {
  dot: boolean
  doh: boolean
  /** The name devices use for DoT and DoH; required while one is on (at most 189 characters, two labels or more). */
  serverName: string
}

/** dns.localizeRecords */
export type LocalizeRecords = 'off' | 'first' | 'only'

/**
 * settings.ECS: `client` sends the /24 (IPv4) or /56 (IPv6) of public client
 * addresses, `custom` sends `customSubnet` (a public IPv4 /8–/24 or IPv6
 * /32–/56; errors "dns.ecs.mode", "dns.ecs.customSubnet").
 */
export interface EcsSettings {
  mode: 'off' | 'client' | 'custom'
  customSubnet: string
}

/** settings.DNS64: `prefix` must be a /96 network (400 field "dns.dns64.prefix"). */
export interface Dns64Settings {
  enabled: boolean
  prefix: string
}

export type BlockingMode = 'null' | 'nxdomain' | 'nodata' | 'refused' | 'custom_ip'

/** The address value meaning "this server's address" (blocking and rule replies). */
export const SELF_ADDRESS = 'self'

/** settings.Filter */
export interface FilterSettings {
  enabled: boolean
  pausedUntil?: Timestamp
  blockingMode: BlockingMode
  /** An IPv4 address or "self" (mode custom_ip). */
  blockingIpv4: string
  /** An IPv6 address, "self" or "" (mode custom_ip). */
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

/**
 * Privacy level derived from the four switches (queryLogEnabled,
 * anonymizeClientIps, hideDomains, statsEnabled): full = (on, off, off, on),
 * hide-domains = (on, off, on, on), anonymous = (on, on, on, on),
 * off = (off, on, on, off), any other combination custom.
 */
export type PrivacyLevel = 'full' | 'hide-domains' | 'anonymous' | 'off' | 'custom'

/** settings.Logs (field errors "logs.<member>", "logs.ignoredDomains[i]"). */
export interface LogsSettings {
  /** Query rows and the live query feed (statistics are counted either way). */
  queryLogEnabled: boolean
  queryLogRetentionHours: number
  cacheLogRetentionHours: number
  sessionRetentionDays: number
  statsRetentionDays: number
  anonymizeClientIps: boolean
  maxDbSizeMiB: number
  /** Query names become "hidden" and answers are dropped before storage; the top kinds domain and blocked are not recorded. */
  hideDomains: boolean
  /** DNS statistics (counts, top lists, query types, unique domains); the download-cache statistics are always counted. */
  statsEnabled: boolean
  /** Domains (with subdomains) whose queries are answered but neither logged nor counted (at most 256; ASCII, no wildcards). */
  ignoredDomains: string[]
  /** DNS statistics count only A, AAAA and HTTPS queries (the query-type chart keeps every type). */
  statsOnlyAddressQueries: boolean
  /** Seconds between writes of new log rows and counts (5..300). */
  flushSeconds: number
  /** Days the addresses seen by the DNS server are kept (7..365). */
  seenRetentionDays: number
  /** Read-only: derived from the four switches; a value sent is ignored. */
  privacyLevel: PrivacyLevel
}

/** settings.Health: thresholds of the health check `host` (warnings only). */
export interface HealthSettings {
  /** Warn when less memory than this is available (1..50 %; the lower of the host and the container limit). */
  memoryAvailableMinPercent: number
  /** Warn when the 15-minute load exceeds this per CPU (1..16). */
  loadPerCpuMax: number
  /** Warn when a temperature sensor reaches this (50..110 °C). */
  temperatureMaxCelsius: number
}

/** settings.Web */
export interface WebSettings {
  sessionIdleMinutes: number
  sessionMaxHours: number
  allowedHosts: string[]
  redirectToHttps: boolean
  metricsEnabled: boolean
  /** '' = the browser's language. */
  language: '' | Locale
  /** The getting-started checklist on the Overview was hidden (false on a fresh installation until then). */
  onboardingDone: boolean
  /** Addresses or CIDRs allowed to use the web UI besides the always allowed ones (at most 64). */
  allowedNetworks: string[]
  /** Only this machine, private and connected networks and the allowed networks may use the web UI. */
  restrictToNetworks: boolean
  /** Reverse proxies whose X-Forwarded-For and X-Forwarded-Proto are read (at most 16). */
  trustedProxies: string[]
  /** Oldest TLS version the HTTPS listener accepts. */
  tlsMinVersion: '1.2' | '1.3'
}

/** settings.Updates (400 field "updates.channel", e.g. "nightly builds have no container image" in Docker). */
export interface UpdatesSettings {
  /** Check GitHub for a new release every day. */
  checkEnabled: boolean
  /** Derived alias of channel (channel != "stable"); send channel only. */
  includePrereleases: boolean
  channel: UpdateChannel
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
 * settings.DHCPOptions: typed DHCPv4 options, each sent only when a device
 * asks for it (field errors "dhcp.options.<member>", lists also "[i]").
 */
export interface DhcpOptions {
  /** Option 42: at most 4 unicast IPv4 addresses. */
  ntpServers: string[]
  /** Option 26: 0 = not sent, else 576..9000. */
  mtu: number
  /** Option 252: "" or an absolute http(s) URL (printable ASCII, at most 255 bytes). */
  wpadUrl: string
  /** Option 119 after the effective domain: at most 4 domains (IPv4 only). */
  extraSearchDomains: string[]
}

/**
 * settings.DHCP (PATCH /settings/dhcp; field errors "dhcp.<member>",
 * "dhcp.options.<member>" and "dhcp.ipv6.<member>"). Interface and range are
 * checked against the live interface only while `enabled` is true.
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
  /** Leases without a usable (or with a taken) host name answer as "192-168-1-23.<domain>" (DNS only; default true). */
  generateNames: boolean
  /** Serve although another DHCP server answers. */
  ignoreOtherServers: boolean
  /** DHCPv4 messages of devices without a reservation are ignored (INFORM and RELEASE excepted). */
  onlyReserved: boolean
  /** Answer a DISCOVER with option 80 by an ACK while no other DHCP server counts (default false). */
  rapidCommit: boolean
  options: DhcpOptions
  ipv6: DhcpIpv6Settings
}

/** settings.Clients: where the names of addresses come from (first hit wins, after a configured client name). */
export interface ClientsSettings {
  nameSources: {
    /** Reverse lookups (PTR) of client addresses. */
    ptr: boolean
    /** Host names of PiCache's own DHCP leases. */
    dhcp: boolean
    /** /etc/hosts of the machine PiCache runs on (in Docker: the container's own file). */
    hostsFile: boolean
    /** RDAP lookups of the network owner of public source addresses (an annotation, never a name; sends data out). */
    whois: boolean
  }
}

/**
 * settings.Sync: this PiCache as a read-only follower of a primary. `token`
 * is write-only (absent or null keeps the stored one, "" removes it, a value
 * replaces it); reads carry only tokenSet.
 */
export interface SyncSettings {
  mode: 'off' | 'follower'
  /** The primary's base URL: https://host[:port]. */
  source: string
  /** Input only: a sync token of the primary. */
  token?: string | null
  /** Output only: a token is stored. */
  tokenSet: boolean
  /** Trust anchor: 1–4 PEM certificates (the primary's local CA or its certificate); "" = the system roots. */
  caPem: string
  /** 5..1440 */
  intervalMinutes: number
  sections: SyncSection[]
}

/**
 * settings.Network: the outbound proxy for the chosen downloads. `password`
 * is write-only like SyncSettings.token; reads carry only passwordSet.
 */
export interface NetworkSettings {
  proxy: {
    /** http://host:port or socks5://host:port ("" = none). */
    url: string
    username: string
    /** Input only. */
    password?: string | null
    /** Output only: a password is stored. */
    passwordSet: boolean
  }
  proxyFor: {
    /** Blocklist and cache-domains downloads. */
    lists: boolean
    /** The release check. */
    updateCheck: boolean
    /** Notification channels and their tests. */
    notifications: boolean
  }
}

/** settings.NTP: answering NTP clients on the ntp listener (PICACHE_NTP_LISTEN). */
export interface NtpSettings {
  enabled: boolean
  /** 2..15, used while the host clock is synchronised. */
  stratum: number
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
  health: HealthSettings
  clients: ClientsSettings
  sync: SyncSettings
  network: NetworkSettings
  ntp: NtpSettings
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
  | 'blocked-upstream'
  | 'blocked-rebind'
  /** An address of the answer is on a list of answer addresses or an IP rule (ruleId is then an IP rule id). */
  | 'blocked-ip'
  | 'safesearch'
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
  'blocked-upstream',
  'blocked-rebind',
  'blocked-ip',
]

/** Status of a traced lookup: a query status, or `dropped` (blocked client or dropped domain; never logged). */
export type LookupStatus = QueryStatus | 'dropped'

/** dnsserver.BlockingStatus */
export interface BlockingStatus {
  enabled: boolean
  pausedUntil?: Timestamp
  permanent: boolean
  /** Host time zone ("CEST", "UTC"), for pauses "until 06:00" on the host's clock. */
  timeZone: string
  /** Its current offset from UTC in minutes (120 for CEST). */
  utcOffsetMinutes: number
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
  /** Queries of dns.blockedClients dropped since the start. */
  blockedClients: number
  /** Queries of dns.droppedDomains dropped since the start. */
  dropped: number
  topRateLimited: RateLimited[]
  /** Validated answers by DNSSEC status since the start (never filtered). */
  dnssec: Record<DnssecStatus, number>
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

/** Why nothing is announced over DDR (first match). */
export type DdrReason = 'off' | 'no-server-name' | 'not-serving' | 'no-ip-address'

/**
 * GET /dns/encrypted (api.EncryptedDNSStatus). A protocol is serving while
 * it is on, one of its listeners is bound and the certificate is usable.
 * Lists are never null.
 */
export interface EncryptedDnsStatus {
  /** dns.encrypted.serverName ("" when unset). */
  serverName: string
  /** enabled = dns.plainDns; served = plain DNS answers other devices now (it stays open while nothing serving). */
  plainDns: { enabled: boolean; served: boolean }
  dot: {
    enabled: boolean
    serving: boolean
    /** The bound DoT addresses. */
    listeners: string[]
    /** The provider host name for Android's Private DNS (absent without a server name). */
    host?: string
    /** The port of the first bound DoT listener. */
    port?: number
    /** Why it is not serving (only while enabled). */
    error?: string
  }
  doh: {
    enabled: boolean
    serving: boolean
    listeners: { address: string; role: 'web-tls' | 'doh' }[]
    /** https://<serverName>[:<port>]/dns-query per distinct port ([] without a server name). */
    urls: string[]
    error?: string
  }
  certificate: {
    source: TlsSource
    /** Not a fallback and not expired. */
    usable: boolean
    /** The served certificate is valid for the server name / for names below it (DoT ClientIDs). */
    covered: boolean
    wildcardCovered: boolean
    /** The local CA does not permit the server name: a new CA is needed. */
    localCaRenewalNeeded: boolean
    error?: string
  }
  /** Discovery of the encrypted endpoints (DDR, SVCB for _dns.resolver.arpa). */
  ddr: { active: boolean; reason?: DdrReason }
  /** Queries answered since the start. */
  queries: { dot: number; doh: number }
}

/** Protocol of an Apple configuration profile. */
export type ProfileProtocol = 'doh' | 'dot'

/**
 * Options of an Apple configuration profile (GET /dns/profile.mobileconfig,
 * POST /dns/profile-links). Errors: field protocol, dnsClientId or ssids (400);
 * 409 when the protocol is not serving, no server name is set and similar.
 */
export interface ProfileOptions {
  protocol: ProfileProtocol
  dnsClientId?: string
  /** Wi-Fi names (SSIDs) the profile applies on (at most 16); none = everywhere. */
  ssids?: string[]
  /** Include PiCache's addresses (ServerAddresses). */
  addresses?: boolean
}

/** POST /dns/profile-links (201): a link to the profile, valid 15 minutes and reusable until then. */
export interface ProfileLink {
  /** https://<serverName>[:<port>]/api/v1/dns/profile-links/<token> */
  url: string
  expiresAt: Timestamp
}

export type RecordType = 'A' | 'AAAA' | 'CNAME' | 'TXT' | 'SRV' | 'MX' | 'PTR' | 'HTTPS' | 'SVCB'

/** Record types whose `data` holds the structured form. */
export const STRUCTURED_RECORD_TYPES: readonly RecordType[] = ['SRV', 'MX', 'PTR', 'HTTPS', 'SVCB']

/** SRV: `<priority> <weight> <port> <target>` (target a name or "."). */
export interface SrvData {
  priority: number
  weight: number
  port: number
  target: string
}

/** MX: `<preference> <host>` (host "." only with preference 0). */
export interface MxData {
  preference: number
  host: string
}

/** PTR: the record name is a complete in-addr.arpa or ip6.arpa name. */
export interface PtrData {
  target: string
}

/** HTTPS and SVCB: priority 0 (alias form) takes no parameters. */
export interface SvcbData {
  priority: number
  target: string
  /** 1–8 protocol ids (e.g. h2, h3). */
  alpn: string[]
  port?: number
  ipv4hint: string[]
  ipv6hint: string[]
}

export type RecordData = SrvData | MxData | PtrData | SvcbData

/** Who a record answers: everyone, or the clients of its groups (none: nobody). */
export type RecordScope = 'all' | 'groups'

/** An A or AAAA answer set without a record of the asked family: no data, or on to the upstreams. */
export type OtherFamily = 'nodata' | 'forward'

/** dnsserver.Record: `value` is the presentation form of every type, `data` the structured form of SRV, MX, PTR, HTTPS and SVCB. */
export interface DnsRecord {
  id: number
  name: string
  type: RecordType
  value: string
  data?: RecordData
  ttl: number
  enabled: boolean
  comment: string
  scope: RecordScope
  groupIds: number[]
  otherFamily: OtherFamily
  createdAt: Timestamp
  updatedAt: Timestamp
}

/**
 * dnsserver.RecordInput (ttl 0 → 300). `data` wins over `value`. Absent
 * scope, groupIds and otherFamily keep the stored value (create: all, [],
 * nodata). Errors name type, name, value, data.<member>, scope, groupIds or
 * otherFamily.
 */
export interface DnsRecordInput {
  name: string
  type: RecordType
  value: string
  data?: RecordData
  ttl: number
  enabled: boolean
  comment: string
  scope?: RecordScope
  groupIds?: number[]
  otherFamily?: OtherFamily
}

/** One refused line of an import (line 0 = the text as a whole; `field` names the part, e.g. syntax or name). */
export interface ImportLineError {
  line: number
  field: string
  message: string
}

/**
 * Result of the rule and hosts imports: with any error nothing is written
 * (the counts say what the valid lines would do). `errors` holds the first
 * error of each line (at most 1000), `errorCount` counts every line with one.
 */
export interface LineImportResult {
  applied: boolean
  added: number
  unchanged: number
  skipped: number
  errors: ImportLineError[]
  errorCount: number
}

/** POST /dns/records/import: hosts lines become A and AAAA records. */
export interface RecordImportRequest {
  format: 'hosts'
  text: string
  scope?: RecordScope
  groupIds?: number[]
  dryRun: boolean
}

/** Batch change of the selected rows (1–1000 ids; all or nothing). */
export type BatchAction = 'delete' | 'enable' | 'disable'

export interface BatchRequest {
  action: BatchAction
  ids: number[]
  /** Lists only: enable although the entry budget would be exceeded (409 with field "force" otherwise). */
  force?: boolean
}

/** Rows whose state changed (deleting counts every id). */
export interface BatchResult {
  changed: number
}

/** Forwarder domain matching single-label names (A, AAAA, HTTPS, SVCB, ANY; never the root). */
export const UNQUALIFIED_DOMAIN = '(unqualified)'
/** Forwarder target meaning "the default upstreams" (must be the only target). */
export const DEFAULT_TARGET = 'default'

/** dnsserver.Forwarder: `domain` is `domains[0]`. */
export interface Forwarder {
  id: number
  domain: string
  /** Every domain of the forwarder (1–16). */
  domains: string[]
  upstreams: string[]
  enabled: boolean
  /** Validate the DNSSEC signatures of its answers (acts only in the DNSSEC mode validate). */
  validate: boolean
  comment: string
  createdAt: Timestamp
  updatedAt: Timestamp
}

/**
 * dnsserver.ForwarderInput: `domains` wins, `domain` alone means [domain]
 * (errors "domain", "domains", "domains[i]", "upstreams", "upstreams[i]",
 * "validate"; 409 for a domain another forwarder holds). PUT replaces
 * `validate` like `enabled` (absent = false), so it is always sent.
 */
export interface ForwarderInput {
  domain?: string
  domains?: string[]
  upstreams: string[]
  enabled: boolean
  /** Only for explicit targets outside the locally served zones and without (unqualified) (400 field "validate"). */
  validate: boolean
  comment: string
}

/** POST /dns/forwarders/import: `[/d1/d2/]t1 t2` lines (`#` = default, `[//]` = single-label names). */
export interface ForwarderImportRequest {
  text: string
  dryRun: boolean
}

/** One refused line of a forwarder import (`field`: syntax, domains, upstreams, or text with line 0). */
export interface ForwarderImportError {
  line: number
  field: string
  message: string
}

/** Result of POST /dns/forwarders/import; nothing is written when there is any error. */
export interface ForwarderImportResult {
  applied: boolean
  added: number
  updated: number
  unchanged: number
  errors: ForwarderImportError[]
}

/** POST /filter/rules/import answer. */
export type RuleImportResult = LineImportResult

/** POST /dns/records/import answer. */
export type RecordImportResult = LineImportResult

/** POST /dns/blocked-clients: `entry` is the stored or the already matching entry. */
export interface BlockClientResult {
  entry: string
  added: boolean
  blockedClients: string[]
}

/** dnsserver.LookupRequest */
export interface LookupRequest {
  name: string
  type?: string
  clientIp?: string
  /** Evaluates the query as a DoT/DoH query from clientIp carrying this ClientID (400 field "dnsClientId"). */
  dnsClientId?: string
}

/** dnsserver.LookupResult */
export interface LookupResult {
  name: string
  type: string
  status: LookupStatus
  rcode: string
  answers: string[]
  reason?: string
  upstream?: string
  durationUs: number
  groupIds: number[]
  steps: string[]
  matches: FilterMatch[]
  /** The DNSSEC verdict of the fetched data (validate mode, validated routes). */
  dnssecStatus?: DnssecStatus
  /** Why: the failure of `bogus` ("bad signature"), the cause of `insecure` and `indeterminate`; `dnssecEde.text` adds the zone. */
  dnssecReason?: string
  /** The extended DNS error a client gets for a `bogus` answer. */
  dnssecEde?: { code: number; text: string }
}

// ---------------------------------------------------------------- upstream

/** upstream.UpstreamStat */
export interface UpstreamStat {
  /** The configured entry (match it with the settings; a DNS stamp is shown as `name`). */
  upstream: string
  /** The name for display: the entry itself, or "sdns:<protocol>:<host>" for a DNS stamp. */
  name: string
  queries: number
  errors: number
  avgRttMs: number
  lastError?: string
  lastErrorAt?: Timestamp
  healthy: boolean
  /** Whether it returns DNSSEC data (validate mode, upstreams of validated routes only). */
  dnssec?: UpstreamDnssecState
  /** What its last probe found wrong (bounded and cleaned by the server). */
  dnssecError?: string
  /** When it was last probed. */
  dnssecCheckedAt?: Timestamp
}

/** upstream.CacheStat */
export interface UpstreamCacheStat {
  entries: number
  capacity: number
  hits: number
  misses: number
  staleHits: number
  /** Answers stored since the start. */
  insertions: number
  /** Entries removed for capacity. */
  evictions: number
  /** Entries removed after their TTL plus the serve-stale window. */
  expired: number
  /** Current entries by record type: the 16 largest, then OTHER for the rest (entries desc). */
  types: { type: string; entries: number }[]
  /** The DNSSEC key cache (validated zones, their bytes) and the failure cache (validate mode only). */
  validation?: { zones: number; bytes: number; failures: number }
}

/** The upstreams of one group upstream list, shared by the groups that name it. */
export interface GroupUpstreamSet {
  groupIds: number[]
  /** Preset key ("" for an own list). */
  preset: string
  upstreams: UpstreamStat[]
  /** The clock guard is active: only the list's plain IP upstreams are asked. */
  clockGuard: boolean
  /** Why the set could not be built (its clients get SERVFAIL). */
  error?: string
}

/** Why DNSSEC time checks are suspended: the clock guard, an unsynchronised host clock, root signatures outside their period. */
export type DnssecTimeReason = 'clock-guard' | 'unsynced' | 'root-signatures'

/** The DNSSEC state of GET /dns/upstreams (validate mode only). */
export interface UpstreamsDnssec {
  /** Signature dates are checked (`suspended`: answers are `indeterminate` until the clock is right). */
  timeChecks: 'active' | 'suspended'
  timeReason?: DnssecTimeReason
  /** A validated root DNSKEY set holds a key this version has no anchor for: update PiCache. */
  newRootKey: boolean
  /** The validating forwarders and their targets (never null). */
  forwarders: { id: number; domains: string[]; upstreams: UpstreamStat[] }[]
}

/** GET /dns/upstreams */
export interface UpstreamsState {
  upstreams: UpstreamStat[]
  /** Statistics of dns.fallbackUpstreams. */
  fallbacks: UpstreamStat[]
  /** When a fallback last answered (absent if never since the start). */
  fallbackLastUsed?: Timestamp
  cache: UpstreamCacheStat
  clockGuard: boolean
  /** The upstream lists of groups. */
  groups: GroupUpstreamSet[]
  /** Present only in the DNSSEC mode validate. */
  dnssec?: UpstreamsDnssec
}

/** One fixed name of POST /dns/dnssec/test. */
export interface DnssecTestCheck {
  name: string
  expect: 'secure' | 'insecure' | 'bogus'
  /** `error`: no reply, SERVFAIL to the query with CD=1 or NXDOMAIN (the test name is gone). */
  status: DnssecStatus | 'error'
  rcode?: string
  reason?: string
  ede?: { code: number; text: string }
  /** The upstream that answered. */
  upstream?: string
  /** The upstream's own validator answered SERVFAIL to the query with CD=0. */
  upstreamRefused: boolean
  verdict: 'pass' | 'fail' | 'inconclusive'
}

/**
 * upstream.DNSSECTest (POST /dns/dnssec/test): the default upstreams probed
 * now and the fixed test names validated locally, in every mode. 409 while
 * one runs, 429 within 10 s of the last start.
 */
export interface DnssecTest {
  /** The mode when the test ran. */
  mode: DnssecMode
  timeChecks: 'active' | 'suspended'
  timeReason?: DnssecTimeReason
  upstreams: UpstreamStat[]
  checks: DnssecTestCheck[]
  durationMs: number
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
  /** The group's own upstreams ([] = none). */
  upstreams: string[]
  /** A family resolver preset ("" = none); excludes `upstreams`. */
  upstreamPreset: string
  /** Made by "Only for this device" for this client. */
  deviceClientId?: number
}

/** clients.GroupInput: absent upstreams and upstreamPreset keep the stored values. */
export interface ClientGroupInput {
  name: string
  comment: string
  enabled: boolean
  upstreams?: string[]
  upstreamPreset?: string
}

/** PUT /groups/{id}/upstreams (both required; [] and "" = none). Errors upstreams, upstreams[i], upstreamPreset. */
export interface GroupUpstreamsInput {
  upstreams: string[]
  upstreamPreset: string
}

/** GET /groups/upstream-presets: a family resolver (DNS over HTTPS) with its plain addresses for the clock guard. */
export interface UpstreamPreset {
  key: string
  name: string
  upstreams: string[]
  plain: string[]
}

/** clients.Client */
export interface Client {
  id: number
  name: string
  /** IP addresses, CIDRs, MAC addresses and clientid:<ClientID> entries. */
  identifiers: string[]
  groupIds: number[]
  comment: string
  downloadCacheBypass: boolean
  /** Its raw data is not recorded: query rows, the live feeds, cache requests, SNI rows, downloads, seen addresses. */
  ignoreLogs: boolean
  /** It is not counted in the DNS and cache statistics. */
  ignoreStats: boolean
  createdAt: Timestamp
  updatedAt: Timestamp
}

/**
 * clients.ClientInput. `ignoreStats` absent or null means "the value of
 * ignoreLogs" (the meaning of the single flag before 0.12); the UI always
 * sends both.
 */
export interface ClientInput {
  name: string
  identifiers: string[]
  groupIds: number[]
  comment: string
  downloadCacheBypass: boolean
  ignoreLogs: boolean
  ignoreStats?: boolean
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
  /** The first dns.blockedClients entry matching the address, MAC or ClientID. */
  blockedBy?: string
  /**
   * The last ClientID (DoT/DoH device ID) the address sent since the start;
   * clientId/name come from its client when the address identifies none itself.
   */
  dnsClientId?: string
  /** The interface replies to the address leave by (computed from the routes when read). */
  interface?: string
  /** Manufacturer of the MAC address (IEEE registry). */
  vendor?: string
  /** The MAC address is locally administered (a private, randomised address): never a vendor. */
  macRandomized?: boolean
  /** Owner of the address's network (RDAP; public addresses only, name source whois). */
  whois?: { org: string; country?: string }
}

/** clients.SeenDNSClientID: a ClientID sent since the start (kept in memory, at most 1024). */
export interface SeenDnsClientId {
  dnsClientId: string
  /** The last address that sent it. */
  address: string
  /** The configured client that has the identifier clientid:<ClientID>. */
  clientId?: number
  name?: string
  firstSeen: Timestamp
  lastSeen: Timestamp
  queries: number
}

/** ID of the built-in "Default" group (cannot be deleted). */
export const DEFAULT_GROUP_ID = 1

// ---------------------------------------------------------------- parental controls

/** Service categories in catalogue order (`software` = app stores, `hosting` = file sharing and cloud storage). */
export type ServiceCategory =
  | 'video'
  | 'social'
  | 'messaging'
  | 'gaming'
  | 'music'
  | 'ai'
  | 'dating'
  | 'gambling'
  | 'shopping'
  | 'privacy'
  | 'software'
  | 'hosting'
  | 'news'

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
  /** Filtering of the group is paused (its lists and rules stop applying; independent of blockAll and lifted). */
  paused: boolean
  pausedUntil?: Timestamp
  /** Host time zone that schedule times refer to ("CEST", "UTC"). */
  timeZone: string
  /** Its current offset from UTC in minutes (120 for CEST). */
  utcOffsetMinutes: number
}

export type YoutubeMode = 'off' | 'moderate' | 'strict'

/** parental.SafeSearch: in effect whenever the group is enabled (not lifted by an allow override). */
export interface SafeSearch {
  google: boolean
  youtube: YoutubeMode
  bing: boolean
  duckduckgo: boolean
  ecosia: boolean
  yandex: boolean
  pixabay: boolean
}

/** Category switches: each binds one catalogue list that is assigned to the group. */
export type CategorySwitch = 'adult' | 'gambling' | 'dating' | 'piracy' | 'bypass'

/**
 * State of a category switch: `on` = every bound list is enabled and
 * assigned to the group; `state` pending = no copy downloaded yet, failed =
 * a bound list failed without a copy.
 */
export interface CategoryState {
  on: boolean
  state: 'off' | 'active' | 'pending' | 'failed'
}

/** parental.GroupControls (GET/PUT /parental/groups/{id}). */
export interface GroupControls {
  groupId: number
  groupName: string
  groupEnabled: boolean
  clientCount: number
  blockedServices: string[]
  schedules: ParentalSchedule[]
  /** The stored configuration (always present). */
  safeSearch: SafeSearch
  /** Filled from the filter lists; a switch removed from the server is absent. */
  categories: Partial<Record<CategorySwitch, CategoryState>>
  override?: ParentalOverride
  state: ParentalGroupState
  updatedAt?: Timestamp
}

/**
 * PUT /parental/groups/{id}. `safeSearch` and `categories` are optional on
 * the server (absent or null members keep the stored value or the list
 * assignment); the UI always sends all four members.
 */
export interface GroupControlsInput {
  blockedServices: string[]
  schedules: ParentalSchedule[]
  safeSearch?: SafeSearch
  categories?: Partial<Record<CategorySwitch, boolean>>
}

/** PUT /parental/groups/{id}/override: either minutes (1..10080) or until (≤ 7 days ahead). */
export type OverrideInput = { mode: OverrideMode; minutes: number } | { mode: OverrideMode; until: Timestamp }

/** PUT /parental/groups/{id}/pause: either minutes (1..10080) or until (in the future, ≤ 7 days ahead). */
export type PauseInput = { minutes: number } | { until: Timestamp }

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
  /**
   * DNS servers (RDNSS) the IPv6 default router announces; present only while
   * PiCache records router advertisements (its own are on), [] = none.
   */
  routerRdnss?: string[]
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
  vendor?: string
  macRandomized?: boolean
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
  /**
   * An IPv4 address of the interface of the IPv4 default route came from a
   * DHCP client (a finite lifetime). Absent in mode "bridge", on systems
   * other than Linux and without an IPv4 default route.
   */
  dynamic4?: boolean
}

/**
 * The effective client of the request that asked for the network check
 * (after the trusted-proxy rules of the web access list).
 */
export interface NetworkRequester {
  address: string
  /** Loopback or one of this machine's addresses. */
  local: boolean
  /** From the device of the network whose addresses contain it. */
  mac?: string
  name?: string
  clientId?: number
  /** Its DNS queries of the last 24 hours (absent while privacy is set). */
  queries24h?: number
  lastQuery?: Timestamp
  /** logs.anonymizeClientIps is on: no counts. */
  privacy?: boolean
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
  /** The device that asked (computed for every request, never cached). */
  requester?: NetworkRequester
}

/** Operational state of an interface (sysfs operstate). */
export type OperState = 'up' | 'down' | 'dormant' | 'lowerlayerdown' | 'notpresent' | 'testing' | 'unknown'

/** An interface of GET /network/interfaces (loopback excluded). Lists are never null. */
export interface NetworkInterface {
  name: string
  index: number
  mac?: string
  up: boolean
  operState: OperState
  mtu: number
  /** Absent when the driver does not report it. */
  speedMbps?: number
  duplex?: 'full' | 'half'
  /** CIDR notation. */
  addresses: string[]
  /** The non-default routes through the interface (at most 64). */
  networks: string[]
  /** Bridges, veth, tunnels and the like. */
  virtual: boolean
  rxBytes: number
  txBytes: number
  rxErrors: number
  txErrors: number
  defaultGateways: { family: 'ipv4' | 'ipv6'; gateway: string }[]
}

/** GET /network/interfaces (sorted by name; [] on systems other than Linux). */
export interface NetworkInterfaces {
  /** bridge: a container bridge network (the container's own interfaces). */
  mode: 'host' | 'bridge'
  interfaces: NetworkInterface[]
}

/** 202 of POST /network/scan */
export interface NetworkScanStarted {
  started: boolean
  addresses: number
}

// ---------------------------------------------------------------- dhcp server

/** unavailable: see DhcpReasonCode; blocked: enabled, but a blocker applies. */
export type DhcpState = 'unavailable' | 'off' | 'blocked' | 'serving' | 'error'

/**
 * Why the DHCP server is unavailable: PICACHE_DHCP=off (opt-out), not Linux,
 * a container network (bridge), the ports could not be opened (socket: e.g.
 * another DHCP server on this host), or they open only at the next start
 * (restart-required: Docker after the switch to PICACHE_RUN_AS).
 */
export type DhcpReasonCode = 'opt-out' | 'not-linux' | 'bridge' | 'socket' | 'restart-required' | (string & {})

/** How PiCache runs: a Docker/Podman container, a systemd service or anything else. */
export type DhcpDeployment = 'systemd' | 'docker' | 'other' | (string & {})

/**
 * Why router advertisements are not available: DHCP itself is unavailable,
 * the raw socket opens at the next start, the process lacked CAP_NET_RAW at
 * this start (also reported while the option is off), dropping CAP_NET_RAW
 * could not be verified, or opening the socket failed otherwise.
 */
export type DhcpRaReasonCode =
  | 'dhcp-unavailable'
  | 'restart-required'
  | 'no-cap-net-raw'
  | 'drop-unverified'
  | 'socket'
  | (string & {})

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
  /** false exactly while reasonCode is set. */
  available: boolean
  reason?: string
  reasonCode?: DhcpRaReasonCode
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

/**
 * Another router or DHCPv6 server that announces DNS on the interface: seen
 * in a router advertisement (kind ra) or answering PiCache's relayed
 * information request (kind dhcpv6).
 */
export interface DhcpAnnouncer {
  kind: 'ra' | 'dhcpv6' | (string & {})
  /** Source address (link-local for router advertisements). */
  address: string
  interface: string
  /** dhcpv6 only: the server's DUID (hex). */
  serverId?: string
  /** Announced DNS servers. */
  dns: string[]
  /** The entries of dns that are addresses of this machine (PiCache itself; they never warn). */
  ownDns: string[]
  /** ra only: the M flag (addresses from DHCPv6). */
  managed?: boolean
  /** ra only: the O flag (other information from DHCPv6). */
  other?: boolean
  /** ra only: seconds; 0 = not a default router. */
  routerLifetime?: number
  firstSeen: Timestamp
  lastSeen: Timestamp
  /** Announces a DNS server that is not PiCache while PiCache announces itself (seen at least twice). */
  conflict: boolean
}

/** GET /dhcp */
export interface DhcpStatus {
  available: boolean
  /** Why the DHCP server is unavailable (English text for logs; the UI uses reasonCode). */
  reason?: string
  /** Set whenever state is unavailable. */
  reasonCode?: DhcpReasonCode
  /**
   * Why a marker file could not be written (English text), in every state.
   * With reasonCode restart-required a restart will not open the ports.
   */
  markerError?: string
  deployment: DhcpDeployment
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
    /** Newest first, at most 32. */
    otherAnnouncers: DhcpAnnouncer[]
    /** The last search for other announcers and which parts ran. */
    lastSearch?: { time: Timestamp; ra: boolean; dhcpv6: boolean }
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
  /** dnsName was generated from the address (generateNames): DNS only. */
  nameGenerated?: boolean
  /** Another active lease holds the host name: this one gets no name of its own. */
  nameConflict?: boolean
  vendor?: string
  macRandomized?: boolean
}

export interface DhcpStaticLease {
  mac: string
  ip: string
  hostname?: string
  comment?: string
  /** Option 61 as colon-separated hex (lower case): also matches a device that sends it. */
  clientId?: string
  /** 300..604800; omitted = the global lease time. */
  leaseSeconds?: number
  createdAt: Timestamp
  updatedAt: Timestamp
  /** A device uses it right now. */
  active: boolean
  vendor?: string
  macRandomized?: boolean
}

/**
 * POST /dhcp/static (PUT /dhcp/static/{mac} ignores mac and replaces the
 * reservation: members left out are cleared). 400 with field mac, ip,
 * hostname, comment, clientId or leaseSeconds.
 */
export interface DhcpStaticLeaseInput {
  mac: string
  ip: string
  hostname?: string
  comment?: string
  clientId?: string
  /** 0 = the global lease time, else 300..604800. */
  leaseSeconds?: number
}

export type DhcpImportFormat = 'csv' | 'hosts' | 'lines'

/** POST /dhcp/static/import (400 with field format or text for the request as a whole). */
export interface DhcpImportRequest {
  format: DhcpImportFormat
  text: string
  /** Delete every reservation whose MAC address is not in the list. */
  replace: boolean
  /** Check only: nothing is written. */
  dryRun: boolean
}

/** A refused line of an import; line 0 = the list as a whole. */
export interface DhcpImportError {
  line: number
  /** mac, ip, hostname, comment, clientId, leaseSeconds, row or text. */
  field: string
  message: string
}

/** 200 of POST /dhcp/static/import: with any error nothing is written (the counts say what the valid rows would do). */
export interface DhcpImportResult {
  applied: boolean
  added: number
  updated: number
  unchanged: number
  removed: number
  errors: DhcpImportError[]
}

/** An entry of GET /dhcp/log (the last 200 handled exchanges, newest first; in memory). */
export interface DhcpLogEntry {
  time: Timestamp
  kind: 'dhcpv4' | 'dhcpv6' | 'ra' | (string & {})
  mac?: string
  /** DHCPv6 client DUID (hex). */
  duid?: string
  /** DHCPv4: the offered, acknowledged or requested address; DHCPv6 and RS: the link-local source. */
  address?: string
  hostname?: string
  /** DISCOVER, REQUEST, DECLINE, RELEASE, INFORM, INFORMATION-REQUEST or RS. */
  in: string
  /** OFFER, ACK, NAK, REPLY or RA; absent without an answer. */
  out?: string
  result: 'answered' | 'nak' | 'processed' | 'ignored' | (string & {})
  /** rapid-commit, not-reserved, other-server, pool-exhausted, address-unavailable, move-to-reservation, client-id-conflict. */
  reason?: string
}

// ---------------------------------------------------------------- filter

export type ListStatus = 'pending' | 'ok' | 'unchanged' | 'failed-cached' | 'failed-empty'

/** Catalogue categories in API and display order. */
export const CATALOG_CATEGORIES = [
  'general',
  'security',
  'privacy',
  'adult',
  'gambling',
  'dating',
  'piracy',
  'social',
  'doh-vpn-bypass',
  'abused-tlds',
  'url-shorteners',
  'stalkerware',
  'regional',
  'allow',
] as const

export type CatalogCategory = (typeof CATALOG_CATEGORIES)[number]

/** Category of a list: a catalogue category or `other` (user lists only). `allow` ⇔ kind allow. */
export type ListCategory = CatalogCategory | 'other'

/**
 * Protection categories: enabled lists of these categories are enforced like
 * parental controls (also while blocking is paused or disabled).
 */
export const PROTECTION_CATEGORIES: readonly ListCategory[] = ['adult', 'gambling', 'dating', 'piracy', 'doh-vpn-bypass']

/** What a list holds: domain names, or addresses that must not appear in answers. */
export type ListFormat = 'domains' | 'ips'

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
  category: ListCategory
  /** Key of the catalogue entry with exactly this URL ("" for the user's own lists). */
  catalogKey: string
  /** `ips`: a list of answer addresses (category security or other; allowlists allow). */
  format: ListFormat
  /** The name is the host name for now: the first download's `! Title:` line replaces it. */
  nameAuto: boolean
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
  /**
   * Entries of the loaded copy that would block a whole top-level domain and
   * that the TLD guard ignores (part of `invalid`; 0 while no copy is loaded).
   * Lists of the category abused-tlds are exempt.
   */
  tldBlocksIgnored: number
  /**
   * Entries of the loaded copy that the IP guard ignores: too broad networks,
   * private, loopback or multicast ranges (part of `invalid`).
   */
  ipBlocksIgnored: number
}

/**
 * filter.ListInput. `category` absent or "" means: the catalogue entry's
 * category on create (else `other`), the stored one on update; `allow` is
 * stored for every allowlist (400 field "category"/"kind" otherwise).
 * `format` absent keeps the stored one (create: domains; 400 "format").
 */
export interface FilterListInput {
  name: string
  url: string
  kind: 'block' | 'allow'
  plainDomains: 'exact' | 'subtree'
  enabled: boolean
  groupIds: number[]
  comment: string
  category?: ListCategory | ''
  format?: ListFormat
}

export type RuleAction = 'allow' | 'block'
export type RuleType = 'exact' | 'subtree' | 'regex'

/** How a block rule answers: "" = the global blocking mode (settings filter.blockingMode). */
export type RuleReply = '' | 'null' | 'nxdomain' | 'nodata' | 'refused' | 'custom_ip'

/** filter.Rule */
export interface FilterRule {
  id: number
  action: RuleAction
  type: RuleType
  pattern: string
  enabled: boolean
  groupIds: number[]
  comment: string
  /** Query types the rule applies to (empty: every type), normalised and sorted by type number. */
  qtypes: string[]
  /** Applies to every type except `qtypes`. */
  qtypesNegate: boolean
  reply: RuleReply
  /** An IPv4 address, "self" or "" (reply custom_ip). */
  replyIpv4: string
  /** An IPv6 address, "self" or "" (reply custom_ip). */
  replyIpv6: string
  /** Names equal to or below these domains are excepted (subtree and regex block rules). */
  denyallow: string[]
  /** Regex block rules: blocks every name the expression does not match. */
  invert: boolean
  createdAt: Timestamp
  updatedAt: Timestamp
}

/**
 * filter.RuleInput: absent optional members keep the stored value (create:
 * the default). Errors name action, type, pattern, groupIds, comment,
 * qtypes[i], qtypesNegate, reply, replyIpv4, replyIpv6, denyallow[i], invert.
 */
export interface FilterRuleInput {
  action: RuleAction
  type: RuleType
  pattern: string
  enabled: boolean
  groupIds?: number[]
  comment: string
  qtypes?: string[]
  qtypesNegate?: boolean
  reply?: RuleReply
  replyIpv4?: string
  replyIpv6?: string
  denyallow?: string[]
  invert?: boolean
}

/** GET /filter/rules query */
export interface RuleQuery {
  action?: RuleAction
  type?: RuleType
  search?: string
}

/** POST /filter/rules/import: groupIds absent = the Default group, [] = no group. */
export interface RuleImportRequest {
  text: string
  groupIds?: number[]
  dryRun: boolean
}

/** POST /filter/rules/device: a rule that applies to one device only (errors clientIp, rule.<field>). */
export interface DeviceRuleRequest {
  clientIp: string
  /** Without groupIds: the server chooses the device's own group. */
  rule: FilterRuleInput
}

/** Answer of POST /filter/rules/device. */
export interface DeviceRuleResult {
  client: Client
  group: ClientGroup
  rule: FilterRule
  createdClient: boolean
  createdGroup: boolean
  createdRule: boolean
}

/** filter.IPRule: blocks or allows an address (or network) in answers. */
export interface IPRule {
  id: number
  action: RuleAction
  /** A canonical address or masked CIDR (at least /8 for IPv4, /32 for IPv6). */
  pattern: string
  enabled: boolean
  groupIds: number[]
  comment: string
  createdAt: Timestamp
  updatedAt: Timestamp
}

/** filter.IPRuleInput (errors action, pattern, comment, groupIds). */
export interface IPRuleInput {
  action: RuleAction
  pattern: string
  enabled: boolean
  groupIds?: number[]
  comment: string
}

/** GET /filter/ip-rules query (search: part of the pattern or comment, at most 256 characters). */
export interface IPRuleQuery {
  action?: RuleAction
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
  /** The list's category ("" for rules); `privacy` marks a known tracker. */
  category: string
  qtypes: string[]
  qtypesNegate: boolean
  denyallow: string[]
  invert: boolean
  /** Rules: the rule's reply ("" = the global mode); always "" for list entries. */
  reply: RuleReply
  /** The entry matches the name but does not apply to this query type or name (then never decisive). */
  skipped?: 'qtype' | 'denyallow'
}

/** One hit of GET /filter/search. */
export interface SearchItem {
  source: 'rule' | 'ip-rule' | 'list'
  ruleId?: number
  listId?: number
  /** The rule pattern or the list name. */
  name: string
  /** The export line of a rule, the line of a list (without its inline comment). */
  entry: string
  kind: 'exact' | 'subtree' | 'regex' | 'ip'
  action: RuleAction
  qtypes: string[]
  qtypesNegate: boolean
  denyallow: string[]
  groupIds: number[]
  /** The rule's or IP rule's flag (list entries: true, only enabled lists are searched). */
  enabled: boolean
  /** Only with clientIp: the entry is enabled and shares an enabled group with the client. */
  applies?: boolean
}

/** GET /filter/search (503 while two searches or explanations run). */
export interface SearchResult {
  q: string
  items: SearchItem[]
  /** The limit was reached. */
  truncated: boolean
  /** The 10 s budget ran out: the hits so far. */
  timedOut: boolean
  scannedLists: number
  totalLists: number
}

/** filter.Stats */
export interface FilterStats {
  lists: number
  /** Every entry, answer addresses and entries with modifiers included. */
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
  /** Enabled own lists (no catalogue key) with entries the TLD guard ignores. */
  tldGuardLists: number
  /** Answer addresses of the lists (part of entries). */
  ipEntries: number
  /** Enabled IP rules. */
  ipRules: number
  /** List entries with query types or exceptions (part of entries). */
  modifiedEntries: number
  /** Such entries beyond the total cap. */
  modifiedDropped: number
  /** Enabled own lists with entries the IP guard ignores. */
  ipGuardLists: number
}

/** filter.CatalogEntry (every member always present). */
export interface CatalogEntry {
  key: string
  name: string
  description: string
  descriptionDe: string
  url: string
  kind: 'block' | 'allow'
  category: CatalogCategory
  plainDomains: 'exact' | 'subtree'
  /** Suggested for its category (listed first). */
  recommended: boolean
  /** Entry count of the release's verification run. */
  entries: number
  maintainer: string
  /** "" when the maintainer states none. */
  license: string
  homepage: string
}

/** POST /filter/explain */
export interface ExplainResult {
  domain: string
  /** The query type evaluated (default A). */
  qtype: string
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
  /** The upstream's answer where it differs from the final one (CNAME, upstream and rebinding blocks, bogus NXDOMAIN, DNS64, a removed ipv6hint). */
  upstreamAnswer?: string
  dnssec?: boolean
  protocol: QueryProtocol
  /** Extended DNS error of the upstream's reply (text bounded and cleaned by the server). */
  upstreamEde?: { code: number; text: string }
  /** The client's own EDNS Client Subnet option, e.g. "203.0.113.0/24". */
  ecs?: string
  /** The ClientID the DoT or DoH query carried (removed while client addresses are anonymised). */
  dnsClientId?: string
  /** PiCache's DNSSEC verdict of the answer (validate mode; `dnssec` says whether AD was sent). */
  dnssecStatus?: DnssecStatus
}

/** How a query reached PiCache: plain DNS over UDP or TCP, DNS over TLS or DNS over HTTPS. */
export type QueryProtocol = 'udp' | 'tcp' | 'dot' | 'doh'

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
  /**
   * Where the top tables start for this range: `from` aligned down to the
   * hour, or to the UTC day for ranges longer than 7 days. Top lists, client
   * statistics, activeClients and uniqueDomains cover [topFrom, to).
   */
  topFrom: Timestamp
  /** Distinct query names (an estimate: shown as "about"). */
  uniqueDomains: number
  uniqueDomainsEstimated: boolean
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

/**
 * Purpose of a blocked (or safe-search) query: the list's category for list
 * decisions, else the mechanism.
 */
export type Purpose = ListCategory | 'rule' | 'service' | 'schedule' | 'upstream' | 'rebind' | 'special' | 'safesearch'

/** logs.PurposeStats (GET /stats/purposes): sorted by count, then purpose. */
export interface PurposeStats {
  /** Start actually covered (like Summary.topFrom). */
  from: Timestamp
  purposes: { purpose: Purpose; count: number }[]
}

/** logs.DNSSECStats (GET /stats/dnssec): validated queries by status, most first, then by status. */
export interface DnssecStats {
  /** Start actually covered (like Summary.topFrom). */
  from: Timestamp
  statuses: { status: DnssecStatus; count: number }[]
}

/** logs.QTypeStats (GET /stats/qtypes): most first, then by name; at most 32 types per hour, the rest as OTHER. */
export interface QTypeStats {
  /** Start actually covered (like Summary.topFrom). */
  from: Timestamp
  qtypes: { qtype: string; count: number }[]
}

/**
 * logs.ClientSeries (GET /stats/clients/{key}/series): queries and cache
 * bytes of one client per step (unix seconds, bucket start).
 */
export interface ClientSeries {
  step: number
  timestamps: number[]
  values: { allowed: number[]; blocked: number[]; cacheBytes: number[] }
  /** The addresses a device key resolved to (at most 256, most recently seen). */
  addresses: string[]
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
  /** Upstreams: the average response time in µs, as avgDurationUs (kept for older clients). */
  bytes?: number
  /** Upstreams: the average response time in µs. */
  avgDurationUs?: number
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
  /** Reply codes, ORed (at most 16). */
  rcode?: string[]
  /** true: answers sent with the AD flag; false: the others. */
  dnssec?: boolean
  /** PiCache's DNSSEC verdicts, ORed (at most 4; 400 field "dnssecStatus"). */
  dnssecStatus?: DnssecStatus[]
  /** The ClientID the query carried (exact; 400 field "dnsClientId" for an invalid one). */
  dnsClientId?: string
  cursor?: string
  limit?: number
}

/** GET /logs/queries/export: the query-log filters without paging. */
export type QueryExportQuery = Omit<QueryLogQuery, 'cursor' | 'limit'>

export type QueryExportFormat = 'ndjson' | 'csv'

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

// ---------------------------------------------------------------- diagnostics

/** Level of an application log record. */
export type LogLevel = 'DEBUG' | 'INFO' | 'WARN' | 'ERROR'

/** api.LogRecord: one record of the application log (attributes flattened as "group.key"). */
export interface LogRecord {
  seq: number
  time: Timestamp
  level: LogLevel
  /** One of SystemLog.components ("app" for records without a component). */
  component: string
  msg: string
  attrs: { key: string; value: string }[]
}

/** A temporary debug level (memory only; ends at `until` or with a restart). */
export interface LogOverride {
  level: string
  /** Absent: all components. */
  component?: string
  until: Timestamp
}

/** GET /system/log */
export interface SystemLog {
  /** Newest first. */
  records: LogRecord[]
  /** The level PICACHE_LOG_LEVEL sets. */
  baseLevel: string
  override?: LogOverride
  components: string[]
  /** Records the ring keeps. */
  capacity: number
  /** Records live subscribers missed because they were too slow. */
  dropped: number
  /** The log file and syslog sinks (PICACHE_LOG_FILE, PICACHE_LOG_SYSLOG); [] without any. */
  sinks: LogSink[]
}

/** api.LogSink: an extra destination of the application log. */
export interface LogSink {
  kind: 'file' | 'syslog'
  /** The file path or host:port. */
  target: string
  /** The last write succeeded. */
  ok: boolean
  error?: string
  /** Records dropped because the sink could not keep up or was unreachable. */
  dropped: number
}

/** Minimum level of GET /system/log and the log stream. */
export type LogLevelFilter = 'debug' | 'info' | 'warn' | 'error'

/** PUT /system/log/level (400 with field level, component or minutes). */
export interface LogLevelInput {
  level: 'debug' | 'info'
  /** Absent: all components. */
  component?: string
  /** 1..240 */
  minutes: number
}

/** Answer of PUT /system/log/level. */
export interface LogLevelState {
  baseLevel: string
  override: LogOverride
}

/** api.HostInfo: the cached host sample (values that cannot be read are absent). */
export interface HostInfo {
  sampledAt: Timestamp
  /** Docker or LXC: load, uptime and memory are the host's. */
  container: boolean
  model?: string
  cpus: number
  load?: { one: number; five: number; fifteen: number }
  uptimeSec?: number
  memory?: { totalBytes: number; availableBytes: number; usedBytes: number; swapTotalBytes: number; swapUsedBytes: number }
  /** The container's memory limit (cgroup v2), when one is set. */
  cgroup?: { limitBytes: number; usageBytes: number; availableBytes: number }
  temperatures: { zone: string; type: string; celsius: number }[]
  disks: { path: string; role: 'data' | 'cache'; totalBytes: number; freeBytes: number }[]
}

/** api.DatabaseInfo: file sizes of the databases. */
export interface DatabaseInfo {
  picache: { bytes: number; walBytes: number }
  logs: {
    bytes: number
    walBytes: number
    /** logs.maxDbSizeMiB in bytes (0 = no cap). */
    capBytes: number
    /** 0 without a cap. */
    fillPercent: number
    /** Why logs.db is not used. */
    disabled?: string
    /** Raw rows are not written while the data disk has less than 1 GiB free. */
    rawPaused: boolean
  }
  cacheIndexes: { storeId: string; bytes: number; walBytes: number }[]
}

/** logs.Event: an entry of the warning history (repeats of an unacknowledged entry are merged). */
export interface HistoryEvent {
  id: number
  /** First occurrence. */
  time: Timestamp
  lastTime: Timestamp
  count: number
  /** Notification event key, e.g. "health.warning". */
  event: string
  severity: NotifySeverity
  title: string
  message: string
  acknowledgedAt?: Timestamp
  /** Only for admins. */
  acknowledgedBy?: string
}

/** GET /system/events query (limit 1..200, default 50). */
export interface HistoryQuery {
  unacknowledged?: boolean
  limit?: number
  cursor?: string
}

/** POST /system/support-bundle */
export interface SupportBundleInput {
  currentPassword: string
  /** Keep host names, client names, MAC and private addresses (replaced by placeholders otherwise). */
  includeClientNames?: boolean
}

// ---------------------------------------------------------------- listeners

/** A listener role of the saved listeners (System → Network), in display order. */
export type ListenerRole = 'dns' | 'cache' | 'sni' | 'web' | 'webTls' | 'dot' | 'doh' | 'ntp'

/** One role of GET /system/listeners. Addresses are "ip:port" or ":port" (IPv6 in brackets). */
export interface ListenerRoleConfig {
  role: ListenerRole
  /** Bound now (dns: the addresses of dns-udp and dns-tcp). */
  bound: string[]
  /** The saved set (for the next start, else the one in use); absent when the file does not set the role. */
  saved?: string[]
  /** The built-in default ([] = off). */
  default: string[]
  /** Set by an environment variable or a command-line flag: not changeable here. */
  locked: boolean
  /** The environment variable (e.g. PICACHE_DNS_LISTEN) or "flag". */
  lockedBy?: string
  /** The saved set may switch the role off ([]): never dns and cache, web only while the saved set gives webTls addresses. */
  canDisable: boolean
}

/**
 * GET /system/listeners (also the answer of PUT). Listeners are bootstrap
 * configuration: a saved set applies at the next start; a role that cannot
 * be bound then falls back to its environment or default value.
 */
export interface ListenersConfig {
  editable: boolean
  /** Why not editable: in Docker the compose file sets them. */
  reason?: 'docker'
  /** A saved set is waiting for the next start. */
  restartRequired: boolean
  roles: ListenerRoleConfig[]
  /** The last saved set that could not be bound completely. */
  failed?: { time: Timestamp; roles: Partial<Record<ListenerRole, string>>; saved: Partial<Record<ListenerRole, string[]>> }
}

/**
 * PUT /system/listeners: the whole saved set (an absent role is removed from
 * the file: environment or default again; [] = off). 400 with field
 * listeners.<role>[i], listeners.<role>, listeners.web or currentPassword;
 * 409 in Docker.
 */
export interface ListenersInput {
  listeners: Partial<Record<ListenerRole, string[]>>
  currentPassword: string
}

// ---------------------------------------------------------------- follower sync

/** GET /system/sync */
export interface SyncStatus {
  mode: 'off' | 'follower'
  /** The primary's origin (scheme://host[:port]). */
  source?: string
  sections: SyncSection[]
  intervalMinutes: number
  running: boolean
  lastRun?: Timestamp
  lastSuccess?: Timestamp
  lastError?: string
  /** contentSha256 of the export applied last. */
  lastAppliedSha256?: string
  /** The primary's version at the last run. */
  primaryVersion?: string
  nextRun?: Timestamp
}

/**
 * GET /system/export?sections=… (admins and sync tokens): the configuration of
 * the chosen syncable sections. Group references are ids; `groups` names
 * every group of the primary.
 */
export interface ConfigExport {
  format: 'picache-export'
  formatVersion: 1
  /** The primary's version. */
  version: string
  schema: Record<string, number>
  exportedAt: Timestamp
  instanceId: string
  /** Lower-case hex SHA-256 of the deterministic JSON of `sections`. */
  contentSha256: string
  groups: { id: number; name: string }[]
  sections: {
    'clients-and-groups'?: { groups: ClientGroup[]; clients: Client[] }
    'lists-and-rules'?: { lists: FilterList[]; rules: FilterRule[]; ipRules: IPRule[] }
    'local-dns'?: { records: DnsRecord[]; forwarders: Forwarder[] }
    parental?: {
      groups: {
        groupId: number
        blockedServices: string[]
        schedules: ParentalSchedule[]
        safeSearch: SafeSearch
        categories: Partial<Record<CategorySwitch, boolean>>
      }[]
    }
    'dns-settings'?: { dns: Partial<DnsSettings>; filter: Partial<FilterSettings> }
  }
}
