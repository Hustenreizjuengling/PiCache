// Query log filters: read from the URL (the overview, the global search and
// other pages link here with range, status, domain and client), turned into
// API queries and applied to live events (the stream filters only by client
// and status on the server). `client` may be repeated: all addresses of one
// device, matched as "any of them".

import { BLOCKED_STATUSES, type QueryEvent, type QueryLogQuery, type QueryStatus, type RangePreset } from '$lib/api'
import { router } from '$lib/router.svelte'
import { isIP } from '../shared/input'

/** Time ranges offered by the query log (bounded by its retention, 7 days by default). */
export const LOG_RANGES: RangePreset[] = ['15m', '1h', '6h', '24h', '7d']
export const DEFAULT_RANGE: RangePreset = '1h'

/** Every query status in display order. */
export const ALL_STATUSES: readonly QueryStatus[] = [
  'forwarded',
  'cached',
  'stale',
  'local',
  'special',
  'override',
  'safesearch',
  ...BLOCKED_STATUSES,
  'refused',
  'error',
]

/** Statuses answered normally (not blocked, refused or failed). */
export const ALLOWED_STATUSES: readonly QueryStatus[] = ['forwarded', 'cached', 'stale', 'local', 'special', 'override', 'safesearch']

/** Record types offered by the type filter (others can still come from a link). */
export const QTYPES = ['A', 'AAAA', 'CNAME', 'HTTPS', 'SVCB', 'MX', 'TXT', 'PTR', 'SRV', 'NS', 'SOA', 'DS', 'DNSKEY', 'ANY']

/** Minimum length of substring searches (the server rejects shorter ones). */
export const MIN_SEARCH = 3

/** Most `client` values sent at once (a device's most recent addresses). */
export const MAX_CLIENTS = 32

export interface QueryFilters {
  range: RangePreset
  /** One address or part of a name; several values are the addresses of one device. */
  client: string[]
  domain: string
  status: QueryStatus[]
  qtype: string
  upstream: string
}

function isStatus(s: string): s is QueryStatus {
  return (ALL_STATUSES as readonly string[]).includes(s)
}

/** The filters in the current URL (unknown statuses and ranges are ignored). */
export function readFilters(): QueryFilters {
  const r = router.param('range') as RangePreset
  return {
    range: LOG_RANGES.includes(r) ? r : DEFAULT_RANGE,
    client: clientValues(router.all('client')),
    domain: router.param('domain').trim(),
    status: router.list('status').filter(isStatus),
    qtype: router.param('qtype').trim().toUpperCase(),
    upstream: router.param('upstream').trim(),
  }
}

/** Trimmed, unique, non-empty client values, at most MAX_CLIENTS (for links and the URL). */
export function clientValues(values: readonly string[]): string[] {
  return [...new Set(values.map((v) => v.trim()).filter(Boolean))].slice(0, MAX_CLIENTS)
}

/** Whether a client filter can be sent: an IP address or at least 3 characters of a name. */
export function validClient(v: string): boolean {
  const s = v.trim()
  return s === '' || isIP(s) || [...s].length >= MIN_SEARCH
}

/** Whether a domain filter can be sent: "exact" in quotes or a substring of at least 3 characters. */
export function validDomain(v: string): boolean {
  const s = v.trim()
  return s === '' || isExact(s) || [...s.replace(/\.$/, '')].length >= MIN_SEARCH
}

function isExact(s: string): boolean {
  return s.length > 2 && s.startsWith('"') && s.endsWith('"')
}

/** API query for the filters (optionally one page further). */
export function apiQuery(f: QueryFilters, cursor?: string): QueryLogQuery {
  return {
    range: f.range,
    client: f.client.length > 1 ? f.client : f.client[0] || undefined,
    domain: f.domain || undefined,
    status: f.status.length > 0 ? f.status : undefined,
    qtype: f.qtype || undefined,
    upstream: f.upstream || undefined,
    cursor: cursor || undefined,
    limit: 100,
  }
}

/** The client filter the live stream applies on the server (it takes one value). */
export function streamClient(f: QueryFilters): string | undefined {
  return f.client.length === 1 ? f.client[0] : undefined
}

/** Whether an event is from one of several clients (an address matches exactly, anything else as part of the name or address). */
function matchesClients(e: QueryEvent, clients: readonly string[]): boolean {
  const name = (e.clientName ?? '').toLowerCase()
  return clients.some((c) => {
    if (isIP(c)) return e.clientIp.toLowerCase() === c.toLowerCase()
    const sub = c.toLowerCase()
    return name.includes(sub) || e.clientIp.includes(sub)
  })
}

/** Applies the filters the live stream cannot apply on the server (several clients, domain, type, upstream). */
export function matchesLocally(e: QueryEvent, f: QueryFilters): boolean {
  if (f.client.length > 1 && !matchesClients(e, f.client)) return false
  if (f.qtype && e.qtype.toUpperCase() !== f.qtype) return false
  if (f.upstream && e.upstream !== f.upstream) return false
  if (f.domain) {
    const d = f.domain.toLowerCase()
    const name = e.qname.toLowerCase()
    if (isExact(d)) {
      if (name !== d.slice(1, -1).trim().replace(/\.$/, '')) return false
    } else if (!name.includes(d.replace(/\.$/, ''))) {
      return false
    }
  }
  return true
}

/** Whether any filter besides the time range is set. */
export function hasFilters(f: QueryFilters): boolean {
  return !!(f.client.length || f.domain || f.status.length || f.qtype || f.upstream)
}
