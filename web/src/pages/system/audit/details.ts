// Audit entry details are compact JSON written by the server (secrets
// already redacted, at most 4 KiB, truncated ones end with "…"). They are
// only ever shown as text.

/** Details indented for reading; text that is not valid JSON is returned as is. */
export function prettyDetails(details: string | undefined): string {
  if (!details) return ''
  try {
    return JSON.stringify(JSON.parse(details), null, 2)
  } catch {
    return details
  }
}

const MAX_SUMMARY = 160

/** One line for the table: `key: value, …` for flat objects, else the compact text. */
export function summarizeDetails(details: string | undefined): string {
  if (!details) return ''
  let out = details
  try {
    const v: unknown = JSON.parse(details)
    if (v && typeof v === 'object' && !Array.isArray(v)) {
      out = Object.entries(v as Record<string, unknown>)
        .map(([k, x]) => `${k}: ${typeof x === 'string' ? x : JSON.stringify(x)}`)
        .join(', ')
    }
  } catch {
    // not JSON (or truncated): keep the text
  }
  return out.length > MAX_SUMMARY ? out.slice(0, MAX_SUMMARY - 1) + '…' : out
}

/** Actions that record a failure (e.g. auth.login_failed). */
export function isFailure(action: string): boolean {
  return /(?:^|[._])failed$|_failure$/.test(action)
}
