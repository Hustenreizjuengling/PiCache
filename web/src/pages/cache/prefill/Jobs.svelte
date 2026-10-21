<!--
  @component
  The last prefill jobs (at most 20, newest first): when, why, the outcome,
  what was read through the cache and how much of it the cache already
  had. A row opens the games of the job and the hosts it talked to.
  Reloaded when the running job changes.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type PrefillJob, type PrefillJobState } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatDuration } from '$lib/format'
  import { Chip, EmptyState, Panel, Table, type Column, type Tone } from '$lib/ui'

  let { running }: { running: number } = $props()

  const history = resource((signal) => api.prefill.history({ signal }), { interval: 60_000 })

  // A job that ends appears in the history.
  let last = 0
  $effect(() => {
    const r = running
    if (r !== last) {
      last = r
      void history.refresh()
    }
  })

  let open = $state(0)

  const TONES: Record<PrefillJobState, Tone> = { running: 'info', done: 'ok', failed: 'fail', cancelled: 'neutral', stopped: 'warn' }

  function counts(j: PrefillJob): string {
    const done = j.apps.filter((a) => a.state === 'done').length
    const current = j.apps.filter((a) => a.state === 'current').length
    const failed = j.apps.filter((a) => a.state === 'failed').length
    return t('cache.prefill.jobs.counts', { done, current, failed })
  }

  const columns: Column<PrefillJob>[] = $derived([
    { key: 'started', label: t('cache.prefill.jobs.started'), value: (j) => j.started, format: (j) => formatDateTime(j.started) },
    { key: 'trigger', label: t('cache.prefill.jobs.trigger'), format: (j) => t(`cache.prefill.trigger.${j.trigger}`) },
    { key: 'state', label: t('cache.prefill.jobs.state'), cell: stateCell },
    { key: 'apps', label: t('cache.prefill.jobs.games'), format: counts },
    { key: 'bytes', label: t('cache.prefill.jobs.read'), align: 'right', format: (j) => formatBytes(j.bytes) },
    { key: 'fromCache', label: t('cache.prefill.jobs.fromCache'), align: 'right', format: (j) => formatBytes(j.fromCache) },
  ])

  const shown = $derived(history.data?.jobs.find((j) => j.id === open))
</script>

{#snippet stateCell(j: PrefillJob)}
  <span class="row nowrap">
    <Chip size="sm" tone={TONES[j.state]} label={t(`cache.prefill.jobs.stateName.${j.state}`)} />
    {#if j.finished}<span class="muted small">{formatDuration(new Date(j.finished).getTime() - new Date(j.started).getTime())}</span>{/if}
  </span>
{/snippet}

<Panel flush title={t('cache.prefill.jobs.title')} description={t('cache.prefill.jobs.description')}>
  <Table
    caption={t('cache.prefill.jobs.title')}
    rows={history.data?.jobs}
    key={(j) => j.id}
    {columns}
    loading={history.loading && !history.loaded}
    error={history.error ? errorText(history.error) : undefined}
    onretry={() => history.refresh()}
    onrowclick={(j) => (open = open === j.id ? 0 : j.id)}
    selected={open || undefined}
  >
    {#snippet empty()}
      <EmptyState compact icon="clock" title={t('cache.prefill.jobs.empty')} />
    {/snippet}
  </Table>
  {#if shown}
    <div class="detail">
      {#if shown.error}<p class="error">{shown.error}</p>{/if}
      <ul class="apps">
        {#each shown.apps as a (a.service + a.id)}
          <li>
            <span class="strong">{a.name}</span>
            <span class="muted small">
              {t(`cache.prefill.jobs.appState.${a.state}`)}{#if a.bytes > 0} · {formatBytes(a.bytes)}{/if}{#if a.error} · {a.error}{/if}
            </span>
          </li>
        {:else}
          <li class="muted small">{t('cache.prefill.jobs.noGames')}</li>
        {/each}
      </ul>
      {#if shown.hosts.length > 0}
        <p class="muted small">{t('cache.prefill.jobs.hosts')} <span class="mono">{shown.hosts.join(', ')}</span></p>
      {/if}
    </div>
  {/if}
</Panel>

<style>
  .detail {
    padding: var(--sp-3) var(--sp-4);
    border-top: 1px solid var(--line);
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .apps {
    margin: 0;
    padding-left: var(--sp-5);
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
  }
  .apps li {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
  }
  .strong {
    font-weight: 600;
  }
  .error {
    color: var(--fail, inherit);
    margin: 0;
  }
</style>
