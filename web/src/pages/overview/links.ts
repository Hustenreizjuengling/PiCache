// Links from the overview into filtered pages. The target pages read these
// query parameters (contract documented in web/README.md).

import { BLOCKED_STATUSES, type RangePreset } from '../../lib/api'
import { href } from '../../lib/router.svelte'

/** Query log ranges are limited by its retention (7 days by default). */
function logRange(range: RangePreset): RangePreset {
  return range === '30d' || range === '90d' ? '7d' : range
}

export const links = {
  queries: (range: RangePreset = '1h') => href('/dns/queries', { range: logRange(range) }),
  blocked: (range: RangePreset = '24h') => href('/dns/queries', { status: BLOCKED_STATUSES, range: logRange(range) }),
  /** Exact domain match ("…" quoting) among blocked queries. */
  blockedDomain: (domain: string, range: RangePreset) =>
    href('/dns/queries', { domain: `"${domain}"`, status: BLOCKED_STATUSES, range: logRange(range) }),
  client: (client: string, range: RangePreset) => href('/dns/queries', { client, range: logRange(range) }),
  downloads: (query?: { client?: string; active?: boolean }) => href('/cache/downloads', query),
  library: () => href('/cache/library'),
  storage: () => href('/cache/storage'),
  cacheSettings: () => href('/cache/settings'),
  health: () => href('/system/health'),
}
