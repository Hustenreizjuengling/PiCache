<!--
  @component
  Edits one schedule inside the restrictions panel: name, days (toggle chips
  Monday to Sunday plus presets), from/to (an end before the start ends the
  next day), what to block (all internet or selected services) and whether
  it is used. The parent owns the draft and checks it when Done is pressed
  or the panel is saved.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, ParentalSchedule, ParentalService } from '$lib/api'
  import { Button, Checkbox, Field, Icon, Input } from '$lib/ui'
  import { formatClock } from '../../system/backup/schedule'
  import { dayName, isOvernight, NAME_MAX, normalizeDays, PRESETS, sameDays, scheduleProblems, WEEK, type PresetId } from './plan'
  import ServicePicker from './ServicePicker.svelte'

  type FieldName = 'name' | 'days' | 'start' | 'end' | 'block' | 'services'

  interface Props {
    draft: ParentalSchedule
    /** Show the client-side problems (after Done or Save was pressed). */
    touched?: boolean
    /** A validation error of the server for this schedule. */
    serverError?: { field: FieldName; message: string }
    isNew?: boolean
    catalog: readonly ParentalService[] | undefined
    catalogError?: ApiError
    onretry?: () => void
    disabled?: boolean
    ondone: () => void
    oncancel: () => void
  }

  let {
    draft = $bindable(),
    touched = false,
    serverError,
    isNew = false,
    catalog,
    catalogError,
    onretry,
    disabled = false,
    ondone,
    oncancel,
  }: Props = $props()

  const auto = $props.id()
  const problems = $derived(touched ? scheduleProblems(draft) : {})

  function err(f: FieldName): string | undefined {
    return (problems as Partial<Record<FieldName, string>>)[f] ?? (serverError?.field === f ? serverError.message : undefined)
  }

  function toggleDay(day: number) {
    draft.days = normalizeDays(draft.days.includes(day) ? draft.days.filter((d) => d !== day) : [...draft.days, day])
  }

  function preset(days: readonly number[]) {
    draft.days = normalizeDays(days)
  }

  function presetLabel(id: PresetId): string {
    return t(`dns.parental.preset.${id}`)
  }

  function setBlock(b: 'all' | 'services') {
    draft.block = b
    if (b === 'all') draft.services = []
  }

  const overnight = $derived(isOvernight(draft))
</script>

<div class="editor" role="group" aria-labelledby="se-{auto}">
  <h4 id="se-{auto}">{isNew ? t('dns.parental.schedule.newTitle') : t('dns.parental.schedule.editTitle', { name: draft.name || '…' })}</h4>

  <Field label={t('dns.parental.schedule.name')} required error={err('name')}>
    <Input bind:value={draft.name} maxlength={NAME_MAX} placeholder={t('dns.parental.schedule.namePlaceholder')} autocomplete="off" {disabled} />
  </Field>

  <fieldset class="days" aria-describedby={err('days') ? `se-${auto}-days-err` : undefined}>
    <legend>{t('dns.parental.schedule.days')}</legend>
    <div class="chips">
      {#each WEEK as day (day)}
        <button
          type="button"
          class="day"
          aria-pressed={draft.days.includes(day)}
          title={dayName(day, 'long')}
          {disabled}
          onclick={() => toggleDay(day)}
        >
          {dayName(day)}
        </button>
      {/each}
    </div>
    <div class="presets">
      <span class="xsmall muted">{t('dns.parental.schedule.presets')}</span>
      {#each PRESETS as p (p.id)}
        <button type="button" class="preset" aria-pressed={sameDays(draft.days, p.days)} {disabled} onclick={() => preset(p.days)}>
          {presetLabel(p.id)}
        </button>
      {/each}
    </div>
    {#if err('days')}
      <p class="error" id="se-{auto}-days-err"><Icon name="alert" size={16} />{err('days')}</p>
    {/if}
  </fieldset>

  <div class="times">
    <Field label={t('dns.parental.schedule.from')} error={err('start')}>
      <Input type="time" bind:value={draft.start} step={60} required {disabled} />
    </Field>
    <Field
      label={t('dns.parental.schedule.to')}
      error={err('end')}
      help={overnight ? t('dns.parental.schedule.overnightHelp', { end: formatClock(draft.end) }) : undefined}
    >
      <Input type="time" bind:value={draft.end} step={60} required {disabled} />
    </Field>
  </div>

  <fieldset class="block">
    <legend>{t('dns.parental.schedule.block')}</legend>
    <label class="radio">
      <input type="radio" name="block-{auto}" checked={draft.block === 'all'} {disabled} onchange={() => setBlock('all')} />
      <span class="text">
        <span>{t('dns.parental.schedule.blockAll')}</span>
        <span class="desc">{t('dns.parental.schedule.blockAllHelp')}</span>
      </span>
    </label>
    <label class="radio">
      <input type="radio" name="block-{auto}" checked={draft.block === 'services'} {disabled} onchange={() => setBlock('services')} />
      <span class="text">
        <span>{t('dns.parental.schedule.blockServices')}</span>
        <span class="desc">{t('dns.parental.schedule.blockServicesHelp')}</span>
      </span>
    </label>
    {#if err('block')}<p class="error"><Icon name="alert" size={16} />{err('block')}</p>{/if}
  </fieldset>

  {#if draft.block === 'services'}
    <ServicePicker
      {catalog}
      {catalogError}
      {onretry}
      bind:value={draft.services}
      label={t('dns.parental.schedule.blockServices')}
      error={err('services')}
      {disabled}
    />
  {/if}

  <Checkbox bind:checked={draft.enabled} label={t('dns.parental.schedule.enabled')} {disabled} />

  <div class="row">
    <Button size="sm" icon="check" {disabled} onclick={ondone}>{t('dns.parental.schedule.done')}</Button>
    <Button size="sm" variant="ghost" onclick={oncancel}>{t('common.action.cancel')}</Button>
  </div>
</div>

<style>
  .editor {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    padding: var(--sp-4);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
  }
  h4 {
    font-size: var(--fs-md);
  }
  fieldset {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  legend {
    padding: 0;
    margin-bottom: 6px;
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .chips {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .day {
    min-width: 46px;
    height: var(--control-h-sm);
    padding: 0 var(--sp-2);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-pill);
    background: var(--surface);
    color: var(--text-2);
    font-size: var(--fs-sm);
    font-weight: 600;
    cursor: pointer;
  }
  .day:hover:not(:disabled) {
    background: var(--surface-2);
    color: var(--text);
  }
  .day[aria-pressed='true'] {
    border-color: var(--text);
    background: var(--text);
    color: var(--surface);
  }
  .day:disabled,
  .preset:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .presets {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
  }
  .preset {
    padding: 2px 0;
    border: 0;
    background: none;
    color: var(--link);
    font-size: var(--fs-sm);
    text-decoration: underline;
    text-underline-offset: 2px;
    cursor: pointer;
  }
  .preset[aria-pressed='true'] {
    color: var(--text);
    font-weight: 600;
    text-decoration: none;
  }
  .times {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 160px));
    gap: var(--sp-3);
  }
  .radio {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    cursor: pointer;
  }
  .radio input {
    flex: none;
    width: 16px;
    height: 16px;
    margin: 3px 0 0;
    accent-color: var(--text);
    cursor: inherit;
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .desc {
    color: var(--text-2);
    font-size: var(--fs-sm);
  }
  .error {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  @media (max-width: 480px) {
    .editor {
      padding: var(--sp-3);
    }
    .times {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
</style>
