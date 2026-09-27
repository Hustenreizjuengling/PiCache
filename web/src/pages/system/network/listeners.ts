// The listener roles of System › Network (GET/PUT /system/listeners): their
// labels, the environment variable that sets each one, and the editor's
// model of a role (the default, off, or its own addresses).

import type { MessageKey } from '$i18n/index.svelte'
import type { ListenerRole, ListenerRoleConfig } from '$lib/api'

export interface RoleInfo {
  label: MessageKey
  env: string
  /** An example address for an empty role. */
  example: string
}

export const ROLE_INFO: Record<ListenerRole, RoleInfo> = {
  dns: { label: 'system.health.listener.dns', env: 'PICACHE_DNS_LISTEN', example: ':53' },
  cache: { label: 'system.health.listener.cache', env: 'PICACHE_CACHE_LISTEN', example: ':80' },
  sni: { label: 'system.health.listener.sni', env: 'PICACHE_SNI_LISTEN', example: ':443' },
  web: { label: 'system.health.listener.web', env: 'PICACHE_WEB_LISTEN', example: ':8080' },
  webTls: { label: 'system.health.listener.web-tls', env: 'PICACHE_WEB_TLS_LISTEN', example: ':8443' },
  dot: { label: 'system.health.listener.dot', env: 'PICACHE_DOT_LISTEN', example: ':853' },
  doh: { label: 'system.health.listener.doh', env: 'PICACHE_DOH_LISTEN', example: ':4443' },
  ntp: { label: 'system.health.listener.ntp', env: 'PICACHE_NTP_LISTEN', example: ':123' },
}

/** How the editor sets a role: not in the file (its default), off ([]), or these addresses. */
export type RoleMode = 'default' | 'off' | 'custom'

export interface RoleDraft {
  mode: RoleMode
  addresses: string[]
}

/** The editor's starting point for a role: its saved set. */
export function draftOf(r: ListenerRoleConfig): RoleDraft {
  if (r.saved === undefined) return { mode: 'default', addresses: [] }
  if (r.saved.length === 0) return { mode: 'off', addresses: [] }
  return { mode: 'custom', addresses: [...r.saved] }
}

/**
 * The saved set for PUT /system/listeners: roles on their default and roles
 * set by the environment are left out; off is [].
 */
export function savedSet(
  roles: readonly ListenerRoleConfig[],
  drafts: Partial<Record<ListenerRole, RoleDraft>>,
): Partial<Record<ListenerRole, string[]>> {
  const out: Partial<Record<ListenerRole, string[]>> = {}
  for (const r of roles) {
    const d = drafts[r.role]
    if (r.locked || !d || d.mode === 'default') continue
    out[r.role] = d.mode === 'off' ? [] : [...d.addresses]
  }
  return out
}

/**
 * The listening sockets an address list stands for: ":53", "0.0.0.0:53"
 * and "[::]:53" all mean every address on port 53 (the bound list names the
 * wildcard of each family, the saved and default lists usually ":port").
 */
function sockets(list: readonly string[]): Set<string> {
  const out = new Set<string>()
  for (const a of list) {
    const i = a.lastIndexOf(':')
    const host = a.slice(0, i).replace(/^\[|\]$/g, '')
    const port = a.slice(i + 1)
    out.add(host === '' || host === '0.0.0.0' || host === '::' ? `*:${port}` : `${host}:${port}`)
  }
  return out
}

/** Whether two address lists listen on the same sockets (order and wildcard spelling aside). */
export function sameAddresses(a: readonly string[] | undefined, b: readonly string[] | undefined): boolean {
  if (!a || !b) return false
  const x = sockets(a)
  const y = sockets(b)
  return x.size === y.size && [...x].every((s) => y.has(s))
}

/** What a role will use at the next start: the environment's value, the saved set or its default. */
export function nextAddresses(r: ListenerRoleConfig): string[] {
  if (r.locked) return r.bound
  return r.saved ?? r.default
}

/** The next addresses of a role in the editor: the draft, else the role's own next set. */
export function draftAddresses(r: ListenerRoleConfig, d: RoleDraft | undefined): string[] {
  if (r.locked || !d) return nextAddresses(r)
  if (d.mode === 'off') return []
  if (d.mode === 'custom') return d.addresses
  return r.default
}

/** Host (brackets removed, lower-case) and port of "ip:port" or ":port". */
function splitAddress(a: string): { host: string; port: string } {
  const i = a.lastIndexOf(':')
  return { host: a.slice(0, i).replace(/^\[|\]$/g, '').toLowerCase(), port: a.slice(i + 1) }
}

const isWildcard = (host: string) => host === '' || host === '0.0.0.0' || host === '::'
const isLoopback = (host: string) => host === 'localhost' || host === '::1' || /^127\./.test(host)
const isIPLiteral = (host: string) => host.includes(':') || /^\d{1,3}(\.\d{1,3}){3}$/.test(host)

/** This page as the listeners see it: https or not, the port, and the host (brackets removed). */
function pageOrigin(loc: Pick<Location, 'protocol' | 'port' | 'hostname'>) {
  const https = loc.protocol === 'https:'
  const host = loc.hostname.replace(/^\[|\]$/g, '').toLowerCase()
  return { https, port: loc.port || (https ? '443' : '80'), host }
}

/** Whether an address list serves the page: its port on a wildcard, or on the page's address when that is an IP literal. */
function servesPage(list: readonly string[], o: ReturnType<typeof pageOrigin>): boolean {
  return list.some((a) => {
    const { host, port } = splitAddress(a)
    return port === o.port && (isWildcard(host) || !isIPLiteral(o.host) || host === o.host)
  })
}

/**
 * The URLs of the web interface at the next start: the web (http) and
 * webTls (https) addresses of next(role), with this page's host for an
 * address on every address; loopback addresses only when the page itself
 * is opened on loopback.
 */
export function webUrls(
  roles: readonly ListenerRoleConfig[],
  next: (r: ListenerRoleConfig) => readonly string[],
  loc: Pick<Location, 'protocol' | 'port' | 'hostname'> = location,
): string[] {
  const page = pageOrigin(loc)
  const out: string[] = []
  for (const [role, scheme] of [['web', 'http'], ['webTls', 'https']] as const) {
    const r = roles.find((x) => x.role === role)
    for (const a of r ? next(r) : []) {
      const { host, port } = splitAddress(a)
      if (isLoopback(host) && !isLoopback(page.host)) continue
      const h = isWildcard(host) ? loc.hostname : host.includes(':') ? `[${host}]` : host
      const url = `${scheme}://${h}:${port}/`
      if (!out.includes(url)) out.push(url)
    }
  }
  return out
}

/**
 * Whether this page is no longer reachable at its current address after the
 * next start: the running listeners serve it (scheme, port and an IP
 * literal host), the next set of the same listener (web for http, webTls
 * for https) does not. false when the running listeners do not serve the
 * page either (a reverse proxy or a mapped port: nothing to compare).
 */
export function pageMoves(
  roles: readonly ListenerRoleConfig[],
  next: (r: ListenerRoleConfig) => readonly string[],
  loc: Pick<Location, 'protocol' | 'port' | 'hostname'> = location,
): boolean {
  const page = pageOrigin(loc)
  const r = roles.find((x) => x.role === (page.https ? 'webTls' : 'web'))
  return !!r && servesPage(r.bound, page) && !servesPage(next(r), page)
}
