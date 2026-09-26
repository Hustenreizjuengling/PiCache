// Clearing the query log and resetting the statistics (Logs & privacy and
// the query log's menu): a confirmation that names what is kept, then the
// request (the server hands it to the log writer, which can take a while).

import { t, tn } from '$i18n/index.svelte'
import { api } from '$lib/api'
import { appStatus } from '$lib/status.svelte'
import { confirm, toast } from '$lib/ui'

/** Deletes every query-log entry after asking; true when it was cleared. */
export async function clearQueryLog(): Promise<boolean> {
  let deleted = 0
  const ok = await confirm({
    title: t('system.logs.clear.queriesTitle'),
    message: t('system.logs.clear.queriesConfirm'),
    confirmLabel: t('system.logs.clear.queries'),
    action: async () => {
      deleted = (await api.logs.clear()).deleted
    },
  })
  if (ok) toast.success(tn('system.logs.clear.queriesDone', deleted))
  return ok
}

/** Deletes the statistics after asking; true when they were reset. */
export async function resetStatistics(): Promise<boolean> {
  let deleted = 0
  const ok = await confirm({
    title: t('system.logs.clear.statsTitle'),
    message: t('system.logs.clear.statsConfirm'),
    confirmLabel: t('system.logs.clear.stats'),
    action: async () => {
      deleted = (await api.stats.reset()).deleted
    },
  })
  if (ok) {
    toast.success(tn('system.logs.clear.statsDone', deleted))
    void appStatus.strip.refresh() // the pair strip reads the statistics
  }
  return ok
}
