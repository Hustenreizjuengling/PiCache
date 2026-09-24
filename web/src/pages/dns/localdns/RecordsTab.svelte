<!--
  @component
  Local DNS records answered by PiCache itself (A, AAAA, CNAME, TXT; auto
  PTR for A/AAAA). Search, enable/disable inline, rows open the editor.
  Query: ?search=…&sel=<record id>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, type DnsRecord } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDuration } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, EmptyState, Field, Input, Panel, Table, toast, Toggle, type Column, type SortState } from '$lib/ui'
  import RecordPanel from './RecordPanel.svelte'

  const records = resource((signal) => api.dns.records.list({ signal }))

  let addOpen = $state(false)
  let toggling = $state<number[]>([])
  let sort = $state<SortState>({ key: 'name', desc: false })
  let search = $state(untrack(() => router.param('search')))

  const needle = $derived(search.trim().toLowerCase())
  const rows = $derived(
    needle
      ? records.data?.filter((r) => `${r.name} ${r.type} ${r.value} ${r.comment}`.toLowerCase().includes(needle))
      : records.data,
  )
  const selId = $derived(Number(router.param('sel')) || 0)
  const selected = $derived(records.data?.find((r) => r.id === selId))

  function onSearch() {
    router.setQuery({ search: search.trim() })
  }

  async function setEnabled(r: DnsRecord, enabled: boolean) {
    toggling = [...toggling, r.id]
    try {
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

  const columns: Column<DnsRecord>[] = $derived([
    { key: 'enabled', label: t('common.label.enabled'), width: '1%', cell: enabledCell },
    { key: 'name', label: t('common.label.name'), mono: true, sortable: true, value: (r) => r.name },
    { key: 'type', label: t('common.label.type'), sortable: true, width: '1%', value: (r) => r.type },
    { key: 'value', label: t('dns.records.value'), mono: true, truncate: true, width: '32%', sortable: true, value: (r) => r.value },
    {
      key: 'ttl',
      label: t('dns.records.ttl'),
      align: 'right',
      sortable: true,
      value: (r) => r.ttl,
      format: (r) => formatDuration(r.ttl * 1000),
    },
    { key: 'comment', label: t('common.label.comment'), truncate: true, value: (r) => r.comment },
  ])
</script>

{#snippet enabledCell(r: DnsRecord)}
  <Toggle
    bind:checked={() => r.enabled, (on) => setEnabled(r, on)}
    ariaLabel={t('dns.records.enableNamed', { name: r.name, type: r.type })}
    disabled={!session.isAdmin || toggling.includes(r.id)}
  />
{/snippet}

<Panel flush title={t('dns.records.title')} description={t('dns.records.description')}>
  {#snippet actions()}
    <Button variant="primary" icon="plus" disabled={!session.isAdmin} onclick={() => (addOpen = true)}>{t('dns.records.add')}</Button>
  {/snippet}
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
</Panel>

<RecordPanel bind:open={addOpen} records={records.data} onsaved={() => records.refresh()} />
<RecordPanel
  bind:open={() => !!selected, (v) => !v && router.setQuery({ sel: null })}
  record={selected}
  records={records.data}
  onsaved={() => records.refresh()}
  ondeleted={() => records.refresh()}
/>

<style>
  .search {
    max-width: 360px;
    padding: 0 var(--sp-4) var(--sp-3);
  }
</style>
