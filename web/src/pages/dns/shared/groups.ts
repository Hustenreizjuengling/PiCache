// Group helpers shared by the filtering and client pages. A list or rule
// applies to a client when they share an enabled group.

import { t } from '$i18n/index.svelte'
import type { ClientGroup } from '$lib/api'

/** Names of the given group ids ("Default, Kids"); unknown ids show as "#7". */
export function groupNames(ids: readonly number[], groups: readonly ClientGroup[] | undefined): string {
  if (ids.length === 0) return t('dns.shared.noGroup')
  return ids.map((id) => groups?.find((g) => g.id === id)?.name ?? `#${id}`).join(', ')
}

/** Sorted, unique group ids. */
export function normalizeIds(ids: readonly number[]): number[] {
  return [...new Set(ids)].sort((a, b) => a - b)
}
