// Display helpers for filter list states.

import { t } from '$i18n/index.svelte'
import type { FilterList, FilterListInput, ListStatus } from '$lib/api'
import type { Tone } from '$lib/ui'

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
  }
}
