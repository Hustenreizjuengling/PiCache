<!--
  @component
  Cache › Services: the download services PiCache caches (from
  uklans/cache-domains plus custom ones) with on/off switches, traffic of the
  last 24 hours and warnings; the state of the service list; and download
  servers without range support. A row opens the service (?service=<id>).
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type DownloadCacheService, type ServiceStat } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatNumber } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import {
    Badge,
    Button,
    Chip,
    EmptyState,
    Notice,
    Panel,
    Table,
    Toggle,
    toast,
    type Column,
    type SortState,
  } from '$lib/ui'
  import CustomServiceDialog from './services/CustomServiceDialog.svelte'
  import NoSliceHosts from './services/NoSliceHosts.svelte'
  import ServicePanel from './services/ServicePanel.svelte'
  import SourcePanel from './services/SourcePanel.svelte'
  import FilterInput from './shared/FilterInput.svelte'
  import { formatHitRatio, hitRatio, mostlyHttps } from './shared/util'

  const services = resource((signal) => api.downloadCache.services({ signal }), { interval: 60_000 })
  const stats = resource((signal) => api.stats.services('24h', { signal }), { interval: 60_000 })
  const source = resource((signal) => api.downloadCache.source({ signal }), { interval: 60_000 })
  const proxy = resource((signal) => api.cache.proxyStats({ signal }), { interval: 60_000 })

  const statMap = $derived(new Map((stats.data ?? []).map((s): [string, ServiceStat] => [s.service, s])))
  const query = $derived(router.param('q').toLowerCase())
  const rows = $derived(
    services.data?.filter(
      (s) => !query || s.name.toLowerCase().includes(query) || s.id.includes(query) || s.description.toLowerCase().includes(query),
    ),
  )
  const enabledCount = $derived((services.data ?? []).filter((s) => s.enabled).length)
  let sort = $state<SortState>({ key: 'name', desc: false })

  // ---- selection

  const selectedId = $derived(router.param('service'))
  const selected = $derived(services.data?.find((s) => s.id === selectedId))

  function open(s: DownloadCacheService) {
    router.setQuery({ service: s.id }, { push: true })
  }

  function replace(updated: DownloadCacheService) {
    if (services.data) services.set(services.data.map((s) => (s.id === updated.id ? { ...updated, domains: s.domains } : s)))
    void services.refresh()
  }

  // ---- on/off from the table

  let toggling = $state('')
  async function setEnabled(s: DownloadCacheService, on: boolean) {
    toggling = s.id
    try {
      replace(await api.downloadCache.setEnabled(s.id, on))
      toast.success(on ? t('cache.services.enabled', { name: s.name }) : t('cache.services.disabled', { name: s.name }))
    } catch (err) {
      toast.error(err)
      void services.refresh()
    } finally {
      toggling = ''
    }
  }

  // ---- custom services, source

  let adding = $state(false)

  let refreshing = $state(false)
  async function refreshSource() {
    refreshing = true
    try {
      source.set(await api.downloadCache.refreshSource())
      toast.success(t('cache.source.refreshed'))
      void services.refresh()
    } catch (err) {
      toast.error(err)
      void source.refresh()
    } finally {
      refreshing = false
    }
  }

  const refusedSteam = $derived(proxy.data?.steamHostsRefused ?? [])

  const columns: Column<DownloadCacheService>[] = $derived([
    { key: 'enabled', label: t('cache.col.cached'), width: '72px', cell: enabledCell },
    { key: 'name', label: t('common.label.service'), sortable: true, value: (s) => s.name, cell: nameCell },
    { key: 'domains', label: t('cache.services.hostNames'), align: 'right', sortable: true, value: (s) => s.domainCount, cell: domainsCell },
    {
      key: 'sent',
      label: t('cache.services.downloaded24h'),
      align: 'right',
      sortable: true,
      value: (s) => statMap.get(s.id)?.bytesSent ?? 0,
      format: (s) => formatBytes(statMap.get(s.id)?.bytesSent ?? 0),
    },
    {
      key: 'hit',
      label: t('cache.col.fromCache'),
      align: 'right',
      sortable: true,
      value: (s) => {
        const st = statMap.get(s.id)
        return st ? hitRatio(st.bytesHit, st.bytesWan) : null
      },
      format: (s) => {
        const st = statMap.get(s.id)
        return st ? formatHitRatio(st.bytesHit, st.bytesWan) : '–'
      },
    },
    {
      key: 'https',
      label: t('cache.services.https24h'),
      align: 'right',
      sortable: true,
      value: (s) => statMap.get(s.id)?.sniBytes ?? 0,
      format: (s) => formatBytes(statMap.get(s.id)?.sniBytes ?? 0),
    },
    { key: 'notes', label: t('cache.services.notes'), cell: notesCell },
  ])
</script>

