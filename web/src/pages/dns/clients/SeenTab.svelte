<!--
  @component
  Recently seen devices (/clients/known grouped by MAC address, newest
  activity first): addresses ("+N addresses" expands) with the interface
  of the most recent one and the network owner of a public one (WHOIS),
  host name, MAC address with its manufacturer (or "Private address
  (randomised)"), the client they belong to and their traffic (summed over
  their addresses). Unconfigured devices can be added as a client in one
  step (MAC address plus IPv4 and ULA addresses). A device's panel shows
  its activity over the range and every address. Admins can block a
  device's DNS queries (by its MAC address when known) or lift the entries
  that block it, and forget a device or one address (it reappears with its
  next query); "Forget all" needs destructive rights. A device whose
  address sent a ClientID (DoT, DoH) shows "via ClientID"; the ClientIDs
  seen since the start and the device-name sources follow in their own
  panels. On a follower that syncs clients and groups (or the DNS
  settings), adding clients (or blocking devices) is left out.
  Query: ?tab=seen&within=24h|7d|30d|<retention>d&ip=<address> (selects the device with that address;
  the choices end at logs.seenRetentionDays, the longest is the whole retention)
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type Client, type ClientGroup, type ClientInput, type ClientStat, type KnownClient, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, confirm, EmptyState, IconButton, KeyValue, Notice, Panel, Select, SidePanel, Table, toast, type Column } from '$lib/ui'
  import { isV4 } from '../network/checks'
  import { clientValues } from '../querylog/filters'
  import AddressList from '../shared/AddressList.svelte'
  import { confirmBlockDevice, confirmUnblock } from '../shared/blockClient'
  import { CLIENT_ID_PREFIX } from '../shared/clientid'
  import MacVendor from '../shared/MacVendor.svelte'
  import { vendorText } from '../shared/vendor'
  import ActivityChart from './ActivityChart.svelte'
  import ClientIdsPanel from './ClientIdsPanel.svelte'
  import ClientPanel from './ClientPanel.svelte'
  import { clientFromKnown, deviceTotals, seenDevices, seriesKey, type SeenDevice, type Totals, type TrafficRange } from './clientStats'
  import NameSourcesPanel from './NameSourcesPanel.svelte'

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
    /** The choices of "Seen within" (up to logs.seenRetentionDays). */
    withinOptions: readonly string[]
    onwithin: (within: string) => void
    onchanged: () => void
  }

  let { known, clients, groups, stats, range, rangePicker, within, withinOptions: choices, onwithin, onchanged }: Props = $props()

  type Row = SeenDevice & { stat?: Totals }

  let addOpen = $state(false)
  let addPreset = $state.raw<Partial<ClientInput>>({})

  // Adding clients writes clients and groups; blocking writes the DNS settings: either may be synced from a primary.
  const canAdd = $derived(session.canEditSection('clients-and-groups'))
  const canBlock = $derived(session.canEditSection('dns-settings'))

  // The ClientIDs seen since the start (their panel below): refreshed after a
  // client is saved, so an added ClientID no longer shows as unknown.
  const dnsClientIds = resource((signal) => api.clients.dnsClientIds({ signal }), { interval: 60_000 })

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

  function addClientId(id: string) {
    addPreset = { name: id, identifiers: [CLIENT_ID_PREFIX + id] }
    addOpen = true
  }

  function saved() {
    router.setQuery({ ip: null })
    void dnsClientIds.refresh()
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

  // ---- forgetting (seen data only: clients, the query log and the statistics stay)

  /** Forgets a device (every address of its MAC address) or, without a MAC or with `ip`, one address. */
  async function forget(d: SeenDevice, ip?: string) {
    const which = ip || !d.mac ? { ip: ip ?? d.ip } : { mac: d.mac }
    let deleted = 0
    const ok = await confirm({
      title: t('dns.seen.forgetTitle', { name: ip ?? clientName(d) ?? d.hostname ?? d.ip }),
      message: 'mac' in which ? t('dns.seen.forgetDeviceText') : t('dns.seen.forgetAddressText'),
      confirmLabel: t('dns.seen.forget'),
      action: async () => {
        deleted = (await api.clients.forget(which)).deleted
      },
    })
    if (!ok) return
    toast.success(tn('dns.seen.forgotten', deleted))
    // The panel stays open on the device's other addresses.
    if (!ip || d.addresses.length === 1) router.setQuery({ ip: null })
    else if (ip === selIp) router.setQuery({ ip: d.addresses.find((a) => a !== ip) ?? null })
    void known.refresh()
  }

  async function forgetAll() {
    let deleted = 0
    const ok = await confirm({
      title: t('dns.seen.forgetAllTitle'),
      message: t('dns.seen.forgetAllText'),
      confirmLabel: t('dns.seen.forgetAll'),
      action: async () => {
        deleted = (await api.clients.forgetAll()).deleted
      },
    })
    if (!ok) return
    toast.success(tn('dns.seen.forgotten', deleted))
    router.setQuery({ ip: null })
    void known.refresh()
  }

  /** The address for the downloads page (downloads come over IPv4 almost always). */
  function downloadAddress(d: SeenDevice): string {
    return d.addresses.find(isV4) ?? d.ip
  }

  function whoisText(w: KnownClient['whois']): string | undefined {
    if (!w) return undefined
    return w.country ? `${w.org} (${w.country})` : w.org
  }

  const withinOptions = $derived(
    choices.map((w) => ({
      value: w,
      label: w === '24h' || w === '7d' || w === '30d' ? t(`common.range.long.${w}`) : t('dns.seen.withinDays', { days: Number.parseInt(w) }),
    })),
  )

  const columns: Column<Row>[] = $derived([
    { key: 'ip', label: t('dns.seen.addresses'), sortable: true, value: (d) => d.ip, cell: addressCell },
    { key: 'host', label: t('dns.seen.hostname'), truncate: true, width: '25%', sortable: true, value: (d) => d.hostname ?? '' },
    { key: 'mac', label: t('dns.seen.mac'), sortable: true, value: (d) => vendorText(d) ?? '', cell: macCell },
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
    <span class="addr-line">
      <AddressList addresses={d.addresses} />
      {#if d.blockedBy.length > 0}
        <Badge tone="fail" title={t('dns.seen.blockedBy', { entry: d.blockedBy.join(', ') })}>{t('dns.seen.blocked')}</Badge>
      {/if}
    </span>
    {#if d.interface || d.whois}
      <span class="where">
        {#if d.interface}<span class="mono" title={t('dns.seen.interfaceHelp')}>{d.interface}</span>{/if}
        {#if d.whois}<span class="owner" title={t('dns.seen.networkOwner')}>{whoisText(d.whois)}</span>{/if}
      </span>
    {/if}
  </span>
{/snippet}

{#snippet macCell(d: Row)}
  <MacVendor mac={d.mac} vendor={d.vendor} macRandomized={d.macRandomized} />
{/snippet}

<!-- Compact: the table is wide already (icons only, their names as tooltips). -->
{#snippet actionCell(d: Row)}
  <span class="acts">
    {#if canBlock}
      {#if d.blockedBy.length > 0}
        <Button size="sm" variant="ghost" onclick={() => unblock(d)}>{t('dns.shared.unblock')}</Button>
      {:else}
        <IconButton icon="ban" size="sm" label={t('dns.shared.blockDevice')} onclick={() => block(d)} />
      {/if}
    {/if}
    <IconButton icon="trash" size="sm" label={d.mac ? t('dns.seen.forgetDevice') : t('dns.seen.forgetAddress', { ip: d.ip })} onclick={() => forget(d)} />
  </span>
{/snippet}

{#snippet clientCell(d: Row)}
  <span class="client-cell">
    {#if d.clientId}
      <a href={href('/dns/clients', { sel: d.clientId })}>{clientName(d) ?? `#${d.clientId}`}</a>
    {:else if canAdd}
      <Button size="sm" variant="ghost" icon="plus" onclick={() => addAsClient(d)}>
        {t('dns.seen.addAsClient')}
      </Button>
    {:else}
      <span class="subtle">{t('dns.seen.notConfigured')}</span>
    {/if}
    {#if d.dnsClientId}<span class="via small muted">{t('dns.seen.viaClientId', { id: d.dnsClientId })}</span>{/if}
  </span>
{/snippet}

{#snippet seenCell(d: Row)}
  <span class="nowrap" title={formatDateTime(d.lastSeen)}>{formatRelative(d.lastSeen)}</span>
{/snippet}

{#snippet reappear()}
  <p class="small muted">{t('dns.seen.reappear')}</p>
{/snippet}

<div class="stack">
  {#if missing}
    <Notice tone="info" title={t('dns.seen.unknownTitle', { ip: selIp })}>
      {t('dns.seen.unknownText')}
      {#snippet actions()}
        {#if canAdd}
          <Button size="sm" icon="plus" onclick={() => addAsClient({ ip: selIp, addresses: [selIp] })}>
            {t('dns.seen.addAsClient')}
          </Button>
        {/if}
      {/snippet}
    </Notice>
  {/if}

  <Panel flush title={t('dns.seen.title')} description={t(`dns.seen.description.${range}`)} footer={reappear}>
    {#snippet actions()}
      {@render rangePicker()}
      <!-- Labelled like the range picker next to it: a different time range (which devices are listed). -->
      <label class="within">
        <span class="small muted">{t('dns.seen.within')}</span>
        <Select size="sm" aria-label={t('dns.seen.within')} value={within} options={withinOptions} onchange={(e) => onwithin(e.currentTarget.value)} />
      </label>
      {#if session.canDestroy}
        <Button size="sm" variant="ghost" icon="trash" disabled={!known.data?.length} onclick={forgetAll}>{t('dns.seen.forgetAll')}</Button>
      {/if}
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

  <ClientIdsPanel seen={dnsClientIds} {clients} onadd={addClientId} />

  <NameSourcesPanel />
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
          { label: t('dns.vendor.label'), value: vendorText(selected) },
          { label: t('common.label.client'), value: clientName(selected) ?? t('dns.seen.notConfigured') },
          ...(selected.dnsClientId ? [{ label: t('dns.seen.clientIds.clientId'), value: selected.dnsClientId, mono: true }] : []),
          { label: t('dns.seen.firstSeen'), value: formatDateTime(selected.firstSeen) },
          { label: t('common.label.lastSeen'), value: formatDateTime(selected.lastSeen) },
          { label: t('dns.seen.queriesSeen'), value: formatNumber(selected.queries) },
          ...(selected.blockedBy.length > 0 ? [{ label: t('dns.seen.blockedByLabel'), value: selected.blockedBy.join(', '), mono: true }] : []),
        ]}
      />
      {#if selected.macRandomized && !selected.vendor}
        <p class="small muted">{t('dns.vendor.privateHelp')}</p>
      {/if}
      <section class="stack-sm" aria-labelledby="seen-addresses">
        <h3 id="seen-addresses">{t('dns.seen.addresses')}</h3>
        <ul class="addrs">
          {#each selected.entries as e (e.ip)}
            <li>
              <span class="addr-main">
                <span class="mono">{e.ip}</span>
                <span class="small muted nowrap" title={formatDateTime(e.lastSeen)}>{formatRelative(e.lastSeen)}</span>
                {#if session.isAdmin && selected.entries.length > 1}
                  <IconButton icon="trash" size="sm" label={t('dns.seen.forgetAddress', { ip: e.ip })} onclick={() => selected && forget(selected, e.ip)} />
                {/if}
              </span>
              {#if e.interface || e.whois}
                <span class="addr-meta small muted">
                  {#if e.interface}<span>{t('dns.seen.interface')}: <span class="mono">{e.interface}</span></span>{/if}
                  {#if e.whois}<span>{t('dns.seen.networkOwner')}: {whoisText(e.whois)}</span>{/if}
                </span>
              {/if}
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
      <ActivityChart
        key={seriesKey(selected)}
        {range}
        excluded={!!selected.clientId && !!clients?.find((c) => c.id === selected?.clientId)?.ignoreStats}
      />
      <div class="row">
        {#if selected.clientId}
          <Button icon="user" href={href('/dns/clients', { sel: selected.clientId })}>{t('dns.seen.openClient')}</Button>
        {:else if canAdd}
          <Button variant="primary" icon="plus" onclick={() => selected && addAsClient(selected)}>
            {t('dns.seen.addAsClient')}
          </Button>
        {/if}
        <Button variant="ghost" icon="list" href={href('/dns/queries', { client: clientValues(selected.addresses) })}>
          {t('dns.clients.showQueries')}
        </Button>
        <Button variant="ghost" icon="download" href={href('/cache/downloads', { client: downloadAddress(selected) })}>
          {t('dns.clients.showDownloads')}
        </Button>
        {#if canBlock}
          {#if selected.blockedBy.length > 0}
            <Button onclick={() => selected && unblock(selected)}>{t('dns.shared.unblock')}</Button>
          {:else}
            <Button icon="ban" onclick={() => selected && block(selected)}>
              {t('dns.shared.blockDevice')}
            </Button>
          {/if}
        {/if}
        {#if session.isAdmin}
          <Button variant="ghost" icon="trash" onclick={() => selected && forget(selected)}>
            {selected.mac ? t('dns.seen.forgetDevice') : t('dns.seen.forget')}
          </Button>
        {/if}
      </div>
      <p class="small muted">{t('dns.seen.reappear')}</p>
    </div>
  {/if}
</SidePanel>

<ClientPanel bind:open={addOpen} preset={addPreset} {groups} {range} onsaved={saved} />

<style>
  .client-cell {
    display: inline-flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
  }
  .via {
    white-space: nowrap;
  }
  .addr-cell {
    display: inline-flex;
    flex-direction: column;
    gap: 2px;
    padding: 2px 0;
  }
  .addr-line {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-1) var(--sp-2);
  }
  .where {
    display: inline-flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-2);
    color: var(--text-3);
    font-size: var(--fs-xs);
    line-height: 1.3;
  }
  .owner {
    max-width: 24ch;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .acts {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-1);
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
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-sm);
  }
  .addrs li {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
  .addr-main {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0 var(--sp-3);
    min-width: 0;
  }
  .addr-main .mono {
    overflow-wrap: anywhere;
  }
  .addr-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-3);
    overflow-wrap: anywhere;
  }
</style>
