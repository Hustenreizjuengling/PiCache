// Translations: English (source) and German ("du", sentence case). Each
// namespace is one flat dictionary per language in en/<ns>.ts and de/<ns>.ts;
// keys are "<ns>.<key>", e.g. t('dns.queryLog.title'). Keys are type-checked.

import { loadPref, savePref } from '../lib/storage'
import type { Messages, Params } from './types'

import enAuth from './en/auth'
import enCache from './en/cache'
import enCommon from './en/common'
import enDns from './en/dns'
import enOverview from './en/overview'
import enSystem from './en/system'

import deAuth from './de/auth'
import deCache from './de/cache'
import deCommon from './de/common'
import deDns from './de/dns'
import deOverview from './de/overview'
import deSystem from './de/system'

export type { Messages, Params } from './types'

const en = {
  common: enCommon,
  auth: enAuth,
  overview: enOverview,
  dns: enDns,
  cache: enCache,
  system: enSystem,
}

type Catalog = typeof en
type Namespace = keyof Catalog

const de: { [N in Namespace]: Messages<Catalog[N]> } = {
  common: deCommon,
  auth: deAuth,
  overview: deOverview,
  dns: deDns,
  cache: deCache,
  system: deSystem,
}

/** Every translation key, e.g. 'common.action.save'. */
export type MessageKey = { [N in Namespace]: `${N}.${Extract<keyof Catalog[N], string>}` }[Namespace]

/** Keys that have '.one' and '.other' variants, without the suffix (for tn()). */
export type PluralKey = MessageKey extends infer K ? (K extends `${infer B}.one` ? B : never) : never

export type Locale = 'en' | 'de'

/** Languages offered by the switcher (names in their own language). */
export const LOCALES: readonly { id: Locale; label: string }[] = [
  { id: 'en', label: 'English' },
  { id: 'de', label: 'Deutsch' },
]

const catalogs: Record<Locale, Record<Namespace, Record<string, string>>> = { en, de }

function isLocale(v: unknown): v is Locale {
  return v === 'en' || v === 'de'
}

function browserLocale(): Locale {
  for (const l of navigator.languages ?? [navigator.language]) {
    const base = l.toLowerCase().split('-')[0]
    if (isLocale(base)) return base
  }
  return 'en'
}

const tags = new Map<Locale, string>()

function tagFor(l: Locale): string {
  let tag = tags.get(l)
  if (!tag) {
    tag = (navigator.languages ?? [navigator.language]).find((x) => x.toLowerCase().split('-')[0] === l)
    tag ??= l === 'de' ? 'de-DE' : 'en-US'
    tags.set(l, tag)
  }
  return tag
}

const stored = loadPref('lang')
const initial: Locale = isLocale(stored) ? stored : browserLocale()
let current = $state<Locale>(initial)
let explicit = isLocale(stored)
document.documentElement.lang = initial

/** The active language (reactive). */
export const i18n = {
  get locale(): Locale {
    return current
  },
  /** BCP 47 tag for Intl formatters: the browser's regional variant of the active language. */
  get tag(): string {
    return tagFor(current)
  },
}

/** Switches the language for this browser (remembered in localStorage). */
export function setLocale(l: Locale): void {
  current = l
  explicit = true
  savePref('lang', l)
  document.documentElement.lang = l
}

/**
 * Applies the server's default language (settings.web.language via
 * /auth/status) unless the user picked one in this browser. "" = browser.
 */
export function applyServerLocale(lang: string): void {
  if (explicit) return
  current = isLocale(lang) ? lang : browserLocale()
  document.documentElement.lang = current
}

function lookup(key: string): string {
  const i = key.indexOf('.')
  const ns = key.slice(0, i) as Namespace
  const k = key.slice(i + 1)
  return catalogs[current][ns]?.[k] ?? catalogs.en[ns]?.[k] ?? key
}

// Numbers in messages keep at most one fraction digit; the plural form is
// chosen for that rounded number, so 0.96 reads "1 query", not "1 queries".
const PARAM_DIGITS = { maximumFractionDigits: 1 }

let numberFormat: Intl.NumberFormat | null = null
let pluralRules: Intl.PluralRules | null = null
let formatLocale = ''

function formats(): { number: Intl.NumberFormat; plural: Intl.PluralRules } {
  if (formatLocale !== current || !numberFormat || !pluralRules) {
    numberFormat = new Intl.NumberFormat(i18n.tag, PARAM_DIGITS)
    pluralRules = new Intl.PluralRules(i18n.tag, PARAM_DIGITS)
    formatLocale = current
  }
  return { number: numberFormat, plural: pluralRules }
}

function formatParam(v: string | number): string {
  return typeof v === 'string' ? v : formats().number.format(v)
}

function interpolate(msg: string, params?: Params): string {
  if (!params) return msg
  return msg.replace(/\{(\w+)\}/g, (m, name: string) => (name in params ? formatParam(params[name]) : m))
}

/** Translates key and fills `{name}` placeholders. Reactive: re-renders on language change. */
export function t(key: MessageKey, params?: Params): string {
  return interpolate(lookup(key), params)
}

/**
 * Plural form: uses '<key>.one' or '<key>.other' and provides {count}. The
 * form follows count rounded to one fraction digit; pass a count rounded like
 * the number that is shown when {count} is formatted differently.
 */
export function tn(key: PluralKey, count: number, params?: Params): string {
  const rule = formats().plural.select(count) === 'one' ? 'one' : 'other'
  return interpolate(lookup(`${key}.${rule}`), { count, ...params })
}

/** One piece of a translated message: plain text or a named placeholder. */
export type Part = { text: string; slot?: undefined } | { slot: string; text?: undefined }

/**
 * Splits a message into text and placeholders so components can render
 * links or markup for some placeholders (see lib/ui/Trans.svelte). Values in
 * `params` are filled in as text; other placeholders become slots.
 */
export function tParts(key: MessageKey, params?: Params): Part[] {
  const msg = lookup(key)
  const out: Part[] = []
  let last = 0
  for (const m of msg.matchAll(/\{(\w+)\}/g)) {
    const idx = m.index ?? 0
    if (idx > last) out.push({ text: msg.slice(last, idx) })
    const name = m[1]
    if (params && name in params) out.push({ text: formatParam(params[name]) })
    else out.push({ slot: name })
    last = idx + m[0].length
  }
  if (last < msg.length) out.push({ text: msg.slice(last) })
  return out
}
