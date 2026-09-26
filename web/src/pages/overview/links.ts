// Links from the overview into filtered pages. The target pages read these
// query parameters (contract documented in web/README.md).

import { BLOCKED_STATUSES, type RangePreset } from '../../lib/api'
import { isCustom, PRESET_SECONDS, type Range } from '../../lib/range'
import { href, type QueryPatch } from '../../lib/router.svelte'
import { clientValues } from '../dns/querylog/filters'

/**
 * The query log's range for an overview range: presets beyond 7 days become
 * 7 days (the query log keeps 7 days by default); a custom window is kept.
 */
function logRange(range: Range): QueryPatch {
  if (isCustom(range)) return { from: range.from, to: range.to }
  const r: RangePreset = PRESET_SECONDS[range] > PRESET_SECONDS['7d'] ? '7d' : range
  return { range: r }
}

export const links = {
  queries: (range: Range = '1h') => href('/dns/queries', logRange(range)),
  blocked: (range: Range = '24h') => href('/dns/queries', { status: BLOCKED_STATUSES, ...logRange(range) }),
  /** Exact domain match ("…" quoting), optionally among blocked queries only. */
  domain: (domain: string, range: Range) => href('/dns/queries', { domain: `"${domain}"`, ...logRange(range) }),
  blockedDomain: (domain: string, range: Range) =>
    href('/dns/queries', { domain: `"${domain}"`, status: BLOCKED_STATUSES, ...logRange(range) }),
  /** One address or name, or every address of a device (repeated `client` parameters). */
  client: (client: string | readonly string[], range: Range) =>
    href('/dns/queries', { client: typeof client === 'string' ? client : clientValues(client), ...logRange(range) }),
  /** Queries answered by one upstream. */
  upstream: (upstream: string, range: Range) => href('/dns/queries', { upstream, ...logRange(range) }),
  /** Queries of one record type. */
  qtype: (qtype: string, range: Range) => href('/dns/queries', { qtype, ...logRange(range) }),
  downloads: (query?: { client?: string; active?: boolean }) => href('/cache/downloads', query),
  library: () => href('/cache/library'),
  storage: () => href('/cache/storage'),
  cacheSettings: () => href('/cache/settings'),
  health: () => href('/system/health'),
  privacy: () => href('/system/logs'),
}
