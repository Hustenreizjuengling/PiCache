// DNSSEC display helpers: the labels of the modes, the tones and labels of
// PiCache's verdicts (secure, insecure, bogus, indeterminate) and of the
// upstreams' probe states, the worst state of several upstreams and why the
// time checks are suspended.

import { t } from '$i18n/index.svelte'
import type { DnssecMode, DnssecStatus, DnssecTimeReason, UpstreamDnssecState, UpstreamStat } from '$lib/api'
import type { Tone } from '$lib/ui'

/** The modes in the order of the mode control. */
export const DNSSEC_MODES: readonly DnssecMode[] = ['off', 'passthrough', 'validate']

/** The verdicts in display order (the query-log filter, the overview). */
export const DNSSEC_STATUSES: readonly DnssecStatus[] = ['secure', 'insecure', 'bogus', 'indeterminate']

export function isDnssecMode(v: string): v is DnssecMode {
  return (DNSSEC_MODES as readonly string[]).includes(v)
}

export function isDnssecStatus(v: string): v is DnssecStatus {
  return (DNSSEC_STATUSES as readonly string[]).includes(v)
}

export function dnssecModeLabel(m: DnssecMode): string {
  return {
    off: t('dns.settings.dnssec.mode.off'),
    passthrough: t('dns.settings.dnssec.mode.passthrough'),
    validate: t('dns.settings.dnssec.mode.validate'),
  }[m]
}

const STATUS_TONE: Record<DnssecStatus | 'error', Tone> = {
  secure: 'ok',
  insecure: 'neutral',
  bogus: 'fail',
  indeterminate: 'warn',
  error: 'neutral',
}

/** A verdict, or `error` (a check of the DNSSEC test without a usable reply). */
export type DnssecChipStatus = DnssecStatus | 'error'

export function dnssecStatusTone(s: DnssecChipStatus): Tone {
  return STATUS_TONE[s] ?? 'neutral'
}

export function dnssecStatusLabel(s: DnssecChipStatus): string {
  return (
    {
      secure: t('dns.queryLog.dnssecStatus.secure'),
      insecure: t('dns.queryLog.dnssecStatus.insecure'),
      bogus: t('dns.queryLog.dnssecStatus.bogus'),
      indeterminate: t('dns.queryLog.dnssecStatus.indeterminate'),
      error: t('dns.queryLog.dnssecStatus.error'),
    }[s] ?? s
  )
}

const STATE_TONE: Record<UpstreamDnssecState, Tone> = { capable: 'ok', 'no-dnssec': 'warn', 'anchor-mismatch': 'fail', unknown: 'neutral' }

export function upstreamDnssecTone(s: UpstreamDnssecState): Tone {
  return STATE_TONE[s] ?? 'neutral'
}

export function upstreamDnssecLabel(s: UpstreamDnssecState): string {
  return (
    {
      capable: t('dns.settings.upstreams.dnssec.capable'),
      'no-dnssec': t('dns.settings.upstreams.dnssec.noDnssec'),
      'anchor-mismatch': t('dns.settings.upstreams.dnssec.anchorMismatch'),
      unknown: t('dns.settings.upstreams.dnssec.unknown'),
    }[s] ?? s
  )
}

const STATE_RANK: Record<UpstreamDnssecState, number> = { capable: 0, unknown: 1, 'no-dnssec': 2, 'anchor-mismatch': 3 }

/** The worst probe state of some upstreams (undefined when none has one). */
export function worstDnssecState(ups: readonly UpstreamStat[]): UpstreamDnssecState | undefined {
  let worst: UpstreamDnssecState | undefined
  for (const u of ups) {
    if (u.dnssec && (!worst || (STATE_RANK[u.dnssec] ?? 0) > STATE_RANK[worst])) worst = u.dnssec
  }
  return worst
}

/** Why the time checks are suspended, as a sentence ending in a full stop. */
export function timeReasonText(r: DnssecTimeReason | undefined): string {
  switch (r) {
    case 'clock-guard':
      return t('dns.settings.dnssec.timeReason.clockGuard')
    case 'unsynced':
      return t('dns.settings.dnssec.timeReason.unsynced')
    case 'root-signatures':
      return t('dns.settings.dnssec.timeReason.rootSignatures')
    default:
      return t('dns.settings.dnssec.timeReason.unknown')
  }
}
