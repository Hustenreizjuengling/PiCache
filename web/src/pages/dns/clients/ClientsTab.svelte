<!--
  @component
  Configured clients: name, identifiers, groups, options and their traffic
  over the selected range (all addresses the client was recognised by; the
  ones beyond its identifiers show as "+N addresses"). Rows open the client
  panel; admins select clients to delete them together.
  Query: ?sel=<client id>&group=<group id> (only the clients of that group)
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, type Client, type ClientGroup, type ClientStat, type KnownClient, type RangePreset, type Resource, type UpstreamPreset } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, BulkBar, Button, Chip, EmptyState, IconButton, Panel, Table, type Column } from '$lib/ui'
  import AddressList from '../shared/AddressList.svelte'
  import { runBatch } from '../shared/batch'
  import { groupNames } from '../shared/groups'
  import ClientPanel from './ClientPanel.svelte'
  import { clientTotals, type Totals } from './clientStats'

  interface Props {
    clients: Resource<Client[]>
    groups: readonly ClientGroup[] | undefined
    presets: readonly UpstreamPreset[] | undefined
    stats: readonly ClientStat[] | undefined
    known: readonly KnownClient[] | undefined
    range: RangePreset
    /** The range control of the traffic columns (shown in the panel header). */
    rangePicker: Snippet
    onchanged: () => void
  }

  let { clients, groups, presets, stats, known, range, rangePicker, onchanged }: Props = $props()

  type Row = Client & { totals: Totals }

  let addOpen = $state(false)
  let checked = $state<number[]>([])
  let busy = $state(false)

  // ?group=<id> (from the parental controls page): only the clients of that group.
  const groupId = $derived(Number(router.param('group')) || 0)
  const groupFilter = $derived(groupId ? groups?.find((g) => g.id === groupId) : undefined)
  const rows = $derived<Row[] | undefined>(
    clients.data
      ?.filter((c) => !groupId || c.groupIds.includes(groupId))
      .map((c) => ({ ...c, totals: clientTotals(c, stats, known) })),
  )
  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(rows?.find((c) => c.id === selId))

  async function deleteChecked() {
    busy = true
    try {
      const ok = await runBatch({
        action: 'delete',
        ids: checked,
        what: (n) => tn('dns.clients.count', n),
        run: (req) => api.clients.batch(req),
        deleteText: t('dns.clients.batchDeleteText'),
      })
      if (!ok) return
      if (selected && checked.includes(selected.id)) router.setQuery({ sel: null })
      checked = []
      onchanged()
    } finally {
      busy = false
    }
  }

  function blockedShare(r: Row): number | undefined {
    return r.totals.queries > 0 ? r.totals.blocked / r.totals.queries : undefined
  }

  const columns: Column<Row>[] = $derived([
    { key: 'name', label: t('common.label.name'), sortable: true, value: (c) => c.name, cell: nameCell },
    { key: 'groups', label: t('common.label.groups'), value: (c) => groupNames(c.groupIds, groups) },
    { key: 'options', label: t('dns.clients.options'), cell: optionsCell },
    {
      key: 'queries',
      label: t('dns.clients.queries'),
      align: 'right',
      sortable: true,
      value: (c) => c.totals.queries,
      format: (c) => formatNumber(c.totals.queries),
    },
    {
      key: 'blocked',
      label: t('dns.clients.blocked'),
      align: 'right',
      sortable: true,
      value: (c) => blockedShare(c),
      format: (c) => formatPercent(blockedShare(c)),
    },
    { key: 'seen', label: t('common.label.lastSeen'), sortable: true, value: (c) => c.totals.lastSeen ?? '', cell: seenCell },
  ])
</script>

