// Time window of the overview's top lists. Top lists are kept per hour on the
// server, so a range is counted from the start of the hour it begins in: at
// 10:05, "15 minutes" would really cover 09:00–10:05. For ranges under a day
// the overview therefore asks for that hour-aligned window explicitly and
// labels the lists "since 09:00", so the numbers match what is shown. Ranges
// longer than 7 days are counted per UTC day: the server reports the start
// it covers (Summary.topFrom, `from` of purposes and query types), and the
// lists are labelled "since <day>". Custom windows are labelled the same way
// when the covered start is earlier than the window's.

import { t } from '$i18n/index.svelte'
import type { RangeArg, RangePreset } from '$lib/api'
import { formatDate, formatDateTimeShort, formatTime } from '$lib/format'
import { isCustom, isLong, type Range } from '$lib/range'

const HOUR = 3600

/** Ranges (in seconds) whose top lists get an explicit, labelled window. */
const SHORT: Partial<Record<RangePreset, number>> = { '15m': 900, '1h': HOUR, '6h': 6 * HOUR }

export interface TopWindow {
  /** What to pass to api.stats.top. */
  arg: RangeArg
  /** Start of the window (unix seconds) when it differs from the range; else undefined. */
  since?: number
}

/** The top-list window for a range at `nowMs`. */
export function topWindow(range: Range, nowMs = Date.now()): TopWindow {
  const secs = isCustom(range) ? undefined : SHORT[range]
  if (!secs) return { arg: range }
  const now = Math.floor(nowMs / 1000)
  const from = Math.floor((now - secs) / HOUR) * HOUR
  return { arg: { from, to: now }, since: from }
}

/**
 * The label of a top list's coverage: "since 09:00" for the short presets,
 * "since Sep 12" for ranges longer than 7 days, and for a custom window
 * whose covered start (`covered`, from the server) is earlier; else undefined.
 */
export function coverage(range: Range, covered: string | undefined, nowMs = Date.now()): string | undefined {
  const w = topWindow(range, nowMs)
  if (w.since !== undefined) return t('overview.dns.topSince', { time: formatTime(w.since * 1000) })
  if (!covered) return undefined
  const start = Date.parse(covered)
  if (isLong(range)) return t('overview.dns.topSinceDay', { date: formatDate(start) })
  if (isCustom(range) && start < range.from * 1000 - 60_000) return t('overview.dns.topSince', { time: formatDateTimeShort(start) })
  return undefined
}

/** The explanation of a coverage label (its tooltip). */
export function coverageHint(range: Range): string {
  return isLong(range) ? t('overview.dns.topSinceDayHint') : t('overview.dns.topSinceHint')
}
