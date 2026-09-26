// Helpers for parental controls: days and presets, schedule summaries, the
// windows of the weekly plan, "until …" texts, the plain-words state of a
// group, safe search and the category switches. Schedules use host local
// time; times are shown on the host's clock ($lib/hostclock.svelte.ts),
// which normally is the browser's as well.

import { i18n, t, tn } from '$i18n/index.svelte'
import {
  DEFAULT_GROUP_ID,
  type CategorySwitch,
  type GroupControls,
  type ListCategory,
  type ParentalSchedule,
  type ParentalService,
  type SafeSearch,
  type ServiceCategory,
} from '$lib/api'
import { formatDateTimeShort, formatTime } from '$lib/format'
import { formatClock, TIME_RE } from '../../system/backup/schedule'
import { nextHostTime, toHost } from '$lib/hostclock.svelte'
import type { IconName } from '$lib/icons'

export { TIME_RE }

export const MAX_SCHEDULES = 10
export const MAX_SERVICES = 256
export const NAME_MAX = 40
/** Minutes of the "lift / block for …" quick actions. */
export const QUICK_MINUTES = [30, 60, 120] as const

/** Days in display order, Monday first (values count from 0 = Sunday like the server). */
export const WEEK: readonly number[] = [1, 2, 3, 4, 5, 6, 0]

/** Service categories in catalogue order, each with a neutral icon (never a brand logo). */
export const CATEGORIES: readonly ServiceCategory[] = [
  'video',
  'social',
  'messaging',
  'gaming',
  'music',
  'ai',
  'dating',
  'gambling',
  'shopping',
  'privacy',
  'software',
  'hosting',
  'news',
]

export const CATEGORY_ICONS: Record<ServiceCategory, IconName> = {
  video: 'video',
  social: 'users',
  messaging: 'chat',
  gaming: 'gamepad',
  music: 'music',
  ai: 'sparkles',
  dating: 'heart',
  gambling: 'dice',
  shopping: 'cart',
  privacy: 'eye-off',
  software: 'grid',
  hosting: 'cloud',
  news: 'newspaper',
}

export const PRESETS = [
  { id: 'schoolNights', days: [0, 1, 2, 3, 4] },
  { id: 'weekdays', days: [1, 2, 3, 4, 5] },
  { id: 'weekend', days: [0, 6] },
  { id: 'everyDay', days: [0, 1, 2, 3, 4, 5, 6] },
] as const

export type PresetId = (typeof PRESETS)[number]['id']

/** Groups in display order: Default last. */
export function ordered(groups: readonly GroupControls[]): GroupControls[] {
  return [...groups].sort((a, b) => Number(a.groupId === DEFAULT_GROUP_ID) - Number(b.groupId === DEFAULT_GROUP_ID) || a.groupId - b.groupId)
}

/** Localised weekday name (0 = Sunday). */
export function dayName(day: number, width: 'short' | 'long' = 'short'): string {
  // 2024-01-07 was a Sunday.
  const f = new Intl.DateTimeFormat(i18n.tag, { weekday: width, timeZone: 'UTC' })
  return f.format(new Date(Date.UTC(2024, 0, 7 + (((day % 7) + 7) % 7))))
}

/** "HH:MM" → minutes after midnight (-1 when invalid). */
export function minutesOf(hhmm: string): number {
  if (!TIME_RE.test(hhmm)) return -1
  const [h, m] = hhmm.split(':').map(Number)
  return h * 60 + m
}

/** The window ends the next day. */
export function isOvernight(s: Pick<ParentalSchedule, 'start' | 'end'>): boolean {
  const a = minutesOf(s.start)
  const b = minutesOf(s.end)
  return a >= 0 && b >= 0 && b < a
}

/** "21:00–07:00" or "21:00 until 07:00 the next day". */
export function windowText(s: Pick<ParentalSchedule, 'start' | 'end'>): string {
  const params = { start: formatClock(s.start), end: formatClock(s.end) }
  return isOvernight(s) ? t('dns.parental.window.overnight', params) : t('dns.parental.window.sameDay', params)
}

/** Sorted, unique days. */
export function normalizeDays(days: readonly number[]): number[] {
  return [...new Set(days.filter((d) => d >= 0 && d <= 6))].sort((a, b) => a - b)
}

