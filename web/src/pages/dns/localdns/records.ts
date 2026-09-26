// Local records: the editable draft of a record (the structured forms of
// SRV, MX, PTR, HTTPS and SVCB included), the input sent for it and the
// scope in words.

import { t } from '$i18n/index.svelte'
import {
  STRUCTURED_RECORD_TYPES,
  type ClientGroup,
  type DnsRecord,
  type DnsRecordInput,
  type MxData,
  type OtherFamily,
  type PtrData,
  type RecordData,
  type RecordScope,
  type RecordType,
  type SrvData,
  type SvcbData,
} from '$lib/api'
import { groupNames } from '../shared/groups'
import { asciiDomain } from '../shared/input'

export const RECORD_TYPES: readonly RecordType[] = ['A', 'AAAA', 'CNAME', 'TXT', 'SRV', 'MX', 'PTR', 'HTTPS', 'SVCB']
export const DEFAULT_TTL = 300

/** Everything the record panel edits; the members of the structured types share one draft. */
export interface RecordDraft {
  name: string
  type: RecordType
  /** A, AAAA, CNAME and TXT. */
  value: string
  ttl: number
  enabled: boolean
  comment: string
  scope: RecordScope
  groupIds: number[]
  otherFamily: OtherFamily
  /** SRV, HTTPS and SVCB. */
  priority: number
  weight: number
  port: number
  /** SRV, PTR, HTTPS and SVCB. */
  target: string
  preference: number
  host: string
  /** HTTPS and SVCB: comma-separated lists and an optional port. */
  alpn: string
  svcPort: string
  ipv4hint: string
  ipv6hint: string
}

export function isStructured(type: RecordType): boolean {
  return STRUCTURED_RECORD_TYPES.includes(type)
}

/** A or AAAA: the other family may go to the upstreams. */
export function isAddress(type: RecordType): boolean {
  return type === 'A' || type === 'AAAA'
}

export function blankRecord(): RecordDraft {
  return {
    name: '',
    type: 'A',
    value: '',
    ttl: DEFAULT_TTL,
    enabled: true,
    comment: '',
    scope: 'all',
    groupIds: [],
    otherFamily: 'nodata',
    priority: 10,
    weight: 0,
    port: 0,
    target: '',
    preference: 10,
    host: '',
    alpn: '',
    svcPort: '',
    ipv4hint: '',
    ipv6hint: '',
  }
}

/** The draft of a stored record. */
export function draftOfRecord(r: DnsRecord): RecordDraft {
  const d: RecordDraft = {
    ...blankRecord(),
    name: r.name,
    type: r.type,
    value: isStructured(r.type) ? '' : r.value,
    ttl: r.ttl,
    enabled: r.enabled,
    comment: r.comment,
    scope: r.scope,
    groupIds: [...r.groupIds],
    otherFamily: r.otherFamily,
  }
  const data = r.data
  if (!data) return d
  switch (r.type) {
    case 'SRV': {
      const s = data as SrvData
      return { ...d, priority: s.priority, weight: s.weight, port: s.port, target: s.target }
    }
    case 'MX': {
      const m = data as MxData
      return { ...d, preference: m.preference, host: m.host }
    }
    case 'PTR':
      return { ...d, target: (data as PtrData).target }
    case 'HTTPS':
    case 'SVCB': {
      const s = data as SvcbData
      return {
        ...d,
        priority: s.priority,
        target: s.target,
        alpn: (s.alpn ?? []).join(', '),
        svcPort: s.port ? String(s.port) : '',
        ipv4hint: (s.ipv4hint ?? []).join(', '),
        ipv6hint: (s.ipv6hint ?? []).join(', '),
      }
    }
  }
  return d
}

/** The HTTPS/SVCB port as typed: empty (none) or a whole number from 1 to 65535. */
export function validSvcPort(s: string): boolean {
  const v = s.trim()
  return v === '' || (/^\d{1,5}$/.test(v) && Number(v) >= 1 && Number(v) <= 65535)
}

/** A target name as typed: "." (none) stays, anything else in its A-label form. */
function targetOf(s: string): string {
  return s.trim() === '.' ? '.' : asciiDomain(s)
}

/** Items of a comma- or space-separated list. */
function items(s: string): string[] {
  return s.split(/[\s,]+/).filter(Boolean)
}

function dataOf(d: RecordDraft): RecordData | undefined {
  switch (d.type) {
    case 'SRV':
      return { priority: d.priority, weight: d.weight, port: d.port, target: targetOf(d.target) }
    case 'MX':
      return { preference: d.preference, host: targetOf(d.host) }
    case 'PTR':
      return { target: targetOf(d.target) }
    case 'HTTPS':
    case 'SVCB': {
      // The alias form (priority 0) takes no parameters.
      if (d.priority === 0) return { priority: 0, target: targetOf(d.target), alpn: [], ipv4hint: [], ipv6hint: [] }
      const port = d.svcPort.trim()
      return {
        priority: d.priority,
        target: targetOf(d.target),
        alpn: items(d.alpn.toLowerCase()),
        ...(port ? { port: Number(port) } : {}),
        ipv4hint: items(d.ipv4hint),
        ipv6hint: items(d.ipv6hint),
      }
    }
  }
  return undefined
}

/** The input for a draft (`data` for the structured types, the other family only for A and AAAA). */
export function recordInput(d: RecordDraft): DnsRecordInput {
  const data = dataOf(d)
  return {
    name: asciiDomain(d.name),
    type: d.type,
    value: data ? '' : d.type === 'CNAME' ? asciiDomain(d.value) : d.value.trim(),
    ...(data ? { data } : {}),
    ttl: d.ttl,
    enabled: d.enabled,
    comment: d.comment.trim(),
    scope: d.scope,
    groupIds: d.scope === 'groups' ? [...d.groupIds] : [],
    ...(isAddress(d.type) ? { otherFamily: d.otherFamily } : {}),
  }
}

/** A scoped record without a group answers nobody. */
export function appliesToNobody(r: Pick<DnsRecord, 'scope' | 'groupIds'>): boolean {
  return r.scope === 'groups' && r.groupIds.length === 0
}

/** "Everyone" or the group names. */
export function scopeText(r: Pick<DnsRecord, 'scope' | 'groupIds'>, groups: readonly ClientGroup[] | undefined): string {
  if (r.scope === 'all') return t('dns.records.scope.all')
  if (r.groupIds.length === 0) return t('dns.records.scope.nobody')
  return groupNames(r.groupIds, groups)
}

/** Two records meet the same clients' lookups: both for everyone, or a shared group. */
export function sameScope(a: Pick<DnsRecord, 'scope' | 'groupIds'>, b: Pick<DnsRecord, 'scope' | 'groupIds'>): boolean {
  if (a.scope === 'all' || b.scope === 'all') return a.scope === b.scope
  return a.groupIds.some((id) => b.groupIds.includes(id))
}
