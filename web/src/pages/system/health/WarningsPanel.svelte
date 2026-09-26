<!--
  @component
  Warning history (GET /system/events): the warnings and errors PiCache
  recorded (health, storage, updates, backups, security), also without
  notification channels. Repeats of an unacknowledged entry are merged
  (×N). Admins acknowledge single entries or all; the header badge counts
  the unacknowledged warnings and errors. Cursor pages load with "Show more".
  Query: ?warnings=open (only unacknowledged entries)
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type HistoryEvent, type Page } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Badge, Button, Chip, EmptyState, IconButton, Panel, Select, Skeleton, toast } from '$lib/ui'
  import { eventText, severityLabel, severityTone } from '../notifications/channels'

  const LIMIT = 50

  const onlyOpen = $derived(router.param('warnings') === 'open')
  const first = resource((signal) => api.system.events({ unacknowledged: onlyOpen || undefined, limit: LIMIT }, { signal }))

  /** Pages after the first ("Show more"); dropped when the filter changes or the list reloads. */
  let pages = $state.raw<Page<HistoryEvent>[]>([])
  let moreLoading = $state(false)
  let acking = $state<number | 'all' | null>(null)

  $effect(() => {
    void onlyOpen
    untrack(() => (pages = []))
  })

  function reload() {
    pages = []
    void first.refresh()
  }

  const items = $derived([...(first.data?.items ?? []), ...pages.flatMap((p) => p.items)])
  const next = $derived(pages.length ? pages[pages.length - 1].next : first.data?.next)
  const open = $derived(items.filter((e) => !e.acknowledgedAt).length)

  async function more() {
    const cursor = next
    if (!cursor) return
    moreLoading = true
    try {
      const p = await api.system.events({ unacknowledged: onlyOpen || undefined, limit: LIMIT, cursor })
      pages = [...pages, p]
    } catch (err) {
      toast.error(err)
    } finally {
      moreLoading = false
    }
  }

  function replace(e: HistoryEvent) {
    const d = first.data
    if (d?.items.some((x) => x.id === e.id)) first.set({ ...d, items: d.items.map((x) => (x.id === e.id ? e : x)) })
    else pages = pages.map((p) => ({ ...p, items: p.items.map((x) => (x.id === e.id ? e : x)) }))
  }

  async function ack(e: HistoryEvent) {
    acking = e.id
    try {
      replace(await api.system.ackEvent(e.id))
      void appStatus.overview.refresh()
    } catch (err) {
      toast.error(err)
    } finally {
      acking = null
    }
  }

  async function ackAll() {
    acking = 'all'
    try {
      const r = await api.system.ackAllEvents()
      toast.success(tn('system.health.warnings.ackAllDone', r.acknowledged))
      void appStatus.overview.refresh()
      pages = []
      await first.refresh()
    } catch (err) {
      toast.error(err)
    } finally {
      acking = null
    }
  }

  const filterOptions = $derived([
    { value: '', label: t('system.health.warnings.filterAll') },
    { value: 'open', label: t('system.health.warnings.filterOpen') },
  ])
</script>

