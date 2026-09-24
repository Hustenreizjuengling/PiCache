// The HTTPS address of a page opened over plain HTTP.

/**
 * Returns `href` on https:// and the given port (the same host, path and
 * hash), or '' when the page is not plain HTTP or no HTTPS listener is bound
 * (port 0).
 */
export function httpsUrl(href: string, port: number): string {
  if (!Number.isInteger(port) || port <= 0 || port > 65535) return ''
  let u: URL
  try {
    u = new URL(href)
  } catch {
    return ''
  }
  if (u.protocol !== 'http:') return ''
  u.protocol = 'https:'
  u.port = port === 443 ? '' : String(port)
  return u.toString()
}
