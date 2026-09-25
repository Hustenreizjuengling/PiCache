<!--
  @component
  State of scheduled backups: a summary (off, running, last one written or
  failed, none yet) and the details: schedule, next and last backup, its
  file and where it is stored. Until `data` is loaded only what the settings
  say is shown.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { BackupsSettings, ScheduledBackups } from '$lib/api'
  import { formatBytes, formatDateTime, formatRelative } from '$lib/format'
  import type { IconName } from '$lib/icons'
  import { session } from '$lib/session.svelte'
  import { Chip, Icon, Spinner, type Tone } from '$lib/ui'
  import { LOCAL, destinationLabel, scheduleText, type Destination } from './schedule'

  interface Props {
    /** The saved settings (what is in effect). */
    settings: BackupsSettings
    /** GET /system/backups/scheduled (undefined until loaded). */
    data: ScheduledBackups | undefined
    /** A backup is being written. */
    running: boolean
    dests: readonly Destination[]
  }

  let { settings, data, running, dests }: Props = $props()

  const last = $derived(data?.last)

  function sentence(s: string): string {
    const x = s.trim()
    return x ? x[0].toUpperCase() + x.slice(1) : x
  }

  interface Summary {
    tone: Tone
    icon: IconName
    title: string
    text?: string
    hint?: string
    busy?: boolean
  }

  const summary = $derived.by((): Summary => {
    if (running) {
      return { tone: 'info', icon: 'archive', title: t('system.backup.scheduled.status.running'), text: t('system.backup.scheduled.status.runningText'), busy: true }
    }
    if (last && !last.ok) {
      return {
        tone: 'fail',
        icon: 'error',
        title: t('system.backup.scheduled.status.failed'),
        text: last.error ? sentence(last.error) : undefined,
        hint: settings.enabled ? t('system.backup.scheduled.status.failedHint') : undefined,
      }
    }
    if (!settings.enabled) {
      return {
        tone: 'neutral',
        icon: 'clock',
        title: t('system.backup.scheduled.status.off'),
        text: last
          ? t('system.backup.scheduled.status.offLast', { time: formatRelative(last.time) })
          : session.isAdmin
            ? t('system.backup.scheduled.status.offText')
            : t('system.backup.scheduled.status.offTextRead'),
      }
    }
    if (last) return { tone: 'ok', icon: 'success', title: t('system.backup.scheduled.status.ok', { time: formatRelative(last.time) }) }
    if (!data) return { tone: 'ok', icon: 'success', title: t('system.backup.scheduled.status.on') }
    return {
      tone: 'neutral',
      icon: 'clock',
      title: t('system.backup.scheduled.status.never'),
      text: data.next ? t('system.backup.scheduled.status.neverText', { time: formatRelative(data.next) }) : undefined,
    }
  })

  /** Name (and path) of a destination value; unknown ids as they are. */
  function where(id: string): string {
    const d = dests.find((x) => x.id === id)
    if (d) return destinationLabel(d)
    return id === LOCAL ? t('system.backup.scheduled.destLocal') : id
  }

  function when(ts: string): string {
    return t('system.backup.scheduled.kv.when', { date: formatDateTime(ts), relative: formatRelative(ts) })
  }
</script>

<div class="stack">
  <div class={['summary', summary.tone]} role="status">
    <span class="ic">
      {#if summary.busy}<Spinner size={20} />{:else}<Icon name={summary.icon} size={22} />{/if}
    </span>
    <div class="stack-sm grow">
      <p class="title">{summary.title}</p>
      {#if summary.text}<p class="small muted text">{summary.text}</p>{/if}
      {#if summary.hint}<p class="small muted">{summary.hint}</p>{/if}
    </div>
  </div>

  <dl class="kv">
    <dt>{t('system.backup.scheduled.kv.schedule')}</dt>
    <dd>
      {scheduleText(settings, data?.timeZone)}
      {#if !settings.enabled}<span class="muted">({t('common.state.off')})</span>{/if}
    </dd>
    {#if data}
      <dt>{t('system.backup.scheduled.kv.next')}</dt>
      <dd>
        {#if data.next}
          {when(data.next)}
        {:else if !settings.enabled}
          {t('system.backup.scheduled.kv.nextOff')}
        {:else}
          <span class="subtle">–</span>
        {/if}
      </dd>
    {/if}
    {#if last}
      <dt>{t('system.backup.scheduled.kv.last')}</dt>
      <dd class="with-chip">
        <span title={formatDateTime(last.time, true)}>{when(last.time)}</span>
        {#if last.ok}
          <Chip size="sm" tone="ok" label={t('system.backup.scheduled.kv.ok')} />
        {:else}
          <Chip size="sm" tone="fail" label={t('system.backup.scheduled.kv.failed')} />
        {/if}
      </dd>
      {#if last.file}
        <dt>{t('system.backup.scheduled.kv.file')}</dt>
        <dd>
          <span class="mono">{last.file}</span>
          {#if last.sizeBytes !== undefined}<span class="muted nowrap"> · {formatBytes(last.sizeBytes)}</span>{/if}
        </dd>
      {/if}
      <dt>{t('system.backup.scheduled.kv.storedIn')}</dt>
      <dd>{where(last.destination)}</dd>
    {:else}
      <dt>{t('system.backup.scheduled.kv.storedIn')}</dt>
      <dd>{where(settings.destination)}</dd>
    {/if}
    <dt>{t('system.backup.scheduled.kv.secrets')}</dt>
    <dd>{settings.includeSecrets ? t('system.backup.scheduled.kv.secretsIn') : t('system.backup.scheduled.kv.secretsOut')}</dd>
  </dl>
</div>

<style>
  .summary {
    --c: var(--text-3);
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .summary.ok {
    --c: var(--ok);
  }
  .summary.info {
    --c: var(--focus);
  }
  .summary.fail {
    --c: var(--fail);
  }
  .ic {
    display: flex;
    color: var(--c);
    min-width: 22px;
    justify-content: center;
    padding-top: 1px;
  }
  .title {
    font-size: var(--fs-lg);
    font-weight: 600;
    line-height: var(--lh-tight);
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .text {
    overflow-wrap: anywhere;
  }
  .kv {
    display: grid;
    grid-template-columns: minmax(120px, max-content) minmax(0, 1fr);
    gap: var(--sp-2) var(--sp-4);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    color: var(--text-2);
  }
  dd {
    margin: 0;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  dd.with-chip {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  @media (max-width: 480px) {
    .kv {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
    dd {
      margin-bottom: var(--sp-2);
    }
  }
</style>
