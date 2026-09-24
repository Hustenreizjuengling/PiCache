<!--
  @component
  Raw cache requests (/cache/requests, cursor pages) with an optional live
  view (/stream/cache) that shows new requests as they happen. Filters and
  the live switch are in the URL.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    resource,
    streamCache,
    type CacheEvent,
    type CacheStatus,
    type LiveStream,
    type RangePreset,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatDuration, formatNumber, formatTime } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import {
    CacheStatusChip,
    Chip,
    CursorStack,
    EmptyState,
    IconButton,
    KeyValue,
    Pager,
    Panel,
    SidePanel,
    Table,
    Toggle,
    type Column,
    type SelectOption,
  } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'
  import { formatHitRatio, isIP, pickRange } from '../shared/util'
  import Filters from './Filters.svelte'

  let { catalog }: { catalog: ServiceCatalog } = $props()

  const RANGES: RangePreset[] = ['15m', '1h', '6h', '24h', '7d']
  const STATUSES: CacheStatus[] = ['HIT', 'PARTIAL', 'MISS', 'BYPASS', 'PASS', 'ERROR']
  const PAGE = 100
  const MAX_LIVE = 500

  const range = $derived(pickRange(router.param('range'), RANGES, '1h'))
  const client = $derived(router.param('client'))
  const service = $derived(router.param('service'))
  const search = $derived(router.param('search'))
  const status = $derived(router.param('status'))
  const following = $derived(router.param('live') === 'true')

  const statusOptions: SelectOption[] = $derived(STATUSES.map((s) => ({ value: s, label: t(`common.cacheStatus.${s}`) })))

  const pages = new CursorStack()
  const filterKey = $derived(JSON.stringify([range, client, service, search, status]))
  $effect.pre(() => {
    void filterKey
    untrack(() => pages.reset())
  })

  const list = resource((signal) =>
    following
      ? Promise.resolve(undefined)
      : api.cache.requests(
          {
            range,
            client: client || undefined,
            service: service || undefined,
            search: search || undefined,
            status: status || undefined,
            cursor: pages.current || undefined,
            limit: PAGE,
          },
          { signal },
        ),
  )

  // ---- live view

  let liveRows = $state.raw<CacheEvent[]>([])
  let stream = $state.raw<LiveStream<CacheEvent> | null>(null)

  // Streamed events have no database id yet (id 0): number them locally with
  // negative ids so rows have unique keys and can be selected.
  let liveSeq = 0

  $effect(() => {
    if (!following) return
    liveRows = []
    const s = streamCache({
      onEvents: (batch) => {
        const numbered = batch.map((e) => ({ ...e, id: -++liveSeq }))
        liveRows = [...numbered.reverse(), ...liveRows].slice(0, MAX_LIVE)
      },
    })
    stream = s
    return () => {
      s.close()
      stream = null
    }
  })

  /** Client-side version of the server filters for the live view. */
  function matches(e: CacheEvent): boolean {
    if (service && e.service !== service) return false
    if (status && e.cacheStatus !== status.toUpperCase()) return false
    if (client) {
      const c = client.toLowerCase()
      if (isIP(client) ? e.clientIp !== client : !(e.clientIp.includes(c) || (e.clientName ?? '').toLowerCase().includes(c)))
        return false
    }
    if (search) {
      const s = search.toLowerCase()
      if (!e.host.includes(s) && !e.path.toLowerCase().includes(s)) return false
    }
    return true
  }

  const rows = $derived(following ? liveRows.filter(matches) : list.data?.items)

  function setFollowing(on: boolean) {
    router.setQuery({ live: on, request: null })
  }

  // ---- details

  const selectedId = $derived(Number(router.param('request')) || 0)
  const selected = $derived(selectedId !== 0 ? rows?.find((e) => e.id === selectedId) : undefined)

  const columns: Column<CacheEvent>[] = $derived([
    { key: 'time', label: t('common.label.time'), cell: timeCell },
    { key: 'status', label: t('common.label.status'), cell: statusCell },
    { key: 'client', label: t('common.label.client'), cell: clientCell },
    { key: 'service', label: t('common.label.service'), format: (e) => catalog.name(e.service) },
    { key: 'host', label: t('cache.col.host'), cell: hostCell },
    { key: 'path', label: t('cache.col.path'), mono: true, truncate: true, width: '30%', value: (e) => e.path },
    { key: 'sent', label: t('cache.col.sent'), align: 'right', format: (e) => formatBytes(e.bytesSent) },
    { key: 'hit', label: t('cache.col.fromCache'), align: 'right', format: (e) => formatHitRatio(e.bytesHit, e.bytesWan) },
    { key: 'duration', label: t('cache.col.duration'), align: 'right', format: (e) => formatDuration(e.durationMs) },
  ])

  const streamLabel = $derived.by(() => {
    switch (stream?.state) {
      case 'open':
        return { tone: 'ok' as const, text: t('cache.requests.liveOpen') }
      case 'paused':
        return { tone: 'neutral' as const, text: t('cache.requests.livePaused') }
      case 'retrying':
        return { tone: 'warn' as const, text: t('cache.requests.liveRetrying') }
      default:
        return { tone: 'neutral' as const, text: t('cache.requests.liveConnecting') }
    }
  })
