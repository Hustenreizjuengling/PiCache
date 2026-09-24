<!--
  @component
  The core component: 36 px rows (32 compact), header, right-aligned numbers,
  mono cells for machine values, sorting, loading skeleton, empty state and
  row click (opens a SidePanel). Scrolls horizontally inside its panel.

  <Table {columns} rows={records.data} key={(r) => r.id} loading={records.loading}
         onrowclick={(r) => (selected = r)} emptyText="No local records yet." />

  const columns: Column<DnsRecord>[] = [
    { key: 'name', label: 'Name', mono: true, sortable: true, value: (r) => r.name },
    { key: 'ttl', label: 'TTL', align: 'right', value: (r) => r.ttl, format: (r) => formatDuration(r.ttl * 1000) },
    { key: 'status', label: 'Status', cell: statusCell },   // {#snippet statusCell(r)}…{/snippet} in markup
  ]

  Sorting: without `onsort` rows are sorted client-side by column.value; with
  `onsort` the table only reports the new SortState (server-side sorting).
  Sticky header: give the table a `maxHeight` so it scrolls inside.
-->
<script lang="ts" generics="T">
  import type { Snippet } from 'svelte'
  import { t } from '../../i18n/index.svelte'
  import Button from './Button.svelte'
  import EmptyState from './EmptyState.svelte'
  import Icon from './Icon.svelte'
  import Skeleton from './Skeleton.svelte'
  import type { Column, SortState } from './types'

  interface Props {
    columns: Column<T>[]
    rows: readonly T[] | undefined
    key: (row: T) => string | number
    loading?: boolean
    /** Error text instead of rows (with a retry button when onretry is set). */
    error?: string
    onretry?: () => void
    skeletonRows?: number
    sort?: SortState
    onsort?: (sort: SortState) => void
    onrowclick?: (row: T) => void
    /** Key of the highlighted row (e.g. the one shown in the side panel). */
    selected?: string | number
    compact?: boolean
    /** Constrains the height; the header then sticks while the body scrolls. */
    maxHeight?: string
    /** Accessible description of the table (visually hidden caption). */
    caption?: string
    emptyText?: string
    empty?: Snippet
    rowClass?: (row: T) => string | undefined
  }

  let {
    columns,
    rows,
    key,
    loading = false,
    error,
    onretry,
    skeletonRows = 5,
    sort = $bindable(),
    onsort,
    onrowclick,
    selected,
    compact = false,
    maxHeight,
    caption,
    emptyText,
    empty,
    rowClass,
  }: Props = $props()

  const collator = $derived(new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' }))

  function compare(a: unknown, b: unknown): number {
    if (a === b) return 0
    if (a === undefined || a === null || a === '') return 1
    if (b === undefined || b === null || b === '') return -1
    if (typeof a === 'number' && typeof b === 'number') return a - b
    if (typeof a === 'boolean' && typeof b === 'boolean') return a ? -1 : 1
    return collator.compare(String(a), String(b))
  }

  const view = $derived.by(() => {
    const list = rows ?? []
    if (!sort || onsort) return list
    const col = columns.find((c) => c.key === sort!.key)
    if (!col?.value) return list
    const get = col.value
    const dir = sort.desc ? -1 : 1
    return [...list].sort((a, b) => {
      const va = get(a)
      const vb = get(b)
      // Missing values stay last in both directions.
      if (va === undefined || va === null || va === '') return vb === undefined || vb === null || vb === '' ? 0 : 1
      if (vb === undefined || vb === null || vb === '') return -1
      return dir * compare(va, vb)
    })
  })

  function toggleSort(col: Column<T>) {
    const next: SortState =
      sort?.key === col.key ? { key: col.key, desc: !sort.desc } : { key: col.key, desc: defaultDesc(col) }
    sort = next
    onsort?.(next)
  }

  function defaultDesc(col: Column<T>): boolean {
    return col.align === 'right' // numbers: largest first
  }

  function text(row: T, col: Column<T>): string {
    if (col.format) return col.format(row)
    const v = col.value?.(row)
    return v === undefined || v === null || v === '' ? '–' : String(v)
  }

  function onRowKey(e: KeyboardEvent, row: T) {
    if (e.target !== e.currentTarget) return
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault()
      onrowclick?.(row)
    }
  }

  function onRowClick(e: MouseEvent, row: T) {
    // Clicks on links, buttons and inputs inside a cell keep their own meaning.
    if ((e.target as HTMLElement).closest('a, button, input, select, textarea, label')) return
    onrowclick?.(row)
  }

  const showSkeleton = $derived(loading && (!rows || rows.length === 0) && !error)
</script>

