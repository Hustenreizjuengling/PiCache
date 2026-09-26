<!--
  @component
  Privacy level: Full, Hide domains, Anonymous, Off or Custom. A level sets
  the four switches (query log, anonymised client addresses, hidden domains,
  DNS statistics) in the draft; Custom shows the switches themselves. Saved
  with the page (PATCH /settings/logs sends the switches, never the level).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { LogsSettings, PrivacyLevel } from '$lib/api'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Checkbox, Panel } from '$lib/ui'
  import { applyPreset, LEVELS, levelOf } from './privacy'

  interface Props {
    form: SettingsForm<'logs'>
    /** "Custom" was chosen (the switches stay visible although they may match a preset). */
    custom: boolean
  }

  let { form, custom = $bindable() }: Props = $props()

  const d = $derived(form.draft as LogsSettings)
  const auto = $props.id()
  const chosen = $derived<PrivacyLevel>(custom ? 'custom' : levelOf(d))

  function pick(l: PrivacyLevel) {
    custom = l === 'custom'
    if (l !== 'custom') applyPreset(d, l)
  }
</script>

<Panel id="privacy" title={t('system.logs.level.title')} description={t('system.logs.level.description')}>
  <div class="stack">
    <div class="levels" role="radiogroup" aria-label={t('system.logs.level.title')}>
      {#each LEVELS as l (l)}
        <label class={['level', chosen === l && 'on']}>
          <input type="radio" name="privacy-{auto}" value={l} checked={chosen === l} onchange={() => pick(l)} />
          <span class="text">
            <span class="name">{t(`system.logs.level.${l}`)}</span>
            <span class="desc">{t(`system.logs.level.${l}.text`)}</span>
          </span>
        </label>
      {/each}
    </div>

    {#if chosen === 'custom'}
      <div class="switches stack-sm">
        <Checkbox bind:checked={d.queryLogEnabled} label={t('system.logs.queryLog')} description={t('system.logs.queryLogHelp')} />
        <Checkbox bind:checked={d.anonymizeClientIps} label={t('system.logs.anonymize')} description={t('system.logs.anonymizeHelp')} />
        <Checkbox bind:checked={d.hideDomains} label={t('system.logs.hideDomains')} description={t('system.logs.hideDomainsHelp')} />
        <Checkbox bind:checked={d.statsEnabled} label={t('system.logs.stats')} description={t('system.logs.statsHelp')} />
      </div>
    {/if}

    <p class="small muted">{t('system.logs.level.note')}</p>
  </div>
</Panel>

<style>
  .levels {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 170px), 1fr));
    gap: var(--sp-2);
  }
  .level {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    cursor: pointer;
  }
  .level:hover {
    background: var(--surface-2);
  }
  .level.on {
    border-color: var(--focus);
    box-shadow: inset 0 0 0 1px var(--focus);
    background: color-mix(in srgb, var(--focus) 6%, var(--surface));
  }
  .level:has(input:disabled) {
    cursor: default;
  }
  .level:has(input:focus-visible) {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  input {
    flex: none;
    width: 16px;
    height: 16px;
    margin: 2px 0 0;
    accent-color: var(--focus);
  }
  input:focus-visible {
    outline: none;
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .name {
    font-weight: 600;
  }
  .desc {
    color: var(--text-2);
    font-size: var(--fs-sm);
    line-height: 1.4;
  }
  .switches {
    padding: var(--sp-3) var(--sp-4);
    border-left: 3px solid var(--line-strong);
  }
</style>
