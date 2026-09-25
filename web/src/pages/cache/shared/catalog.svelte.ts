// Download services (id → display name) for tables, filters and links.
// Call serviceCatalog() during component initialisation.

import { api, resource, type DownloadCacheService } from '$lib/api'
import type { SelectOption } from '$lib/ui'

export interface ServiceCatalog {
  /** Services as returned by /download-cache/services (domains trimmed). */
  readonly list: readonly DownloadCacheService[]
  readonly loaded: boolean
  /** Display name of a service id (the id itself when unknown). */
  name(id: string): string
  /** Select options sorted by name. */
  readonly options: SelectOption[]
  refresh(): Promise<void>
}

/** Loads the services once and offers name lookups. */
export function serviceCatalog(): ServiceCatalog {
  const r = resource((signal) => api.downloadCache.services({ signal }))
  const names = $derived(new Map((r.data ?? []).map((s) => [s.id, s.name])))
  const options = $derived(
    [...(r.data ?? [])]
      .sort((a, b) => a.name.localeCompare(b.name))
      .map((s): SelectOption => ({ value: s.id, label: s.name })),
  )
  return {
    get list() {
      return r.data ?? []
    },
    get loaded() {
      return r.loaded
    },
    name: (id: string) => names.get(id) || id,
    get options() {
      return options
    },
    refresh: () => r.refresh(),
  }
}
