<!--
  @component
  Cache › Library: what is cached. A per-service summary (also the service
  filter), then the content groups with size, completeness, retention and
  usage; searchable (labels too), sortable and paged on the server. A row
  opens the group's details (?group=<key>&groupService=<service>).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, isApiError, resource, type GroupSort, type GroupView } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDate, formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { href, router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { loadPref, savePref } from '$lib/storage'
  import {
    Button,
    EmptyState,
    Icon,
    Notice,
    Pager,
    Panel,
    Table,
    confirm,
    toast,
    type Column,
  } from '$lib/ui'
  import GroupPanel from './library/GroupPanel.svelte'
  import ServiceStrip from './library/ServiceStrip.svelte'
  import { serviceCatalog } from './shared/catalog.svelte'
  import FilterInput from './shared/FilterInput.svelte'
  import { offsetParam, pick } from './shared/util'

  const SORTS: readonly GroupSort[] = ['bytes', 'lastAccess', 'firstCached', 'served', 'name']
  const LIMITS = [25, 50, 100, 250]

  const catalog = serviceCatalog()
  const usage = resource((signal) => api.cache.services({ signal }), { interval: 30_000 })

  const service = $derived(router.param('service'))
  const search = $derived(router.param('search'))
  const sortKey = $derived(pick(router.param('sort'), SORTS, 'bytes'))
  const dir = $derived(router.param('dir'))
  // Names sort A–Z by default, everything else largest/newest first.
  const desc = $derived(dir === 'asc' ? false : dir === 'desc' ? true : sortKey !== 'name')
  const offset = $derived(offsetParam(router.param('offset')))
  const storedLimit = Number(loadPref('cache.library.limit'))
  let limit = $state(LIMITS.includes(storedLimit) ? storedLimit : 50)

  const groups = resource(
    (signal) =>
      api.cache.groups(
        { service: service || undefined, search: search || undefined, sort: sortKey, desc, limit, offset },
        { signal },
      ),
    { interval: 30_000 },
  )

  const storeOffline = $derived(isApiError(groups.error, 'unavailable') || isApiError(usage.error, 'unavailable'))
  const store = $derived(appStatus.overview.data?.store)

  function refresh() {
    void groups.refresh()
    void usage.refresh()
  }

  // ---- selection

  const selKey = $derived(router.param('group'))
  const selService = $derived(router.param('groupService'))
  const selRow = $derived(groups.data?.items.find((g) => g.groupKey === selKey && g.service === selService))

  function open(g: GroupView) {
    router.setQuery({ group: g.groupKey, groupService: g.service }, { push: true })
  }

  function close() {
    router.setQuery({ group: null, groupService: null })
  }

  // ---- purge a whole service

  async function purgeService() {
    const u = usage.data?.find((x) => x.service === service)
    const name = catalog.name(service)
    let freed = 0
    const ok = await confirm({
      title: t('cache.library.purgeServiceTitle', { name }),
      message: t('cache.library.purgeServiceText', {
        size: formatBytes(u?.cachedBytes ?? 0),
        count: formatNumber(u?.groups ?? 0),
      }),
      confirmLabel: t('cache.library.purgeService', { name }),
      action: async () => {
        freed = (await api.cache.purgeService(service)).bytesFreed
      },
    })
    if (!ok) return
    toast.success(t('cache.library.purged', { name, size: formatBytes(freed) }))
    router.setQuery({ service: null, offset: null })
    refresh()
  }

  const columns: Column<GroupView>[] = $derived([
    { key: 'name', label: t('cache.col.content'), sortable: true, cell: contentCell },
    { key: 'service', label: t('common.label.service'), format: (g) => catalog.name(g.service) },
    { key: 'bytes', label: t('cache.col.onDisk'), align: 'right', sortable: true, format: (g) => formatBytes(g.cachedBytes) },
    { key: 'complete', label: t('cache.col.complete'), align: 'right', cell: completeCell },
    { key: 'objects', label: t('cache.col.objects'), align: 'right', format: (g) => formatNumber(g.objects) },
    { key: 'firstCached', label: t('cache.col.firstCached'), align: 'right', sortable: true, cell: firstCell },
    { key: 'lastAccess', label: t('cache.col.lastAccess'), align: 'right', sortable: true, cell: accessCell },
    { key: 'expires', label: t('cache.col.expires'), align: 'right', cell: expiresCell },
    { key: 'served', label: t('cache.col.served'), align: 'right', sortable: true, format: (g) => formatBytes(g.bytesServed) },
    { key: 'clients', label: t('cache.col.clients'), align: 'right', format: (g) => formatNumber(g.clients) },
    { key: 'pinned', label: t('cache.col.pinned'), align: 'center', cell: pinnedCell },
  ])
</script>

