<!--
  @component
  Reserved addresses (static leases): device name, address, MAC address,
  lease time and client ID (when any reservation has one), comment and
  whether a device uses it right now. Rows open the editor. Everyone can
  export the list (CSV, hosts file); admins add reservations and import
  lists here.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type DhcpStaticLease, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDuration } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Menu, Panel, Table, type Column, type MenuItem } from '$lib/ui'
  import ImportDialog from './ImportDialog.svelte'
  import { ipSortKey } from './net'

  interface Props {
    reserved: Resource<DhcpStaticLease[]>
    /** The served network, e.g. "192.168.178.0/24". */
    subnet?: string
    selected?: string
    onadd: () => void
    onedit: (s: DhcpStaticLease) => void
    /** A list was imported. */
    onimported: () => void
  }

  let { reserved, subnet, selected, onadd, onedit, onimported }: Props = $props()

  let importOpen = $state(false)

  const count = $derived(reserved.data?.length ?? 0)
  const hasLease = $derived((reserved.data ?? []).some((s) => (s.leaseSeconds ?? 0) > 0))
  const hasClientId = $derived((reserved.data ?? []).some((s) => !!s.clientId))

  const exportItems = $derived<MenuItem[]>([
    { label: t('dns.dhcp.static.exportCsv'), icon: 'document', href: api.dhcp.static.exportUrl('csv') },
    { label: t('dns.dhcp.static.exportHosts'), icon: 'document', href: api.dhcp.static.exportUrl('hosts') },
  ])

  const columns = $derived<Column<DhcpStaticLease>[]>([
    { key: 'name', label: t('dns.dhcp.static.name'), sortable: true, value: (s) => s.hostname ?? '', cell: nameCell },
    { key: 'ip', label: t('dns.dhcp.static.ip'), mono: true, sortable: true, value: (s) => ipSortKey(s.ip), format: (s) => s.ip },
    { key: 'mac', label: t('dns.dhcp.static.mac'), mono: true, value: (s) => s.mac },
    ...(hasLease
      ? [
          {
            key: 'lease',
            label: t('dns.dhcp.static.leaseTime'),
            sortable: true,
            value: (s: DhcpStaticLease) => s.leaseSeconds ?? 0,
            format: (s: DhcpStaticLease) => (s.leaseSeconds ? formatDuration(s.leaseSeconds * 1000) : t('dns.dhcp.static.leaseGlobalCol')),
          },
        ]
      : []),
    ...(hasClientId
      ? [{ key: 'clientId', label: t('dns.dhcp.static.clientId'), mono: true, value: (s: DhcpStaticLease) => s.clientId ?? '', format: (s: DhcpStaticLease) => s.clientId || '–' }]
      : []),
    { key: 'comment', label: t('common.label.comment'), truncate: true, width: '30%', value: (s) => s.comment ?? '' },
    { key: 'status', label: t('common.label.status'), sortable: true, value: (s) => (s.active ? 0 : 1), cell: statusCell },
  ])
</script>

{#snippet nameCell(s: DhcpStaticLease)}
  {#if s.hostname}
    <span class="name">{s.hostname}</span>
  {:else}
    <span class="subtle">{t('dns.dhcp.leases.noName')}</span>
  {/if}
{/snippet}

{#snippet statusCell(s: DhcpStaticLease)}
  <Chip size="sm" tone={s.active ? 'ok' : 'neutral'} label={s.active ? t('dns.dhcp.static.inUse') : t('dns.dhcp.static.unused')} />
{/snippet}

<Panel
  id="dhcp-reserved"
  flush
  title={t('dns.dhcp.static.title')}
  description={subnet ? t('dns.dhcp.static.description', { subnet }) : t('dns.dhcp.static.descriptionNoSubnet')}
>
  {#snippet actions()}
    <Menu label={t('dns.dhcp.static.export')} icon="download" align="start" items={exportItems} disabled={count === 0} />
    {#if session.isAdmin}
      <Button icon="upload" onclick={() => (importOpen = true)}>{t('dns.dhcp.static.import')}</Button>
      <Button icon="plus" onclick={onadd}>{t('dns.dhcp.static.add')}</Button>
    {/if}
  {/snippet}
  <Table
    {columns}
    rows={reserved.data}
    key={(s) => s.mac}
    loading={reserved.loading && !reserved.loaded}
    error={reserved.error && !reserved.data ? errorText(reserved.error) : undefined}
    onretry={() => reserved.refresh()}
    onrowclick={onedit}
    {selected}
    caption={t('dns.dhcp.static.title')}
  >
    {#snippet empty()}
      <EmptyState compact icon="pin" title={t('dns.dhcp.static.empty')} text={t('dns.dhcp.static.emptyText')}>
        {#if session.isAdmin}
          <Button size="sm" icon="plus" onclick={onadd}>{t('dns.dhcp.static.add')}</Button>
        {/if}
      </EmptyState>
    {/snippet}
  </Table>
</Panel>

{#if session.isAdmin}
  <ImportDialog bind:open={importOpen} {onimported} />
{/if}

<style>
  /* Host names are DNS labels: kept on one line like other machine values. */
  .name {
    white-space: nowrap;
  }
</style>
