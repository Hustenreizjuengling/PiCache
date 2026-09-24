// The "full in ~N days" estimate of the overview's storage summary.
//
//   growthPerDay = (cacheBytesStored − evictedBytes) / coveredDays   (last 7 days)
//   room         = freeBytes − minFreeBytes, or maxSizeBytes − cachedBytes when
//                  a size limit is set and that is smaller (eviction starts there)
//   days         = room / growthPerDay (none when growth ≤ 0)
//
// coveredDays is the time since the first hour with cache traffic in the last
// 7 days (1 to 7): right after installing, the week's totals cover only a few
// days, and dividing by 7 would make the estimate up to 7 times too long.

import type { CacheSeriesKey, Series, StoreState, Summary } from '$lib/api'

const DAY = 86_400

export interface FullEstimate {
  days: number
  /** The size limit (cache.maxSizeBytes) is reached first, not the free space. */
  byLimit: boolean
}

/** Days covered by the last 7 days of statistics (1–7), from an hourly cache series. */
export function coveredDays(series: Series<CacheSeriesKey>, nowMs = Date.now()): number {
  const { timestamps, values } = series
  for (let i = 0; i < timestamps.length; i++) {
    if ((values.hit?.[i] ?? 0) > 0 || (values.wan?.[i] ?? 0) > 0) {
      return Math.min(7, Math.max(1, (nowMs / 1000 - timestamps[i]) / DAY))
    }
  }
  return 7
}

/** Whether the cache is at its size limit (eviction removes old content). */
export function atSizeLimit(store: StoreState): boolean {
  const max = store.maxSizeBytes ?? 0
  return max > 0 && (store.usage?.cachedBytes ?? 0) >= max
}

/** Days until the cache is full or reaches its size limit (null: not growing). */
export function fullEstimate(store: StoreState, week: Summary, days: number): FullEstimate | null {
  const growthPerDay = (week.cacheBytesStored - week.evictedBytes) / Math.min(7, Math.max(1, days))
  if (!(growthPerDay > 0)) return null
  let room = store.freeBytes - store.minFreeBytes
  let byLimit = false
  const max = store.maxSizeBytes ?? 0
  if (max > 0) {
    const toLimit = max - (store.usage?.cachedBytes ?? 0)
    if (toLimit < room) {
      room = toLimit
      byLimit = true
    }
  }
  return { days: Math.max(0, Math.floor(room / growthPerDay)), byLimit }
}
