// Editing one settings section (DNS settings, cache settings, …): loads the
// section and its defaults, keeps an editable draft and saves only the members
// that changed. Sending only changes matters: the filter section also holds the
// blocking pause, which the top bar changes independently.
//
//   const form = settingsForm('dns')
//   {#if form.draft}
//     <Field label="Upstream mode" error={form.error('upstreamMode')}>
//       <Select bind:value={form.draft.upstreamMode} options={…} />
//     </Field>
//     <Button variant="primary" disabled={!form.dirty} loading={form.saving}
//             onclick={async () => (await form.save()) && toast.success(t('common.state.saved'))}>…</Button>
//   {/if}

import { api, toApiError, type ApiError, type Settings, type SettingsSection } from './api'
import { errorText, fieldError } from './errors'
import { guardLeave } from './router.svelte'
import { appStatus } from './status.svelte'

function clone<T>(v: T): T {
  return structuredClone($state.snapshot(v)) as T
}

function same(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b)
}

/** Reactive editor for one settings section. */
export class SettingsForm<S extends SettingsSection> {
  readonly section: S
  /** Values as saved on the server. */
  saved = $state.raw<Settings[S] | undefined>(undefined)
  /** Editable copy: bind inputs to its members. Undefined until loaded. */
  draft = $state<Settings[S] | undefined>(undefined)
  /** Built-in defaults of the section (for "Reset to default"). */
  defaults = $state.raw<Settings[S] | undefined>(undefined)
  loading = $state(false)
  saving = $state(false)
  /** Loading failed. */
  loadError = $state.raw<ApiError | undefined>(undefined)
  /** The last save failed; `field` names the member (e.g. "dns.upstreams[1]"). */
  saveError = $state.raw<ApiError | undefined>(undefined)

  constructor(section: S) {
    this.section = section
  }

  /** Members that differ from the saved values (what save() sends). */
  get changes(): Partial<Settings[S]> {
    const out: Partial<Settings[S]> = {}
    const d = this.draft
    const s = this.saved
    if (!d || !s) return out
    for (const k of Object.keys(d) as (keyof Settings[S])[]) {
      if (!same(d[k], s[k])) out[k] = clone(d[k])
    }
    return out
  }

  /** There are unsaved edits. */
  get dirty(): boolean {
    return Object.keys(this.changes).length > 0
  }

  /** User-facing text of the last save error (field errors included). */
  get errorMessage(): string | undefined {
    return this.saveError ? errorText(this.saveError) : undefined
  }

  /** Validation message for a member: error('upstreams') also matches "dns.upstreams[1]". */
  error(member: string): string | undefined {
    return fieldError(this.saveError, `${this.section}.${member}`)
  }

  /** (Re)loads the section and its defaults, discarding unsaved edits. */
  async load(signal?: AbortSignal): Promise<void> {
    this.loading = true
    try {
      const [all, defs] = await Promise.all([api.settings.get({ signal }), api.settings.defaults({ signal })])
      this.defaults = defs[this.section]
      this.#apply(all[this.section])
      this.loadError = undefined
    } catch (err) {
      const e = toApiError(err)
      if (e.code !== 'aborted') this.loadError = e
    } finally {
      this.loading = false
    }
  }

  /** Saves the changed members (PATCH /settings/{section}); true on success or when nothing changed. */
  async save(): Promise<boolean> {
    const changes = this.changes
    if (Object.keys(changes).length === 0) return true
    this.saving = true
    this.saveError = undefined
    try {
      const all = await api.settings.patch(this.section, changes)
      this.#apply(all[this.section])
      void appStatus.overview.refresh() // LanCache, blocking and store state in the top bar
      return true
    } catch (err) {
      this.saveError = toApiError(err)
      return false
    } finally {
      this.saving = false
    }
  }

  /** Discards unsaved edits. */
  revert(): void {
    if (this.saved) this.draft = clone(this.saved)
    this.saveError = undefined
  }

  /** Puts the default value of one member into the draft (saved with save()). */
  resetToDefault<K extends keyof Settings[S]>(key: K): void {
    if (this.draft && this.defaults) this.draft[key] = clone(this.defaults[key])
  }

  /** Whether a member differs from its default. */
  isDefault<K extends keyof Settings[S]>(key: K): boolean {
    return !!this.draft && !!this.defaults && same(this.draft[key], this.defaults[key])
  }

  #apply(v: Settings[S]): void {
    this.saved = v
    this.draft = clone(v)
  }
}

/**
 * Creates a SettingsForm bound to the calling component: it loads when the
 * component mounts (aborted on destroy), and leaving the page (in-app
 * navigation, reload, closing the tab) asks first while it has unsaved
 * edits. Call during component initialisation.
 */
export function settingsForm<S extends SettingsSection>(section: S): SettingsForm<S> {
  const form = new SettingsForm(section)
  $effect(() => {
    const ctrl = new AbortController()
    void form.load(ctrl.signal)
    return () => ctrl.abort()
  })
  $effect(() => guardLeave(() => form.dirty))
  return form
}
