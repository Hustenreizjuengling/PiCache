<!--
  @component
  How long each kind of log data is kept (query log, cache requests,
  downloads, statistics) and the size cap of logs.db. Formerly the log
  settings of Backup & restore; saved with the Logs & privacy page.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import type { LogsSettings } from '$lib/api'
  import { formatBytes, formatDuration } from '$lib/format'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Field, Input, Panel } from '$lib/ui'
  import { rangeError, RANGES, type Range } from '../forms'

  let { form }: { form: SettingsForm<'logs'> } = $props()

  type NumKey = 'queryLogRetentionHours' | 'cacheLogRetentionHours' | 'sessionRetentionDays' | 'statsRetentionDays' | 'maxDbSizeMiB'

  interface NumField {
    key: NumKey
    label: MessageKey
    help: MessageKey
    range: Range
    /** Formats the value for the help text ("7 days", "2.1 GB"). */
    show: (n: number) => string
  }

  const HOUR = 3_600_000
  const DAY = 24 * HOUR
  const MIB = 1024 * 1024

  const fields: NumField[] = [
    {
      key: 'queryLogRetentionHours',
      label: 'system.logs.queryRetention',
      help: 'system.logs.queryRetentionHelp',
      range: RANGES.queryLogRetentionHours,
      show: (n) => formatDuration(n * HOUR),
    },
    {
      key: 'cacheLogRetentionHours',
      label: 'system.logs.cacheRetention',
      help: 'system.logs.cacheRetentionHelp',
      range: RANGES.cacheLogRetentionHours,
      show: (n) => formatDuration(n * HOUR),
    },
    {
      key: 'sessionRetentionDays',
      label: 'system.logs.downloadRetention',
      help: 'system.logs.downloadRetentionHelp',
      range: RANGES.sessionRetentionDays,
      show: (n) => formatDuration(n * DAY),
    },
    {
      key: 'statsRetentionDays',
      label: 'system.logs.statsRetention',
      help: 'system.logs.statsRetentionHelp',
      range: RANGES.statsRetentionDays,
      show: (n) => formatDuration(n * DAY),
    },
    {
      key: 'maxDbSizeMiB',
      label: 'system.logs.maxDbSize',
      help: 'system.logs.maxDbSizeHelp',
      range: RANGES.maxDbSizeMiB,
      show: (n) => formatBytes(n * MIB),
    },
  ]

  const d = $derived(form.draft as LogsSettings)

  function fieldError(f: NumField): string | undefined {
    return form.error(f.key) ?? rangeError(d[f.key], f.range)
  }

  function helpText(f: NumField): string {
    const v = d[f.key]
    const def = form.defaults?.[f.key]
    return t(f.help, {
      value: typeof v === 'number' && Number.isFinite(v) && v > 0 ? f.show(v) : '–',
      def: def !== undefined ? f.show(def) : '–',
    })
  }
</script>

<Panel id="retention" title={t('system.logs.retention.title')} description={t('system.logs.retention.description')}>
  <div class="grid">
    {#each fields as f (f.key)}
      <Field label={t(f.label)} error={fieldError(f)} help={helpText(f)}>
        <Input type="number" bind:value={d[f.key]} min={f.range.min} max={f.range.max} step={1} inputmode="numeric" />
      </Field>
    {/each}
  </div>
</Panel>

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 260px), 1fr));
    gap: var(--sp-4);
  }
</style>
