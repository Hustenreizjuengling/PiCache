<!--
  @component
  Local DNS records answered by PiCache itself (A, AAAA, CNAME, TXT, SRV,
  MX, PTR, HTTPS, SVCB; auto PTR for A/AAAA), for everyone or for the
  clients of some groups. On top the two answer settings: local records on
  or off (a notice while off) and the order of multi-address answers
  (saved at once). Search, enable/disable inline, rows open the editor;
  admins import hosts files and select rows to enable, disable or delete
  them together.
  Query: ?search=…&sel=<record id>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type BatchAction, type DnsRecord, type LocalizeRecords, type Settings } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDuration } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import {
    Badge,
    BulkBar,
    Button,
    EmptyState,
    Field,
    Input,
    Notice,
    Panel,
    Select,
    Table,
    toast,
    Toggle,
    type Column,
    type SortState,
  } from '$lib/ui'
  import { runBatch } from '../shared/batch'
  import RecordImportDialog from './RecordImportDialog.svelte'
  import RecordPanel from './RecordPanel.svelte'
  import { appliesToNobody, scopeText } from './records'

  const records = resource((signal) => api.dns.records.list({ signal }))
  const groups = resource((signal) => api.groups.list({ signal }))
  const settings = resource((signal) => api.settings.get({ signal }))

  let addOpen = $state(false)
  let importOpen = $state(false)
  let toggling = $state<number[]>([])
  let checked = $state<number[]>([])
  let busy = $state(false)
  let savingSetting = $state(false)
  let sort = $state<SortState>({ key: 'name', desc: false })
  let search = $state(untrack(() => router.param('search')))

  const needle = $derived(search.trim().toLowerCase())
  const rows = $derived(
    needle
      ? records.data?.filter((r) =>
          `${r.name} ${r.type} ${r.value} ${r.comment} ${scopeText(r, groups.data)}`.toLowerCase().includes(needle),
        )
      : records.data,
  )
  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(records.data?.find((r) => r.id === selId))
  const dns = $derived(settings.data?.dns)

  function onSearch() {
    router.setQuery({ search: search.trim() })
  }

  async function setEnabled(r: DnsRecord, enabled: boolean) {
    toggling = [...toggling, r.id]
    try {
      // Scope, groups and the other family are kept when left out.
      const saved = await api.dns.records.update(r.id, {
        name: r.name,
        type: r.type,
        value: r.value,
        ttl: r.ttl,
        enabled,
        comment: r.comment,
      })
      records.set((records.data ?? []).map((x) => (x.id === saved.id ? saved : x)))
      toast.success(enabled ? t('dns.records.enabledToast', { name: r.name }) : t('dns.records.disabledToast', { name: r.name }))
    } catch (e) {
      toast.error(e)
      void records.refresh()
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
        what: (n) => tn('dns.records.count', n),
        run: (req) => api.dns.records.batch(req),
      })
      if (!ok) return
      if (action === 'delete') {
        if (selected && checked.includes(selected.id)) router.setQuery({ sel: null })
        checked = []
      }
      void records.refresh()
    } finally {
      busy = false
    }
  }

  // The answer settings are saved at once (PATCH /settings/dns with the one member).
  async function patchDns(change: Partial<Settings['dns']>, done: string) {
    savingSetting = true
    try {
      settings.set(await api.settings.patch('dns', change))
      toast.success(done)
    } catch (e) {
      toast.error(e)
      void settings.refresh()
    } finally {
      savingSetting = false
    }
  }

  const localizeOptions = $derived([
    { value: 'first', label: t('dns.records.localize.first') },
    { value: 'only', label: t('dns.records.localize.only') },
    { value: 'off', label: t('dns.records.localize.off') },
  ])
  const localizeHelp = $derived(
    dns?.localizeRecords === 'only'
      ? t('dns.records.localizeHelp.only')
      : dns?.localizeRecords === 'off'
        ? t('dns.records.localizeHelp.off')
        : t('dns.records.localizeHelp.first'),
  )

  const columns: Column<DnsRecord>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'name', label: t('common.label.name'), mono: true, sortable: true, value: (r) => r.name },
    { key: 'type', label: t('common.label.type'), sortable: true, width: '1%', value: (r) => r.type },
    { key: 'value', label: t('dns.records.value'), mono: true, truncate: true, width: '32%', sortable: true, value: (r) => r.value },
    { key: 'scope', label: t('dns.records.scope'), sortable: true, value: (r) => scopeText(r, groups.data), cell: scopeCell },
    {
      key: 'ttl',
      label: t('dns.records.ttl'),
      align: 'right',
      sortable: true,
      value: (r) => r.ttl,
      format: (r) => formatDuration(r.ttl * 1000),
    },
    { key: 'comment', label: t('common.label.comment'), truncate: true, width: '24%', value: (r) => r.comment },
  ])
