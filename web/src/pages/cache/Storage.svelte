<!--
  @component
  Cache › Storage: the storage the cache uses now, checking and repairing the
  cache files, all storage locations (local folders and NAS shares) with
  their state, adding and editing them, and what this environment allows.
  A row opens the location (?target=<id>).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type StorageTarget, type StorageTargetWithStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatNumber } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Notice, Panel, Table, type Column } from '$lib/ui'
  import ActiveStore from './storage/ActiveStore.svelte'
  import Capabilities from './storage/Capabilities.svelte'
  import { STATE_TONES, targetState } from './storage/status'
  import TargetDialog from './storage/TargetDialog.svelte'
  import TargetPanel from './storage/TargetPanel.svelte'
  import Verify from './storage/Verify.svelte'

  const store = resource((signal) => api.cache.state({ signal }), { interval: 10_000 })
  const targets = resource((signal) => api.storage.targets({ signal }), { interval: 15_000 })
  const caps = resource((signal) => api.storage.capabilities({ signal }))

  const activeTarget = $derived(targets.data?.find((x) => x.active))
  const selectedId = $derived(router.param('target'))
  const selected = $derived(targets.data?.find((x) => x.id === selectedId))

  async function refresh() {
    await Promise.all([targets.refresh(), store.refresh()])
  }

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
  ])
</script>

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
  {#if !session.isAdmin}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  <ActiveStore store={store.data} error={store.error} target={activeTarget} onchanged={refresh} />

  <Verify online={!!store.data?.online} />

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
      onchanged={refresh}
      onedit={() => edit(selected)}
      onclose={() => {
        testNew = ''
        router.setQuery({ target: null })
      }}
    />
  {/key}
{/if}

<TargetDialog bind:open={dialogOpen} target={editing} caps={caps.data} onsaved={saved} />

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
