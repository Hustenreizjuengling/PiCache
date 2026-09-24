// Locale-aware formatters. They read the active language, so templates that
// call them re-render when the language changes. Byte units are decimal
// (1 GB = 10^9 bytes) like disk vendors and the design examples.

import { i18n } from '../i18n/index.svelte'

const cache = new Map<string, Intl.NumberFormat | Intl.DateTimeFormat | Intl.RelativeTimeFormat>()

function nf(key: string, opts: Intl.NumberFormatOptions): Intl.NumberFormat {
  const k = `n|${i18n.tag}|${key}`
  let f = cache.get(k) as Intl.NumberFormat | undefined
  if (!f) {
    f = new Intl.NumberFormat(i18n.tag, opts)
    cache.set(k, f)
  }
  return f
}

function df(key: string, opts: Intl.DateTimeFormatOptions): Intl.DateTimeFormat {
  const k = `d|${i18n.tag}|${key}`
  let f = cache.get(k) as Intl.DateTimeFormat | undefined
  if (!f) {
    f = new Intl.DateTimeFormat(i18n.tag, opts)
    cache.set(k, f)
  }
  return f
}

function rtf(): Intl.RelativeTimeFormat {
  const k = `r|${i18n.tag}`
  let f = cache.get(k) as Intl.RelativeTimeFormat | undefined
  if (!f) {
    f = new Intl.RelativeTimeFormat(i18n.tag, { numeric: 'auto', style: 'long' })
    cache.set(k, f)
  }
  return f
}

const DASH = '–'

function valid(n: number | null | undefined): n is number {
  return typeof n === 'number' && Number.isFinite(n)
}

/** 12 345 → "12,345" (en) / "12.345" (de). */
export function formatNumber(n: number | null | undefined, maxFractionDigits = 0): string {
  if (!valid(n)) return DASH
  return nf(`num${maxFractionDigits}`, { maximumFractionDigits: maxFractionDigits }).format(n)
}

/** 1 234 567 → "1.2M" / "1,2 Mio." (for axis labels and dense cells). */
export function formatCompact(n: number | null | undefined): string {
  if (!valid(n)) return DASH
  return nf('compact', { notation: 'compact', maximumFractionDigits: 1 }).format(n)
}

/**
 * Formats a ratio (0..1) as a percentage: 0.183 → "18 %" (de) / "18%" (en).
 * Values below 10 % keep one decimal.
 */
export function formatPercent(ratio: number | null | undefined, fractionDigits?: number): string {
  if (!valid(ratio)) return DASH
  const digits = fractionDigits ?? (Math.abs(ratio) < 0.1 && ratio !== 0 ? 1 : 0)
  return nf(`pct${digits}`, { style: 'percent', maximumFractionDigits: digits }).format(ratio)
}

// Index 0 (plain bytes) is written as "B": CLDR's short "byte" reads oddly next to kB/MB.
const BYTE_UNITS = ['byte', 'kilobyte', 'megabyte', 'gigabyte', 'terabyte', 'petabyte'] as const

/** Splits a byte count into a value below 1000 and its unit index; digits keep ~3 significant figures. */
function scaleBytes(n: number): { v: number; i: number; digits: number } {
  let v = Math.abs(n)
  let i = 0
  while (v >= 1000 && i < BYTE_UNITS.length - 1) {
    v /= 1000
    i++
  }
  return { v, i, digits: i === 0 || v >= 100 ? 0 : v >= 10 ? 1 : 2 }
}

/** 38 000 000 000 → "38 GB". Decimal units, up to 3 significant digits. */
export function formatBytes(bytes: number | null | undefined): string {
  if (!valid(bytes)) return DASH
  const { v, i, digits } = scaleBytes(bytes)
  if (i === 0) return `${formatNumber(bytes)} B`
  const unit = BYTE_UNITS[i]
  return nf(`b|${unit}|${digits}`, {
    style: 'unit',
    unit,
    unitDisplay: 'short',
    maximumFractionDigits: digits,
  }).format(Math.sign(bytes) * v)
}

/** Bytes per second → "112 MB/s". */
export function formatRate(bytesPerSecond: number | null | undefined): string {
  if (!valid(bytesPerSecond)) return DASH
  const { v, i, digits } = scaleBytes(bytesPerSecond)
  if (i === 0) return `${formatNumber(v)} B/s`
  const unit = `${BYTE_UNITS[i]}-per-second`
  return nf(`r|${unit}|${digits}`, {
    style: 'unit',
    unit,
    unitDisplay: 'short',
    maximumFractionDigits: digits,
  }).format(v)
}

