<!--
  @component
  Result of a storage speed test: write, read and cache reads in MB/s (decimal,
  so they compare with network speeds), file operations p50/p95 in ms, what
  this means compared with 1, 2.5 and 10 GbE, what was tested, and the
  server's notes. Partial results (a cancelled or failed run) show "Not
  measured" for the phases that did not run.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { BenchmarkResult, BenchmarkRun } from '$lib/api'
  import { formatBytes, formatDateTime, formatDuration, formatNumber, formatRelative } from '$lib/format'
  import { Icon, KeyValue, type KeyValueItem } from '$lib/ui'
  import { spanMs } from '../shared/util'
  import { NETWORKS, formatMBps, formatMiB, formatMs, formatTestSize, hitSpeed, limitFor, measured, networkName, notesOf } from './speed'

  interface Props {
    result: BenchmarkResult
    /** The run the result belongs to (requested size, duration). */
    run?: BenchmarkRun
    /** Level of the sub-headings: 3 inside a page panel, 4 inside a side panel section. */
    level?: 3 | 4
  }

  let { result, run, level = 3 }: Props = $props()

  const r = $derived(result)
  const hit = $derived(hitSpeed(r))
  const notes = $derived(notesOf(r))
  const meta = $derived(r.metadata && r.metadata.ops > 0 ? r.metadata : undefined)
  const heading = $derived(`h${level}`)

  function seconds(s: number): string {
    return formatDuration(s * 1000)
  }

  const details = $derived.by((): KeyValueItem[] => {
    const requested = run ? run.sizeMiB * 2 ** 20 : 0
    const size = r.sizeBytes > 0 ? formatTestSize(r.sizeBytes) : undefined
    const items: KeyValueItem[] = [
      {
        label: t('cache.speed.sizeTested'),
        value: size && run && requested > r.sizeBytes ? t('cache.speed.sizeOf', { size, requested: formatMiB(run.sizeMiB) }) : size,
      },
      { label: t('cache.store.fileSystem'), value: r.fsType },
      { label: t('cache.speed.freeBefore'), value: r.freeBytes > 0 ? formatBytes(r.freeBytes) : undefined },
      { label: t('cache.speed.testedAt'), value: r.testedAt ? `${formatDateTime(r.testedAt)} (${formatRelative(r.testedAt)})` : undefined },
    ]
    const took = run?.finishedAt ? spanMs(run.startedAt, run.finishedAt) : null
    if (took !== null) items.push({ label: t('cache.speed.duration'), value: formatDuration(took) })
    items.push({ label: t('cache.target.storeRoot'), value: r.storeRoot, mono: true })
    return items
  })

  const LIMIT_ICON = { network: 'success', equal: 'info', storage: 'alert' } as const
</script>

