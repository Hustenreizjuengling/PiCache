<!--
  @component
  A small ranked list (top allowed and blocked domains, top clients) with
  inline bars. Items grouped by device show the device's other addresses as
  "+N addresses" (expandable, all of them in its tooltip). `notice`
  replaces the list with a short explanation (e.g. domains are hidden).
-->
<script lang="ts">
  import type { TopItem } from '../../lib/api'
  import { formatNumber } from '../../lib/format'
  import { Table, type Column, type Pair } from '../../lib/ui'
  import AddressList from '../dns/shared/AddressList.svelte'

  interface Props {
    title: string
    /** Shown after the title, e.g. the real start of the counted window ("since 09:00"). */
    note?: string
    /** Explanation of the note (tooltip). */
    noteTitle?: string
    items?: TopItem[]
    loading?: boolean
    error?: string
    onretry?: () => void
    keyLabel: string
    countLabel: string
    emptyText: string
    /** Machine values (domains, IPs) in monospace. */
    mono?: boolean
    /** Bar colour; omit for neutral. */
    pair?: Pair
    link: (item: TopItem) => string
    /** Shown instead of the list. */
    notice?: string
  }

  let {
    title,
    note,
    noteTitle,
    items,
    loading = false,
    error,
    onretry,
    keyLabel,
    countLabel,
    emptyText,
    mono = false,
    pair,
    link,
    notice,
  }: Props = $props()

  const max = $derived(Math.max(1, ...(items ?? []).map((i) => i.count)))

  const columns: Column<TopItem>[] = $derived([
    { key: 'key', label: keyLabel, cell: keyCell },
    { key: 'count', label: countLabel, align: 'right', width: '42%', cell: countCell },
  ])
</script>

{#snippet keyCell(it: TopItem)}
  {@const labelled = !!it.label && it.label !== it.key}
  {@const addresses = it.addresses?.length ? it.addresses : [it.key]}
  <a class={['key', mono && !it.label && 'mono']} href={link(it)} title={addresses.join('\n')}>{it.label || it.key}</a>
  {#if labelled || addresses.some((a) => a !== it.key)}
    <span class="sub mono"><AddressList {addresses} first={it.key} hideFirst={!labelled} /></span>
  {/if}
{/snippet}

{#snippet countCell(it: TopItem)}
  <span class="count">
    <span class="bar" aria-hidden="true"
      ><span style:width="{(it.count / max) * 100}%" style:background={pair ? `var(--${pair})` : 'var(--text-3)'}></span></span
    >
    <span class="n">{formatNumber(it.count)}</span>
  </span>
{/snippet}

<div class="top">
  <h3>{title}{#if note}<span class="note" title={noteTitle}>· {note}</span>{/if}</h3>
  {#if notice}
    <p class="notice small muted">{notice}</p>
  {:else}
    <Table
      compact
      rows={items}
      key={(it) => it.key}
      {loading}
      {error}
      {onretry}
      skeletonRows={5}
      {emptyText}
      caption={title}
      {columns}
    />
  {/if}
</div>

<style>
  .top {
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
  .note {
    margin-left: 0.35em;
    font-weight: 400;
    color: var(--text-3);
  }
  .notice {
    padding: 0 var(--sp-4) var(--sp-4);
  }
  .key {
    display: block;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    max-width: 36ch;
    color: var(--text);
    text-decoration: none;
  }
  .key:hover {
    text-decoration: underline;
  }
  .key.mono {
    font-family: var(--font-mono);
  }
  .sub {
    display: block;
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
  .count {
    display: inline-flex;
    align-items: center;
    justify-content: flex-end;
    gap: var(--sp-2);
    width: 100%;
  }
  .bar {
    flex: 1;
    max-width: 120px;
    height: 6px;
    border-radius: var(--r-pill);
    background: var(--surface-3);
    overflow: hidden;
    display: flex;
    justify-content: flex-end;
  }
  .bar span {
    height: 100%;
    border-radius: var(--r-pill);
  }
  .n {
    min-width: 5ch;
    text-align: right;
  }
</style>
