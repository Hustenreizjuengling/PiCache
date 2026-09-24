// Time window of the overview's top lists. Top lists are kept per hour on the
// server, so a range is counted from the start of the hour it begins in: at
// 10:05, "15 minutes" would really cover 09:00–10:05. For ranges under a day
// the overview therefore asks for that hour-aligned window explicitly and
// labels the lists "since 09:00", so the numbers match what is shown.

import type { RangeArg, RangePreset } from '$lib/api'

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
export function topWindow(range: RangePreset, nowMs = Date.now()): TopWindow {
  const secs = SHORT[range]
  if (!secs) return { arg: range }
  const now = Math.floor(nowMs / 1000)
  const from = Math.floor((now - secs) / HOUR) * HOUR
  return { arg: { from, to: now }, since: from }
}
