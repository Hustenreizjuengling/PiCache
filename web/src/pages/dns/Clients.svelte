<!--
  @component
  Clients & groups: configured clients, recently seen devices (add them as
  clients) and groups, with per-client and per-device traffic over a time
  range.
  Query: ?tab=clients|seen|groups&range=24h|7d|30d&within=…&sel=<id>&ip=<address>&group=<id>
  (without ?range= the range shown last in this browser). Incoming
  ?ip=<address> (global search, query log) opens the client that address
  belongs to, or the address on the "Seen recently" tab.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { router } from '$lib/router.svelte'
  import { loadPref, savePref } from '$lib/storage'
  import { Tabs, TimeRangePicker } from '$lib/ui'
  import ClientsTab from './clients/ClientsTab.svelte'
  import { asTrafficRange, seenDevices, TRAFFIC_RANGES, type TrafficRange } from './clients/clientStats'
  import GroupsTab from './clients/GroupsTab.svelte'
  import SeenTab from './clients/SeenTab.svelte'

  const TABS = ['clients', 'seen', 'groups'] as const
  type Tab = (typeof TABS)[number]
  const DEFAULT_RANGE: TrafficRange = '24h'
  const RANGE_PREF = 'dns.clients.range'
  const WITHIN = ['24h', '7d', '30d']

  const tab = $derived.by((): Tab => {
    const v = router.param('tab') as Tab
    return TABS.includes(v) ? v : 'clients'
  })
  // The range shown last is remembered in this browser: links and tabs
  // without ?range= (e.g. ?tab=seen from the search) keep it.
  let preferred = $state<TrafficRange>(asTrafficRange(loadPref(RANGE_PREF)) ?? DEFAULT_RANGE)
  const range = $derived(asTrafficRange(router.param('range')) ?? preferred)

  function remember(r: TrafficRange) {
    if (r === preferred) return
    preferred = r
    savePref(RANGE_PREF, r === DEFAULT_RANGE ? null : r)
  }

  $effect(() => {
    const r = range
    untrack(() => remember(r))
  })

  function setRange(r: TrafficRange) {
    remember(r)
    router.setQuery({ range: r === DEFAULT_RANGE ? null : r })
  }

  const within = $derived(WITHIN.includes(router.param('within')) ? router.param('within') : '30d')

  const clients = resource((signal) => api.clients.list({ signal }))
  const groups = resource((signal) => api.groups.list({ signal }))
  // Names of the family resolvers groups may use.
  const presets = resource((signal) => api.groups.upstreamPresets({ signal }))
  const known = resource((signal) => api.clients.known(within, { signal }), { interval: 60_000 })
  // Traffic of configured clients, grouped by device on the server: every
  // address a client was recognised by counts (also the IPv6 addresses of a
  // client configured by its IPv4 address).
  const clientStats = resource((signal) => api.stats.clients(range, { signal, group: 'device' }), { interval: 60_000 })
  // Traffic per address, summed per device on "Seen recently" (a client can cover several devices).
  const addressStats = resource((signal) => api.stats.clients(range, { signal }), { interval: 60_000 })

  // ?ip=… on the clients tab: open the client this address belongs to (the
  // server's match from "seen recently", else an exact identifier), or show
  // the address on the "Seen recently" tab.
  $effect(() => {
    const ip = router.param('ip').trim().toLowerCase()
    if (!ip || tab !== 'clients') return
    const ready = clients.loaded && (known.loaded || !!known.error)
    if (!ready) return
    untrack(() => {
      const id = known.data?.find((k) => k.ip === ip)?.clientId || clients.data?.find((c) => c.identifiers.includes(ip))?.id
      if (id) router.setQuery({ ip: null, sel: id })
      else router.setQuery({ tab: 'seen' })
    })
  })

  const tabs = $derived([
    { id: 'clients', label: t('dns.clients.tab.clients'), count: clients.data?.length },
    { id: 'seen', label: t('dns.clients.tab.seen'), count: known.data ? seenDevices(known.data).length : undefined },
    { id: 'groups', label: t('dns.clients.tab.groups'), count: groups.data?.length },
  ])

  function selectTab(id: string) {
    router.setQuery({ tab: id === 'clients' ? null : id, sel: null, ip: null, group: null })
  }

  function changed() {
    void clients.refresh()
    void groups.refresh()
    void known.refresh()
  }
</script>

<!-- In the panels whose traffic columns it sets (not above the tabs: the Groups tab has none). -->
{#snippet rangePicker()}
  <div class="range">
    <span class="small muted">{t('dns.clients.rangeLabel')}</span>
    <TimeRangePicker
      value={range}
      options={[...TRAFFIC_RANGES]}
      label={t('dns.clients.rangeLabel')}
      onchange={(r) => setRange(asTrafficRange(r) ?? DEFAULT_RANGE)}
    />
  </div>
{/snippet}

<div class="page">
  <Tabs {tabs} active={tab} label={t('common.nav.clients')} onchange={selectTab}>
    {#snippet children(active)}
      <div class="tab">
        {#if active === 'seen'}
          <SeenTab
            {known}
            clients={clients.data}
            groups={groups.data}
            stats={addressStats.data}
            {range}
            {rangePicker}
            {within}
            onwithin={(w) => router.setQuery({ within: w === '30d' ? null : w })}
            onchanged={changed}
          />
        {:else if active === 'groups'}
          <GroupsTab {groups} clients={clients.data} presets={presets.data} onchanged={changed} />
        {:else}
          <ClientsTab
            {clients}
            groups={groups.data}
            presets={presets.data}
            stats={clientStats.data}
            known={known.data}
            {range}
            {rangePicker}
            onchanged={changed}
          />
        {/if}
      </div>
    {/snippet}
  </Tabs>
</div>

<style>
  .range {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
  .tab {
    padding-top: var(--sp-4);
  }
</style>
