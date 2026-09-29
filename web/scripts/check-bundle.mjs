#!/usr/bin/env node
// Bundle budget, run by `npm run build` after `vite build`: the entry chunk
// (the module script of internal/webui/dist/index.html) stays at most
// 880 000 bytes, and every language except English has a chunk
// assets/<id>-<hash>.js of at most 450 000 bytes (docs/TRANSLATING.md).

import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { readLocales } from './check-i18n.mjs'

const ENTRY_MAX = 880_000
const LANGUAGE_MAX = 450_000

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..')
const DIST = join(WEB, '..', 'internal', 'webui', 'dist')
const ASSETS = join(DIST, 'assets')

const problems = []
const report = []

const html = existsSync(join(DIST, 'index.html')) ? readFileSync(join(DIST, 'index.html'), 'utf8') : ''
const entry = /<script\b[^>]*\btype="module"[^>]*\bsrc="\.?\/?(assets\/[^"]+\.js)"/.exec(html)?.[1]
if (!entry) {
  problems.push('no module script in internal/webui/dist/index.html (run vite build first)')
} else {
  const size = statSync(join(DIST, entry)).size
  report.push(`${entry}: ${size} bytes (at most ${ENTRY_MAX})`)
  if (size > ENTRY_MAX) problems.push(`the entry chunk ${entry} has ${size} bytes, more than ${ENTRY_MAX}`)
}

const files = existsSync(ASSETS) ? readdirSync(ASSETS) : []
for (const { id } of readLocales()) {
  if (id === 'en') continue
  const re = new RegExp(`^${id.replace(/[\\^$.*+?()[\]{}|-]/g, '\\$&')}-[\\w-]{8}\\.js$`)
  const chunks = files.filter((f) => re.test(f))
  if (chunks.length !== 1) {
    problems.push(`language ${id}: expected one chunk assets/${id}-<hash>.js, found ${chunks.length}`)
    continue
  }
  const size = statSync(join(ASSETS, chunks[0])).size
  report.push(`assets/${chunks[0]}: ${size} bytes`)
  if (size > LANGUAGE_MAX) problems.push(`language chunk assets/${chunks[0]} has ${size} bytes, more than ${LANGUAGE_MAX}`)
}

if (problems.length > 0) {
  for (const p of problems) console.error(p)
  console.error(`check-bundle: ${problems.length} problem${problems.length === 1 ? '' : 's'}`)
  process.exit(1)
}
console.log(`check-bundle: OK\n  ${report.join('\n  ')}`)
