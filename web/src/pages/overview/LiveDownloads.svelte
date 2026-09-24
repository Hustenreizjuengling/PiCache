<!--
  @component
  Downloads running right now (per client and content), refreshed every 3 s.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { api, resource, type ActiveDownload } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import { formatBytes, formatPercent, formatRate } from '../../lib/format'
  import { router } from '../../lib/router.svelte'
  import { EmptyState, Table, type Column } from '../../lib/ui'

  const MAX_ROWS = 8

  const live = resource((signal) => api.cache.live({ signal }), { interval: 3000 })

  const rows = $derived(
    live.data ? [...live.data].sort((a, b) => b.rateBps - a.rateBps || b.bytesSent - a.bytesSent).slice(0, MAX_ROWS) : undefined,
  )

  function hitRatio(d: ActiveDownload): number {
    const total = d.bytesHit + d.bytesWan
    return total > 0 ? d.bytesHit / total : 0
  }

  const rowKey = (d: ActiveDownload) => `${d.clientIp}|${d.service}|${d.groupKey}`

  const columns: Column<ActiveDownload>[] = $derived([
    { key: 'content', label: t('overview.live.content'), cell: contentCell },
    { key: 'client', label: t('overview.live.client'), value: (d) => d.clientName || d.clientIp },
    { key: 'rate', label: t('overview.live.speed'), align: 'right', format: (d) => formatRate(d.rateBps) },
    { key: 'hit', label: t('overview.live.fromCache'), align: 'right', format: (d) => formatPercent(hitRatio(d)) },
    { key: 'sent', label: t('overview.live.sent'), align: 'right', format: (d) => formatBytes(d.bytesSent) },
  ])
</script>

{#snippet contentCell(d: ActiveDownload)}
  <span class="content">
    <span class="label" title={d.groupKey}>{d.label || d.groupKey}</span>
    <span class="service">{d.service}</span>
  </span>
{/snippet}

<div class="live">
  <h3>{t('overview.live.title')}</h3>
  <Table
    compact
    caption={t('overview.live.title')}
    {rows}
    key={rowKey}
    {columns}
    loading={live.loading && !live.loaded}
    error={live.error && !live.data ? errorText(live.error) : undefined}
    onretry={() => live.refresh()}
    skeletonRows={3}
    onrowclick={(d) => router.navigate('/cache/downloads', { client: d.clientIp, active: true })}
  >
    {#snippet empty()}
      <EmptyState compact icon="download" title={t('overview.live.empty')} text={t('overview.live.emptyText')} />
    {/snippet}
  </Table>
</div>

<style>
  .live {
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
  .content {
    display: flex;
    flex-direction: column;
    min-width: 0;
    max-width: 32ch;
  }
  .label {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .service {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
</style>
