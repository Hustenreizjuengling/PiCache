<!--
  @component
  The follower's sync state (GET /system/sync): the primary, the synced
  sections, the interval, whether a run is going, the last run and last
  success, the primary's version, the configuration applied last and the
  next run; the last error in full. Admins start a run with "Sync now"
  (not while sync is off or a run is going; at most every 30 s).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type Resource, type SyncStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, KeyValue, Notice, Panel, Skeleton, Spinner, toast, type KeyValueItem } from '$lib/ui'

  let { status }: { status: Resource<SyncStatus> } = $props()

  const s = $derived(status.data)
  const follower = $derived(s?.mode === 'follower')
  let starting = $state(false)

  function when(ts: string | undefined): string | undefined {
    return ts ? `${formatRelative(ts)} (${formatDateTime(ts)})` : undefined
  }

  const items = $derived.by((): KeyValueItem[] => {
    if (!s || !follower) return []
    return [
      { label: t('system.sync.status.source'), value: s.source, mono: true },
      { label: t('system.sync.status.sections'), value: s.sections.map((x) => t(`system.section.${x}`)).join(', ') || '–' },
      { label: t('system.sync.status.interval'), value: t('system.sync.status.every', { minutes: s.intervalMinutes }) },
      { label: t('system.sync.status.lastRun'), value: when(s.lastRun) ?? t('common.state.never') },
      { label: t('system.sync.status.lastSuccess'), value: when(s.lastSuccess) ?? t('common.state.never') },
      { label: t('system.sync.status.nextRun'), value: s.running ? t('system.sync.status.runningNow') : when(s.nextRun) },
      { label: t('system.sync.status.primaryVersion'), value: s.primaryVersion, mono: true },
      { label: t('system.sync.status.applied'), value: s.lastAppliedSha256 ? s.lastAppliedSha256.slice(0, 12) : undefined, mono: true },
    ]
  })

  async function run() {
    starting = true
    try {
      await api.system.runSync()
      toast.success(t('system.sync.status.started'))
      await status.refresh()
    } catch (err) {
      toast.error(errorText(err))
    } finally {
      starting = false
    }
  }
</script>

<Panel id="sync-set-status" title={t('system.sync.status.title')}>
  {#snippet actions()}
    {#if s?.running}
      <Chip size="sm" tone="info" label={t('system.sync.status.running')} />
    {:else if follower}
      <Chip
        size="sm"
        tone={s?.lastError ? 'fail' : s?.lastSuccess ? 'ok' : 'neutral'}
        label={s?.lastError ? t('system.sync.status.failed') : s?.lastSuccess ? t('system.sync.status.ok') : t('system.sync.status.waiting')}
      />
    {/if}
  {/snippet}

  {#if status.error && !s}
    <Notice tone="fail" title={t('system.sync.status.loadError')}>
      {errorText(status.error)}
      {#snippet actions()}
        <Button size="sm" icon="refresh" onclick={() => status.refresh()}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {:else if !s}
    <Skeleton height="220px" />
  {:else if !follower}
    <p class="muted">{t('system.sync.status.off')}</p>
  {:else}
    <div class="stack">
      {#if s.running}
        <p class="running small" role="status"><Spinner size={14} />{t('system.sync.status.runningText')}</p>
      {/if}
      {#if s.lastError}
        <Notice tone="fail" title={t('system.sync.status.errorTitle')}><p class="msg">{s.lastError}</p></Notice>
      {/if}
      <KeyValue {items} />
    </div>
  {/if}

  {#snippet footer()}
    <p class="small muted grow">{t('system.sync.status.footer')}</p>
    {#if session.canOperate}
      <Button icon="sync" loading={starting} disabled={!follower || !!s?.running} onclick={run}>{t('system.sync.status.run')}</Button>
    {/if}
  {/snippet}
</Panel>

<style>
  .running {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .msg {
    overflow-wrap: anywhere;
  }
  .grow {
    flex: 1 1 200px;
  }
</style>
