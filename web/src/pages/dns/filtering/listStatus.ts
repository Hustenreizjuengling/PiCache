// Display helpers for filter lists: download states, categories and sizes.

import { t, tn, type MessageKey } from '$i18n/index.svelte'
import { PROTECTION_CATEGORIES, type FilterList, type FilterListInput, type ListCategory, type ListStatus } from '$lib/api'
import { formatBytes, formatNumber } from '$lib/format'
import type { Tone } from '$lib/ui'

/** Memory of one list entry, about (matcher plus the kept parse result; ARCHITECTURE 7.2). */
export const ENTRY_BYTES = 24

/** Lists with more entries than this are marked "Large". */
export const LARGE_ENTRIES = 1_000_000

/** Tone and label of a list's download state. */
export function listStatus(status: ListStatus): { tone: Tone; label: string } {
  switch (status) {
    case 'ok':
      return { tone: 'ok', label: t('dns.lists.status.ok') }
    case 'unchanged':
      return { tone: 'ok', label: t('dns.lists.status.unchanged') }
    case 'failed-cached':
      return { tone: 'warn', label: t('dns.lists.status.failedCached') }
    case 'failed-empty':
      return { tone: 'fail', label: t('dns.lists.status.failedEmpty') }
    default:
      return { tone: 'info', label: t('dns.lists.status.pending') }
  }
}

/** Lines that were skipped while parsing (invalid or unsupported). */
export function skippedLines(l: FilterList): number {
  return l.invalid + l.unsupported
}

/** The input to save a list with changed members. */
export function listInput(l: FilterList): FilterListInput {
  return {
    name: l.name,
    url: l.url,
    kind: l.kind,
    plainDomains: l.plainDomains,
    enabled: l.enabled,
    groupIds: [...l.groupIds],
    comment: l.comment,
    category: l.category,
    format: l.format,
  }
}

/** The name of a list category ("Adult content"); unknown ones as they are. */
export function categoryLabel(c: string): string {
  const key = `dns.lists.category.${c}` as MessageKey
  const s = t(key)
  return s === key ? c : s
}

/** Lists of these categories are enforced like parental controls (also while blocking is paused). */
export function isProtection(c: string | undefined): boolean {
  return !!c && (PROTECTION_CATEGORIES as readonly string[]).includes(c as ListCategory)
}

/** The format of a list in words ("Domains", "Answer addresses"). */
export function formatLabel(l: Pick<FilterList, 'format'>): string {
  return l.format === 'ips' ? t('dns.lists.format.ips') : t('dns.lists.format.domains')
}

/** "470,000 entries · about 11 MB of memory". */
export function entriesText(entries: number): string {
  return tn('dns.lists.entriesMemory', entries, { count: formatNumber(entries), size: formatBytes(entries * ENTRY_BYTES) })
}
