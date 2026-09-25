<!--
  @component
  Download sessions: one client downloading one content group (a new session
  starts after a 2-minute pause). Filters, page and the opened session are in
  the URL (?session=<id>).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, type ActiveDownload, type Download, type RangePreset } from '$lib/api'
  import { errorText } from '$lib/errors'
  import {
    formatBytes,
    formatDateTime,
    formatDateTimeShort,
    formatDuration,
    formatNumber,
    formatRate,
    formatRelative,
    formatTime,
    sameDay,
  } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { loadPref, savePref } from '$lib/storage'
  import { Button, Chip, EmptyState, KeyValue, Pager, Panel, SidePanel, Table, type Column } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'
  import { averageRate, formatHitRatio, offsetParam, pickRange, spanMs, timeWindow } from '../shared/util'
  import Filters from './Filters.svelte'

  interface Props {
    catalog: ServiceCatalog
    /** Live downloads (for the current speed of active sessions). */
    live: ActiveDownload[] | undefined
  }

  let { catalog, live }: Props = $props()

  const RANGES: RangePreset[] = ['1h', '24h', '7d', '30d', '90d']
  const LIMITS = [25, 50, 100, 250]

  const range = $derived(pickRange(router.param('range'), RANGES, '7d'))
  const win = $derived(timeWindow(router.param('from'), router.param('to')))
  /** An explicit window from the URL (e.g. the query log's "Cache traffic" link) replaces the range. */
  const time = $derived(win ?? { range })
  const offset = $derived(offsetParam(router.param('offset')))
  let limit = $state(LIMITS.includes(Number(loadPref('cache.downloads.limit'))) ? Number(loadPref('cache.downloads.limit')) : 50)

  const list = resource(
    (signal) =>
      api.cache.downloads(
        {
          ...time,
          client: router.param('client') || undefined,
          service: router.param('service') || undefined,
          search: router.param('search') || undefined,
          group: router.param('group') || undefined,
          active: router.param('active') === 'true' || undefined,
          limit,
          offset,
        },
        { signal },
      ),
    { interval: 10_000 },
  )

  const liveKey = (d: { clientIp: string; service: string; groupKey: string }) => `${d.clientIp}|${d.service}|${d.groupKey}`
  const liveByKey = $derived(new Map((live ?? []).map((d) => [liveKey(d), d])))

  /**
   * The live entry of an active session when it covers the whole session
   * (it started with the session's first request; after a pause of 30 s to
   * 2 min the live entry covers only the part since then). Its counts are the
   * ones "Downloading now" shows: they include requests still running and
   * update every 3 s, while the session log is written when requests end.
   */
  function liveOf(d: Download): ActiveDownload | undefined {
    if (!d.active) return undefined
    const l = liveByKey.get(liveKey(d))
    return l && Date.parse(l.started) <= Date.parse(d.firstSeen) + 1000 ? l : undefined
  }

  /** A session with the live counts of a running download. */
  function current(d: Download): Download {
    const l = liveOf(d)
    return l ? { ...d, bytesSent: l.bytesSent, bytesHit: l.bytesHit, bytesWan: l.bytesWan } : d
  }

  /** Current speed of an active session, else its average speed. */
  function speed(d: Download): number | null {
    if (d.active) {
      const r = liveByKey.get(liveKey(d))?.rateBps
      if (r !== undefined) return r
    }
    return averageRate(d.bytesSent, d.firstSeen, d.lastSeen)
  }

  const rows = $derived(list.data?.items.map(current))

  // A download that starts or ends in "Downloading now" shows in the list at once, not with the next poll.
  let liveKeys: string | undefined
  $effect(() => {
    if (!live) return
    const keys = [...liveByKey.keys()].sort().join('\n')
    if (liveKeys !== undefined && keys !== liveKeys) untrack(() => void list.refresh())
    liveKeys = keys
  })

  const selectedId = $derived(Number(router.param('session')) || 0)
  const selected = $derived(rows?.find((d) => d.id === selectedId))

  function open(d: Download) {
    router.setQuery({ session: d.id }, { push: true })
  }

  function close() {
    router.setQuery({ session: null })
  }

  /** The requests of a session: its client and service; a finished one also its time (the default range is 1 h). */
  function showRequests(d: Download) {
    const from = Math.floor(new Date(d.firstSeen).getTime() / 1000)
    const to = Math.floor(new Date(d.lastSeen).getTime() / 1000) + 1
    const bounded = !d.active && Number.isFinite(from) && Number.isFinite(to) && from < to
    router.setQuery({
      tab: 'requests',
      client: d.clientIp,
      service: d.service,
      search: null,
      status: null,
      active: null,
      group: null,
      range: null,
      from: bounded ? from : null,
      to: bounded ? to : null,
      offset: null,
      session: null,
    })
  }

  // Every other column is one short line; the content label takes the rest of
  // the width and wraps when that is narrow (it is the column people read).
  const columns: Column<Download>[] = $derived([
    { key: 'content', label: t('cache.col.content'), width: '100%', cell: contentCell },
    { key: 'service', label: t('common.label.service'), cell: serviceCell },
    { key: 'client', label: t('common.label.client'), cell: clientCell },
    { key: 'started', label: t('cache.col.started'), cell: startedCell },
    { key: 'ended', label: t('cache.col.ended'), cell: endedCell },
    { key: 'sent', label: t('cache.col.downloaded'), align: 'right', format: (d) => formatBytes(d.bytesSent) },
    { key: 'hit', label: t('cache.col.fromCache'), align: 'right', format: (d) => formatHitRatio(d.bytesHit, d.bytesWan) },
    { key: 'wan', label: t('cache.col.fromInternet'), align: 'right', format: (d) => formatBytes(d.bytesWan) },
    { key: 'speed', label: t('cache.col.speed'), align: 'right', format: (d) => formatRate(speed(d)) },
  ])
