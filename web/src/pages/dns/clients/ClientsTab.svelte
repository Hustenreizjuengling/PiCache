<!--
  @component
  Configured clients: name, identifiers, groups, options and their traffic
  over the selected range. Rows open the client panel.
  Query: ?sel=<client id>
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { Client, ClientGroup, ClientStat, KnownClient, RangePreset, Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, EmptyState, Panel, Table, type Column } from '$lib/ui'
  import { groupNames } from '../shared/groups'
  import ClientPanel from './ClientPanel.svelte'
  import { clientTotals, type Totals } from './clientStats'

  interface Props {
    clients: Resource<Client[]>
    groups: readonly ClientGroup[] | undefined
    stats: readonly ClientStat[] | undefined
    known: readonly KnownClient[] | undefined
    range: RangePreset
    onchanged: () => void
  }

  let { clients, groups, stats, known, range, onchanged }: Props = $props()

  type Row = Client & { totals: Totals }

  let addOpen = $state(false)

  const rows = $derived<Row[] | undefined>(clients.data?.map((c) => ({ ...c, totals: clientTotals(c, stats, known) })))
  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(rows?.find((c) => c.id === selId))

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
  <span class="name">
    <span class="truncate strong">{c.name}</span>
    <span class="ids mono truncate" title={c.identifiers.join(', ')}>{c.identifiers.join(', ')}</span>
  </span>
{/snippet}

{#snippet optionsCell(c: Row)}
  <span class="row">
    {#if c.lanCacheBypass}<Badge title={t('dns.clients.bypassHelp')}>{t('dns.clients.bypassShort')}</Badge>{/if}
    {#if c.ignoreLogs}<Badge title={t('dns.clients.ignoreLogsHelp')}>{t('dns.clients.ignoreLogsShort')}</Badge>{/if}
    {#if !c.lanCacheBypass && !c.ignoreLogs}<span class="subtle">–</span>{/if}
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
  >
    {#snippet empty()}
      <EmptyState compact icon="users" title={t('dns.clients.empty')} text={t('dns.clients.emptyText')}>
        <Button size="sm" variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
          {t('dns.clients.add')}
        </Button>
        <Button size="sm" onclick={() => router.setQuery({ tab: 'seen', sel: null })}>{t('dns.clients.showSeen')}</Button>
      </EmptyState>
    {/snippet}
  </Table>
</Panel>

<ClientPanel bind:open={addOpen} {groups} {range} onsaved={onchanged} />
<ClientPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  client={selected}
  totals={selected?.totals}
  {groups}
  {range}
  onsaved={onchanged}
  ondeleted={onchanged}
/>

<style>
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
