<!--
  @component
  Groups: lists and rules apply to a client when they share an enabled
  group. Enable or disable inline; rows open the group panel.
  Query: ?tab=groups&sel=<group id>
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, type ClientGroup, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDate, formatNumber } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, Panel, Table, toast, Toggle, type Column } from '$lib/ui'
  import GroupPanel from './GroupPanel.svelte'

  interface Props {
    groups: Resource<ClientGroup[]>
    onchanged: () => void
  }

  let { groups, onchanged }: Props = $props()

  let addOpen = $state(false)
  let toggling = $state<number[]>([])

  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(groups.data?.find((g) => g.id === selId))

  async function setEnabled(g: ClientGroup, enabled: boolean) {
    toggling = [...toggling, g.id]
    try {
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

  const columns: Column<ClientGroup>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'name', label: t('common.label.name'), sortable: true, value: (g) => g.name, cell: nameCell },
    { key: 'comment', label: t('common.label.comment'), truncate: true, width: '50%', value: (g) => g.comment },
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
  <span class="row">
    <span>{g.name}</span>
    {#if g.id === DEFAULT_GROUP_ID}<Badge tone="info" title={t('dns.groups.defaultNote')}>{t('dns.groups.default')}</Badge>{/if}
  </span>
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
  />
</Panel>

<GroupPanel bind:open={addOpen} onsaved={onchanged} />
<GroupPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  group={selected}
  onsaved={onchanged}
  ondeleted={onchanged}
/>
