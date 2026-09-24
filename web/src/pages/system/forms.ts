// Client-side checks for the system forms. They mirror the server's
// validation (internal/settings/validate.go, internal/api/routes_auth.go) so
// mistakes show up next to the field before anything is sent; the server
// remains the authority and its field errors are shown as well.

import { t } from '$i18n/index.svelte'

/** Inclusive integer range. */
export interface Range {
  min: number
  max: number
}

export const RANGES = {
  sessionIdleMinutes: { min: 5, max: 10_080 },
  sessionMaxHours: { min: 1, max: 2_160 },
  queryLogRetentionHours: { min: 1, max: 8_784 },
  cacheLogRetentionHours: { min: 1, max: 8_784 },
  sessionRetentionDays: { min: 1, max: 3_650 },
  statsRetentionDays: { min: 1, max: 3_650 },
  maxDbSizeMiB: { min: 64, max: 1_048_576 },
  tokenExpiryDays: { min: 1, max: 3_650 },
} satisfies Record<string, Range>

/** Longest API token name (characters). */
export const MAX_TOKEN_NAME = 64
/** Most entries in web.allowedHosts. */
export const MAX_ALLOWED_HOSTS = 256
/** Shortest password (characters, as the server counts them). */
export const MIN_PASSWORD = 10

/** The message for a number outside its range (or not a whole number), else undefined. */
export function rangeError(value: unknown, r: Range): string | undefined {
  if (typeof value === 'number' && Number.isInteger(value) && value >= r.min && value <= r.max) return undefined
  return t('system.form.range', { min: r.min, max: r.max })
}

/**
 * Splits a text area into entries (one per line; commas and spaces also
 * separate), trimmed, lower-cased and without duplicates – the way the
 * server normalises host lists, so its error indices match the entries.
 */
export function parseHostList(text: string): string[] {
  const out: string[] = []
  for (const raw of text.split(/[\s,]+/)) {
    const h = raw.trim().toLowerCase()
    if (h && !out.includes(h)) out.push(h)
  }
  return out
}

/** Characters of a string as the server counts them (runes, not UTF-16 units). */
export function charCount(s: string): number {
  return [...s].length
}
