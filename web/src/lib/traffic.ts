// Fixed mapping from traffic states to pair colours (docs/DESIGN.md):
// blue = answered normally, orange = blocked, green = served from the cache,
// brown = fetched from the Internet. Striped = secondary state of the same
// meaning. Errors and refusals use status tones, not pair colours.

import type { CacheStatus, HealthStatus, LookupStatus } from './api/types'
import type { Pair, Tone } from './ui/types'

export interface ChipStyle {
  pair?: Pair
  striped?: boolean
  tone?: Tone
}

const queryStyles: Record<LookupStatus, ChipStyle> = {
  forwarded: { pair: 'blue' },
  cached: { pair: 'blue', striped: true },
  stale: { pair: 'blue', striped: true },
  local: { pair: 'blue' },
  special: { pair: 'blue' },
  override: { pair: 'green' },
  'blocked-list': { pair: 'orange' },
  'blocked-rule': { pair: 'orange' },
  'blocked-regex': { pair: 'orange' },
  'blocked-cname': { pair: 'orange', striped: true },
  'blocked-special': { pair: 'orange', striped: true },
  'blocked-schedule': { pair: 'orange' },
  'blocked-service': { pair: 'orange' },
  'blocked-upstream': { pair: 'orange', striped: true },
  'blocked-rebind': { pair: 'orange' },
  // Decided by the answer (like blocked-cname): an address in it is blocked.
  'blocked-ip': { pair: 'orange', striped: true },
  // Answered, but with the search engine's restricted address: a secondary state of "answered normally".
  safesearch: { pair: 'blue', striped: true },
  refused: { tone: 'warn' },
  error: { tone: 'fail' },
  dropped: { tone: 'warn' },
}

/** Chip colours for a DNS query status. */
export function queryStatusStyle(status: string): ChipStyle {
  return queryStyles[status as LookupStatus] ?? { tone: 'neutral' }
}

/** Whether a query status is one of the blocked-* statuses. */
export function isBlockedStatus(status: string): boolean {
  return status.startsWith('blocked-')
}

const cacheStyles: Record<CacheStatus, ChipStyle> = {
  HIT: { pair: 'green' },
  PARTIAL: { pair: 'green', striped: true },
  MISS: { pair: 'brown' },
  BYPASS: { pair: 'brown', striped: true },
  PASS: { pair: 'brown', striped: true },
  ERROR: { tone: 'fail' },
}

/** Chip colours for a cache request status. */
export function cacheStatusStyle(status: string): ChipStyle {
  return cacheStyles[status as CacheStatus] ?? { tone: 'neutral' }
}

/** Tone of a health status. */
export function healthTone(status: HealthStatus): Tone {
  return status === 'ok' ? 'ok' : status === 'warn' ? 'warn' : 'fail'
}
