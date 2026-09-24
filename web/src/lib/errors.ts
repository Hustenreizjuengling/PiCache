// User-facing error text. Server messages (English) are shown as they are,
// transport problems and bare status codes get translated explanations.

import { t } from '../i18n/index.svelte'
import { toApiError, type ApiError } from './api/client'

function sentence(s: string): string {
  const trimmed = s.trim()
  return trimmed ? trimmed[0].toUpperCase() + trimmed.slice(1) : trimmed
}

/** A message for an error thrown by an API call (or anything else). */
export function errorText(err: unknown): string {
  const e: ApiError = toApiError(err)
  switch (e.code) {
    case 'network':
      return t('common.error.network')
    case 'aborted':
      return t('common.error.aborted')
    case 'unauthorized':
      return e.field ? sentence(e.message) : t('common.error.unauthorized')
    case 'too_many_requests':
      return t('common.error.tooMany')
    case 'misdirected':
      return t('common.error.misdirected')
    case 'internal':
      return t('common.error.internal')
  }
  if (e.code === 'forbidden' && /cross-origin/i.test(e.message)) return t('common.error.crossOrigin')
  if (!e.message || /^HTTP \d+$/.test(e.message)) {
    switch (e.code) {
      case 'forbidden':
        return t('common.error.forbidden')
      case 'not_found':
        return t('common.error.notFound')
      case 'unavailable':
        return t('common.error.unavailable')
      case 'conflict':
        return t('common.error.conflict')
      default:
        return t('common.error.generic')
    }
  }
  return sentence(e.message)
}

/**
 * The validation message for one form field, if the error names it.
 * `field` may be a prefix: fieldError(err, 'dns.upstreams') matches "dns.upstreams[1]".
 */
export function fieldError(err: unknown, field: string): string | undefined {
  if (!err) return undefined
  const e = toApiError(err)
  if (!e.field) return undefined
  if (e.field === field || e.field.startsWith(field + '.') || e.field.startsWith(field + '[')) return sentence(e.message)
  return undefined
}
