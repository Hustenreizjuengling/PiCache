// Helpers for the application log page: level order and tones, the filters
// in the URL and the NDJSON file of the loaded records.

import type { LogLevel, LogLevelFilter, LogRecord } from '$lib/api'
import type { Tone } from '$lib/ui'

export const LEVELS: readonly LogLevelFilter[] = ['debug', 'info', 'warn', 'error']

const RANK: Record<string, number> = { debug: 0, info: 1, warn: 2, error: 3 }

/** Order of a level ("DEBUG", "info", …); unknown levels count as info. */
export function levelRank(level: string): number {
  return RANK[level.toLowerCase()] ?? 1
}

export const LEVEL_TONES: Record<LogLevel, Tone> = { DEBUG: 'neutral', INFO: 'info', WARN: 'warn', ERROR: 'fail' }

/** The level filter of a URL value (default debug: everything the ring holds). */
export function asLevel(v: string): LogLevelFilter {
  return (LEVELS as readonly string[]).includes(v) ? (v as LogLevelFilter) : 'debug'
}

/** Whether a record passes the filters (records from the stream are filtered by the server too). */
export function matches(r: LogRecord, level: LogLevelFilter, component: string): boolean {
  return levelRank(r.level) >= levelRank(level) && (!component || r.component === component)
}

/** The records as NDJSON, oldest first (one JSON object per line). */
export function toNdjson(records: readonly LogRecord[]): Blob {
  const lines = [...records].reverse().map((r) => JSON.stringify(r))
  return new Blob([lines.join('\n') + (lines.length ? '\n' : '')], { type: 'application/x-ndjson' })
}
