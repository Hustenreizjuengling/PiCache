// Helpers for the notification pages: limits and checks that mirror the
// server's validation (the server stays the authority), severity order,
// how channel URLs are shown and the translated event texts.

import { i18n, t } from '$i18n/index.svelte'
import type { ApiError, NotifyEvent, NotifyKind, NotifySeverity, NotifyTestResult } from '$lib/api'
import type { Tone } from '$lib/ui'

/** A test message sent from this page (kept until the page is left). */
export interface TestState {
  running: boolean
  /** The receiver's answer (delivered or not). */
  result?: NotifyTestResult
  /** The test request itself failed (channel gone, PiCache unreachable, …). */
  error?: ApiError
  /** Start time (ms), for ordering. */
  at: number
}

/** Most channels the server accepts. */
export const MAX_CHANNELS = 10
/** Longest channel name (characters, as the server counts them). */
export const MAX_NAME = 64
/** Longest channel URL. */
export const MAX_URL = 2048
/** Longest delivery log the server keeps. */
export const LOG_LIMIT = 200

export const KINDS: readonly NotifyKind[] = ['webhook', 'ntfy', 'gotify']
export const SEVERITIES: readonly NotifySeverity[] = ['info', 'warning', 'error']

/** The test event: always delivered to the tested channel, never filtered. */
export const TEST_EVENT = 'notify.test'

const RANK: Record<NotifySeverity, number> = { info: 0, warning: 1, error: 2 }

/** Whether a channel with this minimum severity gets an event of `severity`. */
export function passes(min: NotifySeverity, severity: NotifySeverity): boolean {
  return (RANK[severity] ?? 0) >= (RANK[min] ?? 0)
}

export const SEVERITY_TONES: Record<NotifySeverity, Tone> = { info: 'info', warning: 'warn', error: 'fail' }

/** Tone of a severity (unknown values are neutral). */
export function severityTone(s: string): Tone {
  return SEVERITY_TONES[s as NotifySeverity] ?? 'neutral'
}

/** Translated severity ("Warning"); unknown values as they are. */
export function severityLabel(s: string): string {
  return (SEVERITIES as readonly string[]).includes(s) ? t(`system.notifications.severity.${s as NotifySeverity}`) : s
}

/** Translated kind ("Gotify"); unknown values as they are. */
export function kindLabel(k: string): string {
  return (KINDS as readonly string[]).includes(k) ? t(`system.notifications.kind.${k as NotifyKind}`) : k
}

/**
 * A channel URL for lists: without user info, query string and fragment,
 * which may carry tokens ("https://ntfy.example/alerts?…").
 */
export function displayUrl(raw: string): string {
  let u: URL
  try {
    u = new URL(raw)
  } catch {
    const i = raw.search(/[?#]/)
    return i < 0 ? raw : `${raw.slice(0, i)}?…`
  }
  // new URL() adds "/" to a bare host; keep what the admin typed.
  const path = u.pathname === '/' && !/^[a-z][a-z0-9+.-]*:\/\/[^/?#]*\//i.test(raw) ? '' : u.pathname
  const user = u.username || u.password ? '…@' : ''
  const rest = u.search || u.hash ? '?…' : ''
  return `${u.protocol}//${user}${u.host}${path}${rest}`
}

export type UrlProblem = 'required' | 'tooLong' | 'scheme' | 'invalid'

/** What is wrong with a channel URL, or undefined (http/https with a host). */
export function urlProblem(raw: string): UrlProblem | undefined {
  const s = raw.trim()
  if (!s) return 'required'
  if (s.length > MAX_URL) return 'tooLong'
  if (/\s/.test(s)) return 'invalid'
  let u: URL
  try {
    u = new URL(s)
  } catch {
    return /^[a-z][a-z0-9+.-]*:/i.test(s) ? 'invalid' : 'scheme'
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return 'scheme'
  if (!u.hostname) return 'invalid'
  return undefined
}

/** Control characters cannot be sent in an HTTP header (nor belong in a name). */
export function hasControlChars(s: string): boolean {
  return /[\u0000-\u001f\u007f-\u009f]/.test(s)
}

// ---- event texts

/** Events this version of the UI has translations for. */
const KNOWN_EVENTS = [
  'health.failed',
  'health.warning',
  'health.recovered',
  'storage.offline',
  'storage.online',
  'update.available',
  'update.installed',
  'update.failed',
  'backup.failed',
  'backup.succeeded',
  'security.lockout',
  'notify.test',
] as const

type KnownEvent = (typeof KNOWN_EVENTS)[number]

function known(key: string): key is KnownEvent {
  return (KNOWN_EVENTS as readonly string[]).includes(key)
}

/**
 * Title and description of an event. The server's texts (English) are used
 * in English; other languages use the translation when this version knows
 * the event, else the server's text.
 */
export function eventText(key: string, catalog?: readonly NotifyEvent[]): { title: string; description: string } {
  const ev = catalog?.find((e) => e.key === key)
  const local = known(key)
    ? { title: t(`system.notifications.event.${key}.title`), description: t(`system.notifications.event.${key}.text`) }
    : undefined
  if (i18n.locale !== 'en' && local) return local
  return {
    title: ev?.title || local?.title || key,
    description: ev?.description || local?.description || '',
  }
}
