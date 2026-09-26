<!--
  @component
  Query log: every DNS query with its status, filterable by time range
  (a preset or a custom window), client, domain, status, record type,
  upstream, reply code and DNSSEC (all in the URL), paged by cursor. "Live"
  follows new queries over SSE (pause/resume; not with a custom window,
  which may end in the past: turning Live on returns to the default range
  and ?live=true is ignored while one is set); a row opens the details
  panel. Export downloads the filtered log (NDJSON or CSV); admins may clear
  the query log or reset the statistics from the menu (when the host allows
  destructive actions).
  Query: ?range|from&to&client&domain&status&qtype&upstream&rcode&dnssec&live=true;
  client and rcode may be repeated (every address of one device, any of the codes).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    resource,
    streamQueries,
    type LiveStream,
    type QueryEvent,
  } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatDateTime, formatMicros, formatNumber, formatTime } from '$lib/format'
  import { isCustom } from '$lib/range'
  import { router, type QueryPatch } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { appStatus } from '$lib/status.svelte'
  import {
    Button,
    Chip,
    CursorStack,
    EmptyState,
    IconButton,
    Menu,
    Notice,
    Pager,
    Panel,
    QueryStatusChip,
    Table,
    Toggle,
    type Column,
    type MenuItem,
  } from '$lib/ui'
  import { clearQueryLog, resetStatistics } from '../system/logs/clear'
  import { apiQuery, CLEAR_FILTERS, exportQuery, hasFilters, matchesLocally, readFilters, streamClient } from './querylog/filters'
  import QueryFilters from './querylog/QueryFilters.svelte'
  import QueryPanel from './querylog/QueryPanel.svelte'

  /** Rows kept in live mode (newest first). */
  const MAX_LIVE = 500

  type Row = QueryEvent & { key: string }

  const filters = $derived(readFilters())
  /** A custom window (?from&to) may end in the past: live events would not belong to it. */
  const customRange = $derived(isCustom(filters.range))
  const live = $derived(router.param('live') === 'true' && !customRange)
  const filterKey = $derived(JSON.stringify(filters))

  // Cursor pages belong to one set of filters; new filters start at page 1.
  const pages = new CursorStack()
  let pagesKey = $state('')
  const cursor = $derived(pagesKey === filterKey ? pages.current : '')
  const hasPrev = $derived(pagesKey === filterKey && pages.hasPrev)

  // In live mode this is the newest page the stream continues from.
  const log = resource((signal) => api.logs.queries(apiQuery(filters, live ? '' : cursor), { signal }))
  const groups = resource((signal) => api.groups.list({ signal }))
  const lists = resource((signal) => api.filter.lists.list({ signal }))
  // For the panel: the rebinding allow list and whether client addresses are anonymised.
  const settings = resource((signal) => api.settings.get({ signal }))

  let liveRows = $state.raw<Row[]>([])
  let stream = $state<LiveStream<QueryEvent> | null>(null)
  let localSeq = 0

  // Live events are not stored yet (id 0): they are keyed by their stream
  // sequence number, or locally (older servers; a number seen again after a
  // server restart).
  function liveKey(e: QueryEvent, taken: Set<string>): string {
    let k = e.seq !== undefined ? `s${e.seq}` : ''
    if (!k || taken.has(k)) k = `l${++localSeq}`
    taken.add(k)
    return k
  }

  $effect(() => {
    if (!live) return
    const f = filters
    const s = streamQueries(
      // The stream takes one client; several (a device's addresses) are matched here.
      { client: streamClient(f), status: f.status.length > 0 ? f.status : undefined },
      {
        onEvents: (batch) => {
          const cur = readFilters()
          const taken = new Set(liveRows.map((r) => r.key))
          const add: Row[] = []
          for (let i = batch.length - 1; i >= 0; i--) {
            if (matchesLocally(batch[i], cur)) add.push({ ...batch[i], key: liveKey(batch[i], taken) })
          }
          if (add.length > 0) liveRows = [...add, ...liveRows].slice(0, MAX_LIVE)
        },
        // Events may have been missed: continue from a fresh page.
        onReconnect: async () => {
          await log.refresh()
          liveRows = []
        },
      },
    )
    untrack(() => {
      liveRows = []
      stream = s
    })
    return () => {
      s.close()
      stream = null
    }
  })

  const pageRows = $derived<Row[]>((log.data?.items ?? []).map((e) => ({ ...e, key: `q${e.id}` })))
  const rows = $derived(live ? [...liveRows, ...pageRows].slice(0, MAX_LIVE) : pageRows)

  const upstreams = $derived((appStatus.overview.data?.upstreams ?? []).map((u) => u.upstream))
  /** Reply codes of the rows shown (suggestions of the reply-code filter). */
  const rcodes = $derived([...new Set(rows.map((r) => r.rcode.toUpperCase()).filter(Boolean))])

  // A plain download link with the current filters (the browser sends the session cookie).
  const exportItems = $derived<MenuItem[]>([
    { note: t('dns.queryLog.export.note') },
    { label: t('dns.queryLog.export.ndjson'), icon: 'download', href: api.logs.exportUrl('ndjson', exportQuery(filters)), download: true },
    { label: t('dns.queryLog.export.csv'), icon: 'download', href: api.logs.exportUrl('csv', exportQuery(filters)), download: true },
  ])
  const clearItems = $derived<MenuItem[]>([
    {
      label: t('system.logs.clear.queries'),
      icon: 'trash',
      danger: true,
      onselect: async () => {
        if (await clearQueryLog()) {
          liveRows = []
          void log.refresh()
        }
      },
    },
    { label: t('system.logs.clear.stats'), icon: 'trash', danger: true, onselect: () => void resetStatistics() },
  ])

  const filterErrors = $derived({
    client: fieldError(log.error, 'client'),
    domain: fieldError(log.error, 'domain'),
  })
  const loadError = $derived(log.error && !filterErrors.client && !filterErrors.domain ? errorText(log.error) : undefined)

  let selected = $state.raw<Row | undefined>(undefined)
  let panelOpen = $state(false)

  function openRow(r: Row) {
    selected = r
    panelOpen = true
    // The panel's actions depend on settings another tab may have changed meanwhile.
    void settings.refresh()
  }

  function setFilters(patch: QueryPatch) {
    router.setQuery(patch)
  }

  function setLive(on: boolean) {
    // Live follows the queries arriving now: a custom window gives way to the default range.
    router.setQuery(on && customRange ? { live: on, from: null, to: null } : { live: on })
  }

  function next() {
    const n = log.data?.next
    if (!n) return
    if (pagesKey !== filterKey) {
      pages.reset()
      pagesKey = filterKey
    }
    pages.next(n)
  }

  const today = $derived(new Date().toDateString())

  function timeText(iso: string): string {
    return new Date(iso).toDateString() === today ? formatTime(iso, true) : formatDateTime(iso, true)
  }

  const columns: Column<Row>[] = $derived([
    { key: 'time', label: t('common.label.time'), width: '1%', cell: timeCell },
    { key: 'client', label: t('common.label.client'), cell: clientCell },
    { key: 'domain', label: t('common.label.domain'), width: '40%', value: (e) => e.qname, cell: domainCell },
    { key: 'type', label: t('common.label.type'), width: '1%', value: (e) => e.qtype },
    { key: 'status', label: t('common.label.status'), width: '1%', cell: statusCell },
    {
      key: 'duration',
      label: t('dns.queryLog.duration'),
      align: 'right',
      width: '1%',
      value: (e) => e.durationUs,
      format: (e) => formatMicros(e.durationUs),
    },
  ])

  const streamLabel = $derived.by(() => {
    switch (stream?.state) {
      case 'open':
        return t('dns.queryLog.live.open')
      case 'paused':
        return t('dns.queryLog.live.paused')
      case 'retrying':
        return t('dns.queryLog.live.retrying')
      default:
        return t('dns.queryLog.live.connecting')
    }
  })
  const streamTone = $derived(stream?.state === 'open' ? 'ok' : stream?.state === 'retrying' ? 'warn' : 'neutral')
