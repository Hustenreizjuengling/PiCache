// The PiCache host's clock. Schedules use the host's local time, which can
// differ from this browser's (for example a Docker container on UTC). Dates
// are shifted so that their local fields (getHours, formatTime) show the
// host's wall clock; the shift is 0 when both zones agree.

import type { ParentalGroupState } from '$lib/api'

let shift = $state(0) // minutes: host UTC offset minus this browser's
let zone = $state<string | undefined>(undefined)

/** Takes the host's zone from a group state (GET /parental/groups). */
export function setHostClock(st: Pick<ParentalGroupState, 'timeZone' | 'utcOffsetMinutes'> | undefined): void {
  if (!st || !Number.isFinite(st.utcOffsetMinutes)) return
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
