<!--
  @component
  Overview (docs/DESIGN.md "Layout"): health warnings, the status sentence,
  then one band per product half (DNS, Cache). Not a card grid.
  Query: ?range=15m|1h|24h|7d|30d (charts and top lists; default 24h).
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { api, resource, type RangePreset } from '../lib/api'
  import { router } from '../lib/router.svelte'
  import { appStatus } from '../lib/status.svelte'
  import { Notice, Skeleton, TimeRangePicker } from '../lib/ui'
  import { errorText } from '../lib/errors'
  import CacheBand from './overview/CacheBand.svelte'
  import DnsBand from './overview/DnsBand.svelte'
  import HealthBanner from './overview/HealthBanner.svelte'
  import StatusSentence from './overview/StatusSentence.svelte'

  const RANGES: RangePreset[] = ['15m', '1h', '24h', '7d', '30d']
  const DEFAULT_RANGE: RangePreset = '24h'

  const range = $derived.by((): RangePreset => {
    const r = router.param('range') as RangePreset
    return RANGES.includes(r) ? r : DEFAULT_RANGE
  })

  const overview = $derived(appStatus.overview.data)
  const day = resource((signal) => api.stats.summary('24h', { signal }), { interval: 30_000 })
</script>

<div class="page">
  <HealthBanner {overview} />

  {#if appStatus.overview.error && !overview}
    <Notice tone="fail" title={t('overview.loadError')}>{errorText(appStatus.overview.error)}</Notice>
  {/if}

  <div class="head">
    <StatusSentence {overview} summary={day.data} />
    <div class="range">
      <TimeRangePicker
        value={range}
        options={RANGES}
        label={t('overview.rangeLabel')}
        onchange={(r) => router.setQuery({ range: r === DEFAULT_RANGE ? null : r })}
      />
    </div>
  </div>

  <DnsBand {range} />

  {#if overview}
    <CacheBand {range} {overview} />
  {:else}
    <Skeleton height="320px" />
  {/if}
</div>

<style>
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-end;
    justify-content: space-between;
    gap: var(--sp-3) var(--sp-5);
  }
  .head > :global(.sentence) {
    flex: 1 1 480px;
    min-width: 0;
  }
  .range {
    flex: none;
  }
</style>
