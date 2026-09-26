// Query log filters: read from the URL (the overview, the global search and
// other pages link here with range, status, domain and client), turned into
// API queries and applied to live events (the stream filters only by client
// and status on the server). `client` may be repeated: all addresses of one
// device, matched as "any of them"; so may `rcode` (any of the codes).

import {
  BLOCKED_STATUSES,
  type QueryEvent,
  type QueryExportQuery,
  type QueryLogQuery,
  type QueryStatus,
  type RangePreset,
} from '$lib/api'
import { isCustom, readRange, withinRetention, type Range } from '$lib/range'
import { router } from '$lib/router.svelte'
import { isIP, isIPv4, isIPv6 } from '../shared/input'

/** Time ranges shown as segments (bounded by the query log's retention, 7 days by default). */
export const LOG_RANGES: RangePreset[] = ['15m', '1h', '6h', '24h', '7d']
/** Longer ranges under "More", offered when the retention keeps that much. */
export const LOG_MORE: RangePreset[] = ['30d', '90d', '180d', '365d']
export const DEFAULT_RANGE: RangePreset = '1h'

/** The presets of the segments and of "More" that fit a retention of `hours` (the segments while it is unknown). */
export function logRanges(hours: number | undefined): { segments: RangePreset[]; more: RangePreset[] } {
  const max = hours ? hours * 3600 : undefined
  return { segments: withinRetention(LOG_RANGES, max), more: max ? withinRetention(LOG_MORE, max) : [] }
}

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

/** Reply codes the filter always offers (the codes of the loaded page are added). */
export const RCODES = ['NOERROR', 'NXDOMAIN', 'SERVFAIL', 'REFUSED', 'NOTIMP', 'FORMERR']

/** Most reply codes the server accepts in one filter. */
export const MAX_RCODES = 16

/** Minimum length of substring searches (the server rejects shorter ones). */
export const MIN_SEARCH = 3

/** Most `client` values sent at once (a device's most recent addresses). */
export const MAX_CLIENTS = 32

export interface QueryFilters {
  /** A preset or a custom window (?from&to). */
  range: Range
  /** One address or part of a name; several values are the addresses of one device. */
  client: string[]
  domain: string
  status: QueryStatus[]
  qtype: string
  upstream: string
  /** Reply codes, upper case (any of them). */
  rcode: string[]
  /** '' any, 'true' validated (the AD flag), 'false' not validated. */
  dnssec: '' | 'true' | 'false'
}

/** URL patch that removes every filter besides the time range. */
export const CLEAR_FILTERS = { client: null, domain: null, status: null, qtype: null, upstream: null, rcode: null, dnssec: null }

function isStatus(s: string): s is QueryStatus {
  return (ALL_STATUSES as readonly string[]).includes(s)
}

/** Whether s is a reply code as the filter accepts it (1–16 of A–Z and 0–9). */
export function isRcode(s: string): boolean {
  return /^[A-Z0-9]{1,16}$/.test(s)
}

/** The filters in the current URL (unknown statuses, codes and ranges are ignored). */
export function readFilters(): QueryFilters {
  const dnssec = router.param('dnssec')
  return {
    range: readRange([...LOG_RANGES, ...LOG_MORE], DEFAULT_RANGE),
    client: clientValues(router.all('client')),
    domain: router.param('domain').trim(),
    status: router.list('status').filter(isStatus),
    qtype: router.param('qtype').trim().toUpperCase(),
    upstream: router.param('upstream').trim(),
    rcode: [...new Set(router.list('rcode').map((c) => c.trim().toUpperCase()))].filter(isRcode).slice(0, MAX_RCODES),
    dnssec: dnssec === 'true' || dnssec === 'false' ? dnssec : '',
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

/** The filters as API parameters without paging (also those of the export). */
export function exportQuery(f: QueryFilters): QueryExportQuery {
  return {
    ...(isCustom(f.range) ? { from: f.range.from, to: f.range.to } : { range: f.range }),
    client: f.client.length > 1 ? f.client : f.client[0] || undefined,
    domain: f.domain || undefined,
    status: f.status.length > 0 ? f.status : undefined,
    qtype: f.qtype || undefined,
    upstream: f.upstream || undefined,
    rcode: f.rcode.length > 0 ? f.rcode : undefined,
    dnssec: f.dnssec ? f.dnssec === 'true' : undefined,
  }
}

/** API query for the filters (optionally one page further). */
export function apiQuery(f: QueryFilters, cursor?: string): QueryLogQuery {
  return { ...exportQuery(f), cursor: cursor || undefined, limit: 100 }
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

/**
 * Applies the filters the live stream cannot apply on the server (several
 * clients, domain, type, upstream, reply code, DNSSEC).
 */
export function matchesLocally(e: QueryEvent, f: QueryFilters): boolean {
  if (f.client.length > 1 && !matchesClients(e, f.client)) return false
  if (f.qtype && e.qtype.toUpperCase() !== f.qtype) return false
  if (f.upstream && e.upstream !== f.upstream) return false
  if (f.rcode.length > 0 && !f.rcode.includes(e.rcode.toUpperCase())) return false
  if (f.dnssec && !!e.dnssec !== (f.dnssec === 'true')) return false
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
  return !!(f.client.length || f.domain || f.status.length || f.qtype || f.upstream || f.rcode.length || f.dnssec)
}

/** The eight 16-bit words of an IPv6 address (without zone or embedded IPv4). */
function ipv6Words(ip: string): number[] | undefined {
  const parts = ip.toLowerCase().split('::')
  if (parts.length > 2) return undefined
  const words = (s: string) => (s ? s.split(':') : [])
  const head = words(parts[0])
  const tail = parts.length === 2 ? words(parts[1]) : []
  const zeros = parts.length === 2 ? 8 - head.length - tail.length : 0
  if (zeros < 0) return undefined
  const all = [...head, ...Array<string>(zeros).fill('0'), ...tail]
  if (all.length !== 8 || !all.every((w) => /^[0-9a-f]{1,4}$/.test(w))) return undefined
  return all.map((w) => parseInt(w, 16))
}

/**
 * The client address of an entry looks anonymised: logs.anonymizeClientIps
 * keeps only an IPv4 /16 or an IPv6 /48, so such an entry names a network,
 * not a device, also after anonymisation was switched off. (A device whose
 * address really ends in .0.0 loses only the device actions of the panel.)
 */
export function looksAnonymised(ip: string): boolean {
  if (isIPv4(ip)) return ip.endsWith('.0.0')
  if (!isIPv6(ip)) return false
  const words = ipv6Words(ip)
  return !!words && words.slice(3).every((w) => w === 0)
}
