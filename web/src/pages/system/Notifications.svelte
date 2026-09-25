<!--
  @component
  System › Notifications: the channels PiCache sends messages to (webhook,
  ntfy, Gotify) with a test button, adding, editing and deleting them, the
  delivery log and the events PiCache notifies about. Channels and the log
  are admin-only; everyone can read the event list.
  Query: ?channel=<id> opens a channel.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, toApiError, type NotifyChannel } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, IconButton, Notice, Panel, Table, confirm, toast, type Column } from '$lib/ui'
  import ChannelDialog from './notifications/ChannelDialog.svelte'
  import ChannelPanel from './notifications/ChannelPanel.svelte'
  import { LOG_LIMIT, MAX_CHANNELS, displayUrl, eventText, kindLabel, type TestState } from './notifications/channels'
  import DeliveryLog from './notifications/DeliveryLog.svelte'
  import EventsPanel from './notifications/EventsPanel.svelte'
  import TestResult from './notifications/TestResult.svelte'

  const catalog = resource((signal) => api.notifications.events({ signal }))
  const channels = resource((signal) =>
    session.isAdmin ? api.notifications.channels.list({ signal }) : Promise.resolve(undefined),
  )
  const log = resource(
    (signal) => (session.isAdmin ? api.notifications.log(LOG_LIMIT, { signal }) : Promise.resolve(undefined)),
    { interval: 15_000 },
  )

  const selectedId = $derived(router.param('channel'))
  const selected = $derived(channels.data?.find((c) => c.id === selectedId))
  const full = $derived((channels.data?.length ?? 0) >= MAX_CHANNELS)

  function select(c: NotifyChannel | undefined) {
    router.setQuery({ channel: c?.id }, { push: !!c })
  }

  // ---- test messages (results stay until dismissed or the page is left)

  let tests = $state<Record<string, TestState>>({})

  async function sendTest(c: NotifyChannel) {
    if (tests[c.id]?.running) return
    const at = Date.now()
    tests[c.id] = { running: true, at }
    try {
      const result = await api.notifications.channels.test(c.id)
      tests[c.id] = { running: false, result, at }
    } catch (err) {
      tests[c.id] = { running: false, error: toApiError(err), at }
    }
    void log.refresh()
  }

  function dismissTest(c: NotifyChannel) {
    delete tests[c.id]
  }

  const shownTests = $derived(
    Object.entries(tests)
      .map(([id, test]) => ({ id, test, channel: channels.data?.find((c) => c.id === id) }))
      .filter((x): x is { id: string; test: TestState; channel: NotifyChannel } => !!x.channel)
      .sort((a, b) => b.test.at - a.test.at),
  )

  // ---- add / edit / delete

  let dialogOpen = $state(false)
  let editing = $state.raw<NotifyChannel | undefined>(undefined)

  function add() {
    editing = undefined
    dialogOpen = true
  }

  function edit(c: NotifyChannel) {
    editing = c
    dialogOpen = true
  }

  async function saved(c: NotifyChannel, created: boolean) {
    await channels.refresh()
    if (created) select(c) // the side panel offers "Send test message" right away
  }

  async function remove(c: NotifyChannel) {
    const ok = await confirm({
      title: t('system.notifications.deleteTitle', { name: c.name }),
      message: t('system.notifications.deleteText'),
      confirmLabel: t('system.notifications.delete'),
      action: () => api.notifications.channels.remove(c.id),
    })
    if (!ok) return
    if (selectedId === c.id) select(undefined)
    delete tests[c.id]
    toast.success(t('system.notifications.deleted'))
    void channels.refresh()
  }

  function eventsTitle(c: NotifyChannel): string | undefined {
    if (c.events.length === 0) return undefined
    return c.events.map((k) => eventText(k, catalog.data).title).join(', ')
  }

  const columns = $derived<Column<NotifyChannel>[]>([
    { key: 'name', label: t('common.label.name'), sortable: true, value: (c) => c.name, cell: nameCell },
    {
      key: 'url',
      label: t('system.notifications.col.address'),
      mono: true,
      truncate: true,
      width: '34%',
      value: (c) => displayUrl(c.url),
    },
    {
      key: 'status',
      label: t('common.label.status'),
      width: '1%',
      sortable: true,
      cell: statusCell,
      value: (c) => (c.enabled ? 0 : 1),
    },
    { key: 'sends', label: t('system.notifications.col.sends'), width: '1%', cell: sendsCell, value: (c) => c.minSeverity },
    { key: 'events', label: t('system.notifications.col.events'), width: '1%', cell: eventsCell, value: (c) => c.events.length },
    { key: 'actions', label: t('common.label.actions'), align: 'right', width: '1%', cell: actionsCell },
  ])
