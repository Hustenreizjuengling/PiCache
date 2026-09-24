// Validation messages for list fields edited one entry per line: the server
// names the entry ("identifiers[2]"), the UI points at the line.

import { t } from '$i18n/index.svelte'
import { toApiError } from '$lib/api'
import { fieldError } from '$lib/errors'

/** fieldError for a one-per-line list: "Line 3: must be an IP address …". */
export function lineError(err: unknown, field: string): string | undefined {
  const msg = fieldError(err, field)
  if (!msg || !err) return msg
  const m = /\[(\d+)\]/.exec(toApiError(err).field?.slice(field.length) ?? '')
  return m ? t('dns.shared.lineError', { line: Number(m[1]) + 1, message: msg }) : msg
}