export function sameDays(a: readonly number[], b: readonly number[]): boolean {
  const x = normalizeDays(a)
  const y = normalizeDays(b)
  return x.length === y.length && x.every((d, i) => d === y[i])
}

/**
 * The days as a short text in Monday-first order: "Every day", "Mon–Fri",
 * "Sun–Thu" (a run across the end of the week stays one run), "Mon, Wed, Fri".
 */
export function daysText(days: readonly number[]): string {
  const set = new Set(normalizeDays(days))
  if (set.size === 7) return t('dns.parental.days.every')
  if (set.size === 0) return '–'
  // Runs in Monday-first order.
  const runs: number[][] = []
  for (const d of WEEK) {
    if (!set.has(d)) continue
    const last = runs[runs.length - 1]
    if (last && WEEK.indexOf(last[last.length - 1]) === WEEK.indexOf(d) - 1) last.push(d)
    else runs.push([d])
  }
  // A run ending on Sunday continues into a run starting on Monday.
  if (runs.length > 1) {
    const first = runs[0]
    const last = runs[runs.length - 1]
    if (first[0] === 1 && last[last.length - 1] === 0) {
      runs.pop()
      runs[0] = [...last, ...first]
    }
  }
  return runs
    .map((r) => (r.length >= 3 ? `${dayName(r[0])}–${dayName(r[r.length - 1])}` : r.map((d) => dayName(d)).join(', ')))
    .join(', ')
}

/** Service names for ids (unknown ids as they are), in catalogue order. */
export function serviceNames(ids: readonly string[], catalog: readonly ParentalService[] | undefined): string[] {
  if (!catalog) return [...ids]
  const known = catalog.filter((s) => ids.includes(s.id)).map((s) => s.name)
  const unknown = ids.filter((id) => !catalog.some((s) => s.id === id))
  return [...known, ...unknown]
}

/** "YouTube and TikTok" / "YouTube, TikTok and Roblox" (language-aware). */
export function listText(items: readonly string[]): string {
  return new Intl.ListFormat(i18n.tag, { style: 'long', type: 'conjunction' }).format(items)
}

/** Up to `max` names, then "and 3 more". */
export function shortList(names: readonly string[], max = 3): string {
  if (names.length <= max) return listText(names)
  return listText([...names.slice(0, max), tn('dns.parental.more', names.length - max)])
}

/** What a schedule blocks: "Internet" or the service names. */
export function blockText(s: ParentalSchedule, catalog: readonly ParentalService[] | undefined): string {
  return s.block === 'all' ? t('dns.parental.schedule.internet') : shortList(serviceNames(s.services, catalog))
}

// ---- times relative to now

function startOfDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

/**
 * A future point in time in words. form 'until': "07:00", "tomorrow at
 * 07:00", "Monday at 07:00"; form 'at': the same with "at 07:00" for today.
 */
export function whenText(ts: string | Date, form: 'until' | 'at', at: Date = new Date()): string {
  const d = toHost(ts instanceof Date ? ts : new Date(ts))
  if (Number.isNaN(d.getTime())) return '–'
  const now = toHost(at)
  const days = Math.round((startOfDay(d) - startOfDay(now)) / 86_400_000)
  const time = formatTime(d)
  if (days <= 0) return form === 'at' ? t('dns.parental.when.at', { time }) : time
  if (days === 1) return t('dns.parental.when.tomorrow', { time })
  if (days < 7) return t('dns.parental.when.day', { day: dayName(d.getDay(), 'long'), time })
  return formatDateTimeShort(d)
}

/** The next occurrence of a host clock time ("HH:MM") after now: today, or tomorrow when it has passed. */
export function nextClock(hhmm: string, at: Date = new Date()): Date | undefined {
  const m = minutesOf(hhmm)
  return m < 0 ? undefined : nextHostTime(m, at)
}

// ---- the weekly plan

/** One bar of the weekly plan: a row (0 = Monday … 6 = Sunday) and minutes of that day. */
export interface Segment {
  row: number
  from: number
  to: number
  kind: 'all' | 'services'
  schedule: ParentalSchedule
  /** The bar continues from the previous day / into the next day. */
  fromPrev: boolean
  intoNext: boolean
}

