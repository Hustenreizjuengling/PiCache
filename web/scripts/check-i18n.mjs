#!/usr/bin/env node
// Consistency checks of the translations (docs/TRANSLATING.md), run by
// `npm run check`. Dependency-free: it parses the flat 'key': 'value'
// dictionaries of web/src/i18n/<id>/<ns>.ts and lists every problem
// (language, namespace, key, rule), then exits 1 if there is one:
//   1. every language has every namespace file and exactly the English keys,
//      plus only the allowed plural extras of its language; no empty value;
//   2. every plural base has every plural category of the language
//      (Intl.PluralRules; 'many' is left out for fr, es, it and pt-BR);
//   3. placeholders: the English set for a plain key; for plural forms the
//      union of the English forms apart from {count}, and in the languages
//      after en and de every form contains {count} where English's has it;
//   4. code-like tokens (URLs, absolute paths, --flags, PICACHE_* names, unit
//      names, back-quoted text, command names) equal English's;
//   5. the numbers of < and > equal English's;
//   6. no {@html in web/src/**/*.svelte.

import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..')
const I18N = join(WEB, 'src', 'i18n')

/** Languages whose 'many' category is not translated (numbers of a million and more use 'other'). */
const NO_MANY = new Set(['fr', 'es', 'it', 'pt-BR'])
/** The languages before K2, which keep their own plural texts. */
const LEGACY = new Set(['en', 'de'])
const EXTRA_CATEGORIES = ['zero', 'two', 'few', 'many']

/** Decodes the escapes of a single-quoted JavaScript string. */
function unescape(s) {
  return s.replace(/\\(u\{[0-9a-fA-F]+\}|u[0-9a-fA-F]{4}|x[0-9a-fA-F]{2}|[^])/g, (_, e) => {
    if (e[0] === 'u') return String.fromCodePoint(parseInt(e[1] === '{' ? e.slice(2, -1) : e.slice(1), 16))
    if (e[0] === 'x') return String.fromCharCode(parseInt(e.slice(1), 16))
    return { n: '\n', t: '\t', r: '\r', b: '\b', f: '\f', v: '\v', 0: '\0' }[e] ?? e
  })
}

/**
 * The entries of a dictionary file in order: `'key': 'value'` or
 * `key: 'value'`, the value possibly on the next line. Comment lines are
 * skipped; `duplicates` lists keys that occur twice.
 */
