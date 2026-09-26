// Joins client statistics (/stats/clients) and recently seen addresses
// (/clients/known, which carries the server's identity match, also through
// a learned MAC address) to configured clients, and groups seen addresses
// into devices by their MAC address (a phone with an IPv4 address and
// changing IPv6 addresses is one device).

import type { Client, ClientInput, ClientStat, KnownClient, Timestamp } from '$lib/api'
import { isUla, isV4 } from '../network/checks'
import { isIP } from '../shared/input'

/** Time ranges of the traffic columns. */
export const TRAFFIC_RANGES = ['24h', '7d', '30d'] as const
export type TrafficRange = (typeof TRAFFIC_RANGES)[number]

export function asTrafficRange(v: string | null): TrafficRange | undefined {
  return TRAFFIC_RANGES.find((r) => r === v)
}

export interface Totals {
  queries: number
  blocked: number
  cacheBytes: number
  cacheHitBytes: number
  lastSeen?: string
  /** The addresses the traffic came from (most recent first within a device). */
  addresses: string[]
}

function time(ts: Timestamp | undefined): number {
  return ts ? new Date(ts).getTime() : 0
}

function later(a: string | undefined, b: string | undefined): string | undefined {
  if (!a) return b
  if (!b) return a
  return time(a) >= time(b) ? a : b
}

function empty(): Totals {
  return { queries: 0, blocked: 0, cacheBytes: 0, cacheHitBytes: 0, addresses: [] }
}

function add(out: Totals, s: ClientStat, addresses: readonly string[]): void {
  out.queries += s.queries
  out.blocked += s.blocked
  out.cacheBytes += s.cacheBytes
  out.cacheHitBytes += s.cacheHitBytes
  out.lastSeen = later(out.lastSeen, s.lastSeen)
  for (const a of addresses) if (!out.addresses.includes(a)) out.addresses.push(a)
}

/** The addresses of a statistics row: all of a device's (grouped by device) or its one address. */
export function statAddresses(s: ClientStat): string[] {
  return s.addresses?.length ? s.addresses : [s.clientIp]
}

/** Addresses that belong to a client: its IP identifiers plus addresses the server matched to it. */
export function clientAddresses(c: Client, known: readonly KnownClient[] | undefined): Set<string> {
  const ips = new Set(c.identifiers.filter(isIP))
  for (const k of known ?? []) if (k.clientId === c.id) ips.add(k.ip)
  return ips
}

/**
 * Sums the statistics of a client. Rows the server matched to a client
 * (clientId) count for that client only; rows without a match count when one
 * of their addresses or their name belongs to it.
 */
export function clientTotals(
  c: Client,
  stats: readonly ClientStat[] | undefined,
  known: readonly KnownClient[] | undefined,
): Totals {
  const ips = clientAddresses(c, known)
  const out = empty()
  for (const s of stats ?? []) {
    const addresses = statAddresses(s)
    const mine = s.clientId
      ? s.clientId === c.id
      : addresses.some((a) => ips.has(a)) || (!!s.clientName && s.clientName === c.name)
    if (mine) add(out, s, addresses)
  }
  for (const k of known ?? []) if (k.clientId === c.id) out.lastSeen = later(out.lastSeen, k.lastSeen)
  return out
}

/** Sums per-address statistics over the addresses of one device; undefined while there are none loaded. */
export function deviceTotals(addresses: readonly string[], stats: readonly ClientStat[] | undefined): Totals | undefined {
  if (!stats) return undefined
  const mine = new Set(addresses)
  const out = empty()
  for (const s of stats) {
    const own = statAddresses(s).filter((a) => mine.has(a))
    if (own.length > 0) add(out, s, own)
  }
  return out
}

/** Recently seen addresses of one device (same MAC address), most recent first. */
export interface SeenDevice {
  /** Unique key: "mac:<MAC>", or "ip:<address>" for an address without a MAC. */
  key: string
  mac?: string
  /** The most recently active address. */
  ip: string
  /** Every address with its own data, most recent first. */
  entries: KnownClient[]
  addresses: string[]
  /** Host name, preferably the one of an IPv4 address (the router's DHCP name). */
  hostname?: string
  clientId?: number
  name?: string
  firstSeen: Timestamp
  lastSeen: Timestamp
  /** Queries since first seen, over all addresses. */
  queries: number
  /** The dns.blockedClients entries blocking its addresses or its MAC address (unique). */
  blockedBy: string[]
}

/** Groups recently seen addresses by MAC address; devices with the newest activity first. */
export function seenDevices(known: readonly KnownClient[]): SeenDevice[] {
  const groups = new Map<string, KnownClient[]>()
  for (const k of known) {
    const key = k.mac ? `mac:${k.mac.toLowerCase()}` : `ip:${k.ip}`
    const list = groups.get(key)
    if (list) list.push(k)
    else groups.set(key, [k])
  }
  const out: SeenDevice[] = []
  for (const [key, list] of groups) {
    list.sort((a, b) => time(b.lastSeen) - time(a.lastSeen))
    const named = list.find((k) => k.hostname && isV4(k.ip)) ?? list.find((k) => k.hostname)
    const matched = list.find((k) => k.clientId)
    out.push({
      key,
      mac: list[0].mac,
      ip: list[0].ip,
      entries: list,
      addresses: list.map((k) => k.ip),
      hostname: named?.hostname,
      clientId: matched?.clientId,
      name: matched?.name,
      firstSeen: list.reduce((min, k) => (time(k.firstSeen) < time(min) ? k.firstSeen : min), list[0].firstSeen),
      lastSeen: list[0].lastSeen,
      queries: list.reduce((n, k) => n + k.queries, 0),
      blockedBy: [...new Set(list.flatMap((k) => (k.blockedBy ? [k.blockedBy] : [])))],
    })
  }
  return out.sort((a, b) => time(b.lastSeen) - time(a.lastSeen))
}

/**
 * A new client for a recently seen device: its host name, and as
 * identifiers its MAC address plus the addresses that stay the same (IPv4
 * and ULA; public IPv6 addresses change), or its one address without a MAC.
 */
export function clientFromKnown(d: { ip: string; mac?: string; hostname?: string; addresses?: readonly string[] }): Partial<ClientInput> {
  const stable = (d.addresses ?? [d.ip]).filter((a) => isV4(a) || isUla(a))
  return { name: d.hostname || d.ip, identifiers: d.mac ? [d.mac, ...stable] : [d.ip] }
}
