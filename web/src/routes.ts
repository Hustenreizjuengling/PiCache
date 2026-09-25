// Route table and navigation (docs/ARCHITECTURE.md 12.1). Pages are loaded on
// demand (one chunk each). Every section route ends in '/*', so pages can use
// sub-paths (e.g. '#/dns/filtering/rules'); they receive the rest as
// params['*']. Prefer query parameters for filters and selections.

import type { Component } from 'svelte'
import type { MessageKey } from './i18n/index.svelte'
import type { IconName } from './lib/icons'
import { matchRoute } from './lib/router.svelte'

/** Props every page component receives. */
export interface PageProps {
  /** Route parameters; '*' holds the sub-path below the page's base path ('' if none). */
  params: Record<string, string>
}

/** A lazily loaded page (pages declare their own props; all receive PageProps). */
type PageModule = { default: Component<any> }

export type Section = 'dns' | 'cache' | 'system'

export interface RouteDef {
  /** Base path shown in the navigation, e.g. '/dns/queries'. */
  path: string
  title: MessageKey
  icon: IconName
  section?: Section
  load: () => Promise<PageModule>
}

export const routes: RouteDef[] = [
  { path: '/', title: 'common.nav.overview', icon: 'overview', load: () => import('./pages/Overview.svelte') },

  { path: '/dns/queries', section: 'dns', title: 'common.nav.queryLog', icon: 'list', load: () => import('./pages/dns/QueryLog.svelte') },
  { path: '/dns/filtering', section: 'dns', title: 'common.nav.filtering', icon: 'shield', load: () => import('./pages/dns/Filtering.svelte') },
  { path: '/dns/clients', section: 'dns', title: 'common.nav.clients', icon: 'users', load: () => import('./pages/dns/Clients.svelte') },
  { path: '/dns/local', section: 'dns', title: 'common.nav.localDns', icon: 'home', load: () => import('./pages/dns/LocalDns.svelte') },
  { path: '/dns/settings', section: 'dns', title: 'common.nav.dnsSettings', icon: 'sliders', load: () => import('./pages/dns/DnsSettings.svelte') },

  { path: '/cache/downloads', section: 'cache', title: 'common.nav.downloads', icon: 'download', load: () => import('./pages/cache/Downloads.svelte') },
  { path: '/cache/library', section: 'cache', title: 'common.nav.library', icon: 'layers', load: () => import('./pages/cache/Library.svelte') },
  { path: '/cache/services', section: 'cache', title: 'common.nav.services', icon: 'grid', load: () => import('./pages/cache/Services.svelte') },
  { path: '/cache/storage', section: 'cache', title: 'common.nav.storage', icon: 'drive', load: () => import('./pages/cache/Storage.svelte') },
  { path: '/cache/settings', section: 'cache', title: 'common.nav.cacheSettings', icon: 'sliders', load: () => import('./pages/cache/CacheSettings.svelte') },

  { path: '/system/account', section: 'system', title: 'common.nav.account', icon: 'lock', load: () => import('./pages/system/Account.svelte') },
  { path: '/system/tokens', section: 'system', title: 'common.nav.tokens', icon: 'key', load: () => import('./pages/system/Tokens.svelte') },
  { path: '/system/audit', section: 'system', title: 'common.nav.audit', icon: 'document', load: () => import('./pages/system/Audit.svelte') },
  { path: '/system/backup', section: 'system', title: 'common.nav.backup', icon: 'archive', load: () => import('./pages/system/Backup.svelte') },
  { path: '/system/health', section: 'system', title: 'common.nav.health', icon: 'activity', load: () => import('./pages/system/Health.svelte') },
  { path: '/system/updates', section: 'system', title: 'common.nav.updates', icon: 'update', load: () => import('./pages/system/Updates.svelte') },
]

/** Navigation groups in sidebar order. */
export const sections: { id: Section; title: MessageKey }[] = [
  { id: 'dns', title: 'common.nav.dns' },
  { id: 'cache', title: 'common.nav.cache' },
  { id: 'system', title: 'common.nav.system' },
]

/** Finds the route for a path; null means "not found". */
export function resolve(path: string): { route: RouteDef; params: Record<string, string> } | null {
  for (const route of routes) {
    const params = matchRoute(route.path === '/' ? '/' : route.path + '/*', path)
    if (params) return { route, params }
  }
  return null
}
