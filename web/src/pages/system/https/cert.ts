// HTTPS certificate page: labels of the certificate sources and the dates
// PiCache acts on (internal/app/webtls.go: the health check warns 21 days
// before a certificate expires; the self-signed certificate of 0.10 is
// replaced 30 days before it expires).

import type { MessageKey } from '$i18n/index.svelte'
import type { TlsSource } from '$lib/api'
import type { Tone } from '$lib/ui'

const DAY = 86_400_000

/** The health check warns this many days before the certificate expires. */
export const WARN_DAYS = 21
/** The self-signed certificate is replaced by one of the local CA this many days before it expires. */
export const SWITCH_DAYS = 30

export const SOURCE_LABEL: Record<TlsSource, MessageKey> = {
  files: 'system.https.source.files',
  uploaded: 'system.https.source.uploaded',
  'local-ca': 'system.https.source.local-ca',
  'self-signed': 'system.https.source.self-signed',
  none: 'system.https.source.none',
}

/** Whole days until `notAfter` (negative once it passed). */
export function daysLeft(notAfter: string, now = Date.now()): number {
  return Math.floor((Date.parse(notAfter) - now) / DAY)
}

/** The tone for a certificate that expires at `notAfter`. */
export function expiryTone(notAfter: string, warnDays = WARN_DAYS): Tone {
  const d = daysLeft(notAfter)
  return d < 0 ? 'fail' : d <= warnDays ? 'warn' : 'ok'
}

/** The date `days` days before `iso`. */
export function daysBefore(iso: string, days: number): Date {
  return new Date(Date.parse(iso) - days * DAY)
}

/** Server messages start lower-case: make them a sentence. */
export function sentence(s: string): string {
  const x = s.trim()
  return x ? x[0].toUpperCase() + x.slice(1) : x
}