/** Row of a day (0 = Sunday) in the Monday-first plan. */
export function rowOf(day: number): number {
  return WEEK.indexOf(((day % 7) + 7) % 7)
}

/** The bars of the enabled schedules; overnight windows continue on the next row (Sunday into Monday). */
export function planSegments(schedules: readonly ParentalSchedule[]): Segment[] {
  const out: Segment[] = []
  for (const s of schedules) {
    if (!s.enabled) continue
    const a = minutesOf(s.start)
    const b = minutesOf(s.end)
    if (a < 0 || b < 0 || a === b) continue
    const kind = s.block === 'all' ? 'all' : 'services'
    for (const day of normalizeDays(s.days)) {
      if (b > a) {
        out.push({ row: rowOf(day), from: a, to: b, kind, schedule: s, fromPrev: false, intoNext: false })
      } else {
        out.push({ row: rowOf(day), from: a, to: 1440, kind, schedule: s, fromPrev: false, intoNext: true })
        if (b > 0) out.push({ row: rowOf(day + 1), from: 0, to: b, kind, schedule: s, fromPrev: true, intoNext: false })
      }
    }
  }
  // Service windows first, so block-all bars are drawn on top.
  return out.sort((x, y) => Number(x.kind === 'all') - Number(y.kind === 'all') || x.row - y.row || x.from - y.from)
}

/** "HH:MM" for minutes after midnight (1440 → 24:00 is written as 00:00). */
export function clockOf(minutes: number): string {
  const m = ((minutes % 1440) + 1440) % 1440
  return `${String(Math.floor(m / 60)).padStart(2, '0')}:${String(m % 60).padStart(2, '0')}`
}

// ---- the state in plain words

export type StateKind = 'blocked' | 'services' | 'lifted' | 'none' | 'disabled'

export interface StateText {
  kind: StateKind
  text: string
  /** Second line: the next change of the plan. */
  next?: string
}

/** The current state of a group as one sentence ("Internet blocked until 07:00 (Bedtime)"). */
export function stateText(g: GroupControls, catalog: readonly ParentalService[] | undefined, now: Date = new Date()): StateText {
  const st = g.state
  const next = st.next
    ? t(st.next.starts ? 'dns.parental.state.nextStarts' : 'dns.parental.state.nextEnds', {
        name: st.next.name,
        when: whenText(st.next.time, 'at', now),
      })
    : undefined
  if (!g.groupEnabled) return { kind: 'disabled', text: t('dns.parental.state.disabled') }
  if (st.blockAll) {
    // While blocked by hand the override decides; the plan's next step is not news.
    if (st.reason === 'override') {
      return { kind: 'blocked', text: st.until ? t('dns.parental.state.byHand', { when: whenText(st.until, 'until', now) }) : t('dns.parental.state.byHandNoEnd') }
    }
    const name = st.schedule ?? ''
    // "Bedtime ends at 07:00" repeats the sentence.
    const nextOther = st.next && !st.next.starts && st.next.name === name ? undefined : next
    if (!st.until) return { kind: 'blocked', text: t('dns.parental.state.blockedNoEnd', { name }), next: nextOther }
    return { kind: 'blocked', text: t('dns.parental.state.blocked', { when: whenText(st.until, 'until', now), name }), next: nextOther }
  }
  if (st.lifted) {
    return {
      kind: 'lifted',
      text: st.liftedUntil ? t('dns.parental.state.lifted', { when: whenText(st.liftedUntil, 'until', now) }) : t('dns.parental.state.liftedNoEnd'),
      // Content protection is not lifted by the allow override.
      next: t('dns.parental.state.liftedKeeps'),
    }
  }
  const services = st.blockedServices ?? []
  if (services.length > 0) {
    return { kind: 'services', text: t('dns.parental.state.services', { services: shortList(serviceNames(services, catalog)) }), next }
  }
  // Nothing blocked by the plan: content protection may still apply (always on).
  const safe = safeSearchNames(g.safeSearch).length > 0
  const categories = switchesOf(g).some((s) => g.categories[s.id]?.on)
  if (safe || categories) {
    const key = safe && categories ? 'onlyBoth' : safe ? 'onlySafeSearch' : 'onlyCategories'
    return { kind: 'none', text: t(`dns.parental.state.${key}`), next }
  }
  return { kind: 'none', text: t('dns.parental.state.none'), next }
}

