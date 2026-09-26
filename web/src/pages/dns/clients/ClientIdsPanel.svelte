<!--
  @component
  ClientIDs seen since the start (GET /clients/dns-client-ids, newest
  first; the server keeps them in memory only): the ClientID, the client
  that has it (or "unknown"), the last address that sent it, when and how
  many queries. Admins add an unknown one as a client (identifier
  clientid:<ClientID>, named like it); the owner refreshes the list after
  the client is saved.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { Client, Resource, SeenDnsClientId } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, EmptyState, Panel, Table, type Column } from '$lib/ui'

  interface Props {
    /** GET /clients/dns-client-ids (owned by the caller, which refreshes it after "Add as client"). */
    seen: Resource<SeenDnsClientId[]>
    clients: readonly Client[] | undefined
    /** "Add as client" for a ClientID no client has. */
    onadd: (dnsClientId: string) => void
  }

  let { seen, clients, onadd }: Props = $props()

  function clientName(r: SeenDnsClientId): string | undefined {
    if (!r.clientId) return undefined
    return r.name || clients?.find((c) => c.id === r.clientId)?.name
  }

  const columns: Column<SeenDnsClientId>[] = $derived([
    { key: 'id', label: t('dns.seen.clientIds.clientId'), mono: true, truncate: true, width: '30%', sortable: true, value: (r) => r.dnsClientId },
    { key: 'client', label: t('common.label.client'), width: '1%', sortable: true, value: (r) => clientName(r) ?? '', cell: clientCell },
    { key: 'address', label: t('dns.seen.clientIds.lastAddress'), mono: true, width: '1%', value: (r) => r.address },
    { key: 'seen', label: t('common.label.lastSeen'), width: '1%', sortable: true, value: (r) => r.lastSeen, cell: seenCell },
    {
      key: 'queries',
      label: t('dns.clients.queries'),
      align: 'right',
      width: '1%',
      sortable: true,
      value: (r) => r.queries,
      format: (r) => formatNumber(r.queries),
    },
    ...(session.isAdmin ? [{ key: 'actions', label: t('common.label.actions'), align: 'right' as const, width: '1%', cell: actionCell }] : []),
  ])
</script>

{#snippet clientCell(r: SeenDnsClientId)}
  {#if r.clientId}
    <a class="nowrap" href={href('/dns/clients', { sel: r.clientId })}>{clientName(r) ?? `#${r.clientId}`}</a>
  {:else}
    <span class="subtle">{t('dns.seen.clientIds.unknown')}</span>
  {/if}
{/snippet}

{#snippet actionCell(r: SeenDnsClientId)}
  {#if !r.clientId}
    <Button size="sm" variant="ghost" icon="plus" onclick={() => onadd(r.dnsClientId)}>{t('dns.seen.addAsClient')}</Button>
  {/if}
{/snippet}

{#snippet seenCell(r: SeenDnsClientId)}
  <span class="nowrap" title={formatDateTime(r.lastSeen)}>{formatRelative(r.lastSeen)}</span>
{/snippet}

<Panel flush title={t('dns.seen.clientIds.title')} description={t('dns.seen.clientIds.description')}>
  <Table
    {columns}
    rows={seen.data}
    key={(r) => r.dnsClientId}
    loading={seen.loading && !seen.loaded}
    error={seen.error && !seen.data ? errorText(seen.error) : undefined}
    onretry={() => seen.refresh()}
    caption={t('dns.seen.clientIds.title')}
    compact
  >
    {#snippet empty()}
      <EmptyState compact icon="key" title={t('dns.seen.clientIds.empty')} text={t('dns.seen.clientIds.emptyText')} />
    {/snippet}
  </Table>
</Panel>
