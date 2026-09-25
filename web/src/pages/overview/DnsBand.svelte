<!--
  @component
  Overview band "DNS": allowed/blocked queries per minute (stacked), top
  blocked domains and top clients (one row per device with all its
  addresses; its link shows the queries of every address).
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { api, resource, type RangePreset } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import { formatCompact, formatNumber, formatTime } from '../../lib/format'
  import { seriesRates } from '../../lib/series'
  import { Button, Chart } from '../../lib/ui'
  import Band from './Band.svelte'
  import { links } from './links'
  import TopTable from './TopTable.svelte'
  import { topWindow } from './topWindow'

  let { range }: { range: RangePreset } = $props()

  const REFRESH = 60_000

  // asOf: the end of the range, for the rate of the bucket still in progress.
  const series = resource(async (signal) => ({ asOf: Date.now(), s: await api.stats.dns(range, undefined, { signal }) }), {
    interval: REFRESH,
  })
  // Top lists are hourly: short ranges get an hour-aligned window, labelled with its start.
  // Clients are grouped by device, so a phone with changing IPv6 addresses is one row.
  async function top(kind: 'blocked' | 'clients', signal: AbortSignal) {
    const w = topWindow(range) // read before the first await: reloads when the range changes
    const group = kind === 'clients' ? 'device' : undefined
    return { since: w.since, items: await api.stats.top(kind, w.arg, 10, { signal, group }) }
  }
  const blocked = resource((signal) => top('blocked', signal), { interval: REFRESH })
  const clients = resource((signal) => top('clients', signal), { interval: REFRESH })

  function since(s: number | undefined): string | undefined {
    return s === undefined ? undefined : t('overview.dns.topSince', { time: formatTime(s * 1000) })
  }

  const chart = $derived.by(() => {
    if (!series.data) return { timestamps: [] as number[], allowed: [] as number[], blocked: [] as number[] }
    const keys = ['allowed', 'cached', 'override', 'other', 'blocked'] as const
    const { timestamps, values: v } = seriesRates(series.data.s, keys, 60, series.data.asOf)
    return {
      timestamps,
      allowed: timestamps.map((_, i) => v.allowed[i] + v.cached[i] + v.override[i] + v.other[i]),
      blocked: v.blocked,
    }
  })
</script>

<Band title={t('overview.dns.title')}>
  {#snippet actions()}
    <Button size="sm" variant="ghost" icon="list" href={links.queries(range)}>{t('overview.dns.openLog')}</Button>
  {/snippet}

  <div class="chart">
    <Chart
      label={t('overview.dns.chartLabel')}
      timestamps={chart.timestamps}
      stacked
      loading={series.loading && !series.loaded}
      series={[
        { label: t('overview.dns.allowed'), pair: 'blue', values: chart.allowed },
        { label: t('overview.dns.blocked'), pair: 'orange', values: chart.blocked },
      ]}
      yFormat={formatCompact}
      valueFormat={(v) => t('overview.unit.perMinute', { n: formatNumber(v, v < 10 ? 1 : 0) })}
    />
    {#if series.error}<p class="err">{errorText(series.error)}</p>{/if}
  </div>

  {#snippet split()}
    <TopTable
      title={t('overview.dns.topBlocked')}
      note={since(blocked.data?.since)}
      noteTitle={t('overview.dns.topSinceHint')}
      items={blocked.data?.items}
      loading={blocked.loading && !blocked.loaded}
      error={blocked.error && !blocked.data ? errorText(blocked.error) : undefined}
      onretry={() => blocked.refresh()}
      keyLabel={t('overview.dns.domain')}
      countLabel={t('overview.dns.queries')}
      emptyText={t('overview.dns.noBlocked')}
      mono
      pair="orange"
      link={(it) => links.blockedDomain(it.key, range)}
    />
    <TopTable
      title={t('overview.dns.topClients')}
      note={since(clients.data?.since)}
      noteTitle={t('overview.dns.topSinceHint')}
      items={clients.data?.items}
      loading={clients.loading && !clients.loaded}
      error={clients.error && !clients.data ? errorText(clients.error) : undefined}
      onretry={() => clients.refresh()}
      keyLabel={t('overview.dns.client')}
      countLabel={t('overview.dns.queries')}
      emptyText={t('overview.dns.noClients')}
      mono
      link={(it) => links.client(it.addresses?.length ? it.addresses : it.key, range)}
    />
  {/snippet}
</Band>

<style>
  .chart {
    padding: 0 var(--sp-4);
  }
  .err {
    margin-top: var(--sp-2);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
