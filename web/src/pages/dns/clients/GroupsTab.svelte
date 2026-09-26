<!--
  @component
  Groups: lists and rules apply to a client when they share an enabled
  group. Enable or disable inline; rows open the group panel; the DNS
  resolver of each group and the groups made by "Only for this device" are
  marked. Admins select groups to enable, disable or delete them together
  (never the Default group).
  Query: ?tab=groups&sel=<group id>
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, type BatchAction, type Client, type ClientGroup, type Resource, type UpstreamPreset } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDate, formatNumber } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, BulkBar, Button, Panel, Table, toast, Toggle, type Column } from '$lib/ui'
  import { runBatch } from '../shared/batch'
  import { hasResolver, resolverText } from '../shared/resolver'
  import GroupPanel from './GroupPanel.svelte'

  interface Props {
    groups: Resource<ClientGroup[]>
    clients: readonly Client[] | undefined
    presets: readonly UpstreamPreset[] | undefined
    onchanged: () => void
  }

  let { groups, clients, presets, onchanged }: Props = $props()

  let addOpen = $state(false)
  let toggling = $state<number[]>([])
  let checked = $state<number[]>([])
  let busy = $state(false)

  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(groups.data?.find((g) => g.id === selId))
  const defaultChecked = $derived(checked.includes(DEFAULT_GROUP_ID))

  async function setEnabled(g: ClientGroup, enabled: boolean) {
    toggling = [...toggling, g.id]
    try {
      // The resolver is kept when left out.
      const saved = await api.groups.update(g.id, { name: g.name, comment: g.comment, enabled })
      groups.set((groups.data ?? []).map((x) => (x.id === saved.id ? saved : x)))
      toast.success(enabled ? t('dns.groups.enabledToast', { name: g.name }) : t('dns.groups.disabledToast', { name: g.name }))
    } catch (e) {
      toast.error(e)
      void groups.refresh()
    } finally {
      toggling = toggling.filter((x) => x !== g.id)
    }
  }

  async function batch(action: BatchAction) {
    busy = true
    try {
      const ok = await runBatch({
        action,
        ids: checked,
        what: (n) => tn('dns.groups.count', n),
        run: (req) => api.groups.batch(req),
        deleteText: t('dns.groups.batchDeleteText'),
      })
      if (!ok) return
      if (action === 'delete') {
        if (selected && checked.includes(selected.id)) router.setQuery({ sel: null })
        checked = []
      }
      onchanged()
    } finally {
      busy = false
    }
  }

  function deviceName(g: ClientGroup): string | undefined {
    return clients?.find((c) => c.id === g.deviceClientId)?.name
  }

  const columns: Column<ClientGroup>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'name', label: t('common.label.name'), sortable: true, value: (g) => g.name, cell: nameCell },
    { key: 'resolver', label: t('dns.resolver.column'), value: (g) => resolverText(g, presets), cell: resolverCell },
    { key: 'comment', label: t('common.label.comment'), truncate: true, width: '40%', value: (g) => g.comment },
    {
      key: 'clients',
      label: t('dns.groups.clients'),
      align: 'right',
      sortable: true,
      value: (g) => g.clientCount,
      format: (g) => formatNumber(g.clientCount),
    },
    { key: 'created', label: t('common.label.created'), sortable: true, value: (g) => g.createdAt, format: (g) => formatDate(g.createdAt) },
  ])
</script>

{#snippet enabledCell(g: ClientGroup)}
  <Toggle
    bind:checked={() => g.enabled, (on) => setEnabled(g, on)}
    ariaLabel={t('dns.groups.enableNamed', { name: g.name })}
    disabled={!session.isAdmin || toggling.includes(g.id)}
  />
{/snippet}

{#snippet nameCell(g: ClientGroup)}
  <span class="name">
    <span>{g.name}</span>
    {#if g.id === DEFAULT_GROUP_ID}<Badge tone="info" title={t('dns.groups.defaultNote')}>{t('dns.groups.default')}</Badge>{/if}
    {#if g.deviceClientId}
      {@const name = deviceName(g)}
      <Badge title={name ? t('dns.groups.deviceText', { client: name }) : t('dns.groups.deviceTextNoName')}>{t('dns.groups.deviceBadge')}</Badge>
    {/if}
  </span>
{/snippet}

{#snippet resolverCell(g: ClientGroup)}
  {#if hasResolver(g)}
    <span class="nowrap">{resolverText(g, presets)}</span>
  {:else}
    <span class="subtle nowrap">{t('dns.resolver.default')}</span>
  {/if}
{/snippet}

<Panel flush title={t('dns.groups.title')} description={t('dns.groups.description')}>
  {#snippet actions()}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>{t('dns.groups.add')}</Button>
  {/snippet}
  <Table
    {columns}
    rows={groups.data}
    key={(g) => g.id}
    loading={groups.loading && !groups.loaded}
    error={groups.error && !groups.data ? errorText(groups.error) : undefined}
    onretry={() => groups.refresh()}
    onrowclick={(g) => router.setQuery({ sel: g.id })}
    selected={selected?.id}
    caption={t('dns.groups.title')}
    selectable={session.isAdmin}
    bind:checked
    checkLabel={(g) => t('dns.groups.selectNamed', { name: g.name })}
  />
  {#if session.isAdmin}
    <BulkBar
      count={checked.length}
      {busy}
      note={defaultChecked ? t('dns.groups.batchDefaultNote') : undefined}
      onclear={() => (checked = [])}
      actions={[
        { label: t('common.action.enable'), icon: 'play', onselect: () => batch('enable') },
        { label: t('common.action.disable'), icon: 'pause', onselect: () => batch('disable') },
        {
          label: t('common.action.delete'),
          icon: 'trash',
          danger: true,
          disabled: defaultChecked,
          title: defaultChecked ? t('dns.groups.batchDefaultNote') : undefined,
          onselect: () => batch('delete'),
        },
      ]}
    />
  {/if}
</Panel>

<GroupPanel bind:open={addOpen} {clients} {presets} onsaved={onchanged} />
<GroupPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  group={selected}
  {clients}
  {presets}
  onsaved={onchanged}
  ondeleted={onchanged}
/>

<style>
  .name {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    white-space: nowrap;
  }
</style>
