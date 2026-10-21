<!--
  @component
  The games of the Steam account: choose which ones the prefill downloads.
  The list comes from the prefill service (read from Steam at most every
  12 hours, or now with "Read again"); the selection is saved explicitly.
  Each game shows its download size for the chosen platforms and
  languages, and whether its current version is already in the cache.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type PrefillStatus, type PrefillSteamApp } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDate, formatNumber, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Input, Notice, Panel, Table, toast, type Column } from '$lib/ui'

  let { status, onchanged }: { status: PrefillStatus; onchanged: () => void } = $props()

  const apps = resource((signal) => api.prefill.steamApps(false, { signal }), { interval: 60_000 })

  // The selection being edited; undefined = the saved one.
  let draft = $state<number[] | undefined>(undefined)
  const saved = $derived(status.settings.steam.apps)
  const checked = $derived(draft ?? saved)
  const dirty = $derived(draft !== undefined && (draft.length !== saved.length || draft.some((id) => !saved.includes(id))))

  let filter = $state('')
  const rows = $derived.by(() => {
    const q = filter.trim().toLowerCase()
    const list = apps.data?.apps ?? []
    return q ? list.filter((a) => a.name.toLowerCase().includes(q) || String(a.id) === q) : list
  })
  const selectedBytes = $derived(
    (apps.data?.apps ?? []).filter((a) => checked.includes(a.id)).reduce((n, a) => n + a.bytes, 0),
  )

  let reading = $state(false)
  async function readAgain() {
    reading = true
    try {
      const res = await api.prefill.steamApps(true)
      apps.set(res)
      if (res.error) toast.error(res.error)
    } catch (err) {
      toast.error(err)
    } finally {
      reading = false
    }
  }

  let saving = $state(false)
  async function save() {
    if (!draft) return
    saving = true
    try {
      await api.prefill.steamSelect(draft)
      draft = undefined
      toast.success(t('cache.prefill.games.saved'))
      onchanged()
      void apps.refresh()
    } catch (err) {
      toast.error(err)
    } finally {
      saving = false
    }
  }

  function stateOf(a: PrefillSteamApp): { label: string; tone: 'ok' | 'warn' | 'neutral' } {
    if (!a.prefilled) return { label: t('cache.prefill.games.never'), tone: 'neutral' }
    if (a.current) return { label: t('cache.prefill.games.current'), tone: 'ok' }
    return { label: t('cache.prefill.games.update'), tone: 'warn' }
  }

  const columns: Column<PrefillSteamApp>[] = $derived([
    { key: 'name', label: t('cache.prefill.games.name'), sortable: true, value: (a) => a.name, cell: nameCell, width: '60%' },
    { key: 'bytes', label: t('cache.prefill.games.size'), sortable: true, align: 'right', value: (a) => a.bytes, format: (a) => (a.bytes ? formatBytes(a.bytes) : '–') },
    { key: 'state', label: t('cache.prefill.games.state'), value: (a) => (a.prefilled ? (a.current ? 2 : 1) : 0), cell: stateCell },
  ])
</script>

{#snippet nameCell(a: PrefillSteamApp)}
  <span class="name">
    <span>{a.name}</span>
    <span class="sub mono">{a.id}</span>
  </span>
{/snippet}

{#snippet stateCell(a: PrefillSteamApp)}
  {@const s = stateOf(a)}
  <span class="row nowrap">
    <Chip size="sm" tone={s.tone} label={s.label} />
    {#if a.prefilled}<span class="muted small" title={formatDate(a.prefilled)}>{formatRelative(a.prefilled)}</span>{/if}
  </span>
{/snippet}

<Panel flush title={t('cache.prefill.games.title')} description={t('cache.prefill.games.description')}>
  {#snippet actions()}
    <Button icon="refresh" loading={reading} disabled={!session.canOperate} onclick={readAgain}>{t('cache.prefill.games.read')}</Button>
  {/snippet}
  <div class="bar">
    <Input type="search" icon="search" size="sm" bind:value={filter} placeholder={t('cache.prefill.games.filter')} aria-label={t('cache.prefill.games.filter')} />
    <span class="muted small">
      {tn('cache.prefill.games.selected', checked.length, { count: formatNumber(checked.length), size: formatBytes(selectedBytes) })}
      {#if apps.data?.updated}· {t('cache.prefill.games.updated', { time: formatRelative(apps.data.updated) })}{/if}
    </span>
  </div>
  {#if apps.data?.error}
    <div class="pad"><Notice tone="warn" title={t('cache.prefill.games.readFailed')}>{apps.data.error}</Notice></div>
  {/if}
  <Table
    caption={t('cache.prefill.games.title')}
    {rows}
    key={(a) => a.id}
    {columns}
    sort={{ key: 'name', desc: false }}
    selectable={session.canOperate}
    bind:checked={() => checked, (v) => (draft = v.map(Number))}
    checkLabel={(a) => t('cache.prefill.games.select', { name: a.name })}
    loading={apps.loading && !apps.loaded}
    error={apps.error ? errorText(apps.error) : undefined}
    onretry={() => apps.refresh()}
    maxHeight="480px"
  >
    {#snippet empty()}
      <EmptyState compact icon="gamepad" title={filter ? t('cache.prefill.games.noMatch') : t('cache.prefill.games.empty')} />
    {/snippet}
  </Table>
  {#if dirty}
    <div class="save">
      <Button variant="ghost" onclick={() => (draft = undefined)}>{t('cache.prefill.games.discard')}</Button>
      <Button variant="primary" icon="check" loading={saving} onclick={save}>{t('cache.prefill.games.save')}</Button>
    </div>
  {/if}
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
  .save {
    display: flex;
    justify-content: flex-end;
    gap: var(--sp-2);
    padding: var(--sp-3) var(--sp-4);
    border-top: 1px solid var(--line);
  }
</style>
