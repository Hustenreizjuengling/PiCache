// Labels and links for the health & about page: health check names,
// listener roles and where the master key came from (internal/app/health.go,
// api.ListenerInfo, internal/secrets).

import type { MessageKey } from '$i18n/index.svelte'
import type { HealthStatus } from '$lib/api'

/** A known health check: its translated name and the page where it is fixed. */
export interface CheckInfo {
  label: MessageKey
  /** Route of the page with the related settings. */
  path?: string
  /** Navigation title of that page. */
  page?: MessageKey
}

export const CHECKS: Record<string, CheckInfo> = {
  listeners: { label: 'system.health.check.listeners' },
  upstreams: { label: 'system.health.check.upstreams', path: '/dns/settings', page: 'common.nav.dnsSettings' },
  blocklists: { label: 'system.health.check.blocklists', path: '/dns/filtering', page: 'common.nav.filtering' },
  'dns-rate-limit': {
    label: 'system.health.check.dns-rate-limit',
    path: '/dns/settings',
    page: 'common.nav.dnsSettings',
  },
  'cache-domains': { label: 'system.health.check.cache-domains', path: '/cache/services', page: 'common.nav.services' },
  download_cache: { label: 'system.health.check.download_cache', path: '/cache/settings', page: 'common.nav.cacheSettings' },
  sni: { label: 'system.health.check.sni' },
  'cache-store': { label: 'system.health.check.cache-store', path: '/cache/storage', page: 'common.nav.storage' },
  logs: { label: 'system.health.check.logs', path: '/system/backup', page: 'common.nav.backup' },
  'data-disk': { label: 'system.health.check.data-disk' },
}

const ORDER: Record<HealthStatus, number> = { fail: 0, warn: 1, ok: 2 }

/** Problems first (failures, then warnings), otherwise in the server's order. */
export function byStatus<T extends { status: HealthStatus }>(checks: readonly T[]): T[] {
  return checks
    .map((c, i) => ({ c, i }))
    .sort((a, b) => ORDER[a.c.status] - ORDER[b.c.status] || a.i - b.i)
    .map((x) => x.c)
}

/** Listener roles in display order, with the environment variable that configures them. */
export const LISTENER_ROLES: { role: string; label: MessageKey; env: string }[] = [
  { role: 'dns-udp', label: 'system.health.listener.dns-udp', env: 'PICACHE_DNS_LISTEN' },
  { role: 'dns-tcp', label: 'system.health.listener.dns-tcp', env: 'PICACHE_DNS_LISTEN' },
  { role: 'cache', label: 'system.health.listener.cache', env: 'PICACHE_CACHE_LISTEN' },
  { role: 'sni', label: 'system.health.listener.sni', env: 'PICACHE_SNI_LISTEN' },
  { role: 'web', label: 'system.health.listener.web', env: 'PICACHE_WEB_LISTEN' },
  { role: 'web-tls', label: 'system.health.listener.web-tls', env: 'PICACHE_WEB_TLS_LISTEN' },
]

export type KeyKind = 'systemd' | 'docker' | 'file' | 'memory'

/** How the master key was provided, from the path the server reports. */
export function masterKeyKind(source: string): KeyKind {
  if (source === 'memory') return 'memory'
  if (source === '/run/secrets/picache_master_key') return 'docker'
  if (/[/\\]picache-master-key$/.test(source)) return 'systemd' // $CREDENTIALS_DIRECTORY/picache-master-key
  return 'file'
}

const REPO = 'https://github.com/hustenreizjuengling/picache'

/** Project documentation (opened in a new tab). */
export const DOCS: { label: MessageKey; url: string }[] = [
  { label: 'system.health.docs.readme', url: `${REPO}#readme` },
  { label: 'system.health.docs.deployment', url: `${REPO}/blob/main/docs/DEPLOYMENT.md` },
  { label: 'system.health.docs.troubleshooting', url: `${REPO}/blob/main/docs/DEPLOYMENT.md#troubleshooting` },
  { label: 'system.health.docs.security', url: `${REPO}/blob/main/docs/SECURITY.md` },
  { label: 'system.health.docs.api', url: `${REPO}/blob/main/docs/API.md` },
  { label: 'system.health.docs.issues', url: `${REPO}/issues` },
]
