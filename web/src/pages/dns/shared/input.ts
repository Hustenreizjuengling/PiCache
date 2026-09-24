// Input helpers for the DNS pages. The server validates everything again and
// reports the offending field; these helpers only normalise what people type
// and give early feedback for obviously incomplete input.

/** Splits multi-line text into trimmed, non-empty, unique lines (one entry per line). */
export function lines(text: string): string[] {
  const out: string[] = []
  for (const raw of text.split(/\r?\n/)) {
    const s = raw.trim()
    if (s && !out.includes(s)) out.push(s)
  }
  return out
}

/** Order-sensitive equality of two string lists. */
export function sameList(a: readonly string[], b: readonly string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

/**
 * The lower-case A-label form of a domain typed by a person: trims, removes
 * trailing dots and converts internationalised names with the browser's IDNA
 * implementation ("Bücher.de." → "xn--bcher-kva.de"). A leading "*." is kept.
 * Invalid input is returned trimmed so the server can explain what is wrong.
 */
export function asciiDomain(input: string): string {
  let s = input.trim().toLowerCase().replace(/\.+$/, '')
  let prefix = ''
  if (s.startsWith('*.')) {
    prefix = '*.'
    s = s.slice(2)
  }
  if (/[^\x00-\x7f]/.test(s)) {
    try {
      s = new URL(`http://${s}/`).hostname
    } catch {
      /* not a host name: the server reports it */
    }
  }
  return prefix + s
}

/** Whether s is a dotted-quad IPv4 address. */
export function isIPv4(s: string): boolean {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(s)
  return !!m && m.slice(1).every((p) => Number(p) <= 255)
}

/** Whether s is an IPv6 address (without zone). */
export function isIPv6(s: string): boolean {
  if (!s.includes(':') || !/^[0-9a-f:.]+$/i.test(s)) return false
  try {
    new URL(`http://[${s}]/`)
    return true
  } catch {
    return false
  }
}

/** Whether s is an IPv4 or IPv6 address. */
export function isIP(s: string): boolean {
  return isIPv4(s) || isIPv6(s)
}
