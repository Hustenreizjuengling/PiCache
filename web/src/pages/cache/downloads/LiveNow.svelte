<!--
  @component
  Downloads running right now, per client and content (/cache/live, polled
  by the page). A row filters the download list to that client's active
  downloads.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ActiveDownload, ApiError } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatRate, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { EmptyState, Panel, Table, type Column } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'
  import { formatHitRatio } from '../shared/util'

  interface Props {
    rows: ActiveDownload[] | undefined
    loading: boolean
    error: ApiError | undefined
    onretry: () => void
    catalog: ServiceCatalog
  }

  let { rows, loading, error, onretry, catalog }: Props = $props()

  const sorted = $derived(rows ? [...rows].sort((a, b) => b.rateBps - a.rateBps || b.bytesSent - a.bytesSent) : undefined)

  const rowKey = (d: ActiveDownload) => `${d.clientIp}|${d.service}|${d.groupKey}`

  const columns: Column<ActiveDownload>[] = $derived([
    { key: 'content', label: t('cache.col.content'), cell: contentCell },
    { key: 'client', label: t('common.label.client'), cell: clientCell },
    { key: 'rate', label: t('cache.col.speed'), align: 'right', format: (d) => formatRate(d.rateBps) },
    { key: 'hit', label: t('cache.col.fromCache'), align: 'right', format: (d) => formatHitRatio(d.bytesHit, d.bytesWan) },
    { key: 'sent', label: t('cache.col.downloaded'), align: 'right', format: (d) => formatBytes(d.bytesSent) },
    { key: 'wan', label: t('cache.col.fromInternet'), align: 'right', format: (d) => formatBytes(d.bytesWan) },
    { key: 'started', label: t('cache.col.started'), align: 'right', format: (d) => formatRelative(d.started) },
  ])

  function open(d: ActiveDownload) {
    router.setQuery({ tab: null, client: d.clientIp, active: true, service: null, search: null, group: null, offset: null })
  }
</script>

{#snippet contentCell(d: ActiveDownload)}
  <span class="two">
    <span class="main" title={d.groupKey}>{d.label || d.groupKey}</span>
    <span class="sub">{catalog.name(d.service)}</span>
  </span>
{/snippet}

{#snippet clientCell(d: ActiveDownload)}
  <span class="two">
    {#if d.clientName}<span class="main">{d.clientName}</span>{/if}
    <span class={[d.clientName ? 'sub' : 'main', 'mono']}>{d.clientIp}</span>
  </span>
{/snippet}

<Panel flush title={t('cache.live.title')} description={t('cache.live.description')}>
  {#if rows && rows.length === 0}
    <!-- Outside the table: on narrow screens a wide empty table would cut the text off. -->
    <EmptyState compact icon="download" title={t('cache.live.empty')} text={t('cache.live.emptyText')} />
  {:else}
    <Table
      compact
      caption={t('cache.live.title')}
      rows={sorted}
      key={rowKey}
      {columns}
      loading={loading && !rows}
      error={error && !rows ? errorText(error) : undefined}
      {onretry}
      skeletonRows={2}
      onrowclick={open}
    />
  {/if}
</Panel>

<style>
  .two {
    display: flex;
    flex-direction: column;
    min-width: 0;
    max-width: 36ch;
  }
  .main {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sub {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
</style>
