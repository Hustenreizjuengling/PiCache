<!--
  @component
  Download servers that answered range requests incorrectly. After three
  failures within 24 hours PiCache fetches their files in one piece; an admin
  can reset a host to try parts again right away.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type NoSliceHost } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, EmptyState, Panel, Table, toast, type Column } from '$lib/ui'

  const hosts = resource((signal) => api.cache.noSlice({ signal }), { interval: 60_000 })

  let resetting = $state('')
  async function reset(h: NoSliceHost) {
    resetting = h.host
    try {
      await api.cache.resetNoSlice(h.host)
      toast.success(t('cache.noSlice.resetDone', { host: h.host }))
      await hosts.refresh()
    } catch (err) {
      toast.error(err)
    } finally {
      resetting = ''
    }
  }

  const columns: Column<NoSliceHost>[] = $derived([
    { key: 'host', label: t('cache.col.host'), mono: true, sortable: true, value: (h) => h.host },
    { key: 'state', label: t('common.label.status'), cell: stateCell },
    { key: 'failures', label: t('cache.noSlice.failures'), align: 'right', sortable: true, value: (h) => h.failures, format: (h) => formatNumber(h.failures) },
    { key: 'since', label: t('cache.noSlice.since'), align: 'right', sortable: true, value: (h) => h.since, cell: sinceCell },
    { key: 'actions', label: t('common.label.actions'), align: 'right', cell: actionCell },
  ])
</script>

{#snippet stateCell(h: NoSliceHost)}
  {#if h.marked}
    <Chip size="sm" tone="warn" label={t('cache.noSlice.marked')} />
  {:else}
    <Chip size="sm" tone="neutral" label={t('cache.noSlice.watching')} />
  {/if}
{/snippet}

{#snippet sinceCell(h: NoSliceHost)}
  <span title={formatDateTime(h.since)}>{formatRelative(h.since)}</span>
{/snippet}

{#snippet actionCell(h: NoSliceHost)}
  <Button size="sm" variant="ghost" icon="refresh" loading={resetting === h.host} disabled={!session.isAdmin} onclick={() => reset(h)}>
    {t('cache.noSlice.reset')}
  </Button>
{/snippet}

<Panel flush title={t('cache.noSlice.title')} description={t('cache.noSlice.description')}>
  {#if hosts.data && hosts.data.length === 0 && !hosts.error}
    <EmptyState compact title={t('cache.noSlice.empty')} />
  {:else}
    <Table
      compact
      caption={t('cache.noSlice.title')}
      rows={hosts.data}
      key={(h) => h.host}
      {columns}
      loading={hosts.loading && !hosts.loaded}
      error={hosts.error ? errorText(hosts.error) : undefined}
      onretry={() => hosts.refresh()}
      skeletonRows={2}
    />
  {/if}
</Panel>
