<!--
  @component
  Blocklists tab: every list with its state, entries, last update and
  problems; add from the catalogue or by URL, update one or all, enable or
  disable inline, and open a list for details, settings and Delete.
  Query: ?sel=<list id>
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type ClientGroup, type FilterList } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, IconButton, Panel, Table, toast, Toggle, type Column } from '$lib/ui'
  import { groupNames } from '../shared/groups'
  import AddListDialog from './AddListDialog.svelte'
  import ListPanel from './ListPanel.svelte'
  import { listInput, listStatus, skippedLines } from './listStatus'

  interface Props {
    groups: readonly ClientGroup[] | undefined
    /** Lists are being downloaded right now (poll faster). */
    updating: boolean
    /** Something changed that affects the filter statistics. */
    onchanged: () => void
  }

  let { groups, updating, onchanged }: Props = $props()

  const lists = resource((signal) => api.filter.lists.list({ signal }), { interval: 10_000 })

  let addOpen = $state(false)
  let refreshing = $state<number[]>([])
  let refreshingAll = $state(false)
  let toggling = $state<number[]>([])

  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(lists.data?.find((l) => l.id === selId))

  // A background update finished: show the new state right away.
  let wasUpdating = false
  $effect(() => {
    if (wasUpdating && !updating) void lists.refresh()
    wasUpdating = updating
  })

  function replace(l: FilterList) {
    const cur = lists.data ?? []
    lists.set(cur.some((x) => x.id === l.id) ? cur.map((x) => (x.id === l.id ? l : x)) : [...cur, l])
    onchanged()
  }

  function removed(id: number) {
    lists.set((lists.data ?? []).filter((x) => x.id !== id))
    router.setQuery({ sel: null })
    onchanged()
  }

  async function setEnabled(l: FilterList, enabled: boolean) {
    toggling = [...toggling, l.id]
    try {
      replace(await api.filter.lists.update(l.id, { ...listInput(l), enabled }))
      toast.success(enabled ? t('dns.lists.enabledToast', { name: l.name }) : t('dns.lists.disabledToast', { name: l.name }))
    } catch (e) {
      toast.error(e)
      void lists.refresh()
    } finally {
      toggling = toggling.filter((x) => x !== l.id)
    }
  }

  async function refreshOne(l: FilterList) {
    refreshing = [...refreshing, l.id]
    try {
      const updated = await api.filter.lists.refresh(l.id)
      replace(updated)
      if (updated.status === 'failed-cached' || updated.status === 'failed-empty') {
        toast.error(updated.lastError || listStatus(updated.status).label)
      } else {
        toast.success(tn('dns.lists.updatedNamed', updated.entries, { name: updated.name, count: formatNumber(updated.entries) }))
      }
    } catch (e) {
      toast.error(e)
    } finally {
      refreshing = refreshing.filter((x) => x !== l.id)
    }
  }

  async function refreshAll() {
    refreshingAll = true
    try {
      await api.filter.lists.refreshAll()
      toast.info(t('dns.lists.updatingAll'))
      onchanged()
    } catch (e) {
      toast.error(e)
    } finally {
      refreshingAll = false
    }
  }

  const columns: Column<FilterList>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'name', label: t('common.label.name'), sortable: true, value: (l) => l.name, cell: nameCell },
    { key: 'kind', label: t('dns.lists.kind'), value: (l) => (l.kind === 'allow' ? t('dns.rules.action.allow') : t('dns.rules.action.block')) },
    { key: 'status', label: t('common.label.status'), cell: statusCell },
    {
      key: 'entries',
      label: t('dns.lists.entries'),
      align: 'right',
      sortable: true,
      value: (l) => l.entries,
      format: (l) => formatNumber(l.entries),
    },
    { key: 'updated', label: t('dns.lists.lastUpdated'), sortable: true, value: (l) => l.lastUpdated ?? '', cell: updatedCell },
    { key: 'problems', label: t('dns.lists.problems'), cell: problemsCell },
    { key: 'groups', label: t('common.label.groups'), value: (l) => groupNames(l.groupIds, groups) },
    { key: 'actions', label: t('common.label.actions'), align: 'right', width: '1%', cell: actionsCell },
  ])
</script>

{#snippet enabledCell(l: FilterList)}
  <Toggle
    bind:checked={() => l.enabled, (on) => setEnabled(l, on)}
    ariaLabel={t('dns.lists.enableNamed', { name: l.name })}
    disabled={!session.isAdmin || toggling.includes(l.id)}
  />
{/snippet}

{#snippet nameCell(l: FilterList)}
  <span class="name">
    <span class="truncate">{l.name}</span>
    <span class="url mono truncate" title={l.url}>{l.url}</span>
  </span>
{/snippet}

{#snippet statusCell(l: FilterList)}
  {@const s = listStatus(l.status)}
  <Chip size="sm" tone={s.tone} label={s.label} title={l.lastError} />
{/snippet}

{#snippet updatedCell(l: FilterList)}
  {#if l.lastUpdated}
    <span class="nowrap" title={formatDateTime(l.lastUpdated)}>{formatRelative(l.lastUpdated)}</span>
  {:else}
    <span class="subtle">{t('common.state.never')}</span>
  {/if}
{/snippet}

{#snippet problemsCell(l: FilterList)}
  {#if l.lastError}
    <span class="problem truncate" title={l.lastError}>{l.lastError}</span>
  {:else if skippedLines(l) > 0}
    <span class="muted nowrap" title={t('dns.lists.skippedHelp')}>{tn('dns.lists.skipped', skippedLines(l), { count: formatNumber(skippedLines(l)) })}</span>
  {:else}
    <span class="subtle">–</span>
  {/if}
{/snippet}

{#snippet actionsCell(l: FilterList)}
  <IconButton
    icon="refresh"
    size="sm"
    label={t('dns.lists.updateNamed', { name: l.name })}
    loading={refreshing.includes(l.id)}
    disabled={!session.isAdmin || !l.enabled}
    onclick={() => refreshOne(l)}
  />
{/snippet}

<Panel flush title={t('dns.lists.title')} description={t('dns.lists.description')}>
  {#snippet actions()}
    <Button icon="refresh" loading={refreshingAll || updating} disabled={!session.isAdmin || !lists.data?.length} onclick={refreshAll}>
      {updating ? t('dns.lists.updatingShort') : t('dns.lists.updateAll')}
    </Button>
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>{t('dns.lists.add')}</Button>
  {/snippet}

  <Table
    {columns}
    rows={lists.data}
    key={(l) => l.id}
    loading={lists.loading && !lists.loaded}
    error={lists.error && !lists.data ? errorText(lists.error) : undefined}
    onretry={() => lists.refresh()}
    onrowclick={(l) => router.setQuery({ sel: l.id })}
    selected={selected?.id}
    caption={t('dns.lists.title')}
  >
    {#snippet empty()}
      <EmptyState compact icon="shield" title={t('dns.lists.empty')} text={t('dns.lists.emptyText')}>
        <Button size="sm" variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
          {t('dns.lists.add')}
        </Button>
      </EmptyState>
    {/snippet}
  </Table>
</Panel>

<AddListDialog bind:open={addOpen} {groups} lists={lists.data} onadded={replace} />

<ListPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  list={selected}
  {groups}
  onchanged={replace}
  ondeleted={removed}
/>

<style>
  .name {
    display: flex;
    flex-direction: column;
    min-width: 0;
    max-width: 32ch;
    line-height: 1.3;
    padding: 4px 0;
  }
  .url {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
  .problem {
    display: block;
    max-width: 28ch;
    color: var(--warning);
  }
</style>
