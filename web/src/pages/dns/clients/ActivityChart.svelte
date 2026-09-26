<!--
  @component
  Activity of one client over the page's range (GET /stats/clients/{key}/series):
  allowed and blocked queries per hour (stacked) and, when it downloaded
  anything, its download-cache traffic. `key` is "client:<id>" for a
  configured client, "mac:<MAC>" for a device or "ip:<address>". A note
  replaces the chart where it would show misleading zeros: while the DNS
  statistics are off (the cache chart stays), for a client that is not
  counted (`excluded`, its ignoreStats), and for device keys while client
  addresses are anonymised (the server refuses them).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type RangePreset } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatCompact, formatNumber, formatRate } from '$lib/format'
  import { seriesRates } from '$lib/series'
  import { Button, Chart } from '$lib/ui'

  interface Props {
    key: string
    range: RangePreset
    /** The client is left out of the statistics (ignoreStats). */
    excluded?: boolean
  }

  let { key, range, excluded = false }: Props = $props()

  const HOUR = 3600

  // asOf: the end of the range, for the rate of the bucket still in progress.
  const series = resource(async (signal) => ({ asOf: Date.now(), s: await api.stats.clientSeries(key, range, undefined, { signal }) }), {
    interval: 5 * 60_000,
  })

  const chart = $derived.by(() => {
    if (!series.data) return { timestamps: [] as number[], allowed: [] as number[], blocked: [] as number[], cache: [] as number[] }
    const q = seriesRates(series.data.s, ['allowed', 'blocked'] as const, HOUR, series.data.asOf)
    const c = seriesRates(series.data.s, ['cacheBytes'] as const, 1, series.data.asOf)
    return { timestamps: q.timestamps, allowed: q.values.allowed, blocked: q.values.blocked, cache: c.values.cacheBytes }
  })
  // The privacy switches decide what the statistics hold (readable by viewers too).
  const settings = resource((signal) => api.settings.get({ signal }))
  const logs = $derived(settings.data?.logs)

  const hasCache = $derived(chart.cache.some((v) => v > 0))
  /** A refused key is a note, not an error: while addresses are anonymised it is a device key (translated). */
  const keyNote = $derived.by(() => {
    const msg = fieldError(series.error, 'key')
    return msg && logs?.anonymizeClientIps ? t('dns.clients.activityAnonymised') : msg
  })
  const statsOff = $derived(logs?.statsEnabled === false)
</script>

<section class="stack-sm" aria-label={t('dns.clients.activity')}>
  <h3>{t('dns.clients.activity')} <span class="muted small">· {t(`common.range.long.${range}`)}</span></h3>
  {#if keyNote}
    <p class="small muted">{keyNote}</p>
  {:else if excluded}
    <p class="small muted">{t('dns.clients.activityExcluded')}</p>
  {:else if series.error && !series.data}
    <p class="err small">
      {errorText(series.error)}
      <Button size="sm" variant="ghost" icon="refresh" onclick={() => series.refresh()}>{t('common.action.retry')}</Button>
    </p>
  {:else}
    {#if statsOff}
      <p class="small muted">{t('dns.clients.activityStatsOff')}</p>
    {:else}
      <Chart
        label={t('dns.clients.activityLabel')}
        timestamps={chart.timestamps}
        stacked
        height={160}
        loading={series.loading && !series.loaded}
        series={[
          { label: t('overview.dns.allowed'), pair: 'blue', values: chart.allowed },
          { label: t('overview.dns.blocked'), pair: 'orange', values: chart.blocked },
        ]}
        yFormat={formatCompact}
        valueFormat={(v) => t('dns.clients.perHour', { n: formatNumber(v, v < 10 ? 1 : 0) })}
      />
    {/if}
    {#if hasCache}
      <Chart
        label={t('dns.clients.activityCache')}
        timestamps={chart.timestamps}
        height={120}
        series={[{ label: t('dns.clients.activityCache'), pair: 'green', values: chart.cache }]}
        yFormat={formatRate}
        minMax={1_000_000}
      />
    {/if}
  {/if}
</section>

<style>
  h3 {
    font-size: var(--fs-md);
  }
  .err {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    color: var(--danger);
  }
</style>
