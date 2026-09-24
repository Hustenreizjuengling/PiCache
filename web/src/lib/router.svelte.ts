// Hash router: URLs look like "#/dns/queries?domain=example.com&range=1h".
// The path selects the page (see src/routes.ts); the query string holds page
// state (filters, tabs, selected rows) so views can be linked and reloaded.

/** Values accepted by href()/setQuery(); null, undefined, '' and false remove the key. */
export type QueryValue = string | number | boolean | null | undefined | readonly (string | number)[]
export type QueryPatch = Record<string, QueryValue>

interface Location {
  path: string
  query: URLSearchParams
}

function parse(): Location {
  const raw = location.hash.replace(/^#/, '')
  const q = raw.indexOf('?')
  let path = q >= 0 ? raw.slice(0, q) : raw
  if (!path.startsWith('/')) path = '/' + path
  if (path.length > 1 && path.endsWith('/')) path = path.slice(0, -1)
  return { path, query: new URLSearchParams(q >= 0 ? raw.slice(q + 1) : '') }
}

let loc = $state<Location>(parse())
window.addEventListener('hashchange', () => {
  loc = parse()
})

function setParam(sp: URLSearchParams, k: string, v: QueryValue): void {
  sp.delete(k)
  if (v === null || v === undefined || v === '' || v === false) return
  if (Array.isArray(v)) {
    if (v.length > 0) sp.set(k, v.join(','))
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
  /** A comma-separated list parameter: ?status=a,b → ['a', 'b']. */
  list(name: string): string[] {
    const v = loc.query.get(name)
    return v ? v.split(',').filter(Boolean) : []
  },
  /** Navigates to another page (adds a history entry unless replace). */
  navigate(path: string, query?: QueryPatch, opts: { replace?: boolean } = {}): void {
    const target = href(path, query)
    if (opts.replace) {
      history.replaceState(history.state, '', target)
      loc = parse()
    } else {
      location.hash = target.slice(1)
    }
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
