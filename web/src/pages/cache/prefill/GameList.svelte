<!--
  @component
  A store's games with a check box each: which ones the prefill downloads.
  A change is saved shortly after the last click (and when the tab is
  left); the filter only narrows the view, never the selection. Each game shows its
  size or version (when the store says) and whether its current version is
  already in the cache. Used by the Steam, Battle.net and Epic tabs.
-->
<script lang="ts" module>
  export interface GameRow {
    id: string
    name: string
    /** A second line under the name (an ID, a product code). */
    sub?: string
    bytes?: number
    version?: string
    prefilled: string | null
    current: boolean
  }
</script>

<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { formatBytes, formatDate, formatNumber, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Icon, Input, Notice, Panel, Table, toast, type Column } from '$lib/ui'

  interface Props {
    title: string
    description: string
    /** The store's name in "Read from …". */
    store: string
    rows: GameRow[] | undefined
    /** The saved selection. */
    saved: string[]
    loading: boolean
    /** The list could not be loaded at all. */
    loadError?: string
    /** The store could not be read now (the last list is shown). */
    readError?: string
    updated?: string | null
    /** Shows the size column (else the version). */
    sizes?: boolean
    onretry: () => void
    onread: () => Promise<void>
    onsave: (ids: string[]) => Promise<void>
  }

  let { title, description, store, rows, saved, loading, loadError, readError, updated, sizes = false, onretry, onread, onsave }: Props =
    $props()

  // The selection is saved as it changes: pending holds it from the click
  // until the saved one (the status) shows it.
  let pending = $state<string[] | undefined>(undefined)
  const checked = $derived(pending ?? saved)
  let queued = $state(false)
  let saving = $state(false)
  let savedOnce = $state(false)
  let timer: ReturnType<typeof setTimeout> | undefined
  let again = false

  const same = (a: string[], b: string[]) => a.length === b.length && a.every((id) => b.includes(id))

  function change(ids: string[]) {
    pending = ids
    queued = true
    clearTimeout(timer)
    timer = setTimeout(() => void flush(), 500)
  }

  async function flush() {
    clearTimeout(timer)
    timer = undefined
    if (saving) {
      again = true
      return
    }
    queued = false
    const ids = pending
    if (!ids) return
    saving = true
    try {
      await onsave(ids)
      savedOnce = true
    } catch (err) {
      toast.error(err)
      pending = undefined
    } finally {
      saving = false
      if (again) {
        again = false
        void flush()
      }
    }
  }

  // Once the status shows the saved selection, it is the source again.
  $effect(() => {
    if (pending && !saving && !queued && same(pending, saved)) pending = undefined
  })

  // Leaving the tab saves what still waits.
  $effect(() => () => {
    if (timer) void flush()
  })

  let filter = $state('')
  const shown = $derived.by(() => {
    const q = filter.trim().toLowerCase()
    const list = rows ?? []
    return q ? list.filter((g) => g.name.toLowerCase().includes(q) || g.id.toLowerCase() === q) : list
  })
  const selectedBytes = $derived((rows ?? []).filter((g) => checked.includes(g.id)).reduce((n, g) => n + (g.bytes ?? 0), 0))

  let reading = $state(false)
  async function read() {
    reading = true
    try {
      await onread()
    } catch (err) {
      toast.error(err)
    } finally {
      reading = false
    }
  }

  function stateOf(g: GameRow): { label: string; tone: 'ok' | 'warn' | 'neutral' } {
    if (!g.prefilled) return { label: t('cache.prefill.games.never'), tone: 'neutral' }
    if (g.current) return { label: t('cache.prefill.games.current'), tone: 'ok' }
    return { label: t('cache.prefill.games.update'), tone: 'warn' }
  }

  const columns: Column<GameRow>[] = $derived([
    { key: 'name', label: t('cache.prefill.games.name'), sortable: true, value: (g) => g.name, cell: nameCell, width: '55%' },
    sizes
      ? { key: 'bytes', label: t('cache.prefill.games.size'), sortable: true, align: 'right', value: (g) => g.bytes ?? 0, format: (g) => (g.bytes ? formatBytes(g.bytes) : '–') }
      : { key: 'version', label: t('cache.prefill.games.version'), mono: true, value: (g) => g.version ?? '', format: (g) => g.version || '–' },
    { key: 'state', label: t('cache.prefill.games.state'), value: (g) => (g.prefilled ? (g.current ? 2 : 1) : 0), cell: stateCell },
  ])
</script>

{#snippet nameCell(g: GameRow)}
  <span class="name">
    <span>{g.name}</span>
    {#if g.sub}<span class="sub mono">{g.sub}</span>{/if}
  </span>
{/snippet}

{#snippet stateCell(g: GameRow)}
  {@const s = stateOf(g)}
  <span class="row nowrap">
    <Chip size="sm" tone={s.tone} label={s.label} />
    {#if g.prefilled}<span class="muted small" title={formatDate(g.prefilled)}>{formatRelative(g.prefilled)}</span>{/if}
  </span>
{/snippet}

<Panel flush {title} {description}>
  {#snippet actions()}
    <Button icon="refresh" loading={reading} disabled={!session.canOperate} onclick={read}>{t('cache.prefill.games.read', { store })}</Button>
  {/snippet}
  <div class="bar">
    <Input type="search" icon="search" size="sm" bind:value={filter} placeholder={t('cache.prefill.games.filter')} aria-label={t('cache.prefill.games.filter')} />
    <span class="muted small">
      {#if sizes}
        {tn('cache.prefill.games.selected', checked.length, { count: formatNumber(checked.length), size: formatBytes(selectedBytes) })}
      {:else}
        {tn('cache.prefill.games.chosen', checked.length, { count: formatNumber(checked.length) })}
      {/if}
      {#if updated}· {t('cache.prefill.games.updated', { time: formatRelative(updated) })}{/if}
      {#if queued || saving}
        · <span class="saving">{t('cache.prefill.games.saving')}</span>
      {:else if savedOnce}
        · <span class="done"><Icon name="check" size={14} /> {t('cache.prefill.games.saved')}</span>
      {/if}
    </span>
  </div>
  {#if readError}
    <div class="pad"><Notice tone="warn" title={t('cache.prefill.games.readFailed', { store })}>{readError}</Notice></div>
  {/if}
  <Table
    caption={title}
    rows={shown}
    key={(g) => g.id}
    {columns}
    sort={{ key: 'name', desc: false }}
    selectable={session.isAdmin}
    keepHidden
    bind:checked={() => checked, (v) => change(v.map(String))}
    checkLabel={(g) => t('cache.prefill.games.select', { name: g.name })}
    loading={loading && !rows}
    error={loadError}
    {onretry}
    maxHeight="480px"
  >
    {#snippet empty()}
      <EmptyState compact icon="gamepad" title={filter ? t('cache.prefill.games.noMatch') : t('cache.prefill.games.empty')} />
    {/snippet}
  </Table>
</Panel>

<style>
  .bar {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-3);
    align-items: center;
    justify-content: space-between;
    padding: var(--sp-3) var(--sp-4);
  }
  .pad {
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .name {
    display: flex;
    flex-direction: column;
  }
  .sub {
    font-size: var(--fs-xs);
    color: var(--text-3);
  }
  .done {
    color: var(--ok);
    display: inline-flex;
    align-items: center;
    gap: 2px;
  }
</style>
