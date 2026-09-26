// Blocking and unblocking DNS clients one entry at a time (dns.blockedClients)
// from the query log and the list of seen devices, each behind a
// confirmation that shows the server's refusal (e.g. the router's address).

import { t } from '$i18n/index.svelte'
import { api, type BlockClientResult } from '$lib/api'
import { confirm, toast } from '$lib/ui'

/**
 * Asks, then blocks the device behind an address: by its MAC address when
 * PiCache knows it (every address of the device), else the address. The
 * result names the stored or the already matching entry; undefined when
 * cancelled.
 */
export async function confirmBlockDevice(ip: string, name?: string): Promise<BlockClientResult | undefined> {
  let res: BlockClientResult | undefined
  const ok = await confirm({
    title: t('dns.shared.blockDeviceTitle', { client: name ? `${name} (${ip})` : ip }),
    message: t('dns.shared.blockDeviceText'),
    confirmLabel: t('dns.shared.blockDevice'),
    action: async () => {
      res = await api.dns.blockedClients.add(ip, true)
    },
  })
  if (!ok || !res) return undefined
  toast.success(res.added ? t('dns.shared.blockedToast', { entry: res.entry }) : t('dns.shared.alreadyBlocked', { entry: res.entry }))
  return res
}

/**
 * Asks, then removes the entries of the blocked clients that block a device
 * (e.g. its address and its MAC address); true when removed. more: the
 * device is still blocked after a first round (another entry covers it).
 * The toast names the removed entries only: another entry may still block
 * the device.
 */
export async function confirmUnblock(entries: readonly string[], more = false): Promise<boolean> {
  const entry = entries.join(', ')
  const ok = await confirm({
    title: t(more ? 'dns.shared.unblockMoreTitle' : 'dns.shared.unblockTitle', { entry }),
    message: t(more ? 'dns.shared.unblockMoreText' : 'dns.shared.unblockText'),
    confirmLabel: t('dns.shared.unblock'),
    danger: false,
    action: async () => {
      for (const e of entries) await api.dns.blockedClients.remove(e)
    },
  })
  if (ok) toast.success(t('dns.shared.unblockedToast', { entry }))
  return ok
}