/** Whether the group restricts anything (so lifting makes a difference). */
export function hasRestrictions(g: GroupControls): boolean {
  return (g.blockedServices ?? []).length > 0 || (g.schedules ?? []).some((s) => s.enabled)
}

// ---- editing

/** A new, empty schedule (the server assigns the id). */
export function blankSchedule(): ParentalSchedule {
  return { id: '', name: '', enabled: true, days: [0, 1, 2, 3, 4], start: '21:00', end: '07:00', block: 'all', services: [] }
}

/** Client-side checks the server also makes; field → message. */
export function scheduleProblems(s: ParentalSchedule): Partial<Record<'name' | 'days' | 'start' | 'end' | 'services', string>> {
  const p: Partial<Record<'name' | 'days' | 'start' | 'end' | 'services', string>> = {}
  const name = s.name.trim()
  if (!name) p.name = t('dns.parental.schedule.nameRequired')
  else if ([...name].length > NAME_MAX) p.name = t('dns.parental.schedule.nameTooLong', { max: NAME_MAX })
  if (normalizeDays(s.days).length === 0) p.days = t('dns.parental.schedule.daysRequired')
  if (minutesOf(s.start) < 0) p.start = t('dns.parental.schedule.timeInvalid')
  if (minutesOf(s.end) < 0) p.end = t('dns.parental.schedule.timeInvalid')
  else if (s.start === s.end) p.end = t('dns.parental.schedule.sameTime')
  if (s.block === 'services' && s.services.length === 0) p.services = t('dns.parental.schedule.servicesRequired')
  return p
}

// ---- safe search

/** The search engines in display order. Their names are brands and stay untranslated. */
export const ENGINES = ['google', 'youtube', 'bing', 'duckduckgo', 'ecosia', 'yandex', 'pixabay'] as const
export type Engine = (typeof ENGINES)[number]

export const ENGINE_NAMES: Record<Engine, string> = {
  google: 'Google',
  youtube: 'YouTube',
  bing: 'Bing',
  duckduckgo: 'DuckDuckGo',
  ecosia: 'Ecosia',
  yandex: 'Yandex',
  pixabay: 'Pixabay',
}

/** Safe search switched off everywhere (the server's default). */
export function safeSearchOff(): SafeSearch {
  return { google: false, youtube: 'off', bing: false, duckduckgo: false, ecosia: false, yandex: false, pixabay: false }
}

/** The engines that enforce safe search: "Google", "YouTube (strict)", … */
export function safeSearchNames(s: SafeSearch | undefined): string[] {
  if (!s) return []
  return ENGINES.flatMap((e) => {
    if (e === 'youtube') return s.youtube === 'off' ? [] : [t(`dns.parental.safeSearch.youtubeNamed.${s.youtube}`)]
    return s[e] ? [ENGINE_NAMES[e]] : []
  })
}

// ---- category switches

/**
 * The category switches in display order, the catalogue key of the list
 * each one binds and that list's category (categoryPresets in
 * internal/dns/filter/catalog.go). The server decides what is bound; the key
 * only finds the list's name and size. A bound list of another category is
 * no protection list, so the server reports its switch as off.
 */
export const SWITCHES: readonly { id: CategorySwitch; catalogKey: string; category: ListCategory }[] = [
  { id: 'adult', catalogKey: 'oisd-nsfw', category: 'adult' },
  { id: 'gambling', catalogKey: 'hagezi-gambling-medium', category: 'gambling' },
  { id: 'dating', catalogKey: 'shadowwhisperer-dating', category: 'dating' },
  { id: 'piracy', catalogKey: 'hagezi-anti-piracy', category: 'piracy' },
  { id: 'bypass', catalogKey: 'hagezi-doh-vpn-bypass', category: 'doh-vpn-bypass' },
]

/** The switches the server offers for a group (a switch it dropped is absent). */
export function switchesOf(g: GroupControls): typeof SWITCHES {
  return SWITCHES.filter((s) => !!g.categories?.[s.id])
}
