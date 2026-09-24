<!--
  @component
  HTTPS pass-through connections (/cache/sni-events): encrypted downloads of
  known services that PiCache relays without caching.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, type RangePreset, type SniEvent } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatDuration, formatTime } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { CursorStack, EmptyState, IconButton, KeyValue, Pager, Panel, SidePanel, Table, type Column } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'
  import { pickRange, timeWindow } from '../shared/util'
  import Filters from './Filters.svelte'

  let { catalog }: { catalog: ServiceCatalog } = $props()

  const RANGES: RangePreset[] = ['1h', '6h', '24h', '7d']
  const PAGE = 100

  const range = $derived(pickRange(router.param('range'), RANGES, '24h'))
  const win = $derived(timeWindow(router.param('from'), router.param('to')))
  /** An explicit window from the URL (e.g. the query log's "Cache traffic" link) replaces the range. */
  const time = $derived(win ?? { range })
  const client = $derived(router.param('client'))
  const service = $derived(router.param('service'))
  const search = $derived(router.param('search'))

  const pages = new CursorStack()
  const filterKey = $derived(JSON.stringify([time, client, service, search]))
  $effect.pre(() => {
    void filterKey
    untrack(() => pages.reset())
  })

  const list = resource((signal) =>
    api.cache.sniEvents(
      {
        ...time,
        client: client || undefined,
        service: service || undefined,
        search: search || undefined,
        cursor: pages.current || undefined,
        limit: PAGE,
      },
      { signal },
    ),
  )

  const selectedId = $derived(Number(router.param('connection')) || 0)
  const selected = $derived(list.data?.items.find((e) => e.id === selectedId))

  const columns: Column<SniEvent>[] = $derived([
    { key: 'time', label: t('common.label.time'), cell: timeCell },
    { key: 'client', label: t('common.label.client'), cell: clientCell },
    { key: 'sni', label: t('cache.col.hostName'), cell: sniCell },
    { key: 'service', label: t('common.label.service'), format: (e) => catalog.name(e.service) },
    { key: 'down', label: t('cache.col.downloaded'), align: 'right', format: (e) => formatBytes(e.bytesDown) },
    { key: 'up', label: t('cache.col.uploaded'), align: 'right', format: (e) => formatBytes(e.bytesUp) },
    { key: 'duration', label: t('cache.col.duration'), align: 'right', format: (e) => formatDuration(e.durationMs) },
  ])
</script>

{#snippet timeCell(e: SniEvent)}
  <span class="nowrap" title={formatDateTime(e.time, true)}>{formatTime(e.time, true)}</span>
{/snippet}

{#snippet sniCell(e: SniEvent)}
  <span class="mono nowrap">{e.sni}</span>
{/snippet}

{#snippet clientCell(e: SniEvent)}
  {#if e.clientName}
    <span title={e.clientIp}>{e.clientName}</span>
  {:else}
    <span class="mono nowrap">{e.clientIp}</span>
  {/if}
{/snippet}

<div class="stack">
  <Filters
    {catalog}
    ranges={RANGES}
    rangeDefault="24h"
    searchLabel={t('cache.col.hostName')}
    searchPlaceholder={t('cache.filters.hostPlaceholder')}
  >
    {#snippet actions()}
      <IconButton icon="refresh" variant="secondary" label={t('common.action.refresh')} loading={list.loading} onclick={() => list.refresh()} />
    {/snippet}
  </Filters>

  <Panel flush>
    <p class="note">{t('cache.passThrough.note')}</p>
    {#if list.data && list.data.items.length === 0 && !list.error}
      <EmptyState compact icon="lock" title={t('cache.passThrough.empty')} text={t('cache.passThrough.emptyText')} />
    {:else}
      <Table
        compact
        caption={t('cache.passThrough.caption')}
        rows={list.data?.items}
        key={(e) => e.id}
        {columns}
        loading={list.loading && !list.loaded}
        error={list.error ? errorText(list.error) : undefined}
        onretry={() => list.refresh()}
        onrowclick={(e) => router.setQuery({ connection: e.id }, { push: true })}
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
  bind:open={() => !!selected, (v) => !v && router.setQuery({ connection: null })}
  title={selected ? selected.sni : ''}
  subtitle={selected ? formatDateTime(selected.time, true) : undefined}
>
  {#if selected}
    {@const e = selected}
    <KeyValue
      items={[
        { label: t('common.label.time'), value: formatDateTime(e.time, true) },
        { label: t('common.label.client'), value: e.clientName ? `${e.clientName} (${e.clientIp})` : e.clientIp },
        { label: t('cache.col.hostName'), value: e.sni, mono: true },
        { label: t('common.label.service'), value: catalog.name(e.service) },
        { label: t('cache.col.downloaded'), value: formatBytes(e.bytesDown) },
        { label: t('cache.col.uploaded'), value: formatBytes(e.bytesUp) },
        { label: t('cache.col.duration'), value: formatDuration(e.durationMs) },
      ]}
    />
    <p class="hint">{t('cache.passThrough.detailHint')}</p>
  {/if}
</SidePanel>

<style>
  .note {
    padding: 0 var(--sp-4) var(--sp-3);
    font-size: var(--fs-sm);
    color: var(--text-2);
    max-width: 90ch;
  }
  .hint {
    margin-top: var(--sp-4);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
</style>
