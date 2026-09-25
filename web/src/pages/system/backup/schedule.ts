// Helpers for scheduled backups (settings.backups): the checks the server
// also makes, weekday names, the schedule as a sentence and where backups
// are written for each destination.

import { i18n, t, tn } from '$i18n/index.svelte'
import type { BackupsSettings, StorageTargetWithStatus } from '$lib/api'
import type { SelectOption } from '$lib/ui'

/** Destination value for PiCache's own data directory. */
export const LOCAL = 'local'

/** "HH:MM", 00:00–23:59. */
export const TIME_RE = /^([01]\d|2[0-3]):[0-5]\d$/

/** Weekday options, Monday first; the value counts from 0 = Sunday like the server. */
export function weekdayOptions(): SelectOption[] {
  return [1, 2, 3, 4, 5, 6, 0].map((d) => ({ value: String(d), label: weekdayName(d) }))
}

/** Localised weekday name (0 = Sunday). */
export function weekdayName(day: number): string {
  // 2024-01-07 was a Sunday.
  const f = new Intl.DateTimeFormat(i18n.tag, { weekday: 'long', timeZone: 'UTC' })
  return f.format(new Date(Date.UTC(2024, 0, 7 + (((day % 7) + 7) % 7))))
}

/** "03:30" the way the language writes times ("3:30 AM" / "03:30"); invalid values as they are. */
export function formatClock(hhmm: string): string {
  if (!TIME_RE.test(hhmm)) return hhmm
  const [h, m] = hhmm.split(':').map(Number)
  return new Intl.DateTimeFormat(i18n.tag, { timeStyle: 'short', timeZone: 'UTC' }).format(new Date(Date.UTC(2000, 0, 1, h, m)))
}

/** "Every Monday at 3:30 AM CEST, keeps the newest 7" (with the host time zone if known). */
export function scheduleText(s: BackupsSettings, zone?: string): string {
  const time = zone ? `${formatClock(s.time)} ${zone}` : formatClock(s.time)
  const when =
    s.schedule === 'weekly'
      ? t('system.backup.scheduled.scheduleText.weekly', { day: weekdayName(s.weekday), time })
      : t('system.backup.scheduled.scheduleText.daily', { time })
  return `${when}, ${tn('system.backup.scheduled.keepText', s.keep)}`
}

/** A place scheduled backups can be written to. */
export interface Destination {
  /** Settings value: LOCAL or a storage target id. */
  id: string
  name: string
  /** Directory the backups go to (unknown while a target is offline). */
  path?: string
  online: boolean
  /** Why a target is offline. */
  reason?: string
}

function dirOf(base: string, ...parts: string[]): string {
  const sep = base.includes('\\') && !base.includes('/') ? '\\' : '/'
  return [base.replace(/[\\/]+$/, ''), ...parts].join(sep) + sep
}

/**
 * PiCache's data directory (`<data>/backups/scheduled/`) and every storage
 * target (`<store root>/picache-backups/`). The built-in cache folder (target
 * id "local") is left out: "local" names the data directory here.
 */
export function destinations(targets: readonly StorageTargetWithStatus[] | undefined, dataDir: string | undefined): Destination[] {
  const out: Destination[] = [
    {
      id: LOCAL,
      name: t('system.backup.scheduled.destLocal'),
      path: dataDir ? dirOf(dataDir, 'backups', 'scheduled') : undefined,
      online: true,
    },
  ]
  for (const tg of targets ?? []) {
    if (tg.id === LOCAL) continue
    out.push({
      id: tg.id,
      name: tg.name,
      path: tg.status.storeRoot ? dirOf(tg.status.storeRoot, 'picache-backups') : undefined,
      online: tg.status.online,
      reason: tg.status.reason,
    })
  }
  return out
}

/** `dests` with the directory the server reports for destination `id` (it knows best). */
export function withServerPath(dests: Destination[], id: string | undefined, path: string | undefined): Destination[] {
  if (!id || !path) return dests
  return dests.map((d) => (d.id === id ? { ...d, path: dirOf(path) } : d))
}

/** "NAS (/srv/picache/nas/picache-backups/)", or the name alone when the path is unknown. */
export function destinationLabel(d: Destination): string {
  return d.path ? t('system.backup.scheduled.destOption', { name: d.name, path: d.path }) : d.name
}