export function parseDictionary(file) {
  const text = readFileSync(file, 'utf8')
    .split('\n')
    .filter((l) => !/^\s*\/\//.test(l))
    .join('\n')
  const re = /(?:'((?:[^'\\\n]|\\.)*)'|([A-Za-z_$][\w$]*))\s*:\s*'((?:[^'\\\n]|\\.)*)'/g
  const entries = new Map()
  const duplicates = []
  for (let m = re.exec(text); m; m = re.exec(text)) {
    const key = m[1] !== undefined ? unescape(m[1]) : m[2]
    if (entries.has(key)) duplicates.push(key)
    entries.set(key, unescape(m[3]))
  }
  return { entries, duplicates }
}

/** The languages of locales.ts in order: [{ id, tag }]. */
export function readLocales() {
  const src = readFileSync(join(I18N, 'locales.ts'), 'utf8')
  return [...src.matchAll(/\{\s*id:\s*'([^']+)',\s*label:\s*'[^']*',\s*tag:\s*'([^']+)'\s*\}/g)].map((m) => ({ id: m[1], tag: m[2] }))
}

/** The namespaces: the English dictionary files. */
export function readNamespaces() {
  return readdirSync(join(I18N, 'en'))
    .filter((f) => f.endsWith('.ts') && f !== 'en.ts')
    .map((f) => f.slice(0, -3))
    .sort()
}

const placeholders = (s) => new Set([...s.matchAll(/\{(\w+)\}/g)].map((m) => m[1]))
const sameSet = (a, b) => a.size === b.size && [...a].every((x) => b.has(x))
const show = (set) => `{${[...set].sort().join(', ')}}`

const COMMANDS = [
  'get-picache.sh',
  'install.sh',
  'firewall-cmd',
  'journalctl',
  'systemctl',
  'iptables',
  'openssl',
  'picache',
  'pacman',
  'zypper',
  'docker',
  'sudo',
  'curl',
  'apt',
  'dnf',
  'nft',
  'ufw',
]
const COMMAND_RE = new RegExp(`(?<![\\w.-])(?:${COMMANDS.map((c) => c.replace(/[.-]/g, '\\$&')).join('|')})(?![\\w-])`, 'g')
// Quoted text (UI labels, translated): “…”, „…“, "…", «…», »…«, ”…”.
const QUOTED = /“[^”\n]*”|„[^“”\n]*[“”]|"[^"\n]*"|«[^»\n]*»|»[^«\n]*«|”[^”\n]*”/g

/** The code-like tokens of a text, sorted (a multiset). */
export function codeTokens(text) {
  const out = []
  const take = (re) => {
    text = text.replace(re, (m) => {
      out.push(m)
      return ' '
    })
  }
  take(/`[^`\n]*`/g) // back-quoted text first: it may contain anything
  text = text.replace(QUOTED, ' ')
  // URLs of any scheme (http, https, tls, udp, sdns, …), a bare scheme too
  take(/(?<![\w.+-])[a-z][a-z0-9+.-]*:\/\/(?:[^\s"'“”„«»<>(),;]*[^\s"'“”„«»<>(),;.:!?])?/g)
  // absolute paths: "/" not after a word character, ".", "}" (a value such as {n}/min) or "/"
  take(/(?<![\w.}/])\/[\w.~*{}[\]@%+=-](?:[^\s"'“”„«»<>(),;]*[^\s"'“”„«»<>(),;.:!?])?/g)
  take(/(?<![\w-])--[A-Za-z][\w-]*/g)
  take(/PICACHE_[A-Z0-9_]+/g)
  take(/(?<![\w.@-])[\w@.-]+\.(?:service|path|socket|mount|timer)\b/g)
  take(COMMAND_RE)
  return out.sort()
}

const count = (s, ch) => s.split(ch).length - 1

function checkTranslations(problems) {
  const locales = readLocales()
  const namespaces = readNamespaces()
  const english = new Map(namespaces.map((ns) => [ns, parseDictionary(join(I18N, 'en', `${ns}.ts`))]))
  for (const [ns, d] of english) for (const k of d.duplicates) problems.push(`en ${ns} ${k}: duplicate key`)

  for (const { id, tag } of locales) {
    const categories = new Intl.PluralRules(tag).resolvedOptions().pluralCategories
    const extras = EXTRA_CATEGORIES.filter((c) => categories.includes(c) && !(c === 'many' && NO_MANY.has(id)))
    for (const ns of namespaces) {
      const en = english.get(ns).entries
      const where = (key) => `${id} ${ns} ${key}`
      const bases = [...en.keys()].filter((k) => k.endsWith('.one') && en.has(k.slice(0, -4) + '.other')).map((k) => k.slice(0, -4))
      const pluralOf = new Map() // key → base
      for (const b of bases) {
        for (const c of ['one', 'other', ...extras]) pluralOf.set(`${b}.${c}`, b)
      }
      if (id === 'en') {
        for (const [key, value] of en) if (!value.trim()) problems.push(`${where(key)}: empty value (rule 1)`)
        continue
      }
      const file = join(I18N, id, `${ns}.ts`)
      if (!existsSync(file)) {
        problems.push(`${id} ${ns}: namespace file ${relative(WEB, file)} is missing (rule 1)`)
        continue
      }
      const { entries, duplicates } = parseDictionary(file)
      for (const k of duplicates) problems.push(`${where(k)}: duplicate key (rule 1)`)
      // rule 1: exactly the English keys plus the plural extras
      for (const key of en.keys()) if (!entries.has(key)) problems.push(`${where(key)}: missing (rule 1)`)
      for (const [key, value] of entries) {
        if (!en.has(key) && !pluralOf.has(key)) problems.push(`${where(key)}: not an English key or plural form of ${id} (rule 1)`)
        if (!value.trim()) problems.push(`${where(key)}: empty value (rule 1)`)
      }
      // rule 2: every plural category of the language
      for (const b of bases) {
        for (const c of extras) if (!entries.has(`${b}.${c}`)) problems.push(`${where(`${b}.${c}`)}: plural form missing (rule 2)`)
      }
      for (const [key, value] of entries) {
        const base = pluralOf.get(key)
        const ref = base ? en.get(`${base}.other`) : en.get(key)
        if (ref === undefined) continue
        // rule 3: placeholders
        const have = placeholders(value)
        if (base) {
          const want = new Set([...placeholders(en.get(`${base}.one`)), ...placeholders(en.get(`${base}.other`))])
          want.delete('count')
          const own = new Set(have)
          own.delete('count')
          if (!sameSet(own, want)) problems.push(`${where(key)}: placeholders ${show(have)}, English has ${show(want)} besides {count} (rule 3)`)
          if (!LEGACY.has(id) && placeholders(en.get(`${base}.other`)).has('count') && !have.has('count')) {
            problems.push(`${where(key)}: every plural form must contain {count} (rule 3)`)
          }
        } else if (!sameSet(have, placeholders(ref))) {
          problems.push(`${where(key)}: placeholders ${show(have)}, English has ${show(placeholders(ref))} (rule 3)`)
        }
        // rule 4: code-like tokens
        const a = codeTokens(value)
        const b = codeTokens(ref)
        if (a.join('\n') !== b.join('\n')) {
          problems.push(`${where(key)}: code-like tokens [${a.join(' ')}] differ from English [${b.join(' ')}] (rule 4)`)
        }
        // rule 5: < and >
        for (const ch of ['<', '>']) {
          if (count(value, ch) !== count(ref, ch)) problems.push(`${where(key)}: ${count(value, ch)} × "${ch}", English has ${count(ref, ch)} (rule 5)`)
        }
      }
    }
  }
}

function svelteFiles(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) svelteFiles(p, out)
    else if (name.endsWith('.svelte')) out.push(p)
  }
  return out
}

function checkNoHtml(problems) {
  for (const f of svelteFiles(join(WEB, 'src'))) {
    readFileSync(f, 'utf8')
      .split('\n')
      .forEach((line, i) => {
        if (line.includes('{@html')) problems.push(`${relative(WEB, f)}:${i + 1}: {@html is not allowed (rule 6)`)
      })
  }
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const problems = []
  checkTranslations(problems)
  checkNoHtml(problems)
  if (problems.length > 0) {
    for (const p of problems) console.error(p)
    console.error(`check-i18n: ${problems.length} problem${problems.length === 1 ? '' : 's'}`)
    process.exit(1)
  }
  console.log(`check-i18n: ${readLocales().length} languages, ${readNamespaces().length} namespaces OK`)
}
