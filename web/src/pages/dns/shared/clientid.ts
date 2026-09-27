// ClientIDs: the device ID a DoT or DoH client sends (the TLS name
// <ClientID>.<server name> or the path /dns-query/<ClientID>). One DNS label,
// compared in lower case; clients and blocked clients hold them as
// "clientid:<ClientID>". Clients may also be identified by an interface of
// this machine ("iface:<name>": every source address in its networks) or by
// a device's own host name ("host:<name>", spoofable). The server checks
// everything again.

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

/** Prefix of interface identifiers (the name as the kernel spells it). */
export const IFACE_PREFIX = 'iface:'

/** Prefix of host-name identifiers (stored lower-case, without a trailing dot). */
export const HOST_PREFIX = 'host:'

function withPrefix(identifier: string, prefix: string): string | undefined {
  return identifier.slice(0, prefix.length).toLowerCase() === prefix ? identifier.slice(prefix.length) : undefined
}

/** The interface of an "iface:<name>" identifier (prefix in any case), else undefined. */
export function ifaceOf(identifier: string): string | undefined {
  return withPrefix(identifier.trim(), IFACE_PREFIX)
}

/** The host name of a "host:<name>" identifier (prefix in any case), lower-cased without a trailing dot, else undefined. */
export function hostOf(identifier: string): string | undefined {
  return withPrefix(identifier.trim(), HOST_PREFIX)?.toLowerCase().replace(/\.$/, '')
}

/**
 * An identifier or blocked-client entry for display: "ClientID tims-ipad",
 * "Interface wg0" and "Host name tv" for the prefixed kinds.
 */
export function identifierText(identifier: string): string {
  const id = clientIdOf(identifier)
  if (id !== undefined) return t('dns.clients.clientIdIdentifier', { id })
  const iface = ifaceOf(identifier)
  if (iface !== undefined) return t('dns.clients.ifaceIdentifier', { name: iface })
  const host = hostOf(identifier)
  if (host !== undefined) return t('dns.clients.hostIdentifier', { name: host })
  return identifier
}
