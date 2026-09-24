<!--
  @component
  Clients & groups: configured clients, recently seen addresses (add them as
  clients) and groups, with per-client traffic over a time range.
  Query: ?tab=clients|seen|groups&range=24h|7d|30d&within=…&sel=<id>&ip=<address>
  Incoming ?ip=<address> (global search, query log) opens the client that
  address belongs to, or the address on the "Seen recently" tab.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, type RangePreset } from '$lib/api'
  import { router } from '$lib/router.svelte'
  import { Tabs, TimeRangePicker } from '$lib/ui'
  import ClientsTab from './clients/ClientsTab.svelte'
  import GroupsTab from './clients/GroupsTab.svelte'
  import SeenTab from './clients/SeenTab.svelte'

  const TABS = ['clients', 'seen', 'groups'] as const
  type Tab = (typeof TABS)[number]
  const RANGES: RangePreset[] = ['24h', '7d', '30d']
  const DEFAULT_RANGE: RangePreset = '24h'
  const WITHIN = ['24h', '7d', '30d']

  const tab = $derived.by((): Tab => {
    const v = router.param('tab') as Tab
    return TABS.includes(v) ? v : 'clients'
  })
  const range = $derived.by((): RangePreset => {
    const r = router.param('range') as RangePreset
    return RANGES.includes(r) ? r : DEFAULT_RANGE
  })
  const within = $derived(WITHIN.includes(router.param('within')) ? router.param('within') : '30d')

  const clients = resource((signal) => api.clients.list({ signal }))
  const groups = resource((signal) => api.groups.list({ signal }))
  const known = resource((signal) => api.clients.known(within, { signal }), { interval: 60_000 })
  const stats = resource((signal) => api.stats.clients(range, { signal }), { interval: 60_000 })

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
    { id: 'seen', label: t('dns.clients.tab.seen'), count: known.data?.length },
    { id: 'groups', label: t('dns.clients.tab.groups'), count: groups.data?.length },
  ])

  function selectTab(id: string) {
    router.setQuery({ tab: id === 'clients' ? null : id, sel: null, ip: null })
  }

  function changed() {
    void clients.refresh()
    void groups.refresh()
    void known.refresh()
  }
</script>

<div class="page">
  {#if tab !== 'groups'}
    <div class="range">
      <span class="small muted">{t('dns.clients.rangeLabel')}</span>
      <TimeRangePicker
        value={range}
        options={RANGES}
        label={t('dns.clients.rangeLabel')}
        onchange={(r) => router.setQuery({ range: r === DEFAULT_RANGE ? null : r })}
      />
    </div>
  {/if}

  <Tabs {tabs} active={tab} label={t('common.nav.clients')} onchange={selectTab}>
    {#snippet children(active)}
      <div class="tab">
        {#if active === 'seen'}
          <SeenTab
            {known}
            clients={clients.data}
            groups={groups.data}
            stats={stats.data}
            {range}
            {within}
            onwithin={(w) => router.setQuery({ within: w === '30d' ? null : w })}
            onchanged={changed}
          />
        {:else if active === 'groups'}
          <GroupsTab {groups} onchanged={changed} />
        {:else}
          <ClientsTab {clients} groups={groups.data} stats={stats.data} known={known.data} {range} onchanged={changed} />
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
