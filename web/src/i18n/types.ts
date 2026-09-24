/**
 * A translation of an English dictionary: exactly the same keys (flat,
 * dot-separated), every value a string. `const de: Messages<typeof en> = {…}`
 * makes svelte-check report missing and unknown keys.
 */
export type Messages<T> = { readonly [K in keyof T]: string }

/** Interpolation values for `{name}` placeholders. Numbers are formatted for the locale. */
export type Params = Record<string, string | number>
