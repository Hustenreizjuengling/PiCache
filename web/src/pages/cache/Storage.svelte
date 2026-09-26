<!--
  @component
  Cache › Storage: the storage the cache uses now, checking and repairing the
  cache files, the storage speed test, all storage locations (local folders
  and NAS shares) with their state and last speed test, adding and editing
  them, and what this environment allows. A row opens the location
  (?target=<id>).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    resource,
    type BenchmarkRun,
    type BenchmarkStatus,
    type Resource,
    type StorageTarget,
    type StorageTargetWithStatus,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Notice, Panel, Table, toast, type Column } from '$lib/ui'
  import ActiveStore from './storage/ActiveStore.svelte'
  import Capabilities from './storage/Capabilities.svelte'
  import { formatMBps, measured } from './storage/speed'
  import SpeedDialog from './storage/SpeedDialog.svelte'
  import SpeedTest from './storage/SpeedTest.svelte'
  import { STATE_TONES, targetState } from './storage/status'
  import TargetDialog from './storage/TargetDialog.svelte'
  import TargetPanel from './storage/TargetPanel.svelte'
  import Verify from './storage/Verify.svelte'

  const store = resource((signal) => api.cache.state({ signal }), { interval: 10_000 })
  const targets = resource((signal) => api.storage.targets({ signal }), { interval: 15_000 })
  const caps = resource((signal) => api.storage.capabilities({ signal }))

  // Speed test: the running or most recent run and the last result per
  // target. A run goes on when the page is left; opening the page picks it up
  // again. Polled about every second while one runs, else every 15 s (a test
  // started by another admin shows up).
  const bench: Resource<BenchmarkStatus> = resource((signal) => api.storage.benchmarkState({ signal }), {
    interval: () => (bench.data?.run?.state === 'running' ? 1_000 : 15_000),
  })

  const activeTarget = $derived(targets.data?.find((x) => x.active))
  const selectedId = $derived(router.param('target'))
  const selected = $derived(targets.data?.find((x) => x.id === selectedId))

  async function refresh() {
    await Promise.all([targets.refresh(), store.refresh()])
  }

  function nameOf(id: string): string {
    return targets.data?.find((x) => x.id === id)?.name ?? id
  }

  // ---- speed test

  const onlineTargets = $derived(targets.data?.filter((x) => x.status.online) ?? [])
  const hasSpeed = $derived(bench.data?.run?.state === 'running' || Object.keys(bench.data?.last ?? {}).length > 0)
  let speedOpen = $state(false)
  /** Target the dialog tests; '' = chosen in the dialog (opened from the page). */
  let speedFor = $state('')

  function testSpeed(id = '') {
    speedFor = id
    speedOpen = true
  }

  function speedStarted(run: BenchmarkRun) {
    bench.set({ run, last: bench.data?.last ?? {} })
    void bench.refresh() // polls every second from now on
  }

  // Tell when a run watched here ends (the panel may be scrolled away or
  // covered by a side panel). Not on load: only a change seen by this page.
  let watched: { startedAt: string; running: boolean } | undefined
  $effect(() => {
    const run = bench.data?.run
    if (!run) return
    const was = watched
    watched = { startedAt: run.startedAt, running: run.state === 'running' }
    if (!was?.running || was.startedAt !== run.startedAt) return
    untrack(() => {
      if (run.state === 'done') toast.success(t('cache.speed.finished', { name: nameOf(run.targetId) }))
      else if (run.state === 'failed') toast.error(t('cache.speed.failedToast', { name: nameOf(run.targetId) }))
    })
  })

  // ---- add / edit

  let dialogOpen = $state(false)
  let editing = $state.raw<StorageTargetWithStatus | undefined>(undefined)
  let testNew = $state('')

  function add() {
    editing = undefined
    dialogOpen = true
  }

  function edit(tg: StorageTargetWithStatus) {
    editing = tg
    dialogOpen = true
  }

  async function saved(tg: StorageTarget, created: boolean) {
    await refresh()
    if (created) {
      testNew = tg.id
      router.setQuery({ target: tg.id }, { push: true })
    }
  }

  function location(tg: StorageTargetWithStatus): string {
    if (tg.kind === 'smb') return `//${tg.server}/${tg.share}`
    if (tg.kind === 'nfs') return `${tg.server}:${tg.export}`
    return tg.path
  }

  const columns: Column<StorageTargetWithStatus>[] = $derived([
    { key: 'name', label: t('common.label.name'), sortable: true, value: (tg) => tg.name, cell: nameCell },
    { key: 'location', label: t('cache.target.location'), mono: true, truncate: true, width: '36%', value: location },
    { key: 'status', label: t('common.label.status'), cell: statusCell },
    {
      key: 'free',
      label: t('cache.target.free'),
      align: 'right',
      sortable: true,
      value: (tg) => tg.status.freeBytes,
      format: (tg) => (tg.status.totalBytes > 0 ? t('cache.target.spaceValue', { free: formatBytes(tg.status.freeBytes), total: formatBytes(tg.status.totalBytes) }) : '–'),
    },
    { key: 'fs', label: t('cache.store.fileSystem'), value: (tg) => tg.status.fsType },
    {
      key: 'latency',
      label: t('cache.target.latency'),
      align: 'right',
      value: (tg) => tg.status.latencyMs,
      format: (tg) => (tg.status.latencyMs > 0 ? `${formatNumber(tg.status.latencyMs, 1)} ms` : '–'),
    },
    // Only once there is something to show (results are kept in memory until PiCache restarts).
    ...(hasSpeed ? [{ key: 'speed', label: t('cache.speed.column'), cell: speedCell }] : []),
  ])
