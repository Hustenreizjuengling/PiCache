<!--
  @component
  Overview band "DNS": allowed/blocked queries per minute (stacked), top
  blocked domains and top clients.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { api, resource, type RangePreset, type Series } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import { formatCompact, formatNumber } from '../../lib/format'
  import { Button, Chart } from '../../lib/ui'
  import Band from './Band.svelte'
  import { links } from './links'
  import TopTable from './TopTable.svelte'

  let { range }: { range: RangePreset } = $props()

  const REFRESH = 60_000

  const series = resource((signal) => api.stats.dns(range, undefined, { signal }), { interval: REFRESH })
  const blocked = resource((signal) => api.stats.top('blocked', range, 10, { signal }), { interval: REFRESH })
  const clients = resource((signal) => api.stats.top('clients', range, 10, { signal }), { interval: REFRESH })

  function perMinute(s: Series, key: 'allowed' | 'cached' | 'lancache' | 'blocked' | 'other'): number[] {
    const f = 60 / Math.max(1, s.step)
    return s.timestamps.map((_, i) => (s.values[key]?.[i] ?? 0) * f)
  }

  const chart = $derived.by(() => {
    const s = series.data
    if (!s) return { timestamps: [] as number[], allowed: [] as number[], blocked: [] as number[] }
    const parts = (['allowed', 'cached', 'lancache', 'other'] as const).map((k) => perMinute(s, k))
    return {
      timestamps: s.timestamps,
      allowed: s.timestamps.map((_, i) => parts.reduce((sum, p) => sum + p[i], 0)),
      blocked: perMinute(s, 'blocked'),
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
      items={blocked.data}
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
      items={clients.data}
      loading={clients.loading && !clients.loaded}
      error={clients.error && !clients.data ? errorText(clients.error) : undefined}
      onretry={() => clients.refresh()}
      keyLabel={t('overview.dns.client')}
      countLabel={t('overview.dns.queries')}
      emptyText={t('overview.dns.noClients')}
      mono
      link={(it) => links.client(it.key, range)}
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
