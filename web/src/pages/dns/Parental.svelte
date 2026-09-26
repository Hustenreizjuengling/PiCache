<!--
  @component
  DNS › Parental controls: one card per client group (Default last) with its
  current state in plain words, the weekly plan, schedules, always-blocked
  services, safe search and category switches; quick actions to block the
  internet now, lift the restrictions or pause the group's filtering for a
  while; an edit panel; a test for one device; short hints. Parental
  controls, safe search and protection lists apply even while blocking is
  paused.
  Query: ?edit=<group id> opens the edit panel of a group.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, resource, type GroupControls, type OverrideMode, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Notice, Skeleton, toast } from '$lib/ui'
  import ControlsPanel from './parental/ControlsPanel.svelte'
  import GroupCard from './parental/GroupCard.svelte'
  import Hints from './parental/Hints.svelte'
  import { hostDiffers, hostZone, setHostClock } from '$lib/hostclock.svelte'
  import { ordered, whenText } from './parental/plan'
  import TestBox from './parental/TestBox.svelte'
  import UntilDialog from './parental/UntilDialog.svelte'

  // Faster while a category switch waits for its list's first download.
  const pending = (): boolean =>
    (groups.data ?? []).some((g) => Object.values(g.categories ?? {}).some((c) => c?.state === 'pending'))
  const pace = (): number => (pending() ? 10_000 : 60_000)
  const groups: Resource<GroupControls[]> = resource((signal) => api.parental.groups({ signal }), { interval: pace })
  const catalog = resource((signal) => api.parental.services({ signal }))
  const clients = resource((signal) => api.clients.list({ signal }))
  const known = resource((signal) => api.clients.known('30d', { signal }))
  // Names and sizes of the lists behind the category switches.
  const filterCatalog = resource((signal) => api.filter.catalog({ signal }))
  const lists = resource((signal) => api.filter.lists.list({ signal }), { interval: pace })

  // Schedules use the host's clock; the page shows times on it.
  $effect(() => setHostClock(groups.data?.[0]?.state))

  // The clock of the plan's "now" marker and the "until …" texts.
  let now = $state(new Date())
  $effect(() => {
    const id = setInterval(() => (now = new Date()), 30_000)
    return () => clearInterval(id)
  })

  // Reload right after the next change of any group (a window or an override ends), not only every minute.
  $effect(() => {
    const times = (groups.data ?? []).flatMap((g) => [g.state.until, g.state.liftedUntil, g.state.pausedUntil, g.state.next?.time])
    const next = Math.min(...times.map((x) => (x ? new Date(x).getTime() : Infinity)).filter((x) => x > Date.now()))
    if (!Number.isFinite(next)) return
    const id = setTimeout(() => void groups.refresh(), Math.min(next - Date.now() + 1500, 2 ** 31 - 1))
    return () => clearTimeout(id)
  })

  const list = $derived(groups.data ? ordered(groups.data) : undefined)
  const onlyDefault = $derived(!!list && list.every((g) => g.groupId === DEFAULT_GROUP_ID))

  const editId = $derived(Number(router.param('edit')) || 0)
  const editing = $derived(session.isAdmin ? list?.find((g) => g.groupId === editId) : undefined)

  function replace(g: GroupControls) {
    groups.set((groups.data ?? []).map((x) => (x.groupId === g.groupId ? g : x)))
  }

  // Saving can create or change the lists of category switches.
  function saved(g: GroupControls) {
    replace(g)
    void lists.refresh()
  }

  // ---- quick actions

  let busy = $state<number[]>([])
  let untilOpen = $state(false)
  let untilGroup = $state.raw<GroupControls | undefined>(undefined)
  let untilMode = $state<OverrideMode>('block')

  function announce(g: GroupControls, mode: OverrideMode) {
    const when = g.override?.until ? whenText(g.override.until, 'until') : ''
    toast.success(t(mode === 'block' ? 'dns.parental.blockedToast' : 'dns.parental.liftedToast', { group: g.groupName, when }))
  }

  async function override(g: GroupControls, mode: OverrideMode, minutes: number | 'until') {
    if (minutes === 'until') {
      untilGroup = g
      untilMode = mode
      untilOpen = true
      return
    }
    busy = [...busy, g.groupId]
    try {
      const saved = await api.parental.setOverride(g.groupId, { mode, minutes })
      replace(saved)
      announce(saved, mode)
    } catch (e) {
      toast.error(e)
    } finally {
      busy = busy.filter((x) => x !== g.groupId)
    }
  }

  async function end(g: GroupControls) {
    busy = [...busy, g.groupId]
    try {
      const saved = await api.parental.clearOverride(g.groupId)
      replace(saved)
      toast.success(t('dns.parental.endedToast', { group: g.groupName }))
    } catch (e) {
      toast.error(e)
      void groups.refresh()
    } finally {
      busy = busy.filter((x) => x !== g.groupId)
    }
  }

  async function resume(g: GroupControls) {
    busy = [...busy, g.groupId]
    try {
      replace(await api.parental.resume(g.groupId))
      toast.success(t('dns.pause.resumedToast', { group: g.groupName }))
    } catch (e) {
      toast.error(e)
      void groups.refresh()
    } finally {
      busy = busy.filter((x) => x !== g.groupId)
    }
  }
</script>

<div class="page">
  <p class="intro muted">{t('dns.parental.intro')}</p>
  {#if hostDiffers()}
    <Notice tone="info" icon="clock">
      {hostZone() === 'UTC' ? t('dns.parental.hostZoneUtc') : t('dns.parental.hostZone', { zone: hostZone() ?? '' })}
    </Notice>
  {/if}

  {#if groups.error && !groups.data}
    <Notice tone="fail" title={t('dns.parental.loadError')}>
      {errorText(groups.error)}
      {#snippet actions()}
        <Button size="sm" icon="refresh" onclick={() => groups.refresh()}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {:else if !list}
    {#each [0, 1] as i (i)}<Skeleton height="260px" />{/each}
  {:else}
    {#if onlyDefault}
      <Notice title={t('dns.parental.onlyDefaultTitle')}>
        {t('dns.parental.onlyDefaultText')}
        {#snippet actions()}
          <Button size="sm" icon="users" href={href('/dns/clients', { tab: 'groups' })}>{t('dns.parental.openClients')}</Button>
        {/snippet}
      </Notice>
    {/if}
    {#each list as g (g.groupId)}
      <GroupCard
        group={g}
        catalog={catalog.data}
        {now}
        busy={busy.includes(g.groupId)}
        onedit={(x) => router.setQuery({ edit: x.groupId }, { push: true })}
        onoverride={override}
        onend={end}
        onpaused={replace}
        onresume={resume}
      />
    {/each}
  {/if}

  <TestBox clients={clients.data} known={known.data} groups={groups.data} />
  <Hints />
</div>

{#if session.isAdmin}
  <ControlsPanel
    bind:open={() => !!editing, (v) => !v && router.setQuery({ edit: null })}
    group={editing}
    catalog={catalog.data}
    catalogError={catalog.error}
    onretrycatalog={() => catalog.refresh()}
    filterCatalog={filterCatalog.data}
    lists={lists.data}
    onsaved={saved}
    onpaused={replace}
    onresume={resume}
    resuming={!!editing && busy.includes(editing.groupId)}
  />
  <UntilDialog bind:open={untilOpen} group={untilGroup} mode={untilMode} onsaved={(g) => (replace(g), announce(g, untilMode))} />
{/if}

<style>
  .intro {
    max-width: 90ch;
    font-size: var(--fs-sm);
  }
</style>
