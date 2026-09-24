<!--
  @component
  Rules tab: your own allow and block rules (exact domain, domain with
  subdomains, regular expression) with groups. They always beat list
  entries. Filters are in the URL.
  Query: ?action=allow|block&type=exact|subtree|regex&search=…&sel=<rule id>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    resource,
    type ClientGroup,
    type FilterRule,
    type RuleAction,
    type RuleQuery,
    type RuleType,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Field, Input, Panel, Select, Table, toast, Toggle, type Column } from '$lib/ui'
  import { groupNames } from '../shared/groups'
  import RulePanel from './RulePanel.svelte'

  interface Props {
    groups: readonly ClientGroup[] | undefined
    onchanged: () => void
  }

  let { groups, onchanged }: Props = $props()

  const ACTIONS: RuleAction[] = ['allow', 'block']
  const TYPES: RuleType[] = ['exact', 'subtree', 'regex']

  const query = $derived.by((): RuleQuery => {
    const a = router.param('action') as RuleAction
    const ty = router.param('type') as RuleType
    return {
      action: ACTIONS.includes(a) ? a : undefined,
      type: TYPES.includes(ty) ? ty : undefined,
      search: router.param('search').trim() || undefined,
    }
  })

  const rules = resource((signal) => api.filter.rules.list(query, { signal }))

  let addOpen = $state(false)
  let toggling = $state<number[]>([])
  let searchText = $state(untrack(() => query.search ?? ''))
  let timer: ReturnType<typeof setTimeout> | undefined
  $effect(() => () => clearTimeout(timer))

  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(rules.data?.find((r) => r.id === selId))

  function onSearch() {
    clearTimeout(timer)
    timer = setTimeout(() => router.setQuery({ search: searchText.trim() }), 350)
  }

  function inputOf(r: FilterRule) {
    return { action: r.action, type: r.type, pattern: r.pattern, enabled: r.enabled, groupIds: [...r.groupIds], comment: r.comment }
  }

  function changed() {
    void rules.refresh()
    onchanged()
  }

  async function setEnabled(r: FilterRule, enabled: boolean) {
    toggling = [...toggling, r.id]
    try {
      const saved = await api.filter.rules.update(r.id, { ...inputOf(r), enabled })
      rules.set((rules.data ?? []).map((x) => (x.id === saved.id ? saved : x)))
      toast.success(enabled ? t('dns.rules.enabledToast') : t('dns.rules.disabledToast'))
      onchanged()
    } catch (e) {
      toast.error(e)
      void rules.refresh()
    } finally {
      toggling = toggling.filter((x) => x !== r.id)
    }
  }

  function typeLabel(ty: RuleType): string {
    return ty === 'exact' ? t('dns.rules.type.exact') : ty === 'subtree' ? t('dns.rules.type.subtree') : t('dns.rules.type.regex')
  }

  const actionOptions = $derived([
    { value: '', label: t('dns.rules.filter.allActions') },
    { value: 'block', label: t('dns.rules.action.block') },
    { value: 'allow', label: t('dns.rules.action.allow') },
  ])
  const typeOptions = $derived([
    { value: '', label: t('dns.rules.filter.allTypes') },
    ...TYPES.map((ty) => ({ value: ty, label: typeLabel(ty) })),
  ])

  const filtered = $derived(!!(query.action || query.type || query.search))

  const columns: Column<FilterRule>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'action', label: t('dns.rules.action'), width: '1%', sortable: true, value: (r) => r.action, cell: actionCell },
    { key: 'type', label: t('common.label.type'), sortable: true, value: (r) => typeLabel(r.type) },
    { key: 'pattern', label: t('dns.rules.pattern'), mono: true, truncate: true, width: '36%', sortable: true, value: (r) => r.pattern },
    { key: 'groups', label: t('common.label.groups'), value: (r) => groupNames(r.groupIds, groups) },
    { key: 'comment', label: t('common.label.comment'), truncate: true, value: (r) => r.comment },
    { key: 'updated', label: t('common.label.updated'), sortable: true, value: (r) => r.updatedAt, cell: updatedCell },
  ])
</script>

{#snippet enabledCell(r: FilterRule)}
  <Toggle
    bind:checked={() => r.enabled, (on) => setEnabled(r, on)}
    ariaLabel={t('dns.rules.enableNamed', { pattern: r.pattern })}
    disabled={!session.isAdmin || toggling.includes(r.id)}
  />
{/snippet}

{#snippet actionCell(r: FilterRule)}
  <Chip
    size="sm"
    pair={r.action === 'block' ? 'orange' : 'blue'}
    label={r.action === 'block' ? t('dns.rules.action.block') : t('dns.rules.action.allow')}
  />
{/snippet}

{#snippet updatedCell(r: FilterRule)}
  <span class="nowrap" title={formatDateTime(r.updatedAt)}>{formatRelative(r.updatedAt)}</span>
{/snippet}

<Panel flush title={t('dns.rules.title')} description={t('dns.rules.description')}>
  {#snippet actions()}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>{t('dns.rules.add')}</Button>
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
          placeholder={t('dns.rules.filter.search')}
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
    <div class="sel">
      <Field label={t('common.label.type')} hideLabel>
        <Select size="sm" value={query.type ?? ''} options={typeOptions} onchange={(e) => router.setQuery({ type: e.currentTarget.value, sel: null })} />
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
    caption={t('dns.rules.title')}
  >
    {#snippet empty()}
      {#if filtered}
        <EmptyState compact title={t('dns.rules.emptyFiltered')} />
      {:else}
        <EmptyState compact icon="shield" title={t('dns.rules.empty')} text={t('dns.rules.emptyText')}>
          <Button size="sm" variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
            {t('dns.rules.add')}
          </Button>
        </EmptyState>
      {/if}
    {/snippet}
  </Table>
</Panel>

<RulePanel bind:open={addOpen} {groups} onsaved={changed} />
<RulePanel
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