{#snippet enabledCell(s: DownloadCacheService)}
  <Toggle
    checked={s.enabled}
    ariaLabel={t('cache.services.toggleLabel', { name: s.name })}
    disabled={!session.isAdmin || toggling === s.id}
    onchange={(on) => setEnabled(s, on)}
  />
{/snippet}

{#snippet nameCell(s: DownloadCacheService)}
  <span class="name">
    <span class="row-inline">
      <span class="strong">{s.name}</span>
      {#if s.custom}<Badge tone="info">{t('cache.services.custom')}</Badge>{/if}
    </span>
    {#if s.description}<span class="desc" title={s.description}>{s.description}</span>{/if}
  </span>
{/snippet}

{#snippet domainsCell(s: DownloadCacheService)}
  <span title={tn('cache.services.extraCount', s.extraDomains.length)}>
    {formatNumber(s.domainCount)}{#if s.extraDomains.length > 0 && !s.custom}<span class="subtle"> (+{formatNumber(s.extraDomains.length)})</span>{/if}
  </span>
{/snippet}

{#snippet notesCell(s: DownloadCacheService)}
  {@const st = statMap.get(s.id)}
  <span class="chips">
    {#if st && mostlyHttps(st.bytesSent, st.sniBytes)}
      <Chip size="sm" tone="warn" label={t('cache.services.httpsChip')} title={t('cache.services.httpsTitle')} />
    {/if}
    {#if s.mixedContent}<Chip size="sm" tone="neutral" label={t('cache.services.mixedChip')} title={t('cache.services.mixedText')} />{/if}
    {#if s.notes}<Chip size="sm" tone="info" icon="info" label={t('cache.services.notesChip')} title={s.notes} />{/if}
  </span>
{/snippet}

<div class="page">
  {#if !session.canOperate}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  {#if source.data && !source.data.ready}
    <Notice tone="warn" title={t('cache.source.notReadyTitle')}>{t('cache.source.notReadyText')}</Notice>
  {/if}

  {#if refusedSteam.length > 0}
    <Notice tone="warn" title={t('cache.services.steamRefusedTitle')}>
      {t('cache.services.steamRefusedText')}
      <span class="refused">
        {#each refusedSteam as h (h)}<span class="mono">{h}</span>{/each}
      </span>
      {#snippet actions()}
        <Button size="sm" onclick={() => router.setQuery({ service: 'steam' }, { push: true })}>{t('cache.services.openSteam')}</Button>
      {/snippet}
    </Notice>
  {/if}

  <div class="toolbar">
    <FilterInput
      label={t('cache.services.search')}
      placeholder={t('cache.services.searchPlaceholder')}
      value={router.param('q')}
      onchange={(v) => router.setQuery({ q: v })}
    />
    <span class="spacer"></span>
    <Button icon="plus" disabled={!session.isAdmin} onclick={() => (adding = true)}>{t('cache.services.addCustom')}</Button>
  </div>

  <Panel
    flush
    title={t('cache.services.title')}
    description={services.data
      ? t('cache.services.summary', { enabled: formatNumber(enabledCount), total: formatNumber(services.data.length) })
      : undefined}
  >
    <Table
      caption={t('cache.services.title')}
      {rows}
      key={(s) => s.id}
      {columns}
      bind:sort
      loading={services.loading && !services.loaded}
      error={services.error ? errorText(services.error) : undefined}
      onretry={() => services.refresh()}
      onrowclick={open}
      selected={selected?.id}
    >
      {#snippet empty()}
        {#if query}
          <EmptyState compact icon="search" title={t('cache.services.noMatch')} />
        {:else}
          <EmptyState compact icon="grid" title={t('cache.services.empty')} text={t('cache.services.emptyText')} />
        {/if}
      {/snippet}
    </Table>
  </Panel>

  <SourcePanel source={source.data} error={source.error} {refreshing} onrefresh={refreshSource} />

  <NoSliceHosts />
</div>

{#if selected}
  {#key selected.id}
    <ServicePanel
      row={selected}
      stat={statMap.get(selected.id)}
      onchanged={replace}
      ondeleted={() => {
        router.setQuery({ service: null })
        void services.refresh()
      }}
      onclose={() => router.setQuery({ service: null })}
    />
  {/key}
{/if}

<CustomServiceDialog
  bind:open={adding}
  onsaved={(s) => {
    void services.refresh().then(() => router.setQuery({ service: s.id }, { push: true }))
  }}
/>

<style>
  .name {
    display: flex;
    flex-direction: column;
    min-width: 0;
    max-width: 34ch;
  }
  .row-inline {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .strong {
    font-weight: 600;
  }
  .desc {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
  .chips {
    display: inline-flex;
    gap: var(--sp-1);
    white-space: nowrap;
  }
  .refused {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-1) var(--sp-3);
    margin-top: var(--sp-2);
  }
</style>
