// Hash router: URLs look like "#/dns/queries?domain=example.com&range=1h".
// The path selects the page (see src/routes.ts); the query string holds page
// state (filters, tabs, selected rows) so views can be linked and reloaded.

/**
 * Values accepted by href()/setQuery(); null, undefined, '' and false remove
 * the key; an array repeats it (?status=a&status=b, like the API client).
 */
export type QueryValue = string | number | boolean | null | undefined | readonly (string | number)[]
export type QueryPatch = Record<string, QueryValue>

interface Location {
  path: string
  query: URLSearchParams
}

function parse(hash: string = location.hash): Location {
  const raw = hash.replace(/^#/, '')
  const q = raw.indexOf('?')
  let path = q >= 0 ? raw.slice(0, q) : raw
  if (!path.startsWith('/')) path = '/' + path
  if (path.length > 1 && path.endsWith('/')) path = path.slice(0, -1)
  return { path, query: new URLSearchParams(q >= 0 ? raw.slice(q + 1) : '') }
}

let loc = $state<Location>(parse())

// ---- leaving a page with unsaved edits
//
// Pages with unsaved edits register a check (guardLeave). A change of the
// path then asks first (setLeavePrompt; Shell shows a confirm dialog), for
// links, router.navigate() and Back/Forward alike; query changes stay on the
// page and are never asked about. A hash navigation has already happened
// when hashchange fires, so it is undone with history.go() (entries are
// numbered in history.state to know the direction) and redone if confirmed.

type LeaveCheck = () => boolean

const leaveChecks = new Set<LeaveCheck>()
let leavePrompt: () => Promise<boolean> = () => Promise.resolve(window.confirm('Discard unsaved changes?'))
let prompting = false
/** The next hashchange is our own undo (ignored) or a confirmed navigation (allowed). */
let expect: 'undo' | 'leave' | null = null

const IDX = 'picacheIdx'

function entryIndex(): number | undefined {
  const s: unknown = history.state
  const v = s && typeof s === 'object' ? (s as Record<string, unknown>)[IDX] : undefined
  return typeof v === 'number' ? v : undefined
}

let lastIndex = entryIndex() ?? 0

/** Numbers the current history entry (if new) and returns its number. */
function stampEntry(): number {
  let i = entryIndex()
  if (i === undefined) {
    i = ++lastIndex
    const s: unknown = history.state
    history.replaceState({ ...(s && typeof s === 'object' ? s : {}), [IDX]: i }, '')
  } else {
    lastIndex = Math.max(lastIndex, i)
  }
  return i
}

let current = stampEntry()

function unsaved(): boolean {
  for (const check of leaveChecks) if (check()) return true
  return false
}

/** Whether the page may be left (asks when there are unsaved edits). */
async function mayLeave(): Promise<boolean> {
  if (!unsaved()) return true
  if (prompting) return false
  prompting = true
  try {
    return await leavePrompt()
  } finally {
    prompting = false
  }
}

window.addEventListener('hashchange', (e) => {
  const next = parse()
  const kind = expect
  expect = null
  if (kind === 'undo') {
    current = stampEntry()
    return
  }
  const idx = entryIndex()
  const steps = idx === undefined ? 1 : idx - current // a new entry (link) or Back/Forward
  if (kind !== 'leave' && next.path !== loc.path && steps !== 0 && unsaved()) {
    const target = e.newURL
    expect = 'undo'
    history.go(-steps)
    void mayLeave().then((ok) => {
      if (!ok) return
      expect = 'leave'
      if (idx === undefined) location.hash = new URL(target).hash
      else history.go(steps)
    })
    return
  }
  current = stampEntry()
  loc = next
})

window.addEventListener('beforeunload', (e) => {
  if (unsaved()) e.preventDefault()
})

/**
 * Asks before the page is left while check() reports unsaved edits (also
 * on reload/close). Returns the unregister function; call it from an $effect:
 *   $effect(() => guardLeave(() => form.dirty))
 */
export function guardLeave(check: LeaveCheck): () => void {
  leaveChecks.add(check)
  return () => {
    leaveChecks.delete(check)
  }
}

/** Sets how to ask (resolves true to discard the edits and leave). */
export function setLeavePrompt(prompt: () => Promise<boolean>): void {
  leavePrompt = prompt
}

function setParam(sp: URLSearchParams, k: string, v: QueryValue): void {
  sp.delete(k)
  if (v === null || v === undefined || v === '' || v === false) return
  if (Array.isArray(v)) {
    for (const item of v) if (item !== '') sp.append(k, String(item))
  } else {
    sp.set(k, String(v))
  }
}

/** Builds a link: href('/dns/queries', { domain: 'example.com' }) → '#/dns/queries?domain=example.com'. */
export function href(path: string, query?: QueryPatch): string {
  const sp = new URLSearchParams()
  if (query) for (const [k, v] of Object.entries(query)) setParam(sp, k, v)
  const qs = sp.toString()
  return '#' + path + (qs ? '?' + qs : '')
}

/** Reactive router state and navigation. */
export const router = {
  /** Current path, e.g. "/dns/queries" (no trailing slash). */
  get path(): string {
    return loc.path
  },
  /** Current query parameters (read-only; change them with setQuery). */
  get query(): URLSearchParams {
    return loc.query
  },
  /** One query parameter ('' when absent). */
  param(name: string): string {
    return loc.query.get(name) ?? ''
  },
  /** A list parameter, repeated or comma-separated: ?status=a&status=b or ?status=a,b → ['a', 'b']. */
  list(name: string): string[] {
    return loc.query.getAll(name).flatMap((v) => v.split(',')).filter(Boolean)
  },
  /** Every value of a repeated parameter, not split at commas: ?client=a&client=b → ['a', 'b']. */
  all(name: string): string[] {
    return loc.query.getAll(name)
  },
  /** Navigates to another page (adds a history entry unless replace); asks first if there are unsaved edits. */
  navigate(path: string, query?: QueryPatch, opts: { replace?: boolean } = {}): void {
    const target = href(path, query)
    const go = () => {
      if (opts.replace) {
        history.replaceState(history.state, '', target)
        loc = parse()
      } else if (target !== location.hash) {
        expect = 'leave'
        location.hash = target.slice(1)
      }
    }
    if (parse(target).path === loc.path || !unsaved()) return go()
    void mayLeave().then((ok) => ok && go())
  },
  /**
   * Merges patch into the current query string. By default the history entry
   * is replaced (filters typed into a search box do not flood Back); pass
   * { push: true } for navigational changes such as opening a detail view.
   */
  setQuery(patch: QueryPatch, opts: { push?: boolean } = {}): void {
    const sp = new URLSearchParams(loc.query)
    for (const [k, v] of Object.entries(patch)) setParam(sp, k, v)
    const qs = sp.toString()
    const target = '#' + loc.path + (qs ? '?' + qs : '')
    if (target === location.hash || (target === '#/' && location.hash === '')) return
    if (opts.push) {
      location.hash = target.slice(1)
    } else {
      history.replaceState(history.state, '', target)
      loc = parse()
    }
  },
}

/**
 * Matches a route pattern against a path. Segments starting with ':' capture
 * a parameter; a trailing '/*' captures the rest (possibly empty) as '*'.
 *   matchRoute('/dns/filtering/*', '/dns/filtering/rules') → { '*': 'rules' }
 *   matchRoute('/cache/library/:service', '/cache/library/steam') → { service: 'steam' }
 */
export function matchRoute(pattern: string, path: string): Record<string, string> | null {
  const wildcard = pattern.endsWith('/*')
  const pat = (wildcard ? pattern.slice(0, -2) : pattern).split('/')
  const segs = path.split('/')
  if (wildcard ? segs.length < pat.length : segs.length !== pat.length) return null
  const params: Record<string, string> = {}
  for (let i = 0; i < pat.length; i++) {
    const p = pat[i]
    if (p.startsWith(':')) {
      try {
        params[p.slice(1)] = decodeURIComponent(segs[i])
      } catch {
        return null
      }
    } else if (p !== segs[i]) {
      return null
    }
  }
  if (wildcard) params['*'] = segs.slice(pat.length).join('/')
  return params
}
