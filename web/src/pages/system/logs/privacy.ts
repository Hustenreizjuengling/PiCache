// Privacy levels of the logs settings (docs/ARCHITECTURE.md 11): the four
// switches are the source of truth; a level is a named combination of them
// (the server derives settings.logs.privacyLevel the same way).

import type { LogsSettings, PrivacyLevel } from '$lib/api'

export type Switches = Pick<LogsSettings, 'queryLogEnabled' | 'anonymizeClientIps' | 'hideDomains' | 'statsEnabled'>

type Preset = Exclude<PrivacyLevel, 'custom'>

/** The switches of each preset. */
export const PRESETS: Record<Preset, Switches> = {
  full: { queryLogEnabled: true, anonymizeClientIps: false, hideDomains: false, statsEnabled: true },
  'hide-domains': { queryLogEnabled: true, anonymizeClientIps: false, hideDomains: true, statsEnabled: true },
  anonymous: { queryLogEnabled: true, anonymizeClientIps: true, hideDomains: true, statsEnabled: true },
  off: { queryLogEnabled: false, anonymizeClientIps: true, hideDomains: true, statsEnabled: false },
}

/** Levels in the order of the selector. */
export const LEVELS: readonly PrivacyLevel[] = ['full', 'hide-domains', 'anonymous', 'off', 'custom']

const KEYS: readonly (keyof Switches)[] = ['queryLogEnabled', 'anonymizeClientIps', 'hideDomains', 'statsEnabled']

/** The level of a combination of switches (custom when it is no preset). */
export function levelOf(s: Switches): PrivacyLevel {
  for (const [level, p] of Object.entries(PRESETS) as [Preset, Switches][]) {
    if (KEYS.every((k) => s[k] === p[k])) return level
  }
  return 'custom'
}

/** Sets the switches of a preset in `target`. */
export function applyPreset(target: Switches, level: Preset): void {
  for (const k of KEYS) target[k] = PRESETS[level][k]
}

/**
 * What to offer to delete after a change to a more private setting: the
 * query log when it was switched off, or client addresses or domains are no
 * longer recorded; the statistics when they were switched off. Undefined
 * when nothing became more private.
 */
export function offerAfter(before: Switches, after: Switches): { queries: boolean; stats: boolean } | undefined {
  const logOff = before.queryLogEnabled && !after.queryLogEnabled
  const anonymized = !before.anonymizeClientIps && after.anonymizeClientIps
  const hidden = !before.hideDomains && after.hideDomains
  const statsOff = before.statsEnabled && !after.statsEnabled
  if (!logOff && !anonymized && !hidden && !statsOff) return undefined
  return { queries: logOff || anonymized || hidden, stats: statsOff }
}