</script>

{#snippet contentCell(d: Download)}
  <span class="content" title={d.groupKey}>{d.label || d.groupKey}</span>
{/snippet}

{#snippet serviceCell(d: Download)}
  <span class="nowrap">{catalog.name(d.service)}</span>
{/snippet}

{#snippet clientCell(d: Download)}
  {#if d.clientName}
    <span class="nowrap" title={d.clientIp}>{d.clientName}</span>
  {:else}
    <span class="mono nowrap">{d.clientIp}</span>
  {/if}
{/snippet}

<!-- Compact times (full date and time on hover); the end shows only the time on the day the download started. -->
{#snippet startedCell(d: Download)}
  <span class="nowrap" title={formatDateTime(d.firstSeen, true)}>{formatDateTimeShort(d.firstSeen)}</span>
{/snippet}

{#snippet endedCell(d: Download)}
  {#if d.active}
    <Chip size="sm" tone="info" label={t('cache.downloads.active')} />
  {:else}
    <span class="nowrap" title={formatDateTime(d.lastSeen, true)}>
      {sameDay(d.firstSeen, d.lastSeen) ? formatTime(d.lastSeen) : formatDateTimeShort(d.lastSeen)}
    </span>
  {/if}
{/snippet}

<div class="stack">
  <Filters
    {catalog}
    ranges={RANGES}
    rangeDefault="7d"
    searchLabel={t('cache.filters.content')}
    searchPlaceholder={t('cache.filters.contentPlaceholder')}
    active
    group
  />

  <Panel flush>
    {#if list.data && list.data.items.length === 0 && !list.error}
      <EmptyState compact icon="download" title={t('cache.downloads.empty')} text={t('cache.downloads.emptyText')} />
    {:else}
      <Table
        caption={t('cache.downloads.caption')}
        {rows}
        key={(d) => d.id}
        {columns}
        loading={list.loading && !list.loaded}
        error={list.error ? errorText(list.error) : undefined}
        onretry={() => list.refresh()}
        onrowclick={open}
        selected={selected?.id}
      />
    {/if}
    {#if list.data && list.data.total > 0}
      <Pager
        total={list.data.total}
        bind:limit
        {offset}
        limits={LIMITS}
        onlimit={(l) => {
          savePref('cache.downloads.limit', String(l))
          router.setQuery({ offset: null })
        }}
        onchange={(o) => router.setQuery({ offset: o || null })}
      />
    {/if}
  </Panel>
</div>

<SidePanel
  bind:open={() => !!selected, (v) => !v && close()}
  title={selected ? selected.label || selected.groupKey : ''}
  subtitle={selected ? catalog.name(selected.service) : undefined}
>
  {#if selected}
    {@const d = selected}
    <div class="stack">
      {#if d.active}
        <p><Chip tone="info" label={t('cache.downloads.activeNow')} /></p>
      {/if}
      <KeyValue
        items={[
          { label: t('cache.col.content'), value: d.label || d.groupKey },
          { label: t('cache.detail.groupKey'), value: d.groupKey, mono: true },
          { label: t('common.label.service'), value: catalog.name(d.service) },
          { label: t('common.label.client'), value: d.clientName ? `${d.clientName} (${d.clientIp})` : d.clientIp },
          { label: t('cache.col.started'), value: formatDateTime(d.firstSeen, true) },
          { label: t('cache.detail.lastRequest'), value: `${formatDateTime(d.lastSeen, true)} (${formatRelative(d.lastSeen)})` },
          { label: t('cache.detail.duration'), value: formatDuration(spanMs(d.firstSeen, d.lastSeen)) },
          { label: t('cache.detail.requests'), value: formatNumber(d.requests) },
          { label: t('cache.col.downloaded'), value: formatBytes(d.bytesSent) },
          { label: t('cache.detail.fromCacheBytes'), value: `${formatBytes(d.bytesHit)} (${formatHitRatio(d.bytesHit, d.bytesWan)})` },
          { label: t('cache.col.fromInternet'), value: formatBytes(d.bytesWan) },
          { label: d.active ? t('cache.detail.currentSpeed') : t('cache.detail.averageSpeed'), value: formatRate(speed(d)) },
        ]}
      />
    </div>
  {/if}
  {#snippet actions()}
    {#if selected}
      <Button icon="list" onclick={() => showRequests(selected)}>{t('cache.downloads.showRequests')}</Button>
      <Button icon="layers" href={href('/cache/library', { group: selected.groupKey, groupService: selected.service })}>
        {t('cache.downloads.showInLibrary')}
      </Button>
      <Button icon="users" href={href('/dns/clients', { ip: selected.clientIp })}>{t('cache.downloads.showClient')}</Button>
    {/if}
  {/snippet}
</SidePanel>

<style>
  /* Never narrower than a readable label (the table scrolls sideways
     instead); long IDs without spaces break anywhere. */
  .content {
    display: block;
    min-width: 12em;
    overflow-wrap: anywhere;
  }
</style>
