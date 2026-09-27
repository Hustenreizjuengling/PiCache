// The configuration sections of a partial restore and of a follower sync
// (docs/ARCHITECTURE.md 15, section map) and their dependency rule:
// clients-and-groups replaces the groups that lists and rules, local
// records and parental controls refer to, so choosing it chooses those too.
// The server checks the rule again (400 field "sections" / "sync.sections").

import { GROUP_LINKED_SECTIONS, type ConfigSection } from '$lib/api'

/** A section the rule requires while clients-and-groups is chosen. */
export function isGroupLinked(s: ConfigSection): boolean {
  return (GROUP_LINKED_SECTIONS as readonly ConfigSection[]).includes(s)
}

/**
 * The selection after checking or unchecking one section, in the order of
 * `all`: checking clients-and-groups adds the group-linked sections, and
 * unchecking one of those drops clients-and-groups too.
 */
export function toggleSection<S extends ConfigSection>(all: readonly S[], selected: readonly S[], section: S, on: boolean): S[] {
  const next = new Set<S>(selected)
  if (on) {
    next.add(section)
    if (section === 'clients-and-groups') for (const s of all) if (isGroupLinked(s)) next.add(s)
  } else {
    next.delete(section)
    if (isGroupLinked(section)) next.delete('clients-and-groups' as S)
  }
  return all.filter((s) => next.has(s))
}
