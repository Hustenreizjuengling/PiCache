<!--
  @component
  Cache storage used/free with the "full in ~N days" estimate (see
  storage.ts): the growth of the last 7 days (or of the days since the
  statistics started) against the free space, or against the size limit when
  one is set and reached first.
-->
<script lang="ts">
  import { t, tn } from '../../i18n/index.svelte'
  import { api, resource, type StoreState } from '../../lib/api'
  import { formatBytes } from '../../lib/format'
  import { Button, Chip, Meter, Notice } from '../../lib/ui'
  import { links } from './links'
  import { atSizeLimit, coveredDays, fullEstimate } from './storage'

  let { store }: { store: StoreState } = $props()

  const week = resource((signal) => api.stats.summary('7d', { signal }), { interval: 300_000 })
  // Hourly cache traffic: when the statistics started (they may be younger than a week).
  const traffic = resource((signal) => api.stats.cache('7d', 3600, undefined, { signal }), { interval: 300_000 })

  const cached = $derived(store.usage?.cachedBytes ?? 0)
  const used = $derived(Math.max(0, store.totalBytes - store.freeBytes))
  const other = $derived(Math.max(0, used - cached))

  const atLimit = $derived(atSizeLimit(store))
  const estimate = $derived.by(() => {
    const w = week.data
    if (!w) return null
    if (!traffic.data && !traffic.error) return null // wait for the covered days
    return fullEstimate(store, w, traffic.data ? coveredDays(traffic.data) : 7)
  })
</script>

<div class="storage">
  <h3>{t('overview.storage.title')}</h3>
  <div class="body">
    {#if !store.online}
      <Notice tone="fail" title={t('overview.storage.offline')}>
        {store.reason ?? ''}{#if store.hint}<br />{store.hint}{/if}
        {#snippet actions()}
          <Button size="sm" href={links.storage()}>{t('overview.storage.open')}</Button>
        {/snippet}
      </Notice>
    {:else if store.totalBytes === 0}
      <!-- The filesystem size could not be read: show what is cached only. -->
      <p class="summary">{t('overview.storage.cachedOnly', { cached: formatBytes(cached) })}</p>
      <div class="facts">
        {#if store.sdCard}<Chip tone="warn" label={t('overview.storage.sdCard')} />{/if}
        <a href={links.storage()}>{t('overview.storage.manage')}</a>
      </div>
    {:else}
      <p class="summary">
        {t('overview.storage.summary', {
          cached: formatBytes(cached),
          free: formatBytes(store.freeBytes),
          total: formatBytes(store.totalBytes),
        })}
      </p>
      <Meter
        label={t('overview.storage.title')}
        max={store.totalBytes}
        segments={[
          { label: t('overview.storage.cached'), value: cached, text: formatBytes(cached), pair: 'green' },
          { label: t('overview.storage.other'), value: other, text: formatBytes(other), tone: 'neutral' },
        ]}
        rest={{ label: t('overview.storage.free'), text: formatBytes(store.freeBytes) }}
        marker={store.minFreeBytes > 0 && store.totalBytes > store.minFreeBytes
          ? { value: store.totalBytes - store.minFreeBytes, label: t('overview.storage.minFree', { size: formatBytes(store.minFreeBytes) }) }
          : undefined}
      />
      <div class="facts">
        {#if store.full}
          <Chip tone="warn" label={t('overview.storage.full')} />
        {:else if store.lowSpace}
          <Chip tone="warn" label={t('overview.storage.low')} />
        {:else if atLimit}
          <Chip tone="info" label={t('overview.storage.atLimit', { size: formatBytes(store.maxSizeBytes) })} />
        {:else if estimate}
          <span>{tn(estimate.byLimit ? 'overview.storage.limitIn' : 'overview.storage.fullIn', estimate.days)}</span>
        {/if}
        {#if store.sdCard}<Chip tone="warn" label={t('overview.storage.sdCard')} />{/if}
        <a href={links.storage()}>{t('overview.storage.manage')}</a>
      </div>
    {/if}
  </div>
</div>

<style>
  .storage {
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
  .body {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    padding: var(--sp-1) var(--sp-4) var(--sp-4);
  }
  .summary {
    font-weight: 600;
  }
  .facts {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-4);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
</style>