</script>

{#snippet enabledCell(r: DnsRecord)}
  <Toggle
    bind:checked={() => r.enabled, (on) => setEnabled(r, on)}
    ariaLabel={t('dns.records.enableNamed', { name: r.name, type: r.type })}
    disabled={!session.isAdmin || toggling.includes(r.id)}
  />
{/snippet}

{#snippet scopeCell(r: DnsRecord)}
  <span class="scope">
    {#if appliesToNobody(r)}
      <Badge tone="warn" title={t('dns.records.nobodyWarning')}>{t('dns.records.scope.nobody')}</Badge>
    {:else if r.scope === 'all'}
      <span class="subtle">{t('dns.records.scope.all')}</span>
    {:else}
      <span>{scopeText(r, groups.data)}</span>
    {/if}
    {#if r.otherFamily === 'forward'}
      <Badge title={t('dns.records.otherFamilyHelp.forward')}>{t('dns.records.otherFamily.badge')}</Badge>
    {/if}
  </span>
{/snippet}

<Panel flush title={t('dns.records.title')} description={t('dns.records.description')}>
  {#snippet actions()}
    {#if session.isAdmin}
      <Button icon="upload" onclick={() => (importOpen = true)}>{t('dns.records.import')}</Button>
    {/if}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>{t('dns.records.add')}</Button>
  {/snippet}

  {#if dns}
    <div class="answers">
      <div class="switch">
        <Toggle
          bind:checked={
            () => dns.localRecordsEnabled,
            (on) => patchDns({ localRecordsEnabled: on }, on ? t('dns.records.enabledAllOn') : t('dns.records.enabledAllOff'))
          }
          label={t('dns.records.enabledAll')}
          description={t('dns.records.enabledAllHelp')}
          disabled={!session.isAdmin || savingSetting}
        />
      </div>
      <div class="order">
        <Field label={t('dns.records.localize')} help={localizeHelp}>
          <Select
            size="sm"
            value={dns.localizeRecords}
            options={localizeOptions}
            disabled={!session.isAdmin || savingSetting}
            onchange={(e) => patchDns({ localizeRecords: e.currentTarget.value as LocalizeRecords }, t('common.state.saved'))}
          />
        </Field>
      </div>
    </div>
    {#if !dns.localRecordsEnabled}
      <div class="off"><Notice tone="warn" title={t('dns.records.offTitle')}>{t('dns.records.offText')}</Notice></div>
    {/if}
  {:else if settings.error}
    <div class="off"><Notice tone="fail">{errorText(settings.error)}</Notice></div>
  {/if}

  {#if (records.data?.length ?? 0) > 0}
    <div class="search">
      <Field label={t('common.action.search')} hideLabel>
        <Input
          type="search"
          size="sm"
          icon="search"
          bind:value={search}
          placeholder={t('dns.records.search')}
          maxlength={256}
          oninput={onSearch}
        />
      </Field>
    </div>
  {/if}
  <Table
    {columns}
    {rows}
    bind:sort
    key={(r) => r.id}
    loading={records.loading && !records.loaded}
    error={records.error && !records.data ? errorText(records.error) : undefined}
    onretry={() => records.refresh()}
    onrowclick={(r) => router.setQuery({ sel: r.id })}
    selected={selected?.id}
    caption={t('dns.records.title')}
    selectable={session.isAdmin}
    bind:checked
    checkLabel={(r) => t('dns.records.selectNamed', { name: r.name, type: r.type })}
  >
    {#snippet empty()}
      {#if needle}
        <EmptyState compact title={t('dns.records.emptySearch')} />
      {:else}
        <EmptyState compact icon="home" title={t('dns.records.empty')} text={t('dns.records.emptyText')}>
          <Button size="sm" variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>
            {t('dns.records.add')}
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

<RecordPanel bind:open={addOpen} records={records.data} groups={groups.data} onsaved={() => records.refresh()} />
<RecordPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  record={selected}
  records={records.data}
  groups={groups.data}
  onsaved={() => records.refresh()}
  ondeleted={() => records.refresh()}
/>
{#if session.isAdmin}
  <RecordImportDialog bind:open={importOpen} groups={groups.data} onimported={() => records.refresh()} />
{/if}

<style>
  .answers {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-3) var(--sp-6);
    padding: 0 var(--sp-4) var(--sp-3);
    margin-bottom: var(--sp-3);
    border-bottom: 1px solid var(--line);
  }
  .switch {
    flex: 1 1 280px;
    min-width: 0;
  }
  .order {
    flex: 0 1 320px;
    min-width: 0;
  }
  .off {
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .search {
    max-width: 360px;
    padding: 0 var(--sp-4) var(--sp-3);
  }
  .scope {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
    white-space: nowrap;
  }
</style>