<Panel id="warnings" title={t('system.health.warnings.title')} description={t('system.health.warnings.description')} flush>
  {#snippet actions()}
    <Select
      size="sm"
      aria-label={t('system.health.warnings.filter')}
      value={onlyOpen ? 'open' : ''}
      options={filterOptions}
      onchange={(e) => router.setQuery({ warnings: e.currentTarget.value || null })}
    />
    {#if session.canOperate && open > 0}
      <Button size="sm" icon="check" loading={acking === 'all'} onclick={ackAll}>{t('system.health.warnings.ackAll')}</Button>
    {/if}
    <IconButton icon="refresh" label={t('common.action.refresh')} loading={first.loading} onclick={reload} />
  {/snippet}

  {#if first.error && !first.data}
    <div class="pad">
      <p class="err">
        {errorText(first.error)}
        <Button size="sm" icon="refresh" onclick={reload}>{t('common.action.retry')}</Button>
      </p>
    </div>
  {:else if !first.data}
    <div class="pad"><Skeleton height="120px" /></div>
  {:else if items.length === 0}
    {#if onlyOpen}
      <EmptyState compact icon="success" title={t('system.health.warnings.emptyOpen')} text={t('system.health.warnings.emptyOpenText')} />
    {:else}
      <EmptyState compact icon="bell" title={t('system.health.warnings.empty')} text={t('system.health.warnings.emptyText')} />
    {/if}
  {:else}
    <ul class="events">
      {#each items as e (e.id)}
        {@const text = eventText(e.event)}
        <li class={['event', !e.acknowledgedAt && `open ${e.severity}`]}>
          <div class="head">
            <Chip size="sm" tone={severityTone(e.severity)} label={severityLabel(e.severity)} />
            <span class="title">{e.title || text.title}</span>
            {#if e.count > 1}<Badge title={t('system.health.warnings.countHelp')}>{t('system.health.warnings.count', { count: formatNumber(e.count) })}</Badge>{/if}
          </div>
          {#if e.message}<p class="msg small">{e.message}</p>{/if}
          <p class="meta small">
            <span class="mono">{e.event}</span>
            {#if e.count > 1}
              <span title={formatDateTime(e.time)}>{t('system.health.warnings.first', { time: formatDateTime(e.time) })}</span>
              <span title={formatDateTime(e.lastTime)}>{t('system.health.warnings.last', { time: formatRelative(e.lastTime) })}</span>
            {:else}
              <span title={formatDateTime(e.lastTime)}>{formatDateTime(e.lastTime)}</span>
            {/if}
          </p>
          <div class="ack small">
            {#if e.acknowledgedAt}
              <span class="muted" title={formatDateTime(e.acknowledgedAt)}>
                {e.acknowledgedBy
                  ? t('system.health.warnings.ackedBy', { time: formatRelative(e.acknowledgedAt), user: e.acknowledgedBy })
                  : t('system.health.warnings.acked', { time: formatRelative(e.acknowledgedAt) })}
              </span>
            {:else if session.canOperate}
              <Button size="sm" variant="ghost" icon="check" loading={acking === e.id} disabled={acking === 'all'} onclick={() => ack(e)}>
                {t('system.health.warnings.ack')}<span class="visually-hidden">: {e.title}</span>
              </Button>
            {/if}
          </div>
        </li>
      {/each}
    </ul>
    {#if next}
      <div class="more">
        <Button size="sm" loading={moreLoading} onclick={more}>{t('system.health.warnings.more')}</Button>
      </div>
    {/if}
  {/if}
</Panel>

<style>
  .pad {
    padding: 0 var(--sp-4) var(--sp-4);
  }
  .err {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .events {
    margin: 0;
    padding: 0;
    list-style: none;
    border-top: 1px solid var(--line);
  }
  .event {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    grid-template-areas:
      'head ack'
      'msg ack'
      'meta ack';
    gap: 2px var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border-bottom: 1px solid var(--line);
  }
  .event:last-child {
    border-bottom: 0;
  }
  /* Unacknowledged: a stripe in the severity's tone. */
  .event.open {
    box-shadow: inset 3px 0 0 var(--focus);
  }
  .event.open.warning {
    box-shadow: inset 3px 0 0 var(--warn);
  }
  .event.open.error {
    box-shadow: inset 3px 0 0 var(--fail);
  }
  .head {
    grid-area: head;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
    min-width: 0;
  }
  .title {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .msg {
    grid-area: msg;
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
  .meta {
    grid-area: meta;
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-3);
    color: var(--text-3);
  }
  .ack {
    grid-area: ack;
    align-self: center;
    text-align: right;
  }
  .more {
    display: flex;
    justify-content: center;
    padding: var(--sp-3) var(--sp-4);
    border-top: 1px solid var(--line);
  }
  @media (max-width: 600px) {
    .event {
      grid-template-columns: minmax(0, 1fr);
      grid-template-areas:
        'head'
        'msg'
        'meta'
        'ack';
    }
    .ack {
      text-align: left;
    }
  }
</style>
