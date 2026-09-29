// The settings search of the header (shell/GlobalSearch.svelte): every
// settings panel, and every labelled control bound to a settings member on
// DNS settings, Cache settings, Logs & privacy and System › Network.
// `anchor` and `field` are literal, app-wide unique id="…" attributes of
// the page sources (web/scripts/check-search-index.mjs checks them, the
// routes and the keys). Matching runs in the browser only: the typed text
// never reaches the server.

import { t, tEnglish, type MessageKey } from '../i18n/index.svelte'

export interface SearchEntry {
  /** Route path including a sub-path, e.g. '/dns/settings'. */
  route: string
  /** Element id on that page: scrolled into view. */
  anchor: string
  /** Id of the control to focus (else the anchor gets the focus). */
  field?: string
  title: MessageKey
  help?: MessageKey
  /** The section title shown under an option entry. */
  context?: MessageKey
}

const DNS = '/dns/settings'
const CACHE = '/cache/settings'
const LOGS = '/system/logs'
const NET = '/system/network'

export const SEARCH_INDEX: readonly SearchEntry[] = [
  // DNS settings: sections
  { route: DNS, anchor: 'dns-set-upstreams', title: 'dns.settings.upstreams.title', help: 'dns.settings.upstreams.description' },
  { route: DNS, anchor: 'dns-set-cache', title: 'dns.settings.cache.title', help: 'dns.settings.cache.description' },
  { route: DNS, anchor: 'dns-set-blocking', title: 'dns.settings.blocking.title', help: 'dns.settings.blocking.description' },
  { route: DNS, anchor: 'dns-set-protection', title: 'dns.settings.protection.title', help: 'dns.settings.protection.description' },
  { route: DNS, anchor: 'dns-set-ratelimit', title: 'dns.settings.rate.title', help: 'dns.settings.rate.description' },
  { route: DNS, anchor: 'dns-set-access', title: 'dns.settings.access.title', help: 'dns.settings.access.description' },
  { route: DNS, anchor: 'dns-set-encrypted', title: 'dns.settings.encrypted.title', help: 'dns.settings.encrypted.description' },
  { route: DNS, anchor: 'dns-set-devices', title: 'dns.settings.encrypted.setup.title', help: 'dns.settings.encrypted.setup.description' },
  { route: DNS, anchor: 'dns-set-names', title: 'dns.settings.names.title', help: 'dns.settings.names.description' },
  { route: DNS, anchor: 'dns-set-ipv6', title: 'dns.settings.ipv6.title', help: 'dns.settings.ipv6.description' },
  { route: DNS, anchor: 'dns-set-dnssec', title: 'dns.settings.dnssec.title', help: 'dns.settings.dnssec.help' },

  // DNS settings: upstreams
  { route: DNS, anchor: 'dns-fallback-title', title: 'dns.settings.fallback.title', help: 'dns.settings.fallback.help', context: 'dns.settings.upstreams.title' },
  { route: DNS, anchor: 'dns-set-upstreams', field: 'dns-field-upstreamMode', title: 'dns.settings.mode', context: 'dns.settings.upstreams.title' },
  { route: DNS, anchor: 'dns-set-upstreams', field: 'dns-field-upstreamTimeoutMs', title: 'dns.settings.timeout', help: 'dns.settings.timeoutHelp', context: 'dns.settings.upstreams.title' },
  { route: DNS, anchor: 'dns-set-upstreams', field: 'dns-field-bootstrap', title: 'dns.settings.bootstrap', help: 'dns.settings.bootstrapHelp', context: 'dns.settings.upstreams.title' },
  { route: DNS, anchor: 'dns-set-upstreams', field: 'dns-field-bootstrapPreferIpv6', title: 'dns.settings.preferIpv6', help: 'dns.settings.preferIpv6Help', context: 'dns.settings.upstreams.title' },
  { route: DNS, anchor: 'dns-set-upstreams', field: 'dns-field-localPtrUpstreams', title: 'dns.settings.localPtr', help: 'dns.settings.localPtrHelp', context: 'dns.settings.upstreams.title' },
  { route: DNS, anchor: 'dns-ecs-title', field: 'dns-field-ecs-mode', title: 'dns.settings.ecs.title', help: 'dns.settings.ecs.help', context: 'dns.settings.upstreams.title' },
  { route: DNS, anchor: 'dns-ecs-title', field: 'dns-field-ecs-customSubnet', title: 'dns.settings.ecs.subnet', help: 'dns.settings.ecs.subnetHelp', context: 'dns.settings.ecs.title' },

  // DNS settings: response cache
  { route: DNS, anchor: 'dns-set-cache', field: 'dns-field-cacheEnabled', title: 'dns.settings.cache.enabled', help: 'dns.settings.cache.enabledHelp', context: 'dns.settings.cache.title' },
  { route: DNS, anchor: 'dns-set-cache', field: 'dns-field-cacheSize', title: 'dns.settings.cache.size', context: 'dns.settings.cache.title' },
  { route: DNS, anchor: 'dns-set-cache', field: 'dns-field-cacheMinTtl', title: 'dns.settings.cache.minTtl', help: 'dns.settings.cache.minTtlHelp', context: 'dns.settings.cache.title' },
  { route: DNS, anchor: 'dns-set-cache', field: 'dns-field-cacheMaxTtl', title: 'dns.settings.cache.maxTtl', help: 'dns.settings.cache.maxTtlHelp', context: 'dns.settings.cache.title' },
  { route: DNS, anchor: 'dns-set-cache', field: 'dns-field-serveStale', title: 'dns.settings.cache.serveStale', help: 'dns.settings.cache.serveStaleHelp', context: 'dns.settings.cache.title' },
  { route: DNS, anchor: 'dns-set-cache', field: 'dns-field-serveStaleMaxAgeSec', title: 'dns.settings.cache.staleAge', context: 'dns.settings.cache.title' },

  // DNS settings: blocking replies and special domains (settings.filter)
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-blockingMode', title: 'dns.settings.blocking.mode', context: 'dns.settings.blocking.title' },
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-blockedTtl', title: 'dns.settings.blocking.ttl', help: 'dns.settings.blocking.ttlHelp', context: 'dns.settings.blocking.title' },
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-blockingIpv4', title: 'dns.settings.blocking.ipv4', context: 'dns.settings.blocking.title' },
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-blockingIpv6', title: 'dns.settings.blocking.ipv6', help: 'dns.settings.blocking.ipv6Help', context: 'dns.settings.blocking.title' },
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-cnameInspection', title: 'dns.settings.blocking.cname', help: 'dns.settings.blocking.cnameHelp', context: 'dns.settings.blocking.title' },
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-updateIntervalHours', title: 'dns.settings.blocking.interval', help: 'dns.settings.blocking.intervalHelp', context: 'dns.settings.blocking.title' },
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-blockMozillaCanary', title: 'dns.settings.special.canary', help: 'dns.settings.special.canaryHelp', context: 'dns.settings.special.title' },
  { route: DNS, anchor: 'dns-set-blocking', field: 'dns-field-blockIcloudPrivateRelay', title: 'dns.settings.special.relay', help: 'dns.settings.special.relayHelp', context: 'dns.settings.special.title' },

  // DNS settings: protection
  { route: DNS, anchor: 'dns-set-protection', field: 'dns-field-rebindProtection', title: 'dns.settings.protection.rebind', help: 'dns.settings.protection.rebindHelp', context: 'dns.settings.protection.title' },
  { route: DNS, anchor: 'dns-set-protection', field: 'dns-field-rebindAllow', title: 'dns.settings.protection.allow', help: 'dns.settings.protection.allowHelp', context: 'dns.settings.protection.title' },
  { route: DNS, anchor: 'dns-set-protection', field: 'dns-field-bogusNxdomain', title: 'dns.settings.protection.bogus', help: 'dns.settings.protection.bogusHelp', context: 'dns.settings.protection.title' },
  { route: DNS, anchor: 'dns-set-protection', field: 'dns-field-droppedDomains', title: 'dns.settings.protection.dropped', help: 'dns.settings.protection.droppedHelp', context: 'dns.settings.protection.title' },
  { route: DNS, anchor: 'dns-set-protection', field: 'dns-field-upstreamBlockedTtl', title: 'dns.settings.protection.blockedTtl', help: 'dns.settings.protection.blockedTtlHelp', context: 'dns.settings.protection.title' },

  // DNS settings: rate limiting
  { route: DNS, anchor: 'dns-set-ratelimit', field: 'dns-field-rateLimitQps', title: 'dns.settings.rate.qps', help: 'dns.settings.rate.qpsHelp', context: 'dns.settings.rate.title' },
  { route: DNS, anchor: 'dns-set-ratelimit', field: 'dns-field-rateLimitBurst', title: 'dns.settings.rate.burst', help: 'dns.settings.rate.burstHelp', context: 'dns.settings.rate.title' },
  { route: DNS, anchor: 'dns-set-ratelimit', field: 'dns-field-rateLimitIpv4Prefix', title: 'dns.settings.rate.ipv4Prefix', help: 'dns.settings.rate.ipv4PrefixHelp', context: 'dns.settings.rate.title' },
  { route: DNS, anchor: 'dns-set-ratelimit', field: 'dns-field-rateLimitIpv6Prefix', title: 'dns.settings.rate.ipv6Prefix', help: 'dns.settings.rate.ipv6PrefixHelp', context: 'dns.settings.rate.title' },
  { route: DNS, anchor: 'dns-set-ratelimit', field: 'dns-field-rateLimitExempt', title: 'dns.settings.rate.exemptList', help: 'dns.settings.rate.exemptHelp', context: 'dns.settings.rate.title' },

  // DNS settings: access
  { route: DNS, anchor: 'dns-set-access', field: 'dns-field-trustConnectedNetworks', title: 'dns.settings.access.trustConnected', help: 'dns.settings.access.trustConnectedHelp', context: 'dns.settings.access.title' },
  { route: DNS, anchor: 'dns-set-access', field: 'dns-field-allowedNetworks', title: 'dns.settings.access.networks', help: 'dns.settings.access.networksHelp', context: 'dns.settings.access.title' },
  { route: DNS, anchor: 'dns-set-access', field: 'dns-field-allowAllNetworks', title: 'dns.settings.access.allowAll', help: 'dns.settings.access.allowAllHelp', context: 'dns.settings.access.title' },
  { route: DNS, anchor: 'dns-set-access', field: 'dns-field-refuseAny', title: 'dns.settings.access.refuseAny', help: 'dns.settings.access.refuseAnyHelp', context: 'dns.settings.access.title' },
  { route: DNS, anchor: 'dns-set-access', field: 'dns-field-blockedClients', title: 'dns.settings.access.blocked', help: 'dns.settings.access.blockedHelp', context: 'dns.settings.access.title' },
  { route: DNS, anchor: 'dns-set-access', field: 'dns-field-ednsClientTrusted', title: 'dns.settings.access.trusted', help: 'dns.settings.access.trustedHelp', context: 'dns.settings.access.title' },

  // DNS settings: encrypted DNS
  { route: DNS, anchor: 'dns-set-encrypted', field: 'dns-field-encrypted-dot', title: 'dns.settings.encrypted.dot', help: 'dns.settings.encrypted.dotHelp', context: 'dns.settings.encrypted.title' },
  { route: DNS, anchor: 'dns-set-encrypted', field: 'dns-field-encrypted-doh', title: 'dns.settings.encrypted.doh', help: 'dns.settings.encrypted.dohHelp', context: 'dns.settings.encrypted.title' },
  { route: DNS, anchor: 'dns-set-encrypted', field: 'dns-field-encrypted-serverName', title: 'dns.settings.encrypted.serverName', help: 'dns.settings.encrypted.serverNameHelp', context: 'dns.settings.encrypted.title' },
  { route: DNS, anchor: 'dns-enc-plain', field: 'dns-field-plainDns', title: 'dns.settings.encrypted.plain.label', help: 'dns.settings.encrypted.plain.help', context: 'dns.settings.encrypted.title' },

  // DNS settings: local names
  { route: DNS, anchor: 'dns-set-names', field: 'dns-field-localDomain', title: 'dns.settings.names.localDomain', help: 'dns.settings.names.localDomainHelp', context: 'dns.settings.names.title' },
  { route: DNS, anchor: 'dns-set-names', field: 'dns-field-serverNames', title: 'dns.settings.names.serverNames', help: 'dns.settings.names.serverNamesHelp', context: 'dns.settings.names.title' },
  { route: DNS, anchor: 'dns-names-addr', field: 'dns-field-serverNameAddresses-ipv4', title: 'dns.settings.names.addressesV4', context: 'dns.settings.names.addresses' },
  { route: DNS, anchor: 'dns-names-addr', field: 'dns-field-serverNameAddresses-ipv6', title: 'dns.settings.names.addressesV6', context: 'dns.settings.names.addresses' },
  { route: DNS, anchor: 'dns-set-names', field: 'dns-field-routerResolver', title: 'dns.settings.names.router', help: 'dns.settings.names.routerHelp', context: 'dns.settings.names.title' },
  { route: DNS, anchor: 'dns-set-names', field: 'dns-field-domainNeeded', title: 'dns.settings.names.domainNeeded', help: 'dns.settings.names.domainNeededHelpNoDomain', context: 'dns.settings.names.title' },
  { route: DNS, anchor: 'dns-set-names', field: 'dns-field-privateReverseNetworks', title: 'dns.settings.names.reverse', help: 'dns.settings.names.reverseHelp', context: 'dns.settings.names.title' },

  // DNS settings: IPv6 answers and DNSSEC
  { route: DNS, anchor: 'dns-set-ipv6', field: 'dns-field-disableAAAA', title: 'dns.settings.ipv6.disableAaaa', help: 'dns.settings.ipv6.disableAaaaHelp', context: 'dns.settings.ipv6.title' },
  { route: DNS, anchor: 'dns-set-ipv6', field: 'dns-field-dns64-enabled', title: 'dns.settings.ipv6.dns64', help: 'dns.settings.ipv6.dns64Help', context: 'dns.settings.ipv6.title' },
  { route: DNS, anchor: 'dns-set-ipv6', field: 'dns-field-dns64-prefix', title: 'dns.settings.ipv6.prefix', help: 'dns.settings.ipv6.prefixHelp', context: 'dns.settings.ipv6.title' },
  { route: DNS, anchor: 'dns-set-dnssec', field: 'dns-field-dnssec', title: 'dns.settings.dnssec.label', help: 'dns.settings.dnssec.help', context: 'dns.settings.dnssec.title' },

  // Cache settings
  { route: CACHE, anchor: 'cache-set-download', title: 'cache.settings.downloadCacheTitle', help: 'cache.settings.offText' },
  { route: CACHE, anchor: 'cache-set-address', title: 'cache.settings.addressTitle', help: 'cache.settings.addressText' },
  { route: CACHE, anchor: 'cache-set-clients', title: 'cache.settings.clientsTitle' },
  { route: CACHE, anchor: 'cache-set-retention', title: 'cache.settings.retentionTitle', help: 'cache.settings.retentionText' },
  { route: CACHE, anchor: 'cache-set-performance', title: 'cache.settings.performanceTitle', help: 'cache.settings.performanceText' },
  { route: CACHE, anchor: 'cache-set-source', title: 'cache.settings.sourceTitle', help: 'cache.settings.sourceText' },
  { route: CACHE, anchor: 'cache-set-address', field: 'cache-field-cacheIpv4', title: 'cache.settings.ipv4', help: 'cache.settings.ipv4Help', context: 'cache.settings.addressTitle' },
  { route: CACHE, anchor: 'cache-set-address', field: 'cache-field-cacheIpv6', title: 'cache.settings.ipv6', help: 'cache.settings.ipv6Help', context: 'cache.settings.addressTitle' },
  { route: CACHE, anchor: 'cache-set-address', field: 'cache-field-dnsTtl', title: 'cache.settings.dnsTtl', help: 'cache.settings.dnsTtlHelp', context: 'cache.settings.addressTitle' },
  { route: CACHE, anchor: 'cache-set-clients', field: 'cache-field-nocacheClients', title: 'cache.settings.nocache', help: 'cache.settings.nocacheHelp', context: 'cache.settings.clientsTitle' },
  { route: CACHE, anchor: 'cache-set-clients', field: 'cache-field-allowPrivateUpstreams', title: 'cache.settings.privateUpstreams', help: 'cache.settings.privateUpstreamsHelp', context: 'cache.settings.clientsTitle' },
  { route: CACHE, anchor: 'cache-set-retention', field: 'cache-field-maxAgeDays', title: 'cache.settings.maxAge', help: 'cache.settings.maxAgeHelp', context: 'cache.settings.retentionTitle' },
  { route: CACHE, anchor: 'cache-set-retention', field: 'cache-field-maxSizeBytes', title: 'cache.settings.maxSize', help: 'cache.settings.maxSizeHelp', context: 'cache.settings.retentionTitle' },
  { route: CACHE, anchor: 'cache-set-retention', field: 'cache-field-minFreeBytes', title: 'cache.settings.minFree', help: 'cache.settings.minFreeHelp', context: 'cache.settings.retentionTitle' },
  { route: CACHE, anchor: 'cache-set-performance', field: 'cache-field-sliceSizeBytes', title: 'cache.settings.sliceSize', help: 'cache.settings.sliceSizeHelp', context: 'cache.settings.performanceTitle' },
  { route: CACHE, anchor: 'cache-set-performance', field: 'cache-field-maxConcurrentFills', title: 'cache.settings.fills', help: 'cache.settings.fillsHelp', context: 'cache.settings.performanceTitle' },
  { route: CACHE, anchor: 'cache-set-performance', field: 'cache-field-maxFillsPerClient', title: 'cache.settings.fillsPerClient', help: 'cache.settings.fillsPerClientHelp', context: 'cache.settings.performanceTitle' },
  { route: CACHE, anchor: 'cache-set-performance', field: 'cache-field-readAheadSlices', title: 'cache.settings.readAhead', help: 'cache.settings.readAheadHelp', context: 'cache.settings.performanceTitle' },
  { route: CACHE, anchor: 'cache-set-source', field: 'cache-field-domainsSource', title: 'cache.source.url', help: 'cache.settings.sourceUrlHelp', context: 'cache.settings.sourceTitle' },
  { route: CACHE, anchor: 'cache-set-source', field: 'cache-field-updateIntervalHours', title: 'cache.settings.updateInterval', help: 'cache.settings.updateIntervalHelp', context: 'cache.settings.sourceTitle' },

  // Logs & privacy
  { route: LOGS, anchor: 'logs-set-privacy', title: 'system.logs.level.title', help: 'system.logs.level.description' },
  { route: LOGS, anchor: 'logs-set-recording', title: 'system.logs.recording.title', help: 'system.logs.recording.description' },
  { route: LOGS, anchor: 'logs-set-retention', title: 'system.logs.retention.title', help: 'system.logs.retention.description' },
  { route: LOGS, anchor: 'logs-set-clear', title: 'system.logs.clear.title', help: 'system.logs.clear.description' },
  { route: LOGS, anchor: 'logs-set-privacy', field: 'logs-field-queryLogEnabled', title: 'system.logs.queryLog', help: 'system.logs.queryLogHelp', context: 'system.logs.level.title' },
  { route: LOGS, anchor: 'logs-set-privacy', field: 'logs-field-anonymizeClientIps', title: 'system.logs.anonymize', help: 'system.logs.anonymizeHelp', context: 'system.logs.level.title' },
  { route: LOGS, anchor: 'logs-set-privacy', field: 'logs-field-hideDomains', title: 'system.logs.hideDomains', help: 'system.logs.hideDomainsHelp', context: 'system.logs.level.title' },
  { route: LOGS, anchor: 'logs-set-privacy', field: 'logs-field-statsEnabled', title: 'system.logs.stats', help: 'system.logs.statsHelp', context: 'system.logs.level.title' },
  { route: LOGS, anchor: 'logs-set-recording', field: 'logs-field-ignoredDomains', title: 'system.logs.ignored', help: 'system.logs.ignoredHelp', context: 'system.logs.recording.title' },
  { route: LOGS, anchor: 'logs-set-recording', field: 'logs-field-statsOnlyAddressQueries', title: 'system.logs.addressOnly', help: 'system.logs.addressOnlyHelp', context: 'system.logs.recording.title' },
  { route: LOGS, anchor: 'logs-set-recording', field: 'logs-field-flushSeconds', title: 'system.logs.flush', help: 'system.logs.flushHelp', context: 'system.logs.recording.title' },
  { route: LOGS, anchor: 'logs-set-retention', field: 'logs-field-queryLogRetentionHours', title: 'system.logs.queryRetention', help: 'system.logs.queryRetentionHelp', context: 'system.logs.retention.title' },
  { route: LOGS, anchor: 'logs-set-retention', field: 'logs-field-cacheLogRetentionHours', title: 'system.logs.cacheRetention', help: 'system.logs.cacheRetentionHelp', context: 'system.logs.retention.title' },
  { route: LOGS, anchor: 'logs-set-retention', field: 'logs-field-sessionRetentionDays', title: 'system.logs.downloadRetention', help: 'system.logs.downloadRetentionHelp', context: 'system.logs.retention.title' },
  { route: LOGS, anchor: 'logs-set-retention', field: 'logs-field-statsRetentionDays', title: 'system.logs.statsRetention', help: 'system.logs.statsRetentionHelp', context: 'system.logs.retention.title' },
  { route: LOGS, anchor: 'logs-set-retention', field: 'logs-field-seenRetentionDays', title: 'system.logs.seenRetention', help: 'system.logs.seenRetentionHelp', context: 'system.logs.retention.title' },
  { route: LOGS, anchor: 'logs-set-retention', field: 'logs-field-maxDbSizeMiB', title: 'system.logs.maxDbSize', help: 'system.logs.maxDbSizeHelp', context: 'system.logs.retention.title' },

  // System › Network
  { route: NET, anchor: 'net-listeners', title: 'system.network.listeners.title', help: 'system.network.listeners.description' },
  { route: NET, anchor: 'net-proxy', title: 'system.network.proxy.title', help: 'system.network.proxy.description' },
  { route: NET, anchor: 'net-ntp', title: 'system.network.ntp.title', help: 'system.network.ntp.description' },
  { route: NET, anchor: 'net-proxy', field: 'net-field-proxy-url', title: 'system.network.proxy.url', help: 'system.network.proxy.urlHelp', context: 'system.network.proxy.title' },
  { route: NET, anchor: 'net-proxy', field: 'net-field-proxy-username', title: 'system.network.proxy.username', context: 'system.network.proxy.title' },
  { route: NET, anchor: 'net-proxy', field: 'net-field-proxy-password', title: 'system.network.proxy.password', help: 'system.network.proxy.passwordHelp', context: 'system.network.proxy.title' },
  { route: NET, anchor: 'net-proxy', field: 'net-field-proxyFor-lists', title: 'system.network.proxy.for.lists', help: 'system.network.proxy.for.listsHelp', context: 'system.network.proxy.useFor' },
  { route: NET, anchor: 'net-proxy', field: 'net-field-proxyFor-updateCheck', title: 'system.network.proxy.for.updateCheck', help: 'system.network.proxy.for.updateCheckHelp', context: 'system.network.proxy.useFor' },
  { route: NET, anchor: 'net-proxy', field: 'net-field-proxyFor-notifications', title: 'system.network.proxy.for.notifications', help: 'system.network.proxy.for.notificationsHelp', context: 'system.network.proxy.useFor' },
  { route: NET, anchor: 'net-ntp', field: 'net-field-ntp-enabled', title: 'system.network.ntp.enabled', help: 'system.network.ntp.enabledHelp', context: 'system.network.ntp.title' },
  { route: NET, anchor: 'net-ntp', field: 'net-field-ntp-stratum', title: 'system.network.ntp.stratum', help: 'system.network.ntp.stratumHelp', context: 'system.network.ntp.title' },

  // Other settings panels
  { route: '/system/account', anchor: 'web-access', title: 'system.webAccess.title', help: 'system.webAccess.description' },
  { route: '/system/account', anchor: 'account-set-web', title: 'system.web.title', help: 'system.web.description' },
  { route: '/system/https', anchor: 'https-set-cert', title: 'system.https.cert.title', help: 'system.https.cert.description' },
  { route: '/system/https', anchor: 'https-set-ca', title: 'system.https.ca.title', help: 'system.https.ca.description' },
  { route: '/system/https', anchor: 'https-set-upload', title: 'system.https.upload.title', help: 'system.https.upload.description' },
  { route: '/system/https', anchor: 'https-set-trust', title: 'system.https.trust.title', help: 'system.https.trust.description' },
  { route: '/system/tokens', anchor: 'tokens-set-metrics', title: 'system.metrics.title', help: 'system.metrics.description' },
  { route: '/system/notifications', anchor: 'notify-set-channels', title: 'system.notifications.channels.title', help: 'system.notifications.channels.description' },
  { route: '/system/notifications', anchor: 'notify-set-events', title: 'system.notifications.events.title', help: 'system.notifications.events.description' },
  { route: '/system/backup', anchor: 'backup-set-scheduled', title: 'system.backup.scheduled.title', help: 'system.backup.scheduled.description' },
  { route: '/system/sync', anchor: 'sync-set-follower', title: 'system.sync.form.title', help: 'system.sync.form.description' },
  { route: '/system/sync', anchor: 'sync-set-status', title: 'system.sync.status.title' },
  { route: '/system/health', anchor: 'thresholds', title: 'system.health.thresholds.title', help: 'system.health.thresholds.description' },
  { route: '/system/updates', anchor: 'updates-set-check', title: 'system.updates.settings.title', help: 'system.updates.settings.description' },
]

