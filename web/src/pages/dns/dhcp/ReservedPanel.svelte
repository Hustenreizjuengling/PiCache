<!--
  @component
  Reserved addresses (static leases): device name, address, MAC address,
  comment and whether a device uses it right now. Rows open the editor;
  admins add reservations here.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DhcpStaticLease, Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Panel, Table, type Column } from '$lib/ui'
  import { ipSortKey } from './net'

  interface Props {
    reserved: Resource<DhcpStaticLease[]>
    /** The served network, e.g. "192.168.178.0/24". */
    subnet?: string
    selected?: string
    onadd: () => void
    onedit: (s: DhcpStaticLease) => void
  }

  let { reserved, subnet, selected, onadd, onedit }: Props = $props()

  const columns = $derived<Column<DhcpStaticLease>[]>([
    { key: 'name', label: t('dns.dhcp.static.name'), sortable: true, value: (s) => s.hostname ?? '', cell: nameCell },
    { key: 'ip', label: t('dns.dhcp.static.ip'), mono: true, sortable: true, value: (s) => ipSortKey(s.ip), format: (s) => s.ip },
    { key: 'mac', label: t('dns.dhcp.static.mac'), mono: true, value: (s) => s.mac },
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
    {#if session.isAdmin}
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

<style>
  /* Host names are DNS labels: kept on one line like other machine values. */
  .name {
    white-space: nowrap;
  }
</style>
