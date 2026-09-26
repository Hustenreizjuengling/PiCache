// The DNS resolver of groups and clients (upstreams per group): a family
// resolver preset or the group's own upstreams instead of the default
// upstreams. Among a client's enabled groups a preset wins, else own
// upstreams; among equals the lowest group id (ARCHITECTURE 7.4).

import { t, tn, type MessageKey } from '$i18n/index.svelte'
import type { ClientGroup, UpstreamPreset } from '$lib/api'

type Resolver = Pick<ClientGroup, 'upstreams' | 'upstreamPreset'>

/** The group names a resolver of its own. */
export function hasResolver(g: Resolver): boolean {
  return !!g.upstreamPreset || g.upstreams.length > 0
}

/** The labels of the known presets (the server's table is English). */
const PRESET_LABELS: Readonly<Record<string, MessageKey>> = {
  'cloudflare-family': 'dns.resolver.preset.cloudflareFamily',
  'opendns-familyshield': 'dns.resolver.preset.opendnsFamilyshield',
  'cleanbrowsing-family': 'dns.resolver.preset.cleanbrowsingFamily',
}

/** The name of a preset in the UI language; the server's name for a key the UI does not know. */
export function presetName(key: string, presets: readonly UpstreamPreset[] | undefined): string {
  const label = PRESET_LABELS[key]
  return label ? t(label) : (presets?.find((p) => p.key === key)?.name ?? key)
}

/** "Cloudflare for Families", "Own upstreams (2)" or "Default upstreams". */
export function resolverText(g: Resolver, presets: readonly UpstreamPreset[] | undefined): string {
  if (g.upstreamPreset) return presetName(g.upstreamPreset, presets)
  if (g.upstreams.length > 0) return tn('dns.resolver.own', g.upstreams.length)
  return t('dns.resolver.default')
}

/** Identifies what a group resolves with (groups with the same key share one upstream list). */
function resolverKey(g: Resolver): string {
  return g.upstreamPreset ? `preset:${g.upstreamPreset}` : `own:${g.upstreams.join('\n')}`
}

/** The enabled groups of `ids` that name a resolver, in the order of precedence (the first one applies). */
export function resolverGroups(ids: readonly number[], groups: readonly ClientGroup[] | undefined): ClientGroup[] {
  return (groups ?? [])
    .filter((g) => ids.includes(g.id) && g.enabled && hasResolver(g))
    .sort((a, b) => Number(!!b.upstreamPreset) - Number(!!a.upstreamPreset) || a.id - b.id)
}

/** The groups name different resolvers (only the first one applies). */
export function resolversDiffer(list: readonly ClientGroup[]): boolean {
  return new Set(list.map(resolverKey)).size > 1
}
