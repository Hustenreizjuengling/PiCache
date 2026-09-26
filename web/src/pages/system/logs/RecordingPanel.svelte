<!--
  @component
  What is left out of the logs and statistics and how often they are
  written: ignored domains (answered, but neither logged nor counted),
  counting only address queries, and the write interval (fewer SD-card
  writes against data lost on a power cut).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { LogsSettings } from '$lib/api'
  import { formatDuration, formatNumber } from '$lib/format'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Checkbox, Field, Panel } from '$lib/ui'
  import { lineError } from '../../dns/shared/errors'
  import LinesInput from '../../dns/shared/LinesInput.svelte'
  import NumberInput from '../../dns/shared/NumberInput.svelte'
  import { MAX_IGNORED_DOMAINS, RANGES } from '../forms'

  let { form }: { form: SettingsForm<'logs'> } = $props()

  const d = $derived(form.draft as LogsSettings)
  const tooMany = $derived(d.ignoredDomains.length > MAX_IGNORED_DOMAINS)
  const flushText = $derived(formatDuration(Math.max(RANGES.flushSeconds.min, d.flushSeconds || 0) * 1000))
</script>

<Panel id="recording" title={t('system.logs.recording.title')} description={t('system.logs.recording.description')}>
  <div class="stack">
    <Field
      label={t('system.logs.ignored')}
      optional
      help={t('system.logs.ignoredHelp', { count: formatNumber(d.ignoredDomains.length), max: MAX_IGNORED_DOMAINS })}
      error={lineError(form.saveError, 'logs.ignoredDomains') ??
        (tooMany ? t('system.logs.ignoredTooMany', { max: MAX_IGNORED_DOMAINS }) : undefined)}
    >
      <LinesInput bind:value={d.ignoredDomains} rows={4} placeholder={'connectivitycheck.gstatic.com\nlan'} />
    </Field>

    <Checkbox
      bind:checked={d.statsOnlyAddressQueries}
      label={t('system.logs.addressOnly')}
      description={t('system.logs.addressOnlyHelp')}
    />

    <div class="flush">
      <Field
        label={t('system.logs.flush')}
        help={t('system.logs.flushHelp', { value: flushText })}
        error={form.error('flushSeconds')}
      >
        <NumberInput bind:value={d.flushSeconds} min={RANGES.flushSeconds.min} max={RANGES.flushSeconds.max} unit={t('system.logs.seconds')} />
      </Field>
    </div>
  </div>
</Panel>

<style>
  .flush {
    max-width: 640px;
  }
</style>
