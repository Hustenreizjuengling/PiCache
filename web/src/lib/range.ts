// Time ranges of pages with statistics or logs: a preset ("24h") or a
// custom window, kept in the URL as ?range=… or ?from=…&to=… (unix
// seconds). Custom windows span at most 400 days, like the API accepts.

import { t } from '../i18n/index.svelte'
import type { RangePreset } from './api/types'
import { formatSpan } from './format'
import { router, type QueryPatch } from './router.svelte'

/** A custom window in unix seconds (from < to). */
export interface CustomRange {
  from: number
  to: number
}

/** What a page shows: a preset or a custom window (usable as the API's RangeArg). */
export type Range = RangePreset | CustomRange

const MIN = 60
const HOUR = 3600
export const DAY = 86_400

/** Length of each preset in seconds. */
export const PRESET_SECONDS: Record<RangePreset, number> = {
  '15m': 15 * MIN,
  '1h': HOUR,
  '6h': 6 * HOUR,
  '24h': DAY,
  '7d': 7 * DAY,
  '30d': 30 * DAY,
  '90d': 90 * DAY,
  '180d': 180 * DAY,
  '365d': 365 * DAY,
}

/** Longest custom window (the API accepts up to 400 days). */
export const MAX_CUSTOM_DAYS = 400

/** Ranges longer than this read the daily top tables: their panels load one after another. */
export const LONG_RANGE = 7 * DAY

/** From this length on charts ask for one point per day. */
export const DAILY_STEP_FROM = 90 * DAY

export function isCustom(r: Range): r is CustomRange {
  return typeof r !== 'string'
}

export function isPreset(v: string): v is RangePreset {
  return v in PRESET_SECONDS
}

/** Length of a range in seconds. */
export function rangeSeconds(r: Range): number {
  return isCustom(r) ? r.to - r.from : PRESET_SECONDS[r]
}

/** Whether the range reads the daily top tables (panels then load one after another and poll rarely). */
export function isLong(r: Range): boolean {
  return rangeSeconds(r) > LONG_RANGE
}

/** Chart step for a range: one point per day from 90 days on, else the server's default. */
export function chartStep(r: Range): number | undefined {
  return rangeSeconds(r) >= DAILY_STEP_FROM ? DAY : undefined
}

/** The presets not longer than `maxSeconds` (all when it is unknown). */
export function withinRetention(presets: readonly RangePreset[], maxSeconds: number | undefined): RangePreset[] {
  return maxSeconds ? presets.filter((p) => PRESET_SECONDS[p] <= maxSeconds) : [...presets]
}

/** Parses ?from=&to= (unix seconds); undefined unless it is a valid window of at most 400 days. */
export function parseCustom(from: string, to: string): CustomRange | undefined {
  if (!/^\d{1,12}$/.test(from) || !/^\d{1,12}$/.test(to)) return undefined
  const r = { from: Number(from), to: Number(to) }
  return r.from < r.to && r.to - r.from <= MAX_CUSTOM_DAYS * DAY ? r : undefined
}

/**
 * The range in the current URL: a valid custom window, else a preset of
 * `presets`, else `fallback`.
 */
export function readRange(presets: readonly RangePreset[], fallback: RangePreset): Range {
  const custom = parseCustom(router.param('from'), router.param('to'))
  if (custom) return custom
  const p = router.param('range')
  return isPreset(p) && presets.includes(p) ? p : fallback
}

/** URL parameters for a range (the default preset is left out). */
export function rangeParams(r: Range, fallback: RangePreset): QueryPatch {
  if (isCustom(r)) return { range: null, from: r.from, to: r.to }
  return { range: r === fallback ? null : r, from: null, to: null }
}

/** Two ranges are the same. */
export function sameRange(a: Range, b: Range): boolean {
  if (isCustom(a) || isCustom(b)) return isCustom(a) && isCustom(b) && a.from === b.from && a.to === b.to
  return a === b
}

/** "Last 7 days" or "Sep 12, 14:00 – Sep 20, 18:00". */
export function rangeText(r: Range): string {
  return isCustom(r) ? formatSpan(r.from * 1000, r.to * 1000) : t(`common.range.long.${r}`)
}
