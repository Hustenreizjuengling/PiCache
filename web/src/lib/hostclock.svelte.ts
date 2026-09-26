// The PiCache host's clock. Schedules and pauses "until 06:00" use the host's
// local time, which can differ from this browser's (for example a Docker
// container on UTC). Dates are shifted so that their local fields (getHours,
// formatTime) show the host's wall clock; the shift is 0 when both zones
// agree. The zone comes with every parental group state and with the
// blocking status (GET /dns/blocking, /system/overview).

import { i18n } from '../i18n/index.svelte'
import { formatTime } from './format'

let shift = $state(0) // minutes: host UTC offset minus this browser's
let zone = $state<string | undefined>(undefined)

/** Takes the host's zone from a group state or the blocking status. */
export function setHostClock(st: { timeZone?: string; utcOffsetMinutes?: number } | undefined): void {
  if (!st || typeof st.utcOffsetMinutes !== 'number' || !Number.isFinite(st.utcOffsetMinutes)) return
  zone = st.timeZone || undefined
  shift = st.utcOffsetMinutes + new Date().getTimezoneOffset()
}

/** The host's time zone abbreviation ("CEST", "UTC"), once known. */
export function hostZone(): string | undefined {
  return zone
}

/** True when the host's clock differs from this browser's. */
export function hostDiffers(): boolean {
  return shift !== 0
}

/** An instant as the host's wall clock. */
export function toHost(d: Date): Date {
  return shift ? new Date(d.getTime() + shift * 60_000) : d
}

/** The instant of a host wall-clock date (the inverse of toHost). */
export function fromHost(d: Date): Date {
  return shift ? new Date(d.getTime() - shift * 60_000) : d
}

/** The next time the host's clock shows `minutes` after midnight (today, or tomorrow when it has passed). */
export function nextHostTime(minutes: number, at: Date = new Date()): Date {
  const now = toHost(at)
  const d = new Date(now.getFullYear(), now.getMonth(), now.getDate(), Math.floor(minutes / 60), minutes % 60)
  if (d.getTime() <= now.getTime()) d.setDate(d.getDate() + 1)
  return fromHost(d)
}

/** "Sat 06:00": short weekday and time on the host's clock. */
export function hostDayTime(d: Date): string {
  const h = toHost(d)
  return `${new Intl.DateTimeFormat(i18n.tag, { weekday: 'short' }).format(h)} ${formatTime(h)}`
}
