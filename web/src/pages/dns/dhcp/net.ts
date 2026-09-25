// IPv4 helpers for the DHCP page: subnets of interfaces, a suggested address
// range, range sizes and sorting. The server validates everything again;
// these only prefill the form and give early hints.

import type { DhcpInterface } from '$lib/api'

/** The number of an IPv4 address (unsigned), or undefined for anything else. */
export function ip4ToInt(s: string | undefined): number | undefined {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec((s ?? '').trim())
  if (!m) return undefined
  const parts = m.slice(1).map(Number)
  if (parts.some((p) => p > 255)) return undefined
  return ((parts[0] << 24) | (parts[1] << 16) | (parts[2] << 8) | parts[3]) >>> 0
}

export function intToIp4(n: number): string {
  return [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join('.')
}

/** A served network: PiCache's own address in it and the network and broadcast addresses. */
export interface Subnet {
  own: number
  prefix: number
  network: number
  broadcast: number
}

/** "192.168.178.10/24" → the subnet; undefined for anything else. */
export function subnetOf(cidr: string | undefined): Subnet | undefined {
  const [addr, len] = (cidr ?? '').split('/')
  const own = ip4ToInt(addr)
  const prefix = Number(len)
  if (own === undefined || !/^\d{1,2}$/.test(len ?? '') || prefix > 32) return undefined
  const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0
  const network = (own & mask) >>> 0
  return { own, prefix, network, broadcast: (network | (~mask >>> 0)) >>> 0 }
}

/** "192.168.178.0/24" */
export function subnetText(sn: Subnet): string {
  return `${intToIp4(sn.network)}/${sn.prefix}`
}

/** Whether an IPv4 address (with or without prefix) is private (RFC 1918). */
export function isPrivate4(s: string): boolean {
  const n = ip4ToInt(s.split('/')[0])
  if (n === undefined) return false
  return n >>> 24 === 10 || n >>> 20 === ((172 << 4) | 1) || n >>> 16 === ((192 << 8) | 168)
}

/** The private IPv4 addresses (CIDR) of an interface. */
export function privateAddresses(iface: DhcpInterface): string[] {
  return (iface.ipv4 ?? []).filter(isPrivate4)
}

/** An interface PiCache can serve: not virtual and exactly one private IPv4 address. */
export function usable(iface: DhcpInterface): boolean {
  return !iface.virtual && privateAddresses(iface).length === 1
}

/** The subnet an interface would serve. */
export function interfaceSubnet(iface: DhcpInterface | undefined): Subnet | undefined {
  if (!iface) return undefined
  const p = privateAddresses(iface)
  return p.length === 1 ? subnetOf(p[0]) : undefined
}

/** Whether an address lies inside the subnet (network and broadcast address excluded). */
export function inSubnet(ip: string, sn: Subnet): boolean {
  const n = ip4ToInt(ip)
  return n !== undefined && n > sn.network && n < sn.broadcast
}

/**
 * A sensible range for a subnet: .100–.199 of PiCache's own /24 (then
 * .150–.249 or .20–.99), in smaller networks the upper or lower half of the
 * hosts; the first one that does not contain an address to avoid (PiCache,
 * the router). The server skips such addresses anyway.
 */
export function suggestRange(sn: Subnet, avoid: readonly (string | undefined)[]): { start: string; end: string } | undefined {
  const first = sn.network + 1
  const last = sn.broadcast - 1
  if (last < first) return undefined
  const skip = avoid.map(ip4ToInt).filter((n): n is number => n !== undefined)
  const hits = (a: number, b: number) => skip.some((n) => n >= a && n <= b)
  let candidates: [number, number][]
  if (sn.prefix <= 24) {
    const base = (sn.own & 0xffffff00) >>> 0
    candidates = [
      [100, 199],
      [150, 249],
      [20, 99],
    ].map(([a, b]) => [base + a, base + b])
  } else {
    const half = Math.floor((last - first + 1) / 2)
    candidates = [
      [first + half, last],
      [first, first + half - 1],
    ]
  }
  candidates = candidates.filter(([a, b]) => a >= first && b <= last && b >= a)
  const pick = candidates.find(([a, b]) => !hits(a, b)) ?? candidates[0]
  return pick ? { start: intToIp4(pick[0]), end: intToIp4(pick[1]) } : undefined
}

/** Addresses from start to end (inclusive); undefined when either is not an address or end < start. */
export function rangeSize(start: string, end: string): number | undefined {
  const a = ip4ToInt(start)
  const b = ip4ToInt(end)
  if (a === undefined || b === undefined || b < a) return undefined
  return b - a + 1
}

/** Sort key of an IPv4 address (other text sorts after all addresses). */
export function ipSortKey(ip: string): number {
  return ip4ToInt(ip) ?? 2 ** 33
}
