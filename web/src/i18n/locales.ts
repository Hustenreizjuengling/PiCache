// The languages of the web UI (docs/TRANSLATING.md), in the order the
// pickers list them: id, name in its own language and the default BCP 47 tag
// for the Intl formatters. One entry per line: internal/settings.Languages
// holds the same ids in the same order (a Go test compares them).

export const LOCALES = [
  { id: 'en', label: 'English', tag: 'en-US' },
  { id: 'de', label: 'Deutsch', tag: 'de-DE' },
] as const

/** A language id: 'en' or 'de'. */
export type Locale = (typeof LOCALES)[number]['id']
