// Rates from statistics series (logs.Series: counts or bytes per bucket,
// timestamps = bucket starts). A range that ends now ends in a bucket that is
// still filling: dividing it by the whole step would draw a false drop at the
// end of every live chart. Its rate uses the time elapsed in it instead, and
// while that is too short to say much (under a quarter of the step or 30 s;
// the log is also written every 5 s) the bucket is left out.

import type { Series } from './api/types'

const MIN_ELAPSED = 30 // seconds

export interface Rates<K extends string> {
  /** Bucket starts (unix seconds) that have a rate. */
  timestamps: number[]
  /** Rate per `per` seconds for each key, aligned with timestamps. */
  values: Record<K, number[]>
}

/**
 * Per-bucket rates of `keys`, per `per` seconds (1 = per second, 60 = per
 * minute). `asOfMs` is when the series was requested: the end of its range.
 */
export function seriesRates<K extends string>(s: Series<K>, keys: readonly K[], per: number, asOfMs: number): Rates<K> {
  const step = Math.max(1, s.step)
  const n = s.timestamps.length
  const last = n > 0 ? s.timestamps[n - 1] : 0
  const elapsed = Math.min(step, asOfMs / 1000 - last)
  const keepLast = elapsed >= Math.min(step, Math.max(MIN_ELAPSED, step / 4))
  const count = keepLast ? n : Math.max(0, n - 1)
  const span = (i: number) => (i === n - 1 ? elapsed : step)
  const values = {} as Record<K, number[]>
  for (const k of keys) {
    const v = s.values[k]
    values[k] = Array.from({ length: count }, (_, i) => ((v?.[i] ?? 0) * per) / span(i))
  }
  return { timestamps: s.timestamps.slice(0, count), values }
}