</script>

{#snippet speedCell(tg: StorageTargetWithStatus)}
  {@const run = bench.data?.run}
  {@const last = bench.data?.last?.[tg.id]}
  {#if run?.state === 'running' && run.targetId === tg.id}
    <span class="muted nowrap">{t('cache.speed.cellRunning', { percent: formatPercent(run.progress, 0) })}</span>
  {:else if last}
    <!-- Read and write may break onto two lines when the table is tight. -->
    <span class="name">
      <span>
        <span class="nowrap">{t('cache.speed.cellRead', { speed: measured(last.read) ? formatMBps(last.read.bytesPerSec) : '–' })} ·</span>
        <span class="nowrap">{t('cache.speed.cellWrite', { speed: measured(last.write) ? formatMBps(last.write.bytesPerSec) : '–' })}</span>
      </span>
      <span class="sub nowrap">{formatRelative(last.testedAt)}</span>
    </span>
  {:else}
    <span class="subtle">–</span>
  {/if}
{/snippet}

{#snippet nameCell(tg: StorageTargetWithStatus)}
  <span class="name">
    <span class="strong">{tg.name}</span>
    <span class="sub">{t(`cache.target.kind.${tg.kind}`)} · {t(`cache.target.modeName.${tg.mode}`)}</span>
  </span>
{/snippet}

{#snippet statusCell(tg: StorageTargetWithStatus)}
  {@const s = targetState(tg)}
  <span class="status">
    <Chip size="sm" tone={STATE_TONES[s]} label={t(`cache.target.state.${s}`)} />
    {#if !tg.status.online && tg.status.reason}<span class="reason" title={tg.status.reason}>{tg.status.reason}</span>{/if}
  </span>
{/snippet}

<div class="page">
  {#if !session.canOperate}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  <ActiveStore store={store.data} error={store.error} target={activeTarget} onchanged={refresh} />

  <Verify online={!!store.data?.online} />

  <SpeedTest status={bench.data} error={bench.error} targets={targets.data} onstart={() => testSpeed()} onchanged={() => bench.refresh()} />

  <Panel flush title={t('cache.targets.title')} description={t('cache.targets.description')}>
    {#snippet actions()}
      <Button icon="plus" disabled={!session.isAdmin || !caps.data} onclick={add}>{t('cache.targets.add')}</Button>
    {/snippet}
    <Table
      caption={t('cache.targets.title')}
      rows={targets.data}
      key={(tg) => tg.id}
      {columns}
      loading={targets.loading && !targets.loaded}
      error={targets.error ? errorText(targets.error) : undefined}
      onretry={() => targets.refresh()}
      onrowclick={(tg) => router.setQuery({ target: tg.id }, { push: true })}
      selected={selected?.id}
    >
      {#snippet empty()}
        <EmptyState compact icon="drive" title={t('cache.targets.empty')} />
      {/snippet}
    </Table>
  </Panel>

  <Capabilities caps={caps.data} error={caps.error} />
</div>

{#if selected}
  {#key selected.id}
    <TargetPanel
      target={selected}
      caps={caps.data}
      autoTest={testNew === selected.id}
      speed={bench.data}
      {nameOf}
      onchanged={refresh}
      onspeedtest={() => testSpeed(selected.id)}
      onspeedchanged={() => bench.refresh()}
      onedit={() => edit(selected)}
      onclose={() => {
        testNew = ''
        router.setQuery({ target: null })
      }}
    />
  {/key}
{/if}

<TargetDialog bind:open={dialogOpen} target={editing} caps={caps.data} onsaved={saved} />

{#if speedOpen}
  <SpeedDialog
    bind:open={speedOpen}
    targets={onlineTargets}
    targetId={speedFor || (activeTarget?.status.online ? activeTarget.id : (onlineTargets[0]?.id ?? ''))}
    fixed={!!speedFor}
    running={bench.data?.run?.state === 'running'}
    onstarted={speedStarted}
    onrefused={() => void Promise.all([bench.refresh(), targets.refresh()])}
  />
{/if}

<style>
  .name,
  .status {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
    min-width: 0;
  }
  .strong {
    font-weight: 600;
    white-space: nowrap;
  }
  .sub {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
  .reason {
    max-width: 32ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-2);
    font-size: var(--fs-xs);
  }
</style>
