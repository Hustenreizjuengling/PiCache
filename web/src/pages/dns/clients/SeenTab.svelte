<!--
  @component
  Recently seen addresses (/clients/known): IP, host name, MAC, the client
  they belong to and their traffic. Unconfigured addresses can be added as
  a client in one step.
  Query: ?tab=seen&within=24h|7d|30d&ip=<address>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { Client, ClientGroup, ClientInput, ClientStat, KnownClient, Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, EmptyState, KeyValue, Notice, Panel, Select, SidePanel, Table, type Column } from '$lib/ui'
  import ClientPanel from './ClientPanel.svelte'
  import { addressStat, clientFromKnown, type TrafficRange } from './clientStats'

  interface Props {
    known: Resource<KnownClient[]>
    clients: readonly Client[] | undefined
    groups: readonly ClientGroup[] | undefined
    stats: readonly ClientStat[] | undefined
    range: TrafficRange
    /** The range control of the traffic columns (shown in the panel header). */
    rangePicker: Snippet
    within: string
    onwithin: (within: string) => void
    onchanged: () => void
  }

  let { known, clients, groups, stats, range, rangePicker, within, onwithin, onchanged }: Props = $props()

  type Row = KnownClient & { stat?: ClientStat }

  let addOpen = $state(false)
  let addPreset = $state.raw<Partial<ClientInput>>({})

  const rows = $derived<Row[] | undefined>(known.data?.map((k) => ({ ...k, stat: addressStat(k.ip, stats) })))
  const selIp = $derived(router.param('ip').trim().toLowerCase())
  const selected = $derived(selIp ? rows?.find((k) => k.ip === selIp) : undefined)
  const missing = $derived(!!selIp && known.loaded && !selected)

  function clientName(k: KnownClient): string | undefined {
    if (!k.clientId) return undefined
    return k.name || clients?.find((c) => c.id === k.clientId)?.name
  }

  function addAsClient(k: Pick<KnownClient, 'ip' | 'mac' | 'hostname'>) {
    addPreset = clientFromKnown(k)
    addOpen = true
  }

  function saved() {
    router.setQuery({ ip: null })
    onchanged()
  }

  const withinOptions = $derived([
    { value: '24h', label: t('common.range.long.24h') },
    { value: '7d', label: t('common.range.long.7d') },
    { value: '30d', label: t('common.range.long.30d') },
  ])

  const columns: Column<Row>[] = $derived([
    { key: 'ip', label: t('dns.seen.address'), mono: true, sortable: true, value: (k) => k.ip },
    { key: 'host', label: t('dns.seen.hostname'), truncate: true, width: '25%', sortable: true, value: (k) => k.hostname ?? '' },
    { key: 'mac', label: t('dns.seen.mac'), mono: true, value: (k) => k.mac ?? '' },
    { key: 'client', label: t('common.label.client'), sortable: true, value: (k) => clientName(k) ?? '', cell: clientCell },
    {
      key: 'queries',
      label: t('dns.clients.queries'),
      align: 'right',
      sortable: true,
      value: (k) => k.stat?.queries ?? 0,
      format: (k) => formatNumber(k.stat?.queries ?? 0),
    },
    {
      key: 'blocked',
      label: t('dns.clients.blocked'),
      align: 'right',
      sortable: true,
      value: (k) => k.stat?.blocked ?? 0,
      format: (k) => formatNumber(k.stat?.blocked ?? 0),
    },
    { key: 'seen', label: t('common.label.lastSeen'), sortable: true, value: (k) => k.lastSeen, cell: seenCell },
  ])
</script>

{#snippet clientCell(k: Row)}
  {#if k.clientId}
    <a href={href('/dns/clients', { sel: k.clientId })}>{clientName(k) ?? `#${k.clientId}`}</a>
  {:else}
    <Button size="sm" variant="ghost" icon="plus" disabled={!session.isAdmin} onclick={() => addAsClient(k)}>
      {t('dns.seen.addAsClient')}
    </Button>
  {/if}
{/snippet}

{#snippet seenCell(k: Row)}
  <span class="nowrap" title={formatDateTime(k.lastSeen)}>{formatRelative(k.lastSeen)}</span>
{/snippet}

<div class="stack">
  {#if missing}
    <Notice tone="info" title={t('dns.seen.unknownTitle', { ip: selIp })}>
      {t('dns.seen.unknownText')}
      {#snippet actions()}
        <Button size="sm" icon="plus" disabled={!session.isAdmin} onclick={() => addAsClient({ ip: selIp })}>
          {t('dns.seen.addAsClient')}
        </Button>
      {/snippet}
    </Notice>
  {/if}

  <Panel flush title={t('dns.seen.title')} description={t(`dns.seen.description.${range}`)}>
    {#snippet actions()}
      {@render rangePicker()}
      <!-- Labelled like the range picker next to it: a different time range (which addresses are listed). -->
      <label class="within">
        <span class="small muted">{t('dns.seen.within')}</span>
        <Select size="sm" aria-label={t('dns.seen.within')} value={within} options={withinOptions} onchange={(e) => onwithin(e.currentTarget.value)} />
      </label>
    {/snippet}
    <Table
      {columns}
      {rows}
      key={(k) => k.ip}
      loading={known.loading && !known.loaded}
      error={known.error && !known.data ? errorText(known.error) : undefined}
      onretry={() => known.refresh()}
      onrowclick={(k) => router.setQuery({ ip: k.ip })}
      selected={selected?.ip}
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
          { label: t('dns.seen.address'), value: selected.ip, mono: true },
          { label: t('dns.seen.hostname'), value: selected.hostname },
          { label: t('dns.seen.mac'), value: selected.mac, mono: true },
          { label: t('common.label.client'), value: clientName(selected) ?? t('dns.seen.notConfigured') },
          { label: t('dns.seen.firstSeen'), value: formatDateTime(selected.firstSeen) },
          { label: t('common.label.lastSeen'), value: formatDateTime(selected.lastSeen) },
          { label: t('dns.seen.queriesSeen'), value: formatNumber(selected.queries) },
        ]}
      />
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
        <Button variant="ghost" icon="list" href={href('/dns/queries', { client: selected.ip })}>{t('dns.clients.showQueries')}</Button>
        <Button variant="ghost" icon="download" href={href('/cache/downloads', { client: selected.ip })}>
          {t('dns.clients.showDownloads')}
        </Button>
      </div>
    </div>
  {/if}
</SidePanel>

<ClientPanel bind:open={addOpen} preset={addPreset} {groups} {range} onsaved={saved} />

<style>
  h3 {
    font-size: var(--fs-md);
  }
  .within {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
</style>
