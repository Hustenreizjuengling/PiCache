<!--
  @component
  Addresses the DHCP server handed out (active and ended within 24 h,
  newest first): host name with its DNS name (or why it has none), address
  with a "reserved" badge, MAC address, when the lease ends, the configured
  client. Admin actions per row: reserve the address (opens the reservation
  panel prefilled), add the device as a client (MAC and address filled in)
  and end the lease.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { Client, ClientGroup, ClientInput, DhcpLease, Resource } from '$lib/api'
  import { api } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, confirm, EmptyState, Field, Input, Menu, Panel, Table, toast, type Column, type MenuItem } from '$lib/ui'
  import ClientPanel from '../clients/ClientPanel.svelte'
  import { ipSortKey } from './net'

  interface Props {
    leases: Resource<DhcpLease[]>
    clients: readonly Client[] | undefined
    groups: readonly ClientGroup[] | undefined
    onreserve: (l: DhcpLease) => void
    onchanged: () => void
    onclientadded: () => void
  }

  let { leases, clients, groups, onreserve, onchanged, onclientadded }: Props = $props()

  let search = $state('')
  const needle = $derived(search.trim().toLowerCase())
  const rows = $derived(
    needle
      ? leases.data?.filter((l) =>
          [l.hostname, l.dnsName, l.ip, l.mac, l.clientName].some((v) => (v ?? '').toLowerCase().includes(needle)),
        )
      : leases.data,
  )

  function nameOf(l: DhcpLease): string {
    return l.hostname || l.ip
  }

  function clientId(l: DhcpLease): number | undefined {
    return l.clientName ? clients?.find((c) => c.name === l.clientName)?.id : undefined
  }

  // ---- actions

  let addOpen = $state(false)
  let preset = $state.raw<Partial<ClientInput>>({})

  function addAsClient(l: DhcpLease) {
    preset = { name: l.hostname ?? '', identifiers: [l.mac, l.ip] }
    addOpen = true
  }

  async function end(l: DhcpLease) {
    const ok = await confirm({
      title: t('dns.dhcp.leases.endTitle', { name: nameOf(l) }),
      message: t('dns.dhcp.leases.endText', { ip: l.ip }),
      confirmLabel: t('dns.dhcp.leases.end'),
      action: () => api.dhcp.endLease(l.mac),
    })
    if (!ok) return
    toast.success(t('dns.dhcp.leases.endedToast', { name: nameOf(l) }))
    onchanged()
  }

  function menu(l: DhcpLease): MenuItem[] {
    const items: MenuItem[] = []
    if (!l.static) items.push({ label: t('dns.dhcp.leases.reserve'), icon: 'pin', onselect: () => onreserve(l) })
    if (!l.clientName) items.push({ label: t('dns.seen.addAsClient'), icon: 'users', onselect: () => addAsClient(l) })
    if (l.active && !l.static) {
      if (items.length > 0) items.push({ separator: true })
      items.push({ label: t('dns.dhcp.leases.end'), icon: 'trash', danger: true, onselect: () => end(l) })
    }
    return items
  }

  const columns = $derived<Column<DhcpLease>[]>([
    { key: 'name', label: t('dns.dhcp.leases.device'), sortable: true, value: (l) => l.hostname ?? '', cell: nameCell },
    { key: 'ip', label: t('dns.dhcp.leases.address'), sortable: true, value: (l) => ipSortKey(l.ip), cell: ipCell },
    { key: 'mac', label: t('dns.dhcp.leases.mac'), mono: true, value: (l) => l.mac },
    { key: 'expires', label: t('dns.dhcp.leases.lease'), sortable: true, value: (l) => l.expires, cell: expiresCell },
    { key: 'client', label: t('dns.dhcp.leases.client'), sortable: true, value: (l) => l.clientName ?? '', cell: clientCell },
    ...(session.isAdmin ? [{ key: 'actions', label: t('common.label.actions'), align: 'right' as const, width: '1%', cell: actionCell }] : []),
  ])
</script>

{#snippet nameCell(l: DhcpLease)}
  <span class="name">
    {#if l.hostname}
      <span>{l.hostname}</span>
    {:else}
      <span class="subtle">{t('dns.dhcp.leases.noName')}</span>
    {/if}
    {#if l.nameConflict}
      <span class="conflict small" title={t('dns.dhcp.leases.conflictHelp')}>{t('dns.dhcp.leases.conflict')}</span>
    {:else if l.dnsName}
      <span class="dns mono small">{l.dnsName}</span>
    {/if}
  </span>
{/snippet}

{#snippet ipCell(l: DhcpLease)}
  <span class="ip">
    <span class="mono">{l.ip}</span>
    {#if l.static}<Badge tone="info">{t('dns.dhcp.leases.reserved')}</Badge>{/if}
  </span>
{/snippet}

{#snippet expiresCell(l: DhcpLease)}
  <span class={['nowrap', !l.active && 'subtle']} title={formatDateTime(l.expires)}>
    {l.active ? t('dns.dhcp.leases.ends', { when: formatRelative(l.expires) }) : t('dns.dhcp.leases.ended', { when: formatRelative(l.expires) })}
  </span>
{/snippet}

{#snippet clientCell(l: DhcpLease)}
  {@const id = clientId(l)}
  {#if l.clientName && id}
    <a href={href('/dns/clients', { sel: id })}>{l.clientName}</a>
  {:else if l.clientName}
    {l.clientName}
  {:else}
    <span class="subtle">{t('dns.dhcp.leases.notClient')}</span>
  {/if}
{/snippet}

{#snippet actionCell(l: DhcpLease)}
  {@const items = menu(l)}
  {#if items.length > 0}
    <Menu iconOnly icon="more" variant="ghost" size="sm" label={t('dns.dhcp.leases.actions', { name: nameOf(l) })} {items} />
  {/if}
{/snippet}

<Panel id="dhcp-leases" flush title={t('dns.dhcp.leases.title')} description={t('dns.dhcp.leases.description')}>
  {#if (leases.data?.length ?? 0) > 0}
    <div class="search">
      <Field label={t('common.action.search')} hideLabel>
        <Input type="search" size="sm" icon="search" bind:value={search} placeholder={t('dns.dhcp.leases.search')} maxlength={256} />
      </Field>
    </div>
  {/if}
  <Table
    {columns}
    {rows}
    key={(l) => l.mac}
    loading={leases.loading && !leases.loaded}
    error={leases.error && !leases.data ? errorText(leases.error) : undefined}
    onretry={() => leases.refresh()}
    caption={t('dns.dhcp.leases.title')}
  >
    {#snippet empty()}
      {#if needle}
        <EmptyState compact title={t('dns.dhcp.leases.emptySearch')} />
      {:else}
        <EmptyState compact icon="tag" title={t('dns.dhcp.leases.empty')} text={t('dns.dhcp.leases.emptyText')} />
      {/if}
    {/snippet}
  </Table>
</Panel>

{#if session.isAdmin}
  <ClientPanel bind:open={addOpen} {preset} {groups} range="24h" onsaved={onclientadded} />
{/if}

<style>
  .search {
    max-width: 360px;
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .name {
    display: flex;
    flex-direction: column;
    padding: 4px 0;
    line-height: 1.3;
    /* Host and DNS names are machine values: kept on one line, the table scrolls sideways. */
    white-space: nowrap;
  }
  .dns {
    color: var(--text-2);
    white-space: nowrap;
  }
  .conflict {
    color: var(--warning);
    font-weight: 600;
    cursor: help;
  }
  .ip {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    white-space: nowrap;
  }
</style>
