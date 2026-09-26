<!--
  @component
  IP rules ("Answer addresses" of the rules tab): addresses and networks
  that block an answer containing them, or that lists of answer addresses
  must not block, with groups. Filters are in the URL; admins select rows
  to enable, disable or delete them together.
  Query: ?tab=rules&view=ip&action=allow|block&search=…&sel=<IP rule id>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type BatchAction, type ClientGroup, type IPRule, type IPRuleQuery, type RuleAction } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { BulkBar, Button, Chip, EmptyState, Field, Input, Panel, Select, Table, toast, Toggle, type Column } from '$lib/ui'
  import { runBatch } from '../shared/batch'
  import { groupNames } from '../shared/groups'
  import IpRulePanel from './IpRulePanel.svelte'

  interface Props {
    groups: readonly ClientGroup[] | undefined
    onchanged: () => void
  }

  let { groups, onchanged }: Props = $props()

  const ACTIONS: RuleAction[] = ['allow', 'block']

  const query = $derived.by((): IPRuleQuery => {
    const a = router.param('action') as RuleAction
    return { action: ACTIONS.includes(a) ? a : undefined, search: router.param('search').trim() || undefined }
  })

  const rules = resource((signal) => api.filter.ipRules.list(query, { signal }))

  let addOpen = $state(false)
  let toggling = $state<number[]>([])
  let checked = $state<number[]>([])
  let busy = $state(false)
  let searchText = $state(untrack(() => query.search ?? ''))
  let timer: ReturnType<typeof setTimeout> | undefined
  $effect(() => () => clearTimeout(timer))

  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(rules.data?.find((r) => r.id === selId))
  const filtered = $derived(!!(query.action || query.search))

  function onSearch() {
    clearTimeout(timer)
    timer = setTimeout(() => router.setQuery({ search: searchText.trim() }), 350)
  }

  function changed() {
    void rules.refresh()
    onchanged()
  }

  async function setEnabled(r: IPRule, enabled: boolean) {
    toggling = [...toggling, r.id]
    try {
      const saved = await api.filter.ipRules.update(r.id, {
        action: r.action,
        pattern: r.pattern,
        enabled,
        groupIds: [...r.groupIds],
        comment: r.comment,
      })
      rules.set((rules.data ?? []).map((x) => (x.id === saved.id ? saved : x)))
      toast.success(enabled ? t('dns.ipRules.enabledToast', { pattern: r.pattern }) : t('dns.ipRules.disabledToast', { pattern: r.pattern }))
      onchanged()
    } catch (e) {
      toast.error(e)
      void rules.refresh()
    } finally {
      toggling = toggling.filter((x) => x !== r.id)
    }
  }

  async function batch(action: BatchAction) {
    busy = true
    try {
      const ok = await runBatch({
        action,
        ids: checked,
        what: (n) => tn('dns.ipRules.count', n),
        run: (req) => api.filter.ipRules.batch(req),
      })
      if (!ok) return
      if (action === 'delete') {
        if (selected && checked.includes(selected.id)) router.setQuery({ sel: null })
        checked = []
      }
      changed()
    } finally {
      busy = false
    }
  }

  const actionOptions = $derived([
    { value: '', label: t('dns.rules.filter.allActions') },
    { value: 'block', label: t('dns.ipRules.action.block') },
    { value: 'allow', label: t('dns.ipRules.action.allow') },
  ])

  const columns: Column<IPRule>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'action', label: t('dns.rules.action'), width: '1%', sortable: true, value: (r) => r.action, cell: actionCell },
    { key: 'pattern', label: t('dns.ipRules.pattern'), mono: true, width: '25%', sortable: true, value: (r) => r.pattern },
    { key: 'groups', label: t('common.label.groups'), truncate: true, width: '25%', value: (r) => groupNames(r.groupIds, groups) },
    { key: 'comment', label: t('common.label.comment'), truncate: true, width: '50%', value: (r) => r.comment },
    { key: 'updated', label: t('common.label.updated'), width: '1%', sortable: true, value: (r) => r.updatedAt, cell: updatedCell },
  ])
</script>

{#snippet enabledCell(r: IPRule)}
  <Toggle
    bind:checked={() => r.enabled, (on) => setEnabled(r, on)}
    ariaLabel={t('dns.ipRules.enableNamed', { pattern: r.pattern })}
    disabled={!session.isAdmin || toggling.includes(r.id)}
  />
{/snippet}

{#snippet actionCell(r: IPRule)}
  <Chip
    size="sm"
    pair={r.action === 'block' ? 'orange' : 'blue'}
    label={r.action === 'block' ? t('dns.rules.action.block') : t('dns.rules.action.allow')}
  />
{/snippet}

{#snippet updatedCell(r: IPRule)}
  <span class="nowrap" title={formatDateTime(r.updatedAt)}>{formatRelative(r.updatedAt)}</span>
{/snippet}

<Panel flush title={t('dns.ipRules.title')} description={t('dns.ipRules.description')}>
  {#snippet actions()}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>{t('dns.ipRules.add')}</Button>
  {/snippet}

  <div class="toolbar filters">
    <div class="search">
      <Field label={t('common.action.search')} hideLabel>
        <Input
          type="search"
          size="sm"
          icon="search"
          mono
          bind:value={searchText}
          placeholder={t('dns.ipRules.search')}
          maxlength={256}
          oninput={onSearch}
        />
      </Field>
    </div>
    <div class="sel">
      <Field label={t('dns.rules.action')} hideLabel>
        <Select size="sm" value={query.action ?? ''} options={actionOptions} onchange={(e) => router.setQuery({ action: e.currentTarget.value, sel: null })} />
      </Field>
    </div>
  </div>

  <Table
    {columns}
    rows={rules.data}
    key={(r) => r.id}
    loading={rules.loading && !rules.loaded}
    error={rules.error ? errorText(rules.error) : undefined}
    onretry={() => rules.refresh()}
    onrowclick={(r) => router.setQuery({ sel: r.id })}
    selected={selected?.id}
    caption={t('dns.ipRules.title')}
    selectable={session.isAdmin}
    bind:checked
    checkLabel={(r) => t('dns.ipRules.selectNamed', { pattern: r.pattern })}
  >
    {#snippet empty()}
      {#if filtered}
        <EmptyState compact title={t('dns.rules.emptyFiltered')} />
      {:else}
        <EmptyState compact icon="shield" title={t('dns.ipRules.empty')} text={t('dns.ipRules.emptyText')}>
          <Button size="sm" variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
            {t('dns.ipRules.add')}
          </Button>
        </EmptyState>
      {/if}
    {/snippet}
  </Table>
  {#if session.isAdmin}
    <BulkBar
      count={checked.length}
      {busy}
      onclear={() => (checked = [])}
      actions={[
        { label: t('common.action.enable'), icon: 'play', onselect: () => batch('enable') },
        { label: t('common.action.disable'), icon: 'pause', onselect: () => batch('disable') },
        { label: t('common.action.delete'), icon: 'trash', danger: true, onselect: () => batch('delete') },
      ]}
    />
  {/if}
</Panel>

<IpRulePanel bind:open={addOpen} {groups} onsaved={changed} />
<IpRulePanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  rule={selected}
  {groups}
  onsaved={changed}
  ondeleted={changed}
/>

<style>
  .filters {
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .search {
    flex: 1 1 240px;
    max-width: 360px;
  }
  .sel {
    flex: 0 1 180px;
  }
  @media (max-width: 480px) {
    .search,
    .sel {
      flex: 1 1 100%;
      max-width: none;
    }
  }
</style>
