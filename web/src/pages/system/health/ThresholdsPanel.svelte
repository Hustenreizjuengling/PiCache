<!--
  @component
  Warning thresholds of the health check "Host resources" (settings.health):
  available memory, load per CPU and temperature. Read-only for viewers and
  while the host locks the configuration.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { HealthSettings } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Button, Field, Notice, Panel, Skeleton, toast } from '$lib/ui'
  import NumberInput from '../../dns/shared/NumberInput.svelte'
  import { rangeError, RANGES } from '../forms'

  let { form }: { form: SettingsForm<'health'> } = $props()

  const KEYS = ['memoryAvailableMinPercent', 'loadPerCpuMax', 'temperatureMaxCelsius'] as const

  const invalid = $derived(!!form.draft && KEYS.some((k) => rangeError(form.draft![k], RANGES[k])))
  const general = $derived(form.saveError && !form.saveError.field ? form.errorMessage : undefined)

  function help(k: (typeof KEYS)[number]): string {
    const def = form.defaults?.[k]
    return t(`system.health.thresholds.${k}Help`, { def: def ?? '–' })
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (invalid) return
    if (await form.save()) toast.success(t('common.state.saved'))
  }

  const formId = $props.id()
</script>

{#snippet actions()}
  <div class="row">
    <Button type="submit" form="thresholds-{formId}" variant="primary" loading={form.saving} disabled={!form.dirty || invalid}>
      {t('common.action.save')}
    </Button>
    <Button variant="ghost" disabled={!form.dirty || form.saving} onclick={() => form.revert()}>{t('system.form.discard')}</Button>
  </div>
{/snippet}

<!-- Read-only principals get no buttons. -->
<Panel
  id="thresholds"
  title={t('system.health.thresholds.title')}
  description={t('system.health.thresholds.description')}
  footer={form.draft && session.isAdmin ? actions : undefined}
>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.health.thresholds.loadError')}>{errorText(form.loadError)}</Notice>
  {:else if !form.draft}
    <Skeleton height="160px" />
  {:else}
    {@const d = form.draft as HealthSettings}
    <form id="thresholds-{formId}" onsubmit={save} novalidate>
      <fieldset class="stack" disabled={!session.isAdmin}>
        {#if general}<Notice tone="fail">{general}</Notice>{/if}
        <Field label={t('system.health.thresholds.memory')} help={help('memoryAvailableMinPercent')} error={form.error('memoryAvailableMinPercent')}>
          <NumberInput bind:value={d.memoryAvailableMinPercent} min={1} max={50} unit="%" />
        </Field>
        <Field label={t('system.health.thresholds.load')} help={help('loadPerCpuMax')} error={form.error('loadPerCpuMax')}>
          <NumberInput bind:value={d.loadPerCpuMax} min={1} max={16} unit={t('system.health.thresholds.perCpu')} />
        </Field>
        <Field label={t('system.health.thresholds.temperature')} help={help('temperatureMaxCelsius')} error={form.error('temperatureMaxCelsius')}>
          <NumberInput bind:value={d.temperatureMaxCelsius} min={50} max={110} unit="°C" />
        </Field>
      </fieldset>
    </form>
  {/if}
</Panel>

<style>
  fieldset {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
</style>
