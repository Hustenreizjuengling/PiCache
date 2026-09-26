<!--
  @component
  Recently seen devices (/clients/known grouped by MAC address, newest
  activity first): addresses ("+N addresses" expands), host name, MAC, the
  client they belong to and their traffic (summed over their addresses).
  Unconfigured devices can be added as a client in one step (MAC address
  plus IPv4 and ULA addresses). Admins can block a device's DNS queries
  (by its MAC address when known) or lift the entries that block it.
  Query: ?tab=seen&within=24h|7d|30d&ip=<address> (selects the device with that address)
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { Client, ClientGroup, ClientInput, ClientStat, KnownClient, Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, EmptyState, IconButton, KeyValue, Notice, Panel, Select, SidePanel, Table, type Column } from '$lib/ui'
  import { isV4 } from '../network/checks'
  import { clientValues } from '../querylog/filters'
  import AddressList from '../shared/AddressList.svelte'
  import { confirmBlockDevice, confirmUnblock } from '../shared/blockClient'
  import ClientPanel from './ClientPanel.svelte'
  import { clientFromKnown, deviceTotals, seenDevices, type SeenDevice, type Totals, type TrafficRange } from './clientStats'

  interface Props {
    known: Resource<KnownClient[]>
    clients: readonly Client[] | undefined
    groups: readonly ClientGroup[] | undefined
    /** Traffic per address (not grouped by device). */
    stats: readonly ClientStat[] | undefined
    range: TrafficRange
    /** The range control of the traffic columns (shown in the panel header). */
    rangePicker: Snippet
    within: string
    onwithin: (within: string) => void
    onchanged: () => void
  }

  let { known, clients, groups, stats, range, rangePicker, within, onwithin, onchanged }: Props = $props()

  type Row = SeenDevice & { stat?: Totals }

  let addOpen = $state(false)
  let addPreset = $state.raw<Partial<ClientInput>>({})

  const rows = $derived<Row[] | undefined>(known.data && seenDevices(known.data).map((d) => ({ ...d, stat: deviceTotals(d.addresses, stats) })))
  const selIp = $derived(router.param('ip').trim().toLowerCase())
  const selected = $derived(selIp ? rows?.find((d) => d.addresses.includes(selIp)) : undefined)
  const missing = $derived(!!selIp && known.loaded && !selected)

  function clientName(d: Pick<SeenDevice, 'clientId' | 'name'>): string | undefined {
    if (!d.clientId) return undefined
    return d.name || clients?.find((c) => c.id === d.clientId)?.name
  }

  function addAsClient(d: Pick<SeenDevice, 'ip' | 'mac' | 'hostname' | 'addresses'>) {
    addPreset = clientFromKnown(d)
    addOpen = true
  }

  function saved() {
    router.setQuery({ ip: null })
    onchanged()
  }

  async function block(d: SeenDevice) {
    if (await confirmBlockDevice(d.ip, clientName(d) ?? d.hostname)) void known.refresh()
  }

  // GET /clients/known names only the first matching entry per address: after
  // removing it, another entry (e.g. the MAC address) may still block the device.
  async function unblock(d: SeenDevice) {
    let entries = d.blockedBy
    for (let more = false; entries.length > 0; more = true) {
      if (!(await confirmUnblock(entries, more))) return
      await known.refresh()
      const still = known.data ? seenDevices(known.data).find((x) => x.key === d.key)?.blockedBy : undefined
      entries = still?.filter((e) => !entries.includes(e)) ?? []
    }
  }

  /** The address for the downloads page (downloads come over IPv4 almost always). */
  function downloadAddress(d: SeenDevice): string {
    return d.addresses.find(isV4) ?? d.ip
  }

  const withinOptions = $derived([
    { value: '24h', label: t('common.range.long.24h') },
    { value: '7d', label: t('common.range.long.7d') },
    { value: '30d', label: t('common.range.long.30d') },
  ])

  const columns: Column<Row>[] = $derived([
    { key: 'ip', label: t('dns.seen.addresses'), sortable: true, value: (d) => d.ip, cell: addressCell },
    { key: 'host', label: t('dns.seen.hostname'), truncate: true, width: '25%', sortable: true, value: (d) => d.hostname ?? '' },
    { key: 'mac', label: t('dns.seen.mac'), mono: true, value: (d) => d.mac ?? '' },
    { key: 'client', label: t('common.label.client'), sortable: true, value: (d) => clientName(d) ?? '', cell: clientCell },
    {
      key: 'queries',
      label: t('dns.clients.queries'),
      align: 'right',
      sortable: true,
      value: (d) => d.stat?.queries ?? 0,
      format: (d) => (d.stat ? formatNumber(d.stat.queries) : '–'),
    },
    {
      key: 'blocked',
      label: t('dns.clients.blocked'),
      align: 'right',
      sortable: true,
      value: (d) => d.stat?.blocked ?? 0,
      format: (d) => (d.stat ? formatNumber(d.stat.blocked) : '–'),
    },
    { key: 'seen', label: t('common.label.lastSeen'), sortable: true, value: (d) => d.lastSeen, cell: seenCell },
    ...(session.isAdmin ? [{ key: 'actions', label: t('common.label.actions'), align: 'right' as const, width: '1%', cell: actionCell }] : []),
  ])
