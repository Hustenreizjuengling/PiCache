// Address ranges for early hints in the DNS settings: which upstreams are
// local resolvers whose private answers rebind protection blocks, and which
// private reverse networks are public ranges. The server applies its own
// (complete) rules; these only warn while editing.

import { isIPv4, isIPv6 } from '../shared/input'

interface Net {
  bytes: number[]
  bits: number
}

function bytes4(s: string): number[] | undefined {
  return isIPv4(s) ? s.split('.').map(Number) : undefined
}

function bytes6(s: string): number[] | undefined {
  if (!isIPv6(s)) return undefined
  let x = s.toLowerCase()
  // A trailing dotted quad ("::ffff:10.0.0.1") becomes two hex groups.
  const tail = /^(.*:)(\d+\.\d+\.\d+\.\d+)$/.exec(x)
  if (tail) {
    const b = bytes4(tail[2])
    if (!b) return undefined
    x = `${tail[1]}${((b[0] << 8) | b[1]).toString(16)}:${((b[2] << 8) | b[3]).toString(16)}`
  }
  const halves = x.split('::')
  if (halves.length > 2) return undefined
  const groups = (h: string) => (h === '' ? [] : h.split(':'))
  const head = groups(halves[0])
  const rest = halves.length === 2 ? groups(halves[1]) : []
  const fill = halves.length === 2 ? 8 - head.length - rest.length : 0
  if (halves.length === 2 ? fill < 1 : head.length !== 8) return undefined
  const all = [...head, ...Array<string>(fill).fill('0'), ...rest]
  if (!all.every((g) => /^[0-9a-f]{1,4}$/.test(g))) return undefined
  return all.flatMap((g) => {
    const n = parseInt(g, 16)
    return [n >> 8, n & 255]
  })
}

/** The bytes of an IPv4 (4) or IPv6 (16) address; undefined for anything else. */
function addressBytes(s: string): number[] | undefined {
  return bytes4(s) ?? bytes6(s)
}

/** "10.0.0.0/8" → network; undefined for anything else. */
function parseNet(cidr: string): Net | undefined {
  const [addr, len] = cidr.trim().split('/')
  const bytes = addressBytes(addr ?? '')
  if (!bytes || !/^\d{1,3}$/.test(len ?? '')) return undefined
  const bits = Number(len)
  return bits <= bytes.length * 8 ? { bytes, bits } : undefined
}

function contains(net: Net, bytes: readonly number[], bits = bytes.length * 8): boolean {
  if (net.bytes.length !== bytes.length || bits < net.bits) return false
  for (let i = 0; i < net.bits; i++) {
    const bit = (b: readonly number[]) => (b[i >> 3] >> (7 - (i & 7))) & 1
    if (bit(net.bytes) !== bit(bytes)) return false
  }
  return true
}

function nets(list: readonly string[]): Net[] {
  return list.map((c) => parseNet(c)).filter((n): n is Net => !!n)
}

// Rebind targets (docs/ARCHITECTURE.md 6.1) without the embedded IPv4 forms.
const REBIND_TARGETS = nets([
  '0.0.0.0/8',
  '10.0.0.0/8',
  '100.64.0.0/10',
  '127.0.0.0/8',
  '169.254.0.0/16',
  '172.16.0.0/12',
  '192.168.0.0/16',
  '::/128',
  '::1/128',
  'fc00::/7',
  'fe80::/10',
  '64:ff9b:1::/48',
])

// Networks whose reverse zones are private anyway: other entries of
// dns.privateReverseNetworks take public PTR queries away from the upstreams.
const PRIVATE_NETWORKS = nets(['10.0.0.0/8', '172.16.0.0/12', '192.168.0.0/16', '100.64.0.0/10', 'fc00::/7', 'fe80::/10'])

/**
 * The host of an upstream as typed: "udp://192.168.1.1:53" → "192.168.1.1",
 * "https://[fd00::1]/dns-query" → "fd00::1", "fd00::53" → "fd00::53".
 */
export function upstreamHost(upstream: string): string {
  let s = upstream.trim()
  const scheme = /^[a-z][a-z0-9+.-]*:\/\//i.exec(s)
  if (scheme) s = s.slice(scheme[0].length)
  s = s.split(/[/?#]/)[0]
  if (s.startsWith('[')) {
    const end = s.indexOf(']')
    s = end > 0 ? s.slice(1, end) : s
  } else if ((s.match(/:/g) ?? []).length === 1) {
    s = s.slice(0, s.indexOf(':'))
  }
  return s.split('%')[0]
}

/** Upstreams given by an IP address that rebind protection treats as private (the router, a local resolver). */
export function localUpstreams(upstreams: readonly string[]): string[] {
  return upstreams.filter((u) => {
    const b = addressBytes(upstreamHost(u))
    return !!b && REBIND_TARGETS.some((n) => contains(n, b))
  })
}

/** Whether a network lies outside the private ranges (a public range); false for input that is not a network. */
export function isPublicNetwork(cidr: string): boolean {
  const net = parseNet(cidr)
  return !!net && !PRIVATE_NETWORKS.some((p) => contains(p, net.bytes, net.bits))
}
