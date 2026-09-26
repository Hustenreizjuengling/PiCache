// Batch changes of the rows checked in a table (enable, disable, delete),
// shared by the filtering, local DNS and client pages. The server changes
// all rows or none; deleting asks first and names the count.

import { t } from '$i18n/index.svelte'
import { toApiError, type BatchAction, type BatchRequest, type BatchResult } from '$lib/api'
import { errorText } from '$lib/errors'
import { confirm, toast } from '$lib/ui'

export interface BatchOptions {
  action: BatchAction
  ids: readonly number[]
  /** The rows in words, e.g. "3 rules". */
  what: (count: number) => string
  run: (req: BatchRequest) => Promise<BatchResult>
  /** What deleting also does (a sentence of the confirmation). */
  deleteText?: string
}

/**
 * Runs a batch change and reports it in a toast. Deleting asks first; a
 * 409 on "force" (enabling lists beyond the entry budget) shows the
 * server's message and, when confirmed, sends the change again with force.
 * Resolves true when the server accepted the change (the caller reloads).
 */
export async function runBatch(o: BatchOptions): Promise<boolean> {
  let res: BatchResult | undefined
  const send = async (force = false) => {
    res = await o.run({ action: o.action, ids: [...o.ids], ...(force ? { force } : {}) })
  }
  const what = o.what(o.ids.length)
  if (o.action === 'delete') {
    const ok = await confirm({
      title: t('dns.batch.deleteTitle', { what }),
      message: o.deleteText,
      confirmLabel: t('dns.batch.deleteConfirm', { what }),
      action: () => send(),
    })
    if (!ok) return false
  } else {
    try {
      await send()
    } catch (e) {
      const err = toApiError(e)
      if (err.code !== 'conflict' || err.field !== 'force') {
        toast.error(e)
        return false
      }
      const ok = await confirm({
        title: t('dns.batch.forceTitle'),
        message: errorText(err),
        confirmLabel: t('dns.batch.forceConfirm', { what }),
        danger: false,
        action: () => send(true),
      })
      if (!ok) return false
    }
  }
  const changed = res?.changed ?? 0
  if (changed === 0) toast.info(t('dns.batch.unchanged'))
  else if (o.action === 'delete') toast.success(t('dns.batch.deleted', { what: o.what(changed) }))
  else toast.success(t(o.action === 'enable' ? 'dns.batch.enabled' : 'dns.batch.disabled', { what: o.what(changed) }))
  return true
}
