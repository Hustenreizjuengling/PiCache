<!--
  @component
  Content removed from the cache (/cache/evictions): by retention, size
  limits, low disk space, by hand or because files were damaged.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, type EvictionEvent, type EvictionReason, type RangePreset } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import {
    Chip,
    CursorStack,
    EmptyState,
    IconButton,
    KeyValue,
    Pager,
    Panel,
    SidePanel,
    Table,
    type Column,
    type SelectOption,
    type Tone,
  } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'
  import { pickRange, timeWindow } from '../shared/util'
  import Filters from './Filters.svelte'

  let { catalog }: { catalog: ServiceCatalog } = $props()

  const RANGES: RangePreset[] = ['24h', '7d', '30d', '90d']
  const REASONS: EvictionReason[] = ['inactive', 'size', 'min-free', 'manual', 'corrupt', 'invalidated']
  const TONES: Record<EvictionReason, Tone> = {
    inactive: 'neutral',
    size: 'info',
    'min-free': 'warn',
    manual: 'neutral',
    corrupt: 'fail',
    invalidated: 'neutral',
  }
  const PAGE = 100

  const range = $derived(pickRange(router.param('range'), RANGES, '7d'))
  const win = $derived(timeWindow(router.param('from'), router.param('to')))
  /** An explicit window from the URL (e.g. the query log's "Cache traffic" link) replaces the range. */
  const time = $derived(win ?? { range })
  const service = $derived(router.param('service'))
  const search = $derived(router.param('search'))
  const status = $derived(router.param('status'))

  const reasonOptions: SelectOption[] = $derived(REASONS.map((r) => ({ value: r, label: t(`cache.reason.${r}`) })))

  const pages = new CursorStack()
  const filterKey = $derived(JSON.stringify([time, service, search, status]))
  $effect.pre(() => {
    void filterKey
    untrack(() => pages.reset())
  })

  const list = resource((signal) =>
    api.cache.evictions(
      {
        ...time,
        service: service || undefined,
        search: search || undefined,
        status: status || undefined,
        cursor: pages.current || undefined,
        limit: PAGE,
      },
      { signal },
    ),
  )

  const selectedId = $derived(Number(router.param('eviction')) || 0)
  const selected = $derived(list.data?.items.find((e) => e.id === selectedId))

  function reasonText(r: string): string {
    return (REASONS as readonly string[]).includes(r) ? t(`cache.reason.${r as EvictionReason}`) : r
  }

  const columns: Column<EvictionEvent>[] = $derived([
    { key: 'time', label: t('common.label.time'), cell: timeCell },
    { key: 'reason', label: t('cache.col.reason'), cell: reasonCell },
    { key: 'service', label: t('common.label.service'), format: (e) => catalog.name(e.service) },
    { key: 'group', label: t('cache.col.content'), mono: true, truncate: true, width: '40%', value: (e) => e.groupKey },
    { key: 'bytes', label: t('common.label.size'), align: 'right', format: (e) => formatBytes(e.bytes) },
  ])
</script>

{#snippet timeCell(e: EvictionEvent)}
  <span class="nowrap" title={formatDateTime(e.time, true)}>{formatDateTime(e.time)}</span>
{/snippet}

{#snippet reasonCell(e: EvictionEvent)}
  <Chip size="sm" tone={TONES[e.reason] ?? 'neutral'} label={reasonText(e.reason)} />
{/snippet}

<div class="stack">
  <Filters
    {catalog}
    ranges={RANGES}
    rangeDefault="7d"
    client={false}
    searchLabel={t('cache.filters.contentOrObject')}
    searchPlaceholder={t('cache.filters.contentOrObjectPlaceholder')}
    statuses={reasonOptions}
    statusLabel={t('cache.col.reason')}
  >
    {#snippet actions()}
      <IconButton icon="refresh" variant="secondary" label={t('common.action.refresh')} loading={list.loading} onclick={() => list.refresh()} />
    {/snippet}
  </Filters>

  <Panel flush>
    {#if list.data && list.data.items.length === 0 && !list.error}
      <EmptyState compact icon="trash" title={t('cache.evictions.empty')} text={t('cache.evictions.emptyText')} />
    {:else}
      <Table
        compact
        caption={t('cache.evictions.caption')}
        rows={list.data?.items}
        key={(e) => e.id}
        {columns}
        loading={list.loading && !list.loaded}
        error={list.error ? errorText(list.error) : undefined}
        onretry={() => list.refresh()}
        onrowclick={(e) => router.setQuery({ eviction: e.id }, { push: true })}
        selected={selected?.id}
      />
    {/if}
    {#if list.data && (list.data.items.length > 0 || pages.hasPrev)}
      <Pager
        mode="cursor"
        hasPrev={pages.hasPrev}
        hasNext={!!list.data.next}
        count={list.data.items.length}
        onprev={() => pages.prev()}
        onnext={() => list.data?.next && pages.next(list.data.next)}
        onfirst={() => pages.reset()}
      />
    {/if}
  </Panel>
</div>

<SidePanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ eviction: null })}
  title={selected ? selected.groupKey : ''}
  subtitle={selected ? formatDateTime(selected.time, true) : undefined}
>
  {#if selected}
    {@const e = selected}
    <div class="stack">
      <p><Chip tone={TONES[e.reason] ?? 'neutral'} label={reasonText(e.reason)} /></p>
      <p class="muted small">{t(`cache.reasonText.${e.reason in TONES ? e.reason : 'manual'}`)}</p>
      <KeyValue
        items={[
          { label: t('common.label.time'), value: formatDateTime(e.time, true) },
          { label: t('common.label.service'), value: catalog.name(e.service) },
          { label: t('cache.detail.groupKey'), value: e.groupKey, mono: true },
          { label: t('common.label.size'), value: formatBytes(e.bytes) },
          { label: t('cache.detail.objectId'), value: e.objectId, mono: true },
          { label: t('cache.detail.storeId'), value: e.storeId, mono: true },
        ]}
      />
    </div>
  {/if}
</SidePanel>
