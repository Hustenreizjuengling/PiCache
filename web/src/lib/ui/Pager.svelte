<!--
  @component
  Paging below a table, for offset lists (total known) and cursor lists.

  Offset: <Pager total={page.total} limit={50} bind:offset />
  Cursor: <Pager mode="cursor" hasPrev={pages.hasPrev} hasNext={!!data.next}
                 onprev={…} onnext={…} onfirst={…} />   (see CursorStack)
  Optional page size: limits={[50, 100, 250]} bind:limit
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import { formatNumber } from '../format'
  import IconButton from './IconButton.svelte'
  import Select from './Select.svelte'

  interface Props {
    mode?: 'offset' | 'cursor'
    /** offset mode: total matching items (-1 = unknown). */
    total?: number
    limit?: number
    offset?: number
    onchange?: (offset: number) => void
    /** cursor mode */
    hasPrev?: boolean
    hasNext?: boolean
    onprev?: () => void
    onnext?: () => void
    onfirst?: () => void
    /** Items on the current page (cursor mode summary). */
    count?: number
    /** Offers a page-size select. */
    limits?: number[]
    onlimit?: (limit: number) => void
  }

  let {
    mode = 'offset',
    total = -1,
    limit = $bindable(50),
    offset = $bindable(0),
    onchange,
    hasPrev = false,
    hasNext = false,
    onprev,
    onnext,
    onfirst,
    count,
    limits,
    onlimit,
  }: Props = $props()

  const from = $derived(total === 0 ? 0 : offset + 1)
  const to = $derived(total >= 0 ? Math.min(offset + limit, total) : offset + (count ?? limit))
  const canPrev = $derived(mode === 'offset' ? offset > 0 : hasPrev)
  const canNext = $derived(mode === 'offset' ? (total < 0 ? (count ?? 0) >= limit : offset + limit < total) : hasNext)

  function go(o: number) {
    offset = Math.max(0, o)
    onchange?.(offset)
  }
</script>

<nav class="pager" aria-label={t('common.pager.label')}>
  <span class="summary">
    {#if mode === 'offset'}
      {#if total >= 0}
        {t('common.pager.rangeOf', { from: formatNumber(from), to: formatNumber(to), total: formatNumber(total) })}
      {:else}
        {t('common.pager.range', { from: formatNumber(from), to: formatNumber(to) })}
      {/if}
    {:else if count !== undefined}
      {t('common.pager.items', { count: formatNumber(count) })}
    {/if}
  </span>
  <span class="controls">
    {#if limits && limits.length > 1}
      <span class="size">
        <Select
          size="sm"
          aria-label={t('common.pager.pageSize')}
          bind:value={
            () => String(limit),
            (v) => {
              limit = Number(v)
              onlimit?.(limit)
              if (mode === 'offset') go(0)
              else onfirst?.()
            }
          }
          options={limits.map((l) => ({ value: String(l), label: t('common.pager.perPage', { count: formatNumber(l) }) }))}
        />
      </span>
    {/if}
    {#if mode === 'cursor' && onfirst}
      <IconButton icon="first" size="sm" variant="secondary" label={t('common.pager.first')} disabled={!hasPrev} onclick={onfirst} />
    {/if}
    <IconButton
      icon="chevron-left"
      size="sm"
      variant="secondary"
      label={t('common.pager.previous')}
      disabled={!canPrev}
      onclick={() => (mode === 'offset' ? go(offset - limit) : onprev?.())}
    />
    <IconButton
      icon="chevron-right"
      size="sm"
      variant="secondary"
      label={t('common.pager.next')}
      disabled={!canNext}
      onclick={() => (mode === 'offset' ? go(offset + limit) : onnext?.())}
    />
  </span>
</nav>

<style>
  .pager {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2) var(--sp-4);
    padding: var(--sp-2) var(--sp-4);
    border-top: 1px solid var(--line);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .controls {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .size {
    width: 140px;
  }
</style>
