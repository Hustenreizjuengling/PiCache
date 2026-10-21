<!--
  @component
  Which Steam depots a job downloads: the platforms and the languages, like
  the settings of a Steam client. Saved with the other settings.
-->
<script lang="ts">
  import { i18n, t } from '$i18n/index.svelte'
  import { api, type PrefillOS, type PrefillSettings, type PrefillStatus } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Button, Checkbox, Panel, toast } from '$lib/ui'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  let draft = $state<PrefillSettings | undefined>(undefined)
  const saved = $derived(status.settings)
  const s = $derived(draft ?? saved)
  const dirty = $derived(draft !== undefined && JSON.stringify(draft) !== JSON.stringify(saved))
  const readOnly = $derived(!session.isAdmin)

  function edit(f: (d: PrefillSettings) => void) {
    const d: PrefillSettings = structuredClone($state.snapshot(draft ?? saved))
    f(d)
    draft = d
  }


  const OS: PrefillOS[] = ['windows', 'macos', 'linux']

  // Steam's language names and their BCP 47 tags (for the display names).
  const LANGUAGES: Record<string, string> = {
    arabic: 'ar', brazilian: 'pt-BR', bulgarian: 'bg', czech: 'cs', danish: 'da', dutch: 'nl', english: 'en',
    finnish: 'fi', french: 'fr', german: 'de', greek: 'el', hungarian: 'hu', indonesian: 'id', italian: 'it',
    japanese: 'ja', koreana: 'ko', latam: 'es-419', norwegian: 'no', polish: 'pl', portuguese: 'pt-PT',
    romanian: 'ro', russian: 'ru', schinese: 'zh-Hans', spanish: 'es-ES', swedish: 'sv', tchinese: 'zh-Hant',
    thai: 'th', turkish: 'tr', ukrainian: 'uk', vietnamese: 'vi',
  }
  const languageNames = $derived(new Intl.DisplayNames([i18n.tag], { type: 'language' }))
  const languages = $derived(
    Object.entries(LANGUAGES)
      .map(([id, tag]) => ({ id, label: languageNames.of(tag) ?? id }))
      .sort((a, b) => a.label.localeCompare(b.label, i18n.tag)),
  )

  function toggle<T>(list: T[], v: T, on: boolean): T[] {
    return on ? [...new Set([...list, v])] : list.filter((x) => x !== v)
  }

  let saving = $state(false)
  async function save() {
    if (!draft) return
    saving = true
    try {
      await api.prefill.saveSettings(draft)
      draft = undefined
      toast.success(t('cache.prefill.settings.saved'))
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      saving = false
    }
  }
</script>

<Panel title={t('cache.prefill.steamOptions.title')} description={t('cache.prefill.steamOptions.description')}>
  <div class="stack">
    <fieldset class="checks">
      <legend>{t('cache.prefill.settings.os')}</legend>
      {#each OS as os (os)}
        <Checkbox
          label={t(`cache.prefill.settings.osName.${os}`)}
          checked={s.steam.os.includes(os)}
          disabled={readOnly}
          onchange={(v) => edit((d) => (d.steam.os = toggle(d.steam.os, os, v)))}
        />
      {/each}
    </fieldset>
    <fieldset class="checks languages">
      <legend>{t('cache.prefill.settings.languages')}</legend>
      <p class="muted small hint">{t('cache.prefill.settings.languagesHelp')}</p>
      {#each languages as lang (lang.id)}
        <Checkbox
          label={lang.label}
          checked={s.steam.languages.includes(lang.id)}
          disabled={readOnly}
          onchange={(v) => edit((d) => (d.steam.languages = toggle(d.steam.languages, lang.id, v)))}
        />
      {/each}
    </fieldset>
  </div>
  {#if dirty}
    <div class="save">
      <Button variant="ghost" onclick={() => (draft = undefined)}>{t('cache.prefill.settings.discard')}</Button>
      <Button variant="primary" icon="check" loading={saving} onclick={save}>{t('cache.prefill.settings.save')}</Button>
    </div>
  {/if}
</Panel>

<style>
  .checks {
    border: 0;
    margin: 0;
    padding: 0;
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2) var(--sp-5);
  }
  .checks legend {
    font-weight: 600;
    margin-bottom: var(--sp-2);
    padding: 0;
  }
  .hint {
    grid-column: 1 / -1;
    margin: 0;
  }
  .languages {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
  }
  .save {
    display: flex;
    justify-content: flex-end;
    gap: var(--sp-2);
    margin-top: var(--sp-4);
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
</style>
