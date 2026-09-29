// Translations (docs/TRANSLATING.md): English is the source and the fallback
// and part of the main bundle; every other language is a chunk of its own
// (i18n/<id>/<id>.ts → assets/<id>-<hash>.js), loaded before it is shown.
// Each namespace is one flat dictionary per language in <id>/<ns>.ts; keys
// are "<ns>.<key>", e.g. t('dns.queryLog.title'). Keys are type-checked.

import { loadPref, savePref } from '../lib/storage'
import { toast } from '../lib/ui/toast.svelte'
import en from './en/en'
import { LOCALES, type Locale } from './locales'
import type { Params } from './types'

export type { Messages, Params, Translation } from './types'
export { LOCALES, type Locale } from './locales'

type Catalog = typeof en
type Namespace = keyof Catalog

/** A loaded language: one flat dictionary per namespace. */
type Dictionaries = { readonly [N in Namespace]: Readonly<Record<string, string>> }

/** Every translation key, e.g. 'common.action.save'. */
export type MessageKey = { [N in Namespace]: `${N}.${Extract<keyof Catalog[N], string>}` }[Namespace]

/** Keys that have '.one' and '.other' variants, without the suffix (for tn()). */
export type PluralKey = MessageKey extends infer K ? (K extends `${infer B}.one` ? B : never) : never

// The explicit loader map keeps every language a chunk of its own.
const loaders: Record<Exclude<Locale, 'en'>, () => Promise<{ default: Dictionaries }>> = {
  de: () => import('./de/de'),
}

const catalogs: Partial<Record<Locale, Dictionaries>> = { en }

export function isLocale(v: unknown): v is Locale {
  return LOCALES.some((l) => l.id === v)
}

function browserTags(): readonly string[] {
  return navigator.languages?.length ? navigator.languages : [navigator.language]
}

/**
 * The first browser language PiCache has: per entry of navigator.languages
 * the exact tag (any case), then its base language ('pt-PT' → 'pt-BR').
 */
function browserLocale(): Locale {
  for (const tag of browserTags()) {
    const lower = tag.toLowerCase()
    const exact = LOCALES.find((l) => l.id.toLowerCase() === lower)
    if (exact) return exact.id
    const base = LOCALES.find((l) => l.id.toLowerCase().split('-')[0] === lower.split('-')[0])
    if (base) return base.id
  }
  return 'en'
}

const tags = new Map<Locale, string>()

/**
 * The tag for Intl formatters: the browser's regional variant of the
 * language ('de-AT'; a language with a region such as pt-BR only takes its
 * own tag), else the language's default tag.
 */
function tagFor(l: Locale): string {
  let tag = tags.get(l)
  if (!tag) {
    const id = l.toLowerCase()
    const own = browserTags().find((x) => (id.includes('-') ? x.toLowerCase() === id : x.toLowerCase().split('-')[0] === id))
    tag = own ?? LOCALES.find((x) => x.id === l)?.tag ?? 'en-US'
    tags.set(l, tag)
  }
  return tag
}

let current = $state<Locale>('en')
/** A language was picked in this browser (the server's default does not apply). */
let explicit = false
/** The latest switch wins when several are loading. */
let switchSeq = 0

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

/** Loads a language (once) and shows it; a failed load keeps the current language and says so. */
async function switchTo(l: Locale): Promise<boolean> {
  const seq = ++switchSeq
  if (!catalogs[l] && l !== 'en') {
    try {
      catalogs[l] = (await loaders[l]()).default
    } catch {
      if (seq === switchSeq) {
        const label = LOCALES.find((x) => x.id === l)?.label ?? l
        toast.error(t('common.language.loadFailed', { language: label }))
      }
      return false
    }
  }
  if (seq !== switchSeq) return false
  current = l
  document.documentElement.lang = l
  return true
}

/** Loads and shows the language known at start: the pick of this browser, else the browser's languages. Awaited before mounting. */
export async function initI18n(): Promise<void> {
  const stored = loadPref('lang')
  explicit = isLocale(stored)
  await switchTo(isLocale(stored) ? stored : browserLocale())
}

/** Switches the language for this browser (remembered in localStorage) once it is loaded. */
export async function setLocale(l: Locale): Promise<void> {
  if (!(await switchTo(l))) return
  explicit = true
  savePref('lang', l)
}

/**
 * Applies the server's default language (settings.web.language via
 * /auth/status) unless the user picked one in this browser. "" = browser.
 */
export async function applyServerLocale(lang: string): Promise<void> {
  if (explicit) return
  await switchTo(isLocale(lang) ? lang : browserLocale())
}

function split(key: string): [Namespace, string] {
  const i = key.indexOf('.')
  return [key.slice(0, i) as Namespace, key.slice(i + 1)]
}

function lookup(key: string): string {
  const [ns, k] = split(key)
  return catalogs[current]?.[ns]?.[k] ?? catalogs.en?.[ns]?.[k] ?? key
}

// Numbers in messages keep at most one fraction digit; the plural form is
// chosen for that rounded number, so 0.96 reads "1 query", not "1 queries".
const PARAM_DIGITS = { maximumFractionDigits: 1 }

let numberFormat: Intl.NumberFormat | null = null
let pluralRules: Intl.PluralRules | null = null
let formatLocale = ''
const englishPlural = new Intl.PluralRules('en-US', PARAM_DIGITS)

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

/** The English text of a key (the settings search also matches it), placeholders filled like t(). */
export function tEnglish(key: MessageKey, params?: Params): string {
  const [ns, k] = split(key)
  return interpolate(catalogs.en?.[ns]?.[k] ?? key, params)
}

/**
 * Plural form: '<key>.<category>' for the language's plural category of
 * count (Intl.PluralRules: 'one', 'few', 'many', 'other', …), else
 * '<key>.other', else English; provides {count}. The form follows count
 * rounded to one fraction digit; pass a count rounded like the number that
 * is shown when {count} is formatted differently.
 */
export function tn(key: PluralKey, count: number, params?: Params): string {
  const [ns, k] = split(key)
  const own = catalogs[current]?.[ns]
  const msg =
    own?.[`${k}.${formats().plural.select(count)}`] ??
    own?.[`${k}.other`] ??
    catalogs.en?.[ns]?.[`${k}.${englishPlural.select(count)}`] ??
    catalogs.en?.[ns]?.[`${k}.other`] ??
    key
  return interpolate(msg, { count, ...params })
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
