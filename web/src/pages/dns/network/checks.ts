// Helpers for the network check: the order and verdict of the checks, the
// router set-up steps (FRITZ!Box or any other router, with PiCache's
// addresses filled in) and IPv6 address handling for refused sources.

import { t, tn, type MessageKey } from '$i18n/index.svelte'
import type { NetworkCheck, NetworkCheckId, NetworkCheckItem } from '$lib/api'

/** Display order of the checks. */
export const CHECK_ORDER: readonly NetworkCheckId[] = ['router-forwarding', 'container-nat', 'ipv6-dns', 'ipv6-address', 'refused', 'devices']

/** The known checks in display order: warnings first, then notes, then passed ones. */
export function sortedChecks(checks: readonly NetworkCheckItem[]): NetworkCheckItem[] {
  const rank = { warn: 0, info: 1, ok: 2 } as const
  return checks
    .filter((c) => CHECK_ORDER.includes(c.id))
    .sort((a, b) => rank[a.status] - rank[b.status] || CHECK_ORDER.indexOf(a.id) - CHECK_ORDER.indexOf(b.id))
}

export interface Verdict {
  tone: 'ok' | 'info' | 'warn'
  title: string
  text: string
}

/** One sentence for the whole page: "All devices you can see use PiCache" / "2 things to fix". */
export function verdict(checks: readonly NetworkCheckItem[]): Verdict {
  const known = checks.filter((c) => CHECK_ORDER.includes(c.id))
  const fix = known.filter((c) => c.status === 'warn').length
  const info = known.filter((c) => c.status === 'info').length
  if (fix > 0) return { tone: 'warn', title: tn('dns.network.verdict.fix', fix), text: t('dns.network.verdict.fixText') }
  if (info > 0) return { tone: 'info', title: tn('dns.network.verdict.info', info), text: t('dns.network.verdict.infoText') }
  return { tone: 'ok', title: t('dns.network.verdict.ok'), text: t('dns.network.verdict.okText') }
}

// ---- router set-up steps

/** One step: a message whose placeholders are filled with a menu path, a setting's label or a value to copy. */
export interface Step {
  key: MessageKey
  /** Menu path in the router ("Home Network → Network → …"). */
  path?: string
  /** Label of a setting or section in the router. */
  field?: string
  section?: string
  /** A value to enter (with a copy button); `missing` explains it when PiCache has none. */
  value?: string
  missing?: string
}

function ipv4Value(net: NetworkCheck): Pick<Step, 'value' | 'missing'> {
  const v = net.self.ipv4[0]
  return v ? { value: v } : { missing: t('dns.network.value.ipv4Missing') }
}

function ulaValue(net: NetworkCheck, ula?: readonly string[]): Pick<Step, 'value' | 'missing'> {
  const v = ula?.[0] ?? net.self.ula[0]
  return v ? { value: v } : { missing: t('dns.network.value.ulaMissing') }
}

/** Whether the router is a FRITZ!Box (its menus are named). */
export function isFritz(net: NetworkCheck): boolean {
  return net.router?.kind === 'fritzbox'
}