</script>

{#snippet addressCell(d: Row)}
  <span class="addr-cell">
    <AddressList addresses={d.addresses} />
    {#if d.blockedBy.length > 0}
      <Badge tone="fail" title={t('dns.seen.blockedBy', { entry: d.blockedBy.join(', ') })}>{t('dns.seen.blocked')}</Badge>
    {/if}
  </span>
{/snippet}

<!-- Compact: the table is wide already (icon only for the common action, its name as tooltip). -->
{#snippet actionCell(d: Row)}
  {#if d.blockedBy.length > 0}
    <Button size="sm" variant="ghost" onclick={() => unblock(d)}>{t('dns.shared.unblock')}</Button>
  {:else}
    <IconButton icon="ban" size="sm" label={t('dns.shared.blockDevice')} onclick={() => block(d)} />
  {/if}
{/snippet}

{#snippet clientCell(d: Row)}
  {#if d.clientId}
    <a href={href('/dns/clients', { sel: d.clientId })}>{clientName(d) ?? `#${d.clientId}`}</a>
  {:else}
    <Button size="sm" variant="ghost" icon="plus" disabled={!session.isAdmin} onclick={() => addAsClient(d)}>
      {t('dns.seen.addAsClient')}
    </Button>
  {/if}
{/snippet}

{#snippet seenCell(d: Row)}
  <span class="nowrap" title={formatDateTime(d.lastSeen)}>{formatRelative(d.lastSeen)}</span>
{/snippet}

<div class="stack">
  {#if missing}
    <Notice tone="info" title={t('dns.seen.unknownTitle', { ip: selIp })}>
      {t('dns.seen.unknownText')}
      {#snippet actions()}
        <Button size="sm" icon="plus" disabled={!session.isAdmin} onclick={() => addAsClient({ ip: selIp, addresses: [selIp] })}>
          {t('dns.seen.addAsClient')}
        </Button>
      {/snippet}
    </Notice>
  {/if}

  <Panel flush title={t('dns.seen.title')} description={t(`dns.seen.description.${range}`)}>
    {#snippet actions()}
      {@render rangePicker()}
      <!-- Labelled like the range picker next to it: a different time range (which devices are listed). -->
      <label class="within">
        <span class="small muted">{t('dns.seen.within')}</span>
        <Select size="sm" aria-label={t('dns.seen.within')} value={within} options={withinOptions} onchange={(e) => onwithin(e.currentTarget.value)} />
      </label>
    {/snippet}
    <Table
      {columns}
      {rows}
      key={(d) => d.key}
      loading={known.loading && !known.loaded}
      error={known.error && !known.data ? errorText(known.error) : undefined}
      onretry={() => known.refresh()}
      onrowclick={(d) => router.setQuery({ ip: d.ip })}
      selected={selected?.key}
      caption={t('dns.seen.title')}
    >
      {#snippet empty()}
        <EmptyState compact icon="users" title={t('dns.seen.empty')} text={t('dns.seen.emptyText')} />
      {/snippet}
    </Table>
  </Panel>
</div>

<SidePanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ ip: null })}
  title={selected?.hostname || selected?.ip || ''}
  subtitle={selected?.hostname ? selected.ip : undefined}
>
  {#if selected}
    {@const s = selected.stat}
    <div class="stack">
      <KeyValue
        items={[
          { label: t('dns.seen.hostname'), value: selected.hostname },
          { label: t('dns.seen.mac'), value: selected.mac, mono: true },
          { label: t('common.label.client'), value: clientName(selected) ?? t('dns.seen.notConfigured') },
          { label: t('dns.seen.firstSeen'), value: formatDateTime(selected.firstSeen) },
          { label: t('common.label.lastSeen'), value: formatDateTime(selected.lastSeen) },
          { label: t('dns.seen.queriesSeen'), value: formatNumber(selected.queries) },
          ...(selected.blockedBy.length > 0 ? [{ label: t('dns.seen.blockedByLabel'), value: selected.blockedBy.join(', '), mono: true }] : []),
        ]}
      />
      <section class="stack-sm" aria-labelledby="seen-addresses">
        <h3 id="seen-addresses">{t('dns.seen.addresses')}</h3>
        <ul class="addrs">
          {#each selected.entries as e (e.ip)}
            <li>
              <span class="mono">{e.ip}</span>
              <span class="small muted nowrap" title={formatDateTime(e.lastSeen)}>{formatRelative(e.lastSeen)}</span>
            </li>
          {/each}
        </ul>
      </section>
      <section class="stack-sm" aria-labelledby="seen-stats">
        <h3 id="seen-stats">{t('dns.clients.statsTitle')} <span class="muted small">· {t(`common.range.long.${range}`)}</span></h3>
        <KeyValue
          items={[
            { label: t('dns.clients.queries'), value: formatNumber(s?.queries ?? 0) },
            {
              label: t('dns.clients.blocked'),
              value: s?.queries ? `${formatNumber(s.blocked)} (${formatPercent(s.blocked / s.queries)})` : formatNumber(0),
            },
            { label: t('dns.clients.cacheServed'), value: formatBytes(s?.cacheBytes ?? 0) },
            { label: t('dns.clients.cacheHit'), value: s?.cacheBytes ? formatPercent(s.cacheHitBytes / s.cacheBytes) : undefined },
          ]}
        />
      </section>
      <div class="row">
        {#if selected.clientId}
          <Button icon="user" href={href('/dns/clients', { sel: selected.clientId })}>{t('dns.seen.openClient')}</Button>
        {:else}
          <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => selected && addAsClient(selected)}>
            {t('dns.seen.addAsClient')}
          </Button>
        {/if}
        <Button variant="ghost" icon="list" href={href('/dns/queries', { client: clientValues(selected.addresses) })}>
          {t('dns.clients.showQueries')}
        </Button>
        <Button variant="ghost" icon="download" href={href('/cache/downloads', { client: downloadAddress(selected) })}>
          {t('dns.clients.showDownloads')}
        </Button>
        {#if session.isAdmin}
          {#if selected.blockedBy.length > 0}
            <Button onclick={() => selected && unblock(selected)}>{t('dns.shared.unblock')}</Button>
          {:else}
            <Button icon="ban" onclick={() => selected && block(selected)}>
              {t('dns.shared.blockDevice')}
            </Button>
          {/if}
        {/if}
      </div>
    </div>
  {/if}
</SidePanel>

<ClientPanel bind:open={addOpen} preset={addPreset} {groups} {range} onsaved={saved} />

<style>
  .addr-cell {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-1) var(--sp-2);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .within {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
  .addrs {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-sm);
  }
  .addrs li {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0 var(--sp-3);
    min-width: 0;
  }
  .addrs .mono {
    overflow-wrap: anywhere;
  }
</style>