type DurUnit = 'day' | 'hour' | 'minute' | 'second' | 'millisecond' | 'microsecond'

function unit(n: number, u: DurUnit, digits = 0): string {
  return nf(`u|${u}|${digits}`, { style: 'unit', unit: u, unitDisplay: 'short', maximumFractionDigits: digits }).format(n)
}

/**
 * Human duration from milliseconds: "850 ms", "4.2 sec", "12 min", "3 hr 5 min", "2 days".
 * Uses at most two units.
 */
export function formatDuration(ms: number | null | undefined): string {
  if (!valid(ms)) return DASH
  const neg = ms < 0
  let v = Math.abs(ms)
  let out: string
  if (v < 1) out = unit(v * 1000, 'microsecond')
  else if (v < 1000) out = unit(v, 'millisecond')
  else if (v < 60_000) out = unit(v / 1000, 'second', v < 10_000 ? 1 : 0)
  else {
    v = Math.round(v / 1000)
    const d = Math.floor(v / 86400)
    const h = Math.floor((v % 86400) / 3600)
    const m = Math.floor((v % 3600) / 60)
    if (d > 0) out = h > 0 && d < 7 ? `${unit(d, 'day')} ${unit(h, 'hour')}` : unit(d, 'day')
    else if (h > 0) out = m > 0 ? `${unit(h, 'hour')} ${unit(m, 'minute')}` : unit(h, 'hour')
    else out = unit(m, 'minute')
  }
  return neg ? `−${out}` : out
}

/** DNS latencies in microseconds: 850 → "850 µs", 12 400 → "12 ms". */
export function formatMicros(us: number | null | undefined): string {
  if (!valid(us)) return DASH
  if (us < 1000) return unit(us, 'microsecond')
  return unit(us / 1000, 'millisecond', us < 10_000 ? 1 : 0)
}

/** Accepts RFC 3339 strings, epoch milliseconds or Dates. */
function toDate(v: string | number | Date | null | undefined): Date | null {
  if (v === null || v === undefined || v === '') return null
  const d = v instanceof Date ? v : new Date(v)
  return Number.isNaN(d.getTime()) ? null : d
}

/** "5 minutes ago", "in 2 hours", "yesterday" (relative to now). */
export function formatRelative(v: string | number | Date | null | undefined, now = Date.now()): string {
  const d = toDate(v)
  if (!d) return DASH
  const s = Math.round((d.getTime() - now) / 1000)
  const a = Math.abs(s)
  const f = rtf()
  if (a < 45) return f.format(0, 'second')
  if (a < 45 * 60) return f.format(Math.round(s / 60), 'minute')
  if (a < 22 * 3600) return f.format(Math.round(s / 3600), 'hour')
  if (a < 26 * 86400) return f.format(Math.round(s / 86400), 'day')
  if (a < 320 * 86400) return f.format(Math.round(s / (30 * 86400)), 'month')
  return f.format(Math.round(s / (365 * 86400)), 'year')
}

/** Date and time: "24 Sept 2026, 14:05" (seconds optional). */
export function formatDateTime(v: string | number | Date | null | undefined, seconds = false): string {
  const d = toDate(v)
  if (!d) return DASH
  return df(`dt${seconds}`, {
    dateStyle: 'medium',
    timeStyle: seconds ? 'medium' : 'short',
  }).format(d)
}

/** Time of day: "14:05" or "2:05 PM" (seconds optional). */
export function formatTime(v: string | number | Date | null | undefined, seconds = false): string {
  const d = toDate(v)
  if (!d) return DASH
  return df(`t${seconds}`, { timeStyle: seconds ? 'medium' : 'short' }).format(d)
}

/** Date only: "24 Sept 2026". */
export function formatDate(v: string | number | Date | null | undefined): string {
  const d = toDate(v)
  if (!d) return DASH
  return df('d', { dateStyle: 'medium' }).format(d)
}

/** Short date + time for chart axes: "Sep 24" / "14:00". */
export function formatAxisTime(unixSeconds: number, spanSeconds: number): string {
  const d = new Date(unixSeconds * 1000)
  if (spanSeconds <= 2 * 86400) return df('axis-t', { hour: 'numeric', minute: '2-digit' }).format(d)
  return df('axis-d', { month: 'short', day: 'numeric' }).format(d)
}