/** The steps that fix a check (none for checks without router steps). */
export function stepsFor(check: NetworkCheckItem, net: NetworkCheck): Step[] {
  const fritz = isFritz(net)
  const reconnect: Step = { key: 'dns.network.step.reconnect' }
  switch (check.id) {
    case 'router-forwarding':
      return fritz
        ? [
            { key: 'dns.network.step.fritz.ipv4', path: t('dns.network.fritz.ipv4Path') },
            { key: 'dns.network.step.fritz.localDns', field: t('dns.network.fritz.localDns'), ...ipv4Value(net) },
            { key: 'dns.network.step.fritz.noWan', path: t('dns.network.fritz.wanPath') },
            reconnect,
          ]
        : [
            { key: 'dns.network.step.generic.dhcp' },
            { key: 'dns.network.step.generic.dns', ...ipv4Value(net) },
            { key: 'dns.network.step.generic.noForward' },
            reconnect,
          ]
    case 'ipv6-dns':
      // PiCache cannot answer over IPv6: stop the router announcing itself instead.
      if (!net.self.dnsIpv6) return [{ key: 'dns.network.step.generic.ipv6OffOnly' }, reconnect]
      return fritz
        ? [
            { key: 'dns.network.step.fritz.ipv6', path: t('dns.network.fritz.ipv6Path') },
            { key: 'dns.network.step.fritz.ula', field: t('dns.network.fritz.ula') },
            {
              key: 'dns.network.step.fritz.localDnsv6',
              section: t('dns.network.fritz.dnsv6Section'),
              field: t('dns.network.fritz.localDnsv6'),
              ...ulaValue(net, check.data.ula),
            },
            { key: 'dns.network.step.fritz.ra', field: t('dns.network.fritz.ra') },
            reconnect,
          ]
        : [{ key: 'dns.network.step.generic.ipv6', ...ulaValue(net, check.data.ula) }, { key: 'dns.network.step.generic.ipv6Off' }, reconnect]
    case 'ipv6-address':
      return fritz
        ? [
            { key: 'dns.network.step.fritz.ipv6', path: t('dns.network.fritz.ipv6Path') },
            { key: 'dns.network.step.fritz.ula', field: t('dns.network.fritz.ula') },
            { key: 'dns.network.step.picksUp' },
          ]
        : [{ key: 'dns.network.step.generic.ula' }, { key: 'dns.network.step.picksUp' }]
    default:
      return []
  }
}

// ---- IPv6 addresses

function stripZone(a: string): string {
  const i = a.indexOf('%')
  return i >= 0 ? a.slice(0, i) : a
}

/** The eight 16-bit groups of an IPv6 address, or undefined. */
function expand(addr: string): number[] | undefined {
  const a = stripZone(addr).toLowerCase()
  if (!a.includes(':') || a.includes('.')) return undefined
  const halves = a.split('::')
  if (halves.length > 2) return undefined
  const parse = (s: string) => (s ? s.split(':') : []).map((g) => (/^[0-9a-f]{1,4}$/.test(g) ? parseInt(g, 16) : NaN))
  const head = parse(halves[0])
  const tail = halves.length === 2 ? parse(halves[1]) : []
  const fill = 8 - head.length - tail.length
  if (halves.length === 1 ? head.length !== 8 : fill < 1) return undefined
  const all = [...head, ...Array<number>(halves.length === 2 ? fill : 0).fill(0), ...tail]
  return all.some(Number.isNaN) ? undefined : all
}

/** Canonical text of eight groups (RFC 5952: the longest run of zeros becomes "::"). */
function compress(groups: readonly number[]): string {
  let best = -1
  let bestLen = 1
  for (let i = 0; i < 8; ) {
    if (groups[i] !== 0) {
      i++
      continue
    }
    let j = i
    while (j < 8 && groups[j] === 0) j++
    if (j - i > bestLen) {
      best = i
      bestLen = j - i
    }
    i = j
  }
  const hex = groups.map((g) => g.toString(16))
  if (best < 0) return hex.join(':')
  return `${hex.slice(0, best).join(':')}::${hex.slice(best + bestLen).join(':')}`
}

/** A global unicast IPv6 address (2000::/3): not ULA, link-local, loopback or mapped IPv4. */
export function isGlobalV6(addr: string): boolean {
  const g = expand(addr)
  return !!g && (g[0] & 0xe000) === 0x2000
}

/** The /64 network of an IPv6 address: "2001:db8:1:2::/64". */
export function prefix64(addr: string): string | undefined {
  const g = expand(addr)
  if (!g) return undefined
  return `${compress([...g.slice(0, 4), 0, 0, 0, 0])}/64`
}

/** Unique /64 networks of the global IPv6 addresses among `addresses`. */
export function globalPrefixes(addresses: readonly string[]): string[] {
  const out: string[] = []
  for (const a of addresses) {
    if (!isGlobalV6(a)) continue
    const p = prefix64(a)
    if (p && !out.includes(p)) out.push(p)
  }
  return out
}

/** Whether an address is a unique local IPv6 address (fc00::/7). */
export function isUla(addr: string): boolean {
  const g = expand(addr)
  return !!g && (g[0] & 0xfe00) === 0xfc00
}

/** Whether an address is IPv4. */
export function isV4(addr: string): boolean {
  return !addr.includes(':')
}
