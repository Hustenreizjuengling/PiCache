// ClientIDs: the device ID a DoT or DoH client sends (the TLS name
// <ClientID>.<server name> or the path /dns-query/<ClientID>). One DNS label,
// compared in lower case; clients and blocked clients hold them as
// "clientid:<ClientID>". The server checks everything again.

import { t } from '$i18n/index.svelte'

/** Prefix of ClientID identifiers and blocked-client entries. */
export const CLIENT_ID_PREFIX = 'clientid:'

const CLIENT_ID = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

/** The stored form of a typed ClientID: trimmed and lower-case (never refused for its case). */
export function normalizeClientId(s: string): string {
  return s.trim().toLowerCase()
}

/** Whether s (normalised) is a valid ClientID: 1–63 letters, digits and hyphens, not starting or ending with a hyphen. */
export function isClientId(s: string): boolean {
  return CLIENT_ID.test(s)
}

/** The ClientID of a "clientid:<ClientID>" identifier or entry (prefix in any case), else undefined. */
export function clientIdOf(identifier: string): string | undefined {
  return identifier.slice(0, CLIENT_ID_PREFIX.length).toLowerCase() === CLIENT_ID_PREFIX
    ? identifier.slice(CLIENT_ID_PREFIX.length)
    : undefined
}

/** An identifier or blocked-client entry for display: "ClientID tims-ipad" for clientid: entries. */
export function identifierText(identifier: string): string {
  const id = clientIdOf(identifier)
  return id === undefined ? identifier : t('dns.clients.clientIdIdentifier', { id })
}