<div class={['scroll', compact && 'compact']} style:max-height={maxHeight}>
  <table aria-busy={loading || undefined}>
    {#if caption}<caption class="visually-hidden">{caption}</caption>{/if}
    <thead>
      <tr>
        {#each columns as col (col.key)}
          <th
            scope="col"
            class={[col.align ?? 'left']}
            style:width={col.width}
            title={col.title}
            aria-sort={sort?.key === col.key ? (sort.desc ? 'descending' : 'ascending') : col.sortable ? 'none' : undefined}
          >
            {#if col.sortable}
              <button type="button" class="sort" onclick={() => toggleSort(col)}>
                <span>{col.label}</span>
                <Icon
                  name={sort?.key === col.key ? (sort.desc ? 'arrow-down' : 'arrow-up') : 'sort'}
                  size={14}
                />
              </button>
            {:else}
              {col.label}
            {/if}
          </th>
        {/each}
      </tr>
    </thead>
    <tbody>
      {#if error}
        <tr class="msg">
          <td colspan={columns.length}>
            <div class="error">
              <Icon name="error" size={18} />
              <span>{error}</span>
              {#if onretry}<Button size="sm" icon="refresh" onclick={onretry}>{t('common.action.retry')}</Button>{/if}
            </div>
          </td>
        </tr>
      {:else if showSkeleton}
        {#each Array.from({ length: skeletonRows }, (_, i) => i) as i (i)}
          <tr class="skeleton-row" aria-hidden="true">
            {#each columns as col (col.key)}
              <td class={[col.align ?? 'left']}><Skeleton width={col.align === 'right' ? '48px' : `${50 + ((i * 17) % 40)}%`} /></td>
            {/each}
          </tr>
        {/each}
      {:else if view.length === 0 && !loading}
        <tr class="msg">
          <td colspan={columns.length}>
            {#if empty}
              {@render empty()}
            {:else}
              <EmptyState compact title={emptyText ?? t('common.table.empty')} />
            {/if}
          </td>
        </tr>
      {:else}
        {#each view as row (key(row))}
          <!-- Rows open details on click/Enter; links and buttons inside keep their own meaning. -->
          <!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_noninteractive_element_interactions -->
          <tr
            class={[onrowclick && 'clickable', selected !== undefined && key(row) === selected && 'selected', rowClass?.(row)]}
            tabindex={onrowclick ? 0 : undefined}
            onclick={onrowclick ? (e) => onRowClick(e, row) : undefined}
            onkeydown={onrowclick ? (e) => onRowKey(e, row) : undefined}
          >
            {#each columns as col (col.key)}
              <td class={[col.align ?? 'left', col.mono && 'mono', col.truncate && 'trunc']} title={col.truncate ? text(row, col) : undefined}>
                {#if col.cell}{@render col.cell(row)}{:else}{text(row, col)}{/if}
              </td>
            {/each}
          </tr>
        {/each}
      {/if}
    </tbody>
  </table>
</div>

<style>
  .scroll {
    width: 100%;
    overflow: auto;
    overscroll-behavior-x: contain;
  }
  table {
    width: 100%;
    border-collapse: separate;
    border-spacing: 0;
    font-size: var(--fs-sm);
  }
  th,
  td {
    height: var(--row-h);
    padding: 0 var(--sp-3);
    border-bottom: 1px solid var(--line);
    text-align: left;
    vertical-align: middle;
  }
  .compact th,
  .compact td {
    height: var(--row-h-compact);
  }
  th:first-child,
  td:first-child {
    padding-left: var(--sp-4);
  }
  th:last-child,
  td:last-child {
    padding-right: var(--sp-4);
  }
  th {
    position: sticky;
    top: 0;
    z-index: 1;
    background: var(--surface);
    color: var(--text-2);
    font-weight: 600;
    white-space: nowrap;
    box-shadow: inset 0 -1px 0 var(--line);
    border-bottom: 0;
  }
  .right {
    text-align: right;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .center {
    text-align: center;
  }
  td.mono {
    font-family: var(--font-mono);
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
  td.trunc {
    max-width: 0;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .sort {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    padding: 0;
    border: 0;
    background: none;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }
  .sort:hover {
    color: var(--text);
  }
  th.right .sort {
    flex-direction: row-reverse;
  }
  tbody tr:last-child td {
    border-bottom: 0;
  }
  tr.clickable {
    cursor: pointer;
  }
  tr.clickable:hover td {
    background: var(--surface-2);
  }
  tr.clickable:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: -2px;
  }
  tr.selected td {
    background: color-mix(in srgb, var(--focus) 10%, var(--surface));
  }
  tr.msg td {
    height: auto;
    padding: 0;
  }
  .error {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-4);
    color: var(--danger);
  }
  .error span {
    color: var(--text);
  }
</style>