/** The anchors and fields the shell accepts in ?jump= and ?field= (anything else is ignored). */
export const SEARCH_ANCHORS: ReadonlySet<string> = new Set(SEARCH_INDEX.map((e) => e.anchor))
export const SEARCH_FIELDS: ReadonlySet<string> = new Set(SEARCH_INDEX.flatMap((e) => (e.field ? [e.field] : [])))

/** Case- and accent-insensitive form for matching ("Größe" → "große"). */
export function fold(s: string): string {
  return s.normalize('NFD').replace(/\p{M}/gu, '').toLowerCase()
}

/** Settings are searched from this many characters. */
export const MIN_QUERY = 2
/** At most this many settings hits. */
export const MAX_HITS = 8

export interface SearchHit {
  entry: SearchEntry
  /** Title in the active language. */
  title: string
  /** The section title, when the entry is an option of a section. */
  context?: string
}

/**
 * The settings entries matching q: every white-space separated term occurs
 * in the title, help or context of the active language or of English. Order:
 * the title starts with the query, a title word starts with a term, the
 * title contains a term, only help or context match; then index order.
 */
export function searchSettings(q: string): SearchHit[] {
  const query = fold(q.trim())
  if (query.length < MIN_QUERY) return []
  const terms = query.split(/\s+/).filter(Boolean)
  const hits: { hit: SearchHit; rank: number; i: number }[] = []
  SEARCH_INDEX.forEach((entry, i) => {
    const title = t(entry.title)
    const titles = [fold(title), fold(tEnglish(entry.title))]
    const others = [entry.help, entry.context].flatMap((k) => (k ? [fold(t(k)), fold(tEnglish(k))] : []))
    const all = [...titles, ...others]
    if (!terms.every((term) => all.some((s) => s.includes(term)))) return
    let rank = 3
    if (titles.some((s) => s.startsWith(query))) rank = 0
    else if (titles.some((s) => s.split(/[\s\-/(),.:]+/).some((w) => terms.some((term) => w.startsWith(term))))) rank = 1
    else if (titles.some((s) => terms.some((term) => s.includes(term)))) rank = 2
    hits.push({ hit: { entry, title, context: entry.context ? t(entry.context) : undefined }, rank, i })
  })
  return hits
    .sort((a, b) => a.rank - b.rank || a.i - b.i)
    .slice(0, MAX_HITS)
    .map((h) => h.hit)
}

/** A piece of a displayed text: `hit` marks where a term matched. */
export interface TextPart {
  text: string
  hit: boolean
}

/**
 * Splits text into parts for highlighting the terms of q (as text, never
 * HTML). No highlight when folding changes the text's length (then the
 * positions would not match).
 */
export function highlight(text: string, q: string): TextPart[] {
  const folded = fold(text)
  const terms = fold(q.trim()).split(/\s+/).filter(Boolean)
  if (folded.length !== text.length || terms.length === 0) return [{ text, hit: false }]
  const marks = new Array<boolean>(text.length).fill(false)
  for (const term of terms) {
    for (let at = folded.indexOf(term); at >= 0; at = folded.indexOf(term, at + term.length)) {
      marks.fill(true, at, at + term.length)
    }
  }
  const parts: TextPart[] = []
  for (let i = 0; i < text.length; ) {
    let j = i
    while (j < text.length && marks[j] === marks[i]) j++
    parts.push({ text: text.slice(i, j), hit: marks[i] })
    i = j
  }
  return parts
}
