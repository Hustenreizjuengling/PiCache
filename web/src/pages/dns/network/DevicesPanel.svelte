<!--
  @component
  Devices of the neighbour table (router and PiCache left out): name,
  addresses, MAC, last query, queries in 24 h and whether they use PiCache,
  filterable to the ones that do not. Admins can scan the network so idle
  devices show up, and add a device as a client (MAC and addresses filled
  in). The limits of what PiCache can see are explained below the table.
  Query: ?show=unused
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { ClientGroup, ClientInput, DeviceStatus, NetworkCheck, NetworkDevice } from '$lib/api'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Notice, Panel, Spinner, Table, type Column, type Tone } from '$lib/ui'
  import ClientPanel from '../clients/ClientPanel.svelte'
  import { isUla, isV4 } from './checks'

  interface Props {
    net: NetworkCheck | undefined
    loading: boolean
    error?: string
    onretry: () => void
    groups: readonly ClientGroup[] | undefined
    /** A scan request is being sent. */
    starting: boolean
    /** A scan runs (reported by the server, or just started here). */
    scanning: boolean
    /** Addresses of the running scan (from the start response). */
    scanAddresses?: number
    note?: { tone: 'info' | 'warn' | 'fail'; text: string }
    onscan: () => void
    ondismissnote: () => void
    onchanged: () => void
  }

  let { net, loading, error, onretry, groups, starting, scanning, scanAddresses, note, onscan, ondismissnote, onchanged }: Props =
    $props()

  const bridge = $derived(net?.mode === 'bridge')
  const devices = $derived(net?.devices ?? [])
  const unusedCount = $derived(devices.filter((d) => d.status !== 'active').length)
  const onlyUnused = $derived(router.param('show') === 'unused')
  const rows = $derived(net ? (onlyUnused ? devices.filter((d) => d.status !== 'active') : devices) : undefined)

  const STATUS: Record<DeviceStatus, { tone: Tone; rank: number }> = {
    never: { tone: 'warn', rank: 0 },
    inactive: { tone: 'neutral', rank: 1 },
    active: { tone: 'ok', rank: 2 },
  }

  // ---- add as client

  let addOpen = $state(false)
  let preset = $state.raw<Partial<ClientInput>>({})

  function addAsClient(d: NetworkDevice) {
    // MAC plus the stable addresses (IPv4 and ULA); global and link-local IPv6 addresses change.
    preset = { name: d.name ?? '', identifiers: [d.mac, ...d.ips.filter((ip) => isV4(ip) || isUla(ip))] }
    addOpen = true
  }

  const scan = $derived(net?.scan)
  const running = $derived(scanning || !!scan?.running)
  const runningCount = $derived(scan?.addresses ?? scanAddresses)

  const columns = $derived<Column<NetworkDevice>[]>([
    { key: 'name', label: t('dns.network.devices.name'), sortable: true, value: (d) => d.name ?? '', cell: nameCell },
    { key: 'ips', label: t('dns.network.devices.addresses'), cell: ipsCell },
    { key: 'mac', label: t('dns.network.devices.mac'), mono: true, value: (d) => d.mac },
    { key: 'last', label: t('dns.network.devices.lastQuery'), sortable: true, value: (d) => d.lastQuery ?? '', cell: lastCell },
    {
      key: 'queries',
      label: t('dns.network.devices.queries'),
      align: 'right',
      sortable: true,
      value: (d) => d.queries24h,
      format: (d) => formatNumber(d.queries24h),
    },
    { key: 'status', label: t('common.label.status'), sortable: true, value: (d) => STATUS[d.status]?.rank ?? 9, cell: statusCell },
    ...(session.isAdmin ? [{ key: 'actions', label: t('common.label.actions'), align: 'right' as const, width: '1%', cell: actionCell }] : []),
  ])
</script>

