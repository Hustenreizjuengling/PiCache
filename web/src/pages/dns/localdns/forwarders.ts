// Display helpers for conditional forwarders: their domains (with the
// single-label choice "(unqualified)" in words) and the "default upstreams"
// target.

import { t } from '$i18n/index.svelte'
import { DEFAULT_TARGET, UNQUALIFIED_DOMAIN, type Forwarder } from '$lib/api'

/** Every domain of a forwarder (older answers carry only `domain`). */
export function forwarderDomains(f: Forwarder): string[] {
  return f.domains?.length ? f.domains : [f.domain]
}

/** Whether the forwarder sends its domains to the default upstreams. */
export function usesDefault(f: Pick<Forwarder, 'upstreams'>): boolean {
  return f.upstreams.length === 1 && f.upstreams[0] === DEFAULT_TARGET
}

/** A domain as shown: "(unqualified)" becomes "single-label names". */
export function domainLabel(domain: string): string {
  return domain === UNQUALIFIED_DOMAIN ? t('dns.forwarders.unqualifiedShort') : domain
}

/** The domains of a forwarder in one line (titles, confirmations, toasts). */
export function forwarderTitle(f: Forwarder): string {
  return forwarderDomains(f).map(domainLabel).join(', ')
}