</script>

{#snippet timeCell(e: CacheEvent)}
  <span class="nowrap" title={formatDateTime(e.time, true)}>{formatTime(e.time, true)}</span>
{/snippet}

{#snippet statusCell(e: CacheEvent)}
  <CacheStatusChip status={e.cacheStatus} />
{/snippet}

{#snippet hostCell(e: CacheEvent)}
  <span class="mono nowrap">{e.host}</span>
{/snippet}

{#snippet clientCell(e: CacheEvent)}
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
    rangeDefault="1h"
    searchLabel={t('cache.filters.hostOrPath')}
    searchPlaceholder={t('cache.filters.hostOrPathPlaceholder')}
    statuses={statusOptions}
    statusLabel={t('cache.col.result')}
  >
    {#snippet actions()}
      {#if following}
        <Chip size="sm" tone={streamLabel.tone} label={streamLabel.text} />
      {/if}
      <Toggle label={t('cache.requests.live')} checked={following} onchange={setFollowing} />
      {#if !following}
        <IconButton icon="refresh" variant="secondary" label={t('common.action.refresh')} loading={list.loading} onclick={() => list.refresh()} />
      {/if}
    {/snippet}
  </Filters>

  <Panel flush>
    {#if following}
      <p class="note">{t('cache.requests.liveNote', { count: MAX_LIVE })}</p>
    {/if}
    {#if rows && rows.length === 0 && (following || !list.error)}
      {#if following}
        <EmptyState compact icon="activity" title={t('cache.requests.liveEmpty')} />
      {:else}
        <EmptyState compact icon="list" title={t('cache.requests.empty')} text={t('cache.requests.emptyText')} />
      {/if}
    {:else}
      <Table
        compact
        caption={t('cache.requests.caption')}
        {rows}
        key={(e) => e.id}
        {columns}
        loading={!following && list.loading && !list.loaded}
        error={!following && list.error ? errorText(list.error) : undefined}
        onretry={() => list.refresh()}
        onrowclick={(e) => router.setQuery({ request: e.id }, { push: true })}
        selected={selected?.id}
      />
    {/if}
    {#if !following && list.data && (list.data.items.length > 0 || pages.hasPrev)}
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
  bind:open={() => !!selected, (v) => !v && router.setQuery({ request: null })}
  title={selected ? selected.host : ''}
  subtitle={selected ? formatDateTime(selected.time, true) : undefined}
>
  {#if selected}
    {@const e = selected}
    <div class="stack">
      <p><CacheStatusChip status={e.cacheStatus} size="md" /></p>
      <KeyValue
        items={[
          { label: t('common.label.time'), value: formatDateTime(e.time, true) },
          { label: t('common.label.client'), value: e.clientName ? `${e.clientName} (${e.clientIp})` : e.clientIp },
          { label: t('common.label.service'), value: catalog.name(e.service) },
          { label: t('cache.col.content'), value: e.label || e.groupKey },
          { label: t('cache.detail.groupKey'), value: e.groupKey, mono: true },
          { label: t('cache.detail.request'), value: `${e.method} ${e.path}`, mono: true },
          { label: t('cache.col.host'), value: e.host, mono: true },
          { label: t('cache.detail.range'), value: e.range, mono: true },
          { label: t('cache.detail.httpStatus'), value: String(e.status) },
          { label: t('cache.col.sent'), value: formatBytes(e.bytesSent) },
          { label: t('cache.detail.fromCacheBytes'), value: formatBytes(e.bytesHit) },
          { label: t('cache.col.fromInternet'), value: formatBytes(e.bytesWan) },
          { label: t('cache.detail.stored'), value: formatBytes(e.bytesStored) },
          { label: t('cache.col.duration'), value: formatDuration(e.durationMs) },
          { label: t('cache.detail.userAgent'), value: e.userAgent, mono: true },
          { label: t('cache.detail.eventId'), value: e.id > 0 ? formatNumber(e.id) : undefined },
        ]}
      />
    </div>
  {/if}
</SidePanel>

<style>
  .note {
    padding: 0 var(--sp-4) var(--sp-3);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
</style>
