<!--
  @component
  Cache › Prefill: the prefill service (picache prefill, a process of its
  own under its own user): whether it runs, the Steam sign-in by QR code,
  the games to prefill, the schedule and limits, the last jobs and every
  host the service talks to. Polled every 1.5 s while a sign-in waits or a
  job runs, else every 10 s.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type PrefillState, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { router } from '$lib/router.svelte'
  import { Notice, Skeleton, Tabs } from '$lib/ui'
  import BattleNet from './prefill/BattleNet.svelte'
  import EpicAccount from './prefill/EpicAccount.svelte'
  import EpicGames from './prefill/EpicGames.svelte'
  import Jobs from './prefill/Jobs.svelte'
  import Schedule from './prefill/Schedule.svelte'
  import Setup from './prefill/Setup.svelte'
  import Status from './prefill/Status.svelte'
  import SteamAccount from './prefill/SteamAccount.svelte'
  import SteamGames from './prefill/SteamGames.svelte'
  import SteamOptions from './prefill/SteamOptions.svelte'
  import Transparency from './prefill/Transparency.svelte'

  // Admins only (the endpoints refuse others). The first load checks the
  // cache's heartbeat, the polls do not.
  let checked = false
  const pf: Resource<PrefillState | undefined> = resource(
    (signal) => {
      if (!session.isAdmin) return Promise.resolve(undefined)
      const check = !checked
      checked = true
      return api.prefill.state(check, { signal })
    },
    {
      interval: () => {
        const st = pf.data?.status
        const fast =
          st?.job || st?.steam.login?.state === 'waiting' || st?.epic.login?.state === 'waiting' || pf.data?.setup?.state === 'running'
        return fast ? 1_500 : 10_000
      },
    },
  )
  const status = $derived(pf.data?.status ?? undefined)
  const jobId = $derived(status?.job?.id ?? 0)

  async function refresh() {
    await pf.refresh()
  }

  // ?store=steam|battlenet|epic
  const STORES = ['steam', 'battlenet', 'epic']
  const store = $derived(STORES.includes(router.param('store') ?? '') ? (router.param('store') as string) : 'steam')
  const tabs = $derived([
    { id: 'steam', label: 'Steam' },
    { id: 'battlenet', label: 'Battle.net' },
    { id: 'epic', label: 'Epic Games' },
  ])
  function selectStore(id: string) {
    router.setQuery({ store: id === 'steam' ? null : id })
  }
</script>

<div class="page">
  {#if !session.isAdmin}
    <Notice tone="info">{t('cache.prefill.adminOnly')}</Notice>
  {:else if pf.error && !pf.data}
    <Notice tone="fail">{errorText(pf.error)}</Notice>
  {:else if !pf.data}
    <Skeleton height="160px" />
  {:else if !pf.data.running || !status}
    <Setup info={pf.data} onchanged={refresh} />
    <Transparency status={undefined} />
  {:else}
    <Status {status} mode={pf.data.mode} onchanged={refresh} />
    <Tabs {tabs} active={store} label={t('cache.prefill.storesLabel')} onchange={selectStore}>
      {#snippet children(active)}
        <div class="tab stack">
          {#if active === 'battlenet'}
            <BattleNet {status} onchanged={refresh} />
          {:else if active === 'epic'}
            <EpicAccount {status} onchanged={refresh} />
            {#if status.epic.account}
              <EpicGames {status} onchanged={refresh} />
            {/if}
          {:else}
            <SteamAccount {status} onchanged={refresh} />
            {#if status.steam.account}
              <SteamGames {status} onchanged={refresh} />
              <SteamOptions {status} onchanged={refresh} />
            {/if}
          {/if}
        </div>
      {/snippet}
    </Tabs>
    <Schedule {status} onchanged={refresh} />
    <Jobs running={jobId} />
    <Transparency {status} />
  {/if}
</div>

<style>
  .tab {
    padding-top: var(--sp-4);
  }
</style>