{#snippet contentCell(g: GroupView)}
  <span class="content">
    <span class="label" title={g.label}>{g.label}</span>
    {#if g.label !== g.groupKey}<span class="key mono" title={g.groupKey}>{g.groupKey}</span>{/if}
  </span>
{/snippet}

{#snippet completeCell(g: GroupView)}
  <span title={t('cache.library.completeOf', { cached: formatBytes(g.cachedBytes), total: formatBytes(g.totalBytes) })}>
    {g.totalBytes > 0 ? formatPercent(Math.min(1, g.cachedBytes / g.totalBytes)) : '–'}
  </span>
{/snippet}

{#snippet firstCell(g: GroupView)}
  <span title={formatDateTime(g.firstCached)}>{formatDate(g.firstCached)}</span>
{/snippet}

{#snippet accessCell(g: GroupView)}
  <span title={formatDateTime(g.lastAccess)}>{formatRelative(g.lastAccess)}</span>
{/snippet}

{#snippet expiresCell(g: GroupView)}
  {#if g.pinned}
    <span class="subtle">{t('common.state.never')}</span>
  {:else}
    <span title={g.expiresAt ? formatDateTime(g.expiresAt) : undefined}>{formatDate(g.expiresAt)}</span>
  {/if}
{/snippet}

{#snippet pinnedCell(g: GroupView)}
  {#if g.pinned}<Icon name="pin" size={16} label={t('cache.col.pinned')} />{/if}
{/snippet}

<div class="page">
  {#if storeOffline}
    <Notice tone="fail" title={t('cache.library.offline')}>
      {store?.reason ?? t('cache.library.offlineText')}{#if store?.hint}<br />{store.hint}{/if}
      {#snippet actions()}
        <Button size="sm" icon="drive" href={href('/cache/storage')}>{t('cache.library.openStorage')}</Button>
      {/snippet}
    </Notice>
  {:else}
    <Panel title={t('cache.library.byService')} description={t('cache.library.byServiceText')}>
      <ServiceStrip
        usage={usage.data}
        {catalog}
        selected={service}
        onselect={(s) => router.setQuery({ service: s, offset: null })}
      />
      {#if usage.error}<p class="err">{errorText(usage.error)}</p>{/if}
    </Panel>

    <div class="toolbar">
      <FilterInput
        label={t('cache.library.search')}
        placeholder={t('cache.library.searchPlaceholder')}
        value={search}
        width="320px"
        onchange={(v) => router.setQuery({ search: v, offset: null })}
      />
      <span class="spacer"></span>
      {#if service}
        <Button variant="danger" icon="trash" disabled={!session.isAdmin} onclick={purgeService}>
          {t('cache.library.purgeService', { name: catalog.name(service) })}
        </Button>
      {/if}
    </div>

    <Panel flush>
      {#if groups.data && groups.data.items.length === 0 && !groups.error}
        {#if search || service}
          <EmptyState compact icon="search" title={t('cache.library.noMatch')} text={t('cache.library.noMatchText')} />
        {:else}
          <EmptyState compact icon="layers" title={t('cache.library.empty')} text={t('cache.library.emptyText')} />
        {/if}
      {:else}
        <Table
          caption={t('cache.library.caption')}
          rows={groups.data?.items}
          key={(g) => `${g.service}|${g.groupKey}`}
          {columns}
          loading={groups.loading && !groups.loaded}
          error={groups.error ? errorText(groups.error) : undefined}
          onretry={refresh}
          sort={{ key: sortKey, desc }}
          onsort={(s) =>
            router.setQuery({
              sort: s.key === 'bytes' ? null : s.key,
              dir: s.desc === (s.key !== 'name') ? null : s.desc ? 'desc' : 'asc',
              offset: null,
            })}
          onrowclick={open}
          selected={selRow ? `${selRow.service}|${selRow.groupKey}` : undefined}
        />
      {/if}
      {#if groups.data && groups.data.total > 0}
        <Pager
          total={groups.data.total}
          bind:limit
          {offset}
          limits={LIMITS}
          onlimit={(l) => {
            savePref('cache.library.limit', String(l))
            router.setQuery({ offset: null })
          }}
          onchange={(o) => router.setQuery({ offset: o || null })}
        />
      {/if}
    </Panel>
  {/if}
</div>

{#if selKey && selService}
  {#key `${selService}|${selKey}`}
    <GroupPanel
      service={selService}
      groupKey={selKey}
      fallbackTitle={selRow?.label ?? selKey}
      {catalog}
      onchanged={refresh}
      onclose={close}
    />
  {/key}
{/if}

<style>
  .content {
    display: flex;
    flex-direction: column;
    min-width: 0;
    max-width: 40ch;
  }
  .label,
  .key {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .key {
    font-size: var(--fs-xs);
    color: var(--text-3);
  }
  .err {
    margin-top: var(--sp-2);
    color: var(--danger);
    font-size: var(--fs-sm);
  }
</style>