{#snippet nameCell(d: NetworkDevice)}
  <span class="dname">
    {#if d.clientId}
      <a href={href('/dns/clients', { sel: d.clientId })}>{d.name || `#${d.clientId}`}</a>
    {:else if d.name}
      {d.name}
    {:else}
      <span class="subtle">{t('dns.network.devices.unknown')}</span>
    {/if}
  </span>
{/snippet}

{#snippet ipsCell(d: NetworkDevice)}
  <span class="ips mono" title={d.ips.join('\n')}>
    {#each d.ips.slice(0, 2) as ip (ip)}<span>{ip}</span>{/each}
    {#if d.ips.length > 2}<span class="more">{tn('dns.network.devices.moreAddresses', d.ips.length - 2)}</span>{/if}
  </span>
{/snippet}

{#snippet lastCell(d: NetworkDevice)}
  {#if d.lastQuery}
    <span class="nowrap" title={formatDateTime(d.lastQuery)}>{formatRelative(d.lastQuery)}</span>
  {:else}
    <span class="subtle">{t('common.state.never')}</span>
  {/if}
{/snippet}

{#snippet statusCell(d: NetworkDevice)}
  <Chip size="sm" tone={STATUS[d.status]?.tone ?? 'neutral'} label={t(`dns.network.devices.status.${d.status}`)} />
{/snippet}

<!-- Configured clients need no action: their name links to the client. -->
{#snippet actionCell(d: NetworkDevice)}
  {#if !d.clientId}
    <!-- Short visible label in narrow languages; the accessible name contains it. -->
    <Button size="sm" variant="ghost" icon="plus" aria-label={t('dns.seen.addAsClient')} onclick={() => addAsClient(d)}>
      {t('dns.network.devices.add')}
    </Button>
  {/if}
{/snippet}

{#snippet limits()}
  <p class="small muted">{t('dns.network.limits')}</p>
{/snippet}

<Panel id="network-devices" flush title={t('dns.network.devices.title')} description={t('dns.network.devices.description')} footer={limits}>
  {#snippet actions()}
    {#if session.isAdmin}
      <Button icon="refresh" loading={starting} disabled={bridge || running || !net} onclick={onscan}>{t('dns.network.scan.button')}</Button>
    {/if}
  {/snippet}

  <div class="top">
    {#if bridge}
      <p class="small muted">{t('dns.network.scan.bridge')}</p>
    {:else if session.isAdmin}
      <p class="small muted">{t('dns.network.scan.explain')}</p>
    {/if}
    {#if bridge}
      <!-- nothing to report: scanning is not possible here -->
    {:else if running}
      <p class="scan small" role="status">
        <Spinner size={14} />
        {runningCount ? tn('dns.network.scan.running', runningCount) : t('dns.network.scan.runningNoCount')}
      </p>
    {:else if scan?.finishedAt}
      <p class="small muted">{tn('dns.network.scan.last', scan.addresses ?? 0, { when: formatRelative(scan.finishedAt) })}</p>
    {/if}
    {#if note}
      <Notice tone={note.tone} ondismiss={ondismissnote}>{note.text}</Notice>
    {/if}
    {#if net && !bridge && devices.length > 0}
      <div class="filters" role="group" aria-label={t('dns.network.devices.filterLabel')}>
        <button type="button" class="fchip" aria-pressed={!onlyUnused} onclick={() => router.setQuery({ show: null })}>
          {t('dns.network.devices.all')} <span class="count">{formatNumber(devices.length)}</span>
        </button>
        <button type="button" class="fchip" aria-pressed={onlyUnused} onclick={() => router.setQuery({ show: 'unused' })}>
          {t('dns.network.devices.unused')} <span class="count">{formatNumber(unusedCount)}</span>
        </button>
      </div>
    {/if}
  </div>

  {#if bridge}
    <div class="msg">
      <EmptyState compact icon="network" title={t('dns.network.bridgeTitle')} text={t('dns.network.bridgeText')} />
    </div>
  {:else}
    <Table
      {columns}
      {rows}
      key={(d) => d.mac}
      {loading}
      {error}
      {onretry}
      caption={t('dns.network.devices.title')}
    >
      {#snippet empty()}
        {#if onlyUnused && devices.length > 0}
          <EmptyState compact icon="success" title={t('dns.network.devices.emptyUnused')} />
        {:else}
          <EmptyState compact icon="network" title={t('dns.network.devices.empty')} text={t('dns.network.devices.emptyText')}>
            {#if session.isAdmin}
              <Button size="sm" icon="refresh" loading={starting} disabled={running} onclick={onscan}>{t('dns.network.scan.button')}</Button>
            {/if}
          </EmptyState>
        {/if}
      {/snippet}
    </Table>
  {/if}
</Panel>

{#if session.isAdmin}
  <ClientPanel bind:open={addOpen} {preset} {groups} range="24h" onsaved={onchanged} />
{/if}

<style>
  .top {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .scan {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .filters {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
    padding-top: var(--sp-1);
  }
  .fchip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: 28px;
    padding: 0 var(--sp-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-pill);
    background: var(--surface);
    color: var(--text-2);
    font-size: var(--fs-sm);
    font-weight: 600;
    cursor: pointer;
  }
  .fchip:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  .fchip[aria-pressed='true'] {
    border-color: var(--text);
    background: var(--text);
    color: var(--surface);
  }
  .count {
    font-weight: 400;
    font-variant-numeric: tabular-nums;
    opacity: 0.8;
  }
  .dname {
    display: inline-block;
    min-width: 8ch;
    overflow-wrap: anywhere;
  }
  .ips {
    display: flex;
    flex-direction: column;
    padding: 4px 0;
    line-height: 1.3;
  }
  /* A split IPv6 address is easily misread: the table scrolls sideways instead. */
  .ips > span {
    white-space: nowrap;
    overflow-wrap: normal;
  }
  .more {
    color: var(--text-3);
    font-family: var(--font);
    font-size: var(--fs-xs);
  }
  .msg {
    padding-bottom: var(--sp-2);
  }
</style>