{#snippet nameCell(c: Row)}
  {@const extra = c.totals.addresses.filter((a) => !c.identifiers.includes(a))}
  <span class="name">
    <span class="truncate strong">{c.name}</span>
    <span class="ids mono truncate" title={c.identifiers.join(', ')}>{c.identifiers.join(', ')}</span>
    {#if extra.length > 0}<span class="ids mono"><AddressList addresses={extra} first="" /></span>{/if}
  </span>
{/snippet}

{#snippet optionsCell(c: Row)}
  <span class="row">
    {#if c.downloadCacheBypass}<Badge title={t('dns.clients.bypassHelp')}>{t('dns.clients.bypassShort')}</Badge>{/if}
    {#if c.ignoreLogs}<Badge title={t('dns.clients.ignoreLogsHelp')}>{t('dns.clients.ignoreLogsShort')}</Badge>{/if}
    {#if c.ignoreStats}<Badge title={t('dns.clients.ignoreStatsHelp')}>{t('dns.clients.ignoreStatsShort')}</Badge>{/if}
    {#if !c.downloadCacheBypass && !c.ignoreLogs && !c.ignoreStats}<span class="subtle">–</span>{/if}
  </span>
{/snippet}

{#snippet seenCell(c: Row)}
  {#if c.totals.lastSeen}
    <span class="nowrap" title={formatDateTime(c.totals.lastSeen)}>{formatRelative(c.totals.lastSeen)}</span>
  {:else}
    <span class="subtle">–</span>
  {/if}
{/snippet}

<Panel flush title={t('dns.clients.title')} description={t('dns.clients.description')}>
  {#snippet actions()}
    {#if groupId}
      <span class="filter">
        <Chip size="sm" label={t('dns.clients.groupFilter', { name: groupFilter?.name ?? `#${groupId}` })} />
        <IconButton icon="close" size="sm" label={t('dns.clients.groupFilterClear')} onclick={() => router.setQuery({ group: null })} />
      </span>
    {/if}
    {@render rangePicker()}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>{t('dns.clients.add')}</Button>
  {/snippet}
  <Table
    {columns}
    {rows}
    key={(c) => c.id}
    loading={clients.loading && !clients.loaded}
    error={clients.error && !clients.data ? errorText(clients.error) : undefined}
    onretry={() => clients.refresh()}
    onrowclick={(c) => router.setQuery({ sel: c.id })}
    selected={selected?.id}
    caption={t('dns.clients.title')}
    selectable={session.isAdmin}
    bind:checked
    checkLabel={(c) => t('dns.clients.selectNamed', { name: c.name })}
  >
    {#snippet empty()}
      {#if groupId && clients.data?.length}
        <EmptyState compact icon="users" title={t('dns.clients.emptyGroup')} text={t('dns.clients.emptyGroupText')}>
          <Button size="sm" onclick={() => router.setQuery({ group: null })}>{t('dns.clients.groupFilterClear')}</Button>
        </EmptyState>
      {:else}
        <EmptyState compact icon="users" title={t('dns.clients.empty')} text={t('dns.clients.emptyText')}>
          <Button size="sm" variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
            {t('dns.clients.add')}
          </Button>
          <Button size="sm" onclick={() => router.setQuery({ tab: 'seen', sel: null })}>{t('dns.clients.showSeen')}</Button>
        </EmptyState>
      {/if}
    {/snippet}
  </Table>
  {#if session.isAdmin}
    <BulkBar
      count={checked.length}
      {busy}
      onclear={() => (checked = [])}
      actions={[{ label: t('common.action.delete'), icon: 'trash', danger: true, onselect: deleteChecked }]}
    />
  {/if}
</Panel>

<ClientPanel bind:open={addOpen} {groups} {presets} {range} onsaved={onchanged} />
<ClientPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  client={selected}
  totals={selected?.totals}
  {groups}
  {presets}
  {range}
  onsaved={onchanged}
  ondeleted={onchanged}
/>

<style>
  .filter {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
  }
  .name {
    display: flex;
    flex-direction: column;
    min-width: 0;
    max-width: 40ch;
    line-height: 1.3;
    padding: 4px 0;
  }
  .strong {
    font-weight: 600;
  }
  .ids {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
</style>
