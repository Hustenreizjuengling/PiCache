#!/usr/bin/env node
// Checks the settings search index (web/src/lib/searchIndex.ts), run by
// `npm run check`: every entry's route exists in src/routes.ts (sub-paths
// allowed), its anchor and field occur as a literal id="…" exactly once in
// web/src/**/*.svelte, and its title, help and context keys exist in the
// English catalog. Dependency-free; lists every problem and exits 1.

import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseDictionary } from './check-i18n.mjs'

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..')
const SRC = join(WEB, 'src')

function svelteFiles(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) svelteFiles(p, out)
    else if (name.endsWith('.svelte')) out.push(p)
  }
  return out
}

// The index: the object literals of SEARCH_INDEX; `const X = '…'` route constants are resolved.
const indexSrc = readFileSync(join(SRC, 'lib', 'searchIndex.ts'), 'utf8')
const constants = new Map([...indexSrc.matchAll(/^const (\w+) = '([^']*)'$/gm)].map((m) => [m[1], m[2]]))
const body = indexSrc.slice(indexSrc.indexOf('SEARCH_INDEX'), indexSrc.indexOf('\n]\n'))
const entries = [...body.matchAll(/\{([^{}]*)\}/g)].map((m, i) => {
  const prop = (name) => {
    const r = new RegExp(`\\b${name}:\\s*(?:'([^']*)'|(\\w+))`).exec(m[1])
    return r ? (r[1] ?? constants.get(r[2]) ?? `<unknown constant ${r[2]}>`) : undefined
  }
  return { n: i + 1, route: prop('route'), anchor: prop('anchor'), field: prop('field'), title: prop('title'), help: prop('help'), context: prop('context') }
})

const routes = [...readFileSync(join(SRC, 'routes.ts'), 'utf8').matchAll(/\bpath:\s*'([^']+)'/g)].map((m) => m[1])

const ids = new Map()
for (const f of svelteFiles(SRC)) {
  for (const m of readFileSync(f, 'utf8').matchAll(/\bid="([^"{}]+)"/g)) ids.set(m[1], (ids.get(m[1]) ?? 0) + 1)
}

const catalogs = new Map()
function hasKey(key) {
  const i = key.indexOf('.')
  const ns = key.slice(0, i)
  if (!catalogs.has(ns)) {
    try {
      catalogs.set(ns, parseDictionary(join(SRC, 'i18n', 'en', `${ns}.ts`)).entries)
    } catch {
      catalogs.set(ns, new Map())
    }
  }
  return catalogs.get(ns).has(key.slice(i + 1))
}

const problems = []
if (entries.length === 0) problems.push('no entries found in src/lib/searchIndex.ts')
for (const e of entries) {
  const at = `entry ${e.n} (${e.anchor}${e.field ? ' / ' + e.field : ''})`
  if (!e.route) problems.push(`${at}: no route`)
  else if (!routes.some((r) => e.route === r || (r !== '/' && e.route.startsWith(r + '/')))) problems.push(`${at}: route ${e.route} is not in routes.ts`)
  for (const [what, id] of [
    ['anchor', e.anchor],
    ['field', e.field],
  ]) {
    if (id === undefined) {
      if (what === 'anchor') problems.push(`${at}: no anchor`)
      continue
    }
    const n = ids.get(id) ?? 0
    if (n !== 1) problems.push(`${at}: ${what} id="${id}" occurs ${n} times in src/**/*.svelte (must be once)`)
  }
  if (!e.title) problems.push(`${at}: no title`)
  for (const [what, key] of [
    ['title', e.title],
    ['help', e.help],
    ['context', e.context],
  ]) {
    if (key && !hasKey(key)) problems.push(`${at}: ${what} key ${key} is not in the English catalog`)
  }
}

if (problems.length > 0) {
  for (const p of problems) console.error(p)
  console.error(`check-search-index: ${problems.length} problem${problems.length === 1 ? '' : 's'}`)
  process.exit(1)
}
console.log(`check-search-index: ${entries.length} entries OK`)
