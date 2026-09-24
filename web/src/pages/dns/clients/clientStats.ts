// Joins per-address statistics (/stats/clients) and recently seen addresses
// (/clients/known, which carries the server's identity match) to configured
// clients.

import type { Client, ClientInput, ClientStat, KnownClient } from '$lib/api'
import { isIP } from '../shared/input'

export interface Totals {
  queries: number
  blocked: number
  cacheBytes: number
  cacheHitBytes: number
  lastSeen?: string
}

function later(a: string | undefined, b: string | undefined): string | undefined {
  if (!a) return b
  if (!b) return a
  return new Date(a).getTime() >= new Date(b).getTime() ? a : b
}

/** Addresses that belong to a client: its IP identifiers plus addresses the server matched to it. */
export function clientAddresses(c: Client, known: readonly KnownClient[] | undefined): Set<string> {
  const ips = new Set(c.identifiers.filter(isIP))
  for (const k of known ?? []) if (k.clientId === c.id) ips.add(k.ip)
  return ips
}

/** Sums the statistics of a client's addresses. */
export function clientTotals(
  c: Client,
  stats: readonly ClientStat[] | undefined,
  known: readonly KnownClient[] | undefined,
): Totals {
  const ips = clientAddresses(c, known)
  const out: Totals = { queries: 0, blocked: 0, cacheBytes: 0, cacheHitBytes: 0 }
  for (const s of stats ?? []) {
    if (!ips.has(s.clientIp) && !(s.clientName && s.clientName === c.name)) continue
    out.queries += s.queries
    out.blocked += s.blocked
    out.cacheBytes += s.cacheBytes
    out.cacheHitBytes += s.cacheHitBytes
    out.lastSeen = later(out.lastSeen, s.lastSeen)
  }
  for (const k of known ?? []) if (k.clientId === c.id) out.lastSeen = later(out.lastSeen, k.lastSeen)
  return out
}

/** The statistics row of one address. */
export function addressStat(ip: string, stats: readonly ClientStat[] | undefined): ClientStat | undefined {
  return stats?.find((s) => s.clientIp === ip)
}

/** A new client for a recently seen address (name from its host name, IP and MAC as identifiers). */
export function clientFromKnown(k: Pick<KnownClient, 'ip' | 'mac' | 'hostname'>): Partial<ClientInput> {
  return { name: k.hostname || k.ip, identifiers: k.mac ? [k.ip, k.mac] : [k.ip] }
}
