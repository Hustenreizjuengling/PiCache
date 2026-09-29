/**
 * A translation of an English dictionary: exactly the same keys (flat,
 * dot-separated), every value a string. `const de: Messages<typeof en> = {…}`
 * makes svelte-check report missing and unknown keys.
 */
export type Messages<T> = { readonly [K in keyof T]: string }

/** The plural bases of a dictionary: the keys ending in '.one', without the suffix. */
export type PluralBase<T> = { [K in keyof T]: K extends `${infer B}.one` ? B : never }[keyof T]

/** Plural categories besides 'one' and 'other' (Intl.PluralRules), e.g. 'few' and 'many' in Polish. */
export type ExtraPluralCategory = 'zero' | 'two' | 'few' | 'many'

/**
 * A translation into a language whose plural rules have more categories
 * than English: the keys of Messages<T> plus '<base>.few' and the like for
 * every plural base (web/scripts/check-i18n.mjs checks which ones a
 * language needs).
 */
export type Translation<T> = Messages<T> & {
  readonly [K in `${PluralBase<T> & string}.${ExtraPluralCategory}`]?: string
}

/** Interpolation values for `{name}` placeholders. Numbers are formatted for the locale. */
export type Params = Record<string, string | number>
