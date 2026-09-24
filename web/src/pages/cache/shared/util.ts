// Helpers shared by the cache pages: ratios, URL filter parsing and the
// client-side checks that mirror the server's validation (the server stays
// authoritative; these only give early feedback).

import type { RangePreset } from '$lib/api'
import { formatPercent } from '$lib/format'

/** Server-side substring searches need at least this many characters. */
export const SEARCH_MIN = 3

/** Share of bytes served from the cache (null when nothing was transferred). */
export function hitRatio(hit: number, wan: number): number | null {
  const total = hit + wan
  return total > 0 ? hit / total : null
}

/** "83 %" or "–". */
export function formatHitRatio(hit: number, wan: number): string {
  return formatPercent(hitRatio(hit, wan))
}

/** Pass-through traffic below this is too little to warn about. */
const HTTPS_WARN_MIN_BYTES = 50e6

/**
 * Whether a service's traffic was mostly HTTPS (passed through uncached),
 * e.g. the Epic launcher since 20.0.3: caching it then saves little.
 */
export function mostlyHttps(httpBytes: number, sniBytes: number): boolean {
  return sniBytes >= HTTPS_WARN_MIN_BYTES && sniBytes > httpBytes
}

/** A range preset from the URL, if it is one of `allowed`; otherwise `fallback`. */
export function pickRange(value: string, allowed: readonly RangePreset[], fallback: RangePreset): RangePreset {
  return (allowed as readonly string[]).includes(value) ? (value as RangePreset) : fallback
}

/** One of `allowed` or `fallback`. */
export function pick<T extends string>(value: string, allowed: readonly T[], fallback: T): T {
  return (allowed as readonly string[]).includes(value) ? (value as T) : fallback
}

/** A non-negative integer from the URL (0 when missing or invalid). */
export function offsetParam(value: string): number {
  const n = Number(value)
  return Number.isInteger(n) && n > 0 ? n : 0
}

const IPV4 = /^(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)(\.(25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)){3}$/

/** Dotted-quad IPv4 address. */
export function isIPv4(s: string): boolean {
  return IPV4.test(s)
}

/** IPv6 address (syntax check incl. "::" compression and an IPv4 tail; no zone). */
export function isIPv6(s: string): boolean {
  if (!s.includes(':') || s.includes('%') || s.length > 45) return false
  const halves = s.split('::')
  if (halves.length > 2) return false
  let groups = 0
  for (const [i, half] of halves.entries()) {
    if (half === '') continue
    const parts = half.split(':')
    for (const [j, p] of parts.entries()) {
      const lastOfAll = i === halves.length - 1 && j === parts.length - 1
      if (lastOfAll && isIPv4(p)) {
        groups += 2
      } else if (/^[0-9a-f]{1,4}$/i.test(p)) {
        groups++
      } else {
        return false
      }
    }
  }
  return halves.length === 2 ? groups < 8 : groups === 8
}

/** An IPv4 or IPv6 address literal. */
export function isIP(s: string): boolean {
  return isIPv4(s) || isIPv6(s)
}

/** A private IPv4 address (10/8, 172.16/12, 192.168/16): the only cache addresses clients accept. */
export function isRFC1918(s: string): boolean {
  if (!isIPv4(s)) return false
  const [a, b] = s.split('.').map(Number)
  return a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168)
}

/** A unique local IPv6 address (fc00::/7). */
export function isULA(s: string): boolean {
  return isIPv6(s) && /^f[cd][0-9a-f]{2}:/i.test(s) // the first group must be fc00–fdff
}

/** An IP address or a CIDR prefix ("192.168.1.0/24", "fd00::/64"). */
export function isIPOrCIDR(s: string): boolean {
  const slash = s.indexOf('/')
  if (slash < 0) return isIP(s)
  const ip = s.slice(0, slash)
  const bits = s.slice(slash + 1)
  if (!/^\d{1,3}$/.test(bits)) return false
  const n = Number(bits)
  return isIPv4(ip) ? n <= 32 : isIPv6(ip) && n <= 128
}

/** Splits user input (one entry per line; commas and spaces also separate) into trimmed entries. */
export function parseList(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

/** Whether two string lists are equal (order matters). */
export function sameList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

/** Whether a client filter can be sent: empty, an IP address or at least 3 characters. */
export function validClientFilter(v: string): boolean {
  const s = v.trim()
  return s === '' || isIP(s) || [...s].length >= SEARCH_MIN
}

/** Whether a substring search can be sent: empty or at least 3 characters. */
export function validSearch(v: string): boolean {
  const s = v.trim()
  return s === '' || [...s].length >= SEARCH_MIN
}

/** Average transfer rate (bytes/s) between two timestamps, or null for spans under a second. */
export function averageRate(bytes: number, from: string, to: string): number | null {
  const ms = new Date(to).getTime() - new Date(from).getTime()
  return Number.isFinite(ms) && ms >= 1000 ? bytes / (ms / 1000) : null
}

/** Milliseconds between two timestamps (null if invalid). */
export function spanMs(from: string, to: string): number | null {
  const ms = new Date(to).getTime() - new Date(from).getTime()
  return Number.isFinite(ms) && ms >= 0 ? ms : null
}

/** Binary sizes for powers of two (slice sizes): "1 MiB", "256 KiB". */
export function formatBinary(bytes: number): string {
  const MiB = 1 << 20
  if (bytes >= MiB && bytes % MiB === 0) return `${bytes / MiB} MiB`
  if (bytes >= 1024 && bytes % 1024 === 0) return `${bytes / 1024} KiB`
  return `${bytes} B`
}