</script>

{#snippet timeCell(e: Row)}
  <span class="nowrap num" title={formatDateTime(e.time, true)}>{timeText(e.time)}</span>
{/snippet}

{#snippet clientCell(e: Row)}
  {#if e.clientName}
    <span class="client" title={e.clientIp}><span class="truncate">{e.clientName}</span><span class="ip mono">{e.clientIp}</span></span>
  {:else}
    <span class="mono nowrap">{e.clientIp}</span>
  {/if}
{/snippet}

{#snippet domainCell(e: Row)}
  {#if e.qname === 'hidden'}
    <span class="subtle" title={t('dns.queryLog.hiddenDomain')}>{t('dns.queryLog.hiddenShort')}</span>
  {:else}
    <span class="domain mono" title={e.qname}>{e.qname}</span>
  {/if}
{/snippet}

{#snippet statusCell(e: Row)}
  <QueryStatusChip status={e.status} />
{/snippet}

<div class="page">
  <QueryFilters
    {filters}
    {live}
    {upstreams}
    {rcodes}
    retentionHours={settings.data?.logs.queryLogRetentionHours}
    errors={filterErrors}
    onchange={setFilters}
  />

  {#if appStatus.overview.data && !appStatus.overview.data.blocking.enabled}
    <Notice tone="warn">{t('dns.queryLog.blockingOff')}</Notice>
  {/if}

  <Panel flush>
    <div class="head">
      <Toggle
        bind:checked={() => live, setLive}
        label={t('dns.queryLog.live.label')}
        description={customRange ? t('dns.queryLog.live.customRange') : undefined}
      />
      {#if live && stream}
        <Chip size="sm" tone={streamTone} label={streamLabel} />
        {#if stream.paused}
          <IconButton icon="play" variant="secondary" size="sm" label={t('dns.queryLog.live.resume')} onclick={() => stream?.resume()} />
        {:else}
          <IconButton icon="pause" variant="secondary" size="sm" label={t('dns.queryLog.live.pause')} onclick={() => stream?.pause()} />
        {/if}
        <span class="small muted">{t('dns.queryLog.live.count', { count: formatNumber(rows.length), max: formatNumber(MAX_LIVE) })}</span>
      {/if}
      <span class="spacer"></span>
      <Menu label={t('dns.queryLog.export.label')} icon="download" size="sm" items={exportItems} />
      {#if session.canDestroy}
        <Menu label={t('common.action.more')} icon="more" iconOnly size="sm" variant="ghost" items={clearItems} />
      {/if}
      {#if !live}
        <IconButton icon="refresh" size="sm" label={t('common.action.refresh')} loading={log.loading} onclick={() => log.refresh()} />
      {/if}
    </div>

    <Table
      {columns}
      {rows}
      key={(r) => r.key}
      loading={log.loading && !log.loaded}
      error={loadError}
      onretry={() => log.refresh()}
      onrowclick={openRow}
      selected={panelOpen ? selected?.key : undefined}
      compact
      maxHeight="max(360px, calc(100vh - 320px))"
      caption={t('dns.queryLog.caption')}
    >
      {#snippet empty()}
        {#if live}
          <EmptyState compact icon="activity" title={t('dns.queryLog.live.waiting')} text={t('dns.queryLog.live.waitingText')} />
        {:else if hasFilters(filters)}
          <EmptyState compact icon="filter" title={t('dns.queryLog.emptyFiltered')} text={t('dns.queryLog.emptyFilteredText')}>
            <Button size="sm" onclick={() => setFilters(CLEAR_FILTERS)}>
              {t('dns.queryLog.filter.clear')}
            </Button>
          </EmptyState>
        {:else}
          <EmptyState compact icon="list" title={t('dns.queryLog.empty')} text={t('dns.queryLog.emptyText')} />
        {/if}
      {/snippet}
    </Table>

    {#if !live}
      <Pager
        mode="cursor"
        {hasPrev}
        hasNext={!!log.data?.next}
        count={log.data?.items.length}
        onprev={() => pages.prev()}
        onnext={next}
        onfirst={() => pages.reset()}
      />
    {/if}
  </Panel>
</div>

<QueryPanel
  bind:open={panelOpen}
  event={selected}
  groups={groups.data}
  lists={lists.data}
  settings={settings.data}
  onsettings={(s) => settings.set(s)}
  onfilter={setFilters}
/>

<style>
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border-bottom: 1px solid var(--line);
  }
  .client {
    display: inline-flex;
    align-items: baseline;
    gap: 6px;
    min-width: 0;
    max-width: 34ch;
    white-space: nowrap;
  }
  .domain {
    display: block;
    min-width: 16ch;
    max-width: 60ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .ip {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
</style>
