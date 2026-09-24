<!--
  @component
  What is cached per service, as a row of filter buttons: "All services"
  plus one button per service with its size and share of the cache.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { ServiceUsage } from '$lib/api'
  import { formatBytes, formatPercent } from '$lib/format'
  import { Skeleton } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'

  interface Props {
    usage: ServiceUsage[] | undefined
    catalog: ServiceCatalog
    /** Selected service id ('' = all). */
    selected: string
    onselect: (service: string) => void
  }

  let { usage, catalog, selected, onselect }: Props = $props()

  const sorted = $derived([...(usage ?? [])].sort((a, b) => b.cachedBytes - a.cachedBytes))
  const total = $derived(sorted.reduce((s, u) => s + u.cachedBytes, 0))
  const groups = $derived(sorted.reduce((s, u) => s + u.groups, 0))
</script>

<div class="strip" role="group" aria-label={t('cache.library.byService')}>
  {#if !usage}
    {#each [0, 1, 2] as i (i)}
      <span class="item skeleton"><Skeleton width="80px" /><Skeleton width="56px" /></span>
    {/each}
  {:else}
    <button type="button" class="item" aria-pressed={selected === ''} onclick={() => onselect('')}>
      <span class="name">{t('cache.library.allServices')}</span>
      <span class="size">{formatBytes(total)}</span>
      <span class="meta">{tn('cache.library.items', groups)}</span>
    </button>
    {#each sorted as u (u.service)}
      <button
        type="button"
        class="item"
        aria-pressed={selected === u.service}
        onclick={() => onselect(selected === u.service ? '' : u.service)}
        title={t('cache.library.share', { share: formatPercent(total > 0 ? u.cachedBytes / total : 0) })}
      >
        <span class="name">{catalog.name(u.service)}</span>
        <span class="size">{formatBytes(u.cachedBytes)}</span>
        <span class="meta">{tn('cache.library.items', u.groups)}</span>
        <span class="bar" aria-hidden="true"><span style:width="{total > 0 ? (u.cachedBytes / total) * 100 : 0}%"></span></span>
      </button>
    {/each}
  {/if}
</div>

<style>
  .strip {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
  }
  .item {
    display: grid;
    grid-template-columns: auto auto;
    align-items: baseline;
    gap: 2px var(--sp-3);
    min-width: 132px;
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    text-align: left;
    cursor: pointer;
  }
  .item:hover {
    background: var(--surface-2);
  }
  .item[aria-pressed='true'] {
    border-color: var(--text);
    box-shadow: inset 0 0 0 1px var(--text);
  }
  .item:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 2px;
  }
  .skeleton {
    cursor: default;
    min-height: 52px;
  }
  .name {
    font-weight: 600;
    font-size: var(--fs-sm);
  }
  .size {
    justify-self: end;
    font-variant-numeric: tabular-nums;
    font-size: var(--fs-sm);
  }
  .meta {
    grid-column: 1 / -1;
    font-size: var(--fs-xs);
    color: var(--text-3);
  }
  .bar {
    grid-column: 1 / -1;
    height: 3px;
    border-radius: var(--r-pill);
    background: var(--surface-3);
    overflow: hidden;
  }
  .bar span {
    display: block;
    height: 100%;
    background: var(--text-3);
  }
</style>