{#snippet notMeasured(why?: string)}
  <dd class="value none">–</dd>
  <dd class="detail">{t('cache.speed.notMeasured')}</dd>
  {#if why}<dd class="detail">{why}</dd>{/if}
{/snippet}

<div class="result">
  <dl class="metrics">
    <div class="metric">
      <dt>{t('cache.speed.write')}</dt>
      {#if measured(r.write)}
        <dd class="value">{formatMBps(r.write.bytesPerSec)}</dd>
        <dd class="detail">{t('cache.speed.amount', { size: formatBytes(r.write.bytes), time: seconds(r.write.seconds) })}</dd>
      {:else}
        {@render notMeasured()}
      {/if}
    </div>
    <div class="metric">
      <dt>{t('cache.speed.read')}</dt>
      {#if measured(r.read)}
        <dd class="value">{formatMBps(r.read.bytesPerSec)}</dd>
        <dd class="detail">{t('cache.speed.amount', { size: formatBytes(r.read.bytes), time: seconds(r.read.seconds) })}</dd>
        {#if r.read.cacheDropped === false}<dd class="detail warn">{t('cache.speed.fromMemory')}</dd>{/if}
      {:else}
        {@render notMeasured()}
      {/if}
    </div>
    <div class="metric">
      <dt>{t('cache.speed.slices')}</dt>
      {#if measured(r.slices)}
        <dd class="value">{formatMBps(r.slices.bytesPerSec)}</dd>
        <dd class="detail">{tn('cache.speed.parts', r.slices.count, { count: formatNumber(r.slices.count) })}</dd>
        <dd class="detail">{t('cache.speed.perPart', { p50: formatMs(r.slices.p50Ms), p95: formatMs(r.slices.p95Ms) })}</dd>
      {:else}
        {@render notMeasured(t('cache.speed.slicesSkipped'))}
      {/if}
    </div>
    <div class="metric">
      <dt>{t('cache.speed.metadata')}</dt>
      {#if meta}
        <dd class="value">{formatMs(meta.p50Ms)} <span class="tag">{t('cache.speed.p50')}</span></dd>
        <dd class="detail">{t('cache.speed.metadataDetail', { p95: formatMs(meta.p95Ms), max: formatMs(meta.maxMs) })}</dd>
        <dd class="detail">{t('cache.speed.ops', { count: formatNumber(meta.ops) })}</dd>
      {:else}
        {@render notMeasured()}
      {/if}
    </div>
  </dl>
  {#if meta || measured(r.slices)}
    <p class="xsmall subtle">{t('cache.speed.percentiles')}</p>
  {/if}

  <div class="more">
    {#if hit !== undefined}
      <section class="meaning">
        <svelte:element this={heading} class="sub">{t('cache.speed.meaning')}</svelte:element>
        <p>{t('cache.speed.hitSpeed', { speed: formatMBps(hit) })}</p>
        <ul class="nets">
          {#each NETWORKS as n (n.gbit)}
            {@const limit = limitFor(hit, n.bytesPerSec)}
            <li class={limit}>
              <Icon name={LIMIT_ICON[limit]} size={16} />
              <span>
                <span class="net">{t('cache.speed.network', { name: networkName(n.gbit), speed: formatMBps(n.bytesPerSec) })}</span>:
                {t(`cache.speed.limit.${limit}`)}
              </span>
            </li>
          {/each}
        </ul>
        <p class="small muted">{t('cache.speed.help')}</p>
      </section>
    {/if}
    <KeyValue items={details} />
  </div>

  {#if notes.length}
    <section class="stack-sm">
      <svelte:element this={heading} class="sub">{t('cache.speed.notes')}</svelte:element>
      <ul class="notes">
        {#each notes as note, i (i)}
          <li>{note}</li>
        {/each}
      </ul>
    </section>
  {/if}
</div>

<style>
  .result {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    min-width: 0;
    container-type: inline-size;
  }
  .more {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    min-width: 0;
  }
  /* Wide panels: what it means next to what was tested. */
  @container (min-width: 880px) {
    .more {
      display: grid;
      grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);
      gap: var(--sp-6);
      align-items: start;
    }
  }
  .metrics {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(min(100%, 136px), 1fr));
    gap: var(--sp-3);
    margin: 0;
  }
  .result > .xsmall {
    margin-top: calc(-1 * var(--sp-2));
  }
  .metric {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    padding: var(--sp-3);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  dt {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  dd {
    margin: 0;
    min-width: 0;
  }
  .value {
    margin: 2px 0 var(--sp-1);
    font-size: var(--fs-xl);
    font-weight: 600;
    line-height: var(--lh-tight);
    white-space: nowrap;
  }
  .value.none {
    color: var(--text-3);
  }
  .tag {
    font-size: var(--fs-sm);
    font-weight: 400;
    color: var(--text-2);
  }
  .detail {
    font-size: var(--fs-xs);
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  .detail.warn {
    color: var(--warning);
  }
  .sub {
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .meaning {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    font-size: var(--fs-sm);
  }
  .nets {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .nets li {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
  }
  .nets li :global(.icon) {
    flex: none;
    margin-top: 2px;
  }
  .nets .network :global(.icon) {
    color: var(--ok);
  }
  .nets .equal :global(.icon) {
    color: var(--text-2);
  }
  .nets .storage :global(.icon) {
    color: var(--warn);
  }
  .net {
    font-weight: 600;
    white-space: nowrap;
  }
  .notes {
    margin: 0;
    padding-left: var(--sp-5);
    font-size: var(--fs-sm);
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  @container (max-width: 420px) {
    .value {
      font-size: var(--fs-lg);
    }
  }
</style>
