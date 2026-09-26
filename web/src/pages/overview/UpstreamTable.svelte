<!--
  @component
  Upstreams of the range (GET /stats/top?kind=upstreams): each upstream's
  share of the forwarded queries and its average response time. A row links
  to the queries it answered.
-->
<script lang="ts">
  import type { TopItem } from '../../lib/api'
  import { t } from '../../i18n/index.svelte'
  import { formatMicros, formatNumber, formatPercent } from '../../lib/format'
  import { Table, type Column } from '../../lib/ui'

  interface Props {
    /** Shown after the title (the start the list covers). */
    note?: string
    noteTitle?: string
    items?: TopItem[]
    loading?: boolean
    error?: string
    onretry?: () => void
    link: (item: TopItem) => string
  }

  let { note, noteTitle, items, loading = false, error, onretry, link }: Props = $props()

  const total = $derived((items ?? []).reduce((n, i) => n + i.count, 0))

  function avg(it: TopItem): number | undefined {
    return it.avgDurationUs ?? it.bytes
  }

  const columns: Column<TopItem>[] = $derived([
    { key: 'key', label: t('overview.dns.upstream'), width: '100%', cell: keyCell },
    { key: 'share', label: t('overview.dns.share'), align: 'right', width: '1%', cell: shareCell },
    { key: 'avg', label: t('overview.dns.avgTime'), align: 'right', width: '1%', format: (it) => formatMicros(avg(it)) },
  ])
</script>

{#snippet keyCell(it: TopItem)}
  <a class="key mono" href={link(it)} title={it.key}>{it.key}</a>
{/snippet}

{#snippet shareCell(it: TopItem)}
  {@const share = total > 0 ? it.count / total : 0}
  <span class="count" title={t('overview.dns.upstreamQueries', { count: formatNumber(it.count) })}>
    <span class="bar" aria-hidden="true"><span style:width="{share * 100}%"></span></span>
    <span class="n">{formatPercent(share)}</span>
  </span>
{/snippet}

<div class="top">
  <h3>{t('overview.dns.upstreams')}{#if note}<span class="note" title={noteTitle}>· {note}</span>{/if}</h3>
  <Table
    compact
    rows={items}
    key={(it) => it.key}
    {loading}
    {error}
    {onretry}
    skeletonRows={3}
    emptyText={t('overview.dns.noUpstreams')}
    caption={t('overview.dns.upstreams')}
    {columns}
  />
</div>

<style>
  .top {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
  }
  h3 {
    padding: 0 var(--sp-4);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .note {
    margin-left: 0.35em;
    font-weight: 400;
    color: var(--text-3);
  }
  /* The address may be long (a DoH URL): it wraps instead of pushing the numbers out of view. */
  .key {
    display: block;
    padding: 4px 0;
    overflow-wrap: anywhere;
    line-height: 1.3;
    color: var(--text);
    text-decoration: none;
    font-family: var(--font-mono);
  }
  .key:hover {
    text-decoration: underline;
  }
  .count {
    display: inline-flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--sp-2);
    width: 100%;
  }
  .bar {
    flex: none;
    width: 72px;
    height: 6px;
    border-radius: var(--r-pill);
    background: var(--surface-3);
    overflow: hidden;
    display: flex;
    justify-content: flex-end;
  }
  .bar span {
    height: 100%;
    border-radius: var(--r-pill);
    background: var(--blue);
  }
  .n {
    min-width: 5ch;
    text-align: right;
  }
  /* Phones: the percentage alone leaves room for the upstream's name. */
  @media (max-width: 480px) {
    .bar {
      display: none;
    }
  }
</style>
