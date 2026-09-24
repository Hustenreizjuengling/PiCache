<!--
  @component
  Overview band "Cache": throughput from the cache (green) vs the Internet
  (brown), live downloads and storage with a "full in ~N days" estimate. When
  LanCache is off it explains how to turn it on.
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { api, resource, type RangePreset, type SystemOverview } from '../../lib/api'
  import { errorText } from '../../lib/errors'
  import { formatRate } from '../../lib/format'
  import { Button, Chart, Notice } from '../../lib/ui'
  import Band from './Band.svelte'
  import LanCacheOff from './LanCacheOff.svelte'
  import { links } from './links'
  import LiveDownloads from './LiveDownloads.svelte'
  import StorageSummary from './StorageSummary.svelte'

  let { range, overview }: { range: RangePreset; overview: SystemOverview } = $props()

  // A derived boolean: the overview object changes every poll, the flag rarely.
  const enabled = $derived(overview.lancacheEnabled)
  const series = resource(
    (signal) => (enabled ? api.stats.cache(range, undefined, undefined, { signal }) : Promise.resolve(undefined)),
    { interval: 60_000 },
  )

  const chart = $derived.by(() => {
    const s = series.data
    if (!s) return { timestamps: [] as number[], hit: [] as number[], wan: [] as number[] }
    const step = Math.max(1, s.step)
    return {
      timestamps: s.timestamps,
      hit: s.timestamps.map((_, i) => (s.values.hit?.[i] ?? 0) / step),
      wan: s.timestamps.map((_, i) => (s.values.wan?.[i] ?? 0) / step),
    }
  })
</script>

{#snippet lower()}
  <LiveDownloads />
  <StorageSummary store={overview.store} />
{/snippet}

<Band title={t('overview.cache.title')} split={enabled ? lower : undefined}>
  {#snippet actions()}
    {#if enabled}
      <Button size="sm" variant="ghost" icon="download" href={links.downloads()}>{t('overview.cache.openDownloads')}</Button>
    {/if}
  {/snippet}

  {#if !enabled}
    <LanCacheOff />
  {:else}
    {#if !overview.cacheIps.ready}
      <div class="pad">
        <Notice tone="fail" title={t('overview.cache.notReady')}>
          {overview.cacheIps.reason ?? t('overview.cache.notReadyText')}
          {#snippet actions()}
            <Button size="sm" href={links.cacheSettings()}>{t('overview.cache.openSettings')}</Button>
          {/snippet}
        </Notice>
      </div>
    {:else if !overview.servicesReady}
      <div class="pad">
        <Notice tone="warn" title={t('overview.cache.servicesNotReady')}>{t('overview.cache.servicesNotReadyText')}</Notice>
      </div>
    {/if}
    <div class="pad">
      <Chart
        label={t('overview.cache.chartLabel')}
        timestamps={chart.timestamps}
        stacked
        loading={series.loading && !series.loaded}
        series={[
          { label: t('overview.cache.hit'), pair: 'green', values: chart.hit },
          { label: t('overview.cache.wan'), pair: 'brown', values: chart.wan },
        ]}
        yFormat={formatRate}
        minMax={1_000_000}
      />
      {#if series.error}<p class="err">{errorText(series.error)}</p>{/if}
    </div>
  {/if}
</Band>

<style>
  .pad {
    padding: 0 var(--sp-4);
  }
  .err {
    margin-top: var(--sp-2);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