</script>

{#snippet limitNote()}
  <p class="small muted">{t('system.notifications.channels.limit', { max: MAX_CHANNELS })}</p>
{/snippet}

{#snippet nameCell(c: NotifyChannel)}
  <span class="name">
    <span class="strong">{c.name}</span>
    <span class="sub">{kindLabel(c.kind)}</span>
  </span>
{/snippet}

{#snippet statusCell(c: NotifyChannel)}
  <Chip
    size="sm"
    tone={c.enabled ? 'ok' : 'neutral'}
    label={c.enabled ? t('common.state.enabled') : t('common.state.disabled')}
  />
{/snippet}

{#snippet sendsCell(c: NotifyChannel)}
  <span class="nowrap" title={t(`system.notifications.min.${c.minSeverity}`)}>
    {t(`system.notifications.minShort.${c.minSeverity}`)}
  </span>
{/snippet}

{#snippet eventsCell(c: NotifyChannel)}
  <span class="nowrap" title={eventsTitle(c)}>
    {c.events.length === 0 ? t('system.notifications.allEvents') : tn('system.notifications.eventCount', c.events.length)}
  </span>
{/snippet}

{#snippet actionsCell(c: NotifyChannel)}
  <span class="actions">
    <IconButton
      icon="send"
      size="sm"
      label={t('system.notifications.testNamed', { name: c.name })}
      loading={tests[c.id]?.running}
      disabled={!session.isAdmin}
      onclick={() => sendTest(c)}
    />
    <IconButton
      icon="edit"
      size="sm"
      label={t('system.notifications.editNamed', { name: c.name })}
      disabled={!session.isAdmin}
      onclick={() => edit(c)}
    />
    <IconButton
      icon="trash"
      size="sm"
      variant="danger"
      label={t('system.notifications.deleteNamed', { name: c.name })}
      disabled={!session.isAdmin}
      onclick={() => remove(c)}
    />
  </span>
{/snippet}

<div class="page">
  {#if !session.isAdmin}
    <Notice tone="info">{t('system.notifications.adminOnly')}</Notice>
  {:else}
    <Panel
      title={t('system.notifications.channels.title')}
      description={t('system.notifications.channels.description')}
      flush
      footer={full ? limitNote : undefined}
    >
      {#snippet actions()}
        <Button variant="primary" icon="plus" disabled={full || !channels.data} onclick={add}>
          {t('system.notifications.channels.add')}
        </Button>
      {/snippet}
      {#if shownTests.length > 0}
        <div class="tests">
          {#each shownTests as x (x.id)}
            <TestResult
              name={x.channel.name}
              test={x.test}
              ondismiss={x.test.running ? undefined : () => dismissTest(x.channel)}
            />
          {/each}
        </div>
      {/if}
      <Table
        {columns}
        rows={channels.data}
        key={(c) => c.id}
        loading={channels.loading}
        error={channels.error && !channels.data ? errorText(channels.error) : undefined}
        onretry={() => channels.refresh()}
        onrowclick={(c) => select(c)}
        selected={selected?.id}
        caption={t('system.notifications.channels.title')}
        skeletonRows={3}
      >
        {#snippet empty()}
          <EmptyState icon="bell" title={t('system.notifications.channels.emptyTitle')} text={t('system.notifications.channels.emptyText')} compact>
            <Button icon="plus" onclick={add}>{t('system.notifications.channels.add')}</Button>
          </EmptyState>
        {/snippet}
      </Table>
    </Panel>

    <DeliveryLog
      entries={log.data}
      loading={log.loading}
      error={log.error}
      catalog={catalog.data}
      onrefresh={() => log.refresh()}
    />
  {/if}

  <EventsPanel events={catalog.data} loading={catalog.loading} error={catalog.error} onretry={() => catalog.refresh()} />
</div>

{#if session.isAdmin}
  <ChannelDialog
    bind:open={dialogOpen}
    channel={editing}
    catalog={catalog.data}
    catalogError={catalog.error}
    onsaved={saved}
  />

  {#if selectedId}
    <ChannelPanel
      channel={selected}
      loaded={channels.loaded}
      catalog={catalog.data}
      test={selected ? tests[selected.id] : undefined}
      ontest={sendTest}
      ondismisstest={dismissTest}
      onedit={edit}
      ondelete={remove}
      onclose={() => select(undefined)}
    />
  {/if}
{/if}

<style>
  .tests {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .name {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
    min-width: 12ch;
    padding: var(--sp-1) 0;
  }
  .strong {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .sub {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
  .actions {
    display: inline-flex;
    gap: var(--sp-1);
  }
</style>
