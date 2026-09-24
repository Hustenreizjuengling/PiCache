<!--
  @component
  Log retention and privacy (settings.logs): query log on/off, client IP
  anonymisation, retention per log and the size cap of logs.db.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import type { LogsSettings } from '$lib/api'
  import { formatBytes, formatDuration } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Checkbox, Field, Input, Notice, Panel, Skeleton, toast } from '$lib/ui'
  import { rangeError, RANGES, type Range } from '../forms'

  const form = settingsForm('logs')

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

  function fieldError(f: NumField, d: LogsSettings): string | undefined {
    return form.error(f.key) ?? rangeError(d[f.key], f.range)
  }

  function helpText(f: NumField, d: LogsSettings): string {
    const v = d[f.key]
    const def = form.defaults?.[f.key]
    return t(f.help, {
      value: typeof v === 'number' && Number.isFinite(v) && v > 0 ? f.show(v) : '–',
      def: def !== undefined ? f.show(def) : '–',
    })
  }

  const invalid = $derived(!!form.draft && fields.some((f) => rangeError(form.draft![f.key], f.range)))
  const general = $derived(form.saveError && !form.saveError.field ? form.errorMessage : undefined)

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (invalid) return
    if (await form.save()) toast.success(t('common.state.saved'))
  }

  const formId = $props.id()
</script>

<Panel title={t('system.logs.title')} description={t('system.logs.description')}>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.logs.loadError')}>{form.loadError.message}</Notice>
  {:else if !form.draft}
    <Skeleton height="260px" />
  {:else}
    {@const draft = form.draft}
    <form id="logs-{formId}" class="stack" onsubmit={save} novalidate>
      {#if general}<Notice tone="fail">{general}</Notice>{/if}
      <div class="stack-sm">
        <Checkbox
          bind:checked={draft.queryLogEnabled}
          label={t('system.logs.queryLog')}
          description={t('system.logs.queryLogHelp')}
          disabled={!session.isAdmin}
        />
        <Checkbox
          bind:checked={draft.anonymizeClientIps}
          label={t('system.logs.anonymize')}
          description={t('system.logs.anonymizeHelp')}
          disabled={!session.isAdmin}
        />
      </div>
      <div class="grid">
        {#each fields as f (f.key)}
          <Field label={t(f.label)} error={fieldError(f, draft)} help={helpText(f, draft)}>
            <Input
              type="number"
              bind:value={draft[f.key]}
              min={f.range.min}
              max={f.range.max}
              step={1}
              inputmode="numeric"
              disabled={!session.isAdmin}
            />
          </Field>
        {/each}
      </div>
    </form>
  {/if}
  {#snippet footer()}
    {#if form.draft}
      <div class="row">
        <Button
          type="submit"
          form="logs-{formId}"
          variant="primary"
          loading={form.saving}
          disabled={!form.dirty || invalid || !session.isAdmin}
        >
          {t('common.action.save')}
        </Button>
        <Button variant="ghost" disabled={!form.dirty || form.saving} onclick={() => form.revert()}>
          {t('system.form.discard')}
        </Button>
      </div>
    {/if}
  {/snippet}
</Panel>

<style>
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 260px), 1fr));
    gap: var(--sp-4);
  }
</style>
