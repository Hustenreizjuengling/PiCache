<!--
  @component
  Overview (docs/DESIGN.md "Layout"): health warnings, the status sentence,
  then one band per product half (DNS, Cache). Not a card grid.
  Query: ?range=15m|1h|24h|7d|30d|90d|180d|365d or ?from=&to= (unix seconds,
  a custom window of at most 400 days) for the charts and top lists
  (default 24h). "More" offers the longer presets the statistics retention
  keeps and the custom range.
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { api, resource, type RangePreset } from '../lib/api'
  import { DAY, isCustom, rangeParams, readRange, withinRetention, type Range } from '../lib/range'
  import { router } from '../lib/router.svelte'
  import { appStatus } from '../lib/status.svelte'
  import { CustomRangeDialog, Notice, Skeleton, TimeRangePicker } from '../lib/ui'
  import { errorText } from '../lib/errors'
  import CacheBand from './overview/CacheBand.svelte'
  import DnsBand from './overview/DnsBand.svelte'
  import HealthBanner from './overview/HealthBanner.svelte'
  import StatusSentence from './overview/StatusSentence.svelte'

  const RANGES: RangePreset[] = ['15m', '1h', '24h', '7d', '30d']
  const MORE: RangePreset[] = ['90d', '180d', '365d']
  const DEFAULT_RANGE: RangePreset = '24h'

  const range = $derived(readRange([...RANGES, ...MORE], DEFAULT_RANGE))
  const custom = $derived(isCustom(range) ? range : null)

  const overview = $derived(appStatus.overview.data)
  const day = resource((signal) => api.stats.summary('24h', { signal }), { interval: 30_000 })
  // Privacy switches (statistics off, domains hidden) and the statistics retention.
  const settings = resource((signal) => api.settings.get({ signal }), { interval: 5 * 60_000 })
  const logs = $derived(settings.data?.logs)
  /** Presets longer than the statistics retention are not offered. */
  const more = $derived(withinRetention(MORE, logs ? logs.statsRetentionDays * DAY : undefined))

  let customOpen = $state(false)

  function setRange(r: Range) {
    router.setQuery(rangeParams(r, DEFAULT_RANGE))
  }
</script>

<div class="page">
  <HealthBanner {overview} />

  {#if appStatus.overview.error && !overview}
    <Notice tone="fail" title={t('overview.loadError')}>{errorText(appStatus.overview.error)}</Notice>
  {/if}

  <div class="head">
    <StatusSentence {overview} summary={day.data} statsOn={logs?.statsEnabled !== false} />
    <div class="range">
      <TimeRangePicker
        value={custom ? null : (range as RangePreset)}
        options={RANGES}
        {more}
        {custom}
        oncustom={() => (customOpen = true)}
        label={t('overview.rangeLabel')}
        onchange={setRange}
      />
    </div>
  </div>

  <DnsBand {range} {logs} />

  {#if overview}
    <CacheBand {range} {overview} />
  {:else}
    <Skeleton height="320px" />
  {/if}
</div>

<CustomRangeDialog
  bind:open={customOpen}
  value={custom}
  earliest={logs ? Math.floor(Date.now() / 1000) - logs.statsRetentionDays * DAY : undefined}
  onapply={setRange}
/>

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
    max-width: 100%;
  }
</style>
