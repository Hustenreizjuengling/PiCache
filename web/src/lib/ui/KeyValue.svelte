<!--
  @component
  Definition list for details in side panels.
  <KeyValue items={[{ label: 'IP', value: '192.168.1.5', mono: true }, { label: 'Queries', value: formatNumber(n) }]} />
  Extra rows: pass children with <dt>/<dd> pairs.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import type { KeyValueItem } from './types'

  let { items = [], children }: { items?: KeyValueItem[]; children?: Snippet } = $props()
</script>

<dl class="kv">
  {#each items as it (it.label)}
    <dt>{it.label}</dt>
    <dd class={{ mono: it.mono }}>
      {#if it.value === null || it.value === undefined || it.value === ''}
        <span class="subtle">–</span>
      {:else if it.href}
        <a href={it.href}>{it.value}</a>
      {:else}
        {it.value}
      {/if}
    </dd>
  {/each}
  {@render children?.()}
</dl>

<style>
  .kv {
    display: grid;
    grid-template-columns: minmax(120px, max-content) minmax(0, 1fr);
    gap: var(--sp-2) var(--sp-4);
    margin: 0;
    font-size: var(--fs-sm);
  }
  .kv :global(dt) {
    color: var(--text-2);
  }
  .kv :global(dd) {
    margin: 0;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  @media (max-width: 480px) {
    .kv {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
    .kv :global(dd) {
      margin-bottom: var(--sp-2);
    }
  }
</style>
