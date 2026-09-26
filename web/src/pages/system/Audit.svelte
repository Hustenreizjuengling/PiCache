<!--
  @component
  Audit log: every state-changing admin action and failed sign-in, newest
  first, with search and paging. Row click shows the details (JSON as text).
  Query: ?q=<search>&offset=<n>&limit=<n>&entry=<id>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type AuditEntry } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatDateTimeShort, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import {
    Badge,
    CopyButton,
    EmptyState,
    Field,
    IconButton,
    Input,
    KeyValue,
    Notice,
    Pager,
    Panel,
    SidePanel,
    Table,
    type Column,
  } from '$lib/ui'
  import { isFailure, prettyDetails, summarizeDetails } from './audit/details'

  const LIMITS = [25, 50, 100, 200]
  const DEFAULT_LIMIT = 50
  const MAX_OFFSET = 100_000 // the server's limit
  const MAX_SEARCH = 100

  const q = $derived(router.param('q').slice(0, MAX_SEARCH))
  const limit = $derived.by(() => {
    const n = Number(router.param('limit'))
    return LIMITS.includes(n) ? n : DEFAULT_LIMIT
  })
  const offset = $derived(Math.min(MAX_OFFSET, Math.max(0, Math.floor(Number(router.param('offset')) || 0))))
  const entryId = $derived(Number(router.param('entry')) || undefined)

  const log = resource((signal) =>
    session.canOperate
      ? api.system.audit({ search: q || undefined, limit, offset }, { signal })
      : Promise.resolve(undefined),
  )
  const selected = $derived(log.data?.items.find((e) => e.id === entryId))

  // Search box: typing updates the URL after a short pause; Back/Forward update the box.
  let search = $state(router.param('q').slice(0, MAX_SEARCH))
  let timer: ReturnType<typeof setTimeout> | undefined
  $effect(() => {
    const fromUrl = q
    untrack(() => {
      if (search.trim() !== fromUrl) search = fromUrl
    })
  })
  $effect(() => () => clearTimeout(timer))

  function typed(value: string) {
    clearTimeout(timer)
    timer = setTimeout(() => router.setQuery({ q: value.trim(), offset: null, entry: null }), 300)
  }

  function select(e: AuditEntry | undefined) {
    router.setQuery({ entry: e?.id }, { push: !!e })
  }

  // Time, action, user and address keep their natural width (1 %); target and
  // details share the rest, details the larger part.
  const columns = $derived<Column<AuditEntry>[]>([
    { key: 'time', label: t('common.label.time'), width: '1%', cell: timeCell, value: (e) => e.time },
    { key: 'action', label: t('system.audit.action'), width: '1%', cell: actionCell, value: (e) => e.action },
    { key: 'target', label: t('system.audit.target'), mono: true, truncate: true, width: '30%', value: (e) => e.target },
    { key: 'user', label: t('system.audit.user'), width: '1%', cell: userCell, value: (e) => e.username },
    { key: 'ip', label: t('system.audit.ip'), width: '1%', cell: ipCell, value: (e) => e.ip },
    {
      key: 'details',
      label: t('common.label.details'),
      mono: true,
      truncate: true,
      width: '70%',
      value: (e) => summarizeDetails(e.details),
    },
  ])
</script>

{#snippet timeCell(e: AuditEntry)}
  <span class="nowrap" title={formatDateTime(e.time, true)}>{formatDateTimeShort(e.time, true)}</span>
{/snippet}

{#snippet userCell(e: AuditEntry)}
  <span class="nowrap">{e.username || '–'}</span>
{/snippet}

{#snippet ipCell(e: AuditEntry)}
  <span class="mono nowrap">{e.ip || '–'}</span>
{/snippet}

{#snippet actionCell(e: AuditEntry)}
  <span class="action">
    <span class="mono nowrap">{e.action}</span>
    {#if isFailure(e.action)}<Badge tone="warn">{t('system.audit.failed')}</Badge>{/if}
  </span>
{/snippet}

<div class="page">
  {#if !session.canOperate}
    <Notice tone="info">{t('system.audit.adminOnly')}</Notice>
  {:else}
    <Panel title={t('system.audit.title')} description={t('system.audit.description')} flush>
      {#snippet actions()}
        <IconButton
          icon="refresh"
          label={t('common.action.refresh')}
          loading={log.loading}
          onclick={() => log.refresh()}
        />
      {/snippet}
      <div class="toolbar pad">
        <div class="search">
          <Field label={t('system.audit.search')} hideLabel>
            <Input
              type="search"
              icon="search"
              bind:value={search}
              oninput={(ev) => typed((ev.currentTarget as HTMLInputElement).value)}
              placeholder={t('system.audit.searchPlaceholder')}
              maxlength={MAX_SEARCH}
              autocomplete="off"
            />
          </Field>
        </div>
        {#if log.data}
          <p class="small muted" aria-live="polite">{tn('system.audit.count', log.data.total)}</p>
        {/if}
      </div>
      <Table
        {columns}
        rows={log.data?.items}
        key={(e) => e.id}
        loading={log.loading}
        error={log.error ? errorText(log.error) : undefined}
        onretry={() => log.refresh()}
        onrowclick={(e) => select(e)}
        selected={entryId}
        caption={t('system.audit.title')}
        compact
      >
        {#snippet empty()}
          {#if q}
            <EmptyState compact title={t('system.audit.noMatch', { q })} text={t('system.audit.noMatchText')} />
          {:else}
            <EmptyState compact icon="document" title={t('system.audit.emptyTitle')} text={t('system.audit.emptyText')} />
          {/if}
        {/snippet}
      </Table>
      {#if log.data && log.data.total > 0}
        <Pager
          total={log.data.total}
          {limit}
          {offset}
          limits={LIMITS}
          onchange={(o) => router.setQuery({ offset: o, entry: null })}
          onlimit={(l) => router.setQuery({ limit: l === DEFAULT_LIMIT ? null : l, offset: null, entry: null })}
        />
      {/if}
    </Panel>
  {/if}
</div>

<SidePanel
  open={!!entryId}
  title={selected?.action ?? t('system.audit.entry')}
  subtitle={selected ? formatDateTime(selected.time, true) : undefined}
  onclose={() => select(undefined)}
>
  {#if selected}
    <div class="stack">
      <KeyValue
        items={[
          { label: t('common.label.time'), value: `${formatDateTime(selected.time, true)} (${formatRelative(selected.time)})` },
          { label: t('system.audit.action'), value: selected.action, mono: true },
          { label: t('system.audit.target'), value: selected.target, mono: true },
          { label: t('system.audit.user'), value: selected.username },
          { label: t('system.audit.ip'), value: selected.ip, mono: true },
        ]}
      />
      <div class="stack-sm">
        <div class="details-head">
          <h3>{t('common.label.details')}</h3>
          {#if selected.details}<CopyButton text={prettyDetails(selected.details)} />{/if}
        </div>
        {#if selected.details}
          <pre class="details mono">{prettyDetails(selected.details)}</pre>
        {:else}
          <p class="small muted">{t('system.audit.noDetails')}</p>
        {/if}
      </div>
    </div>
  {:else if log.loaded}
    <p class="small muted">{t('system.audit.entryGone')}</p>
  {/if}
</SidePanel>

<style>
  .pad {
    padding: 0 var(--sp-4) var(--sp-3);
    justify-content: space-between;
    align-items: center;
  }
  .search {
    flex: 1 1 260px;
    max-width: 420px;
    min-width: 0;
  }
  .action {
    display: inline-flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .details-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: var(--sp-2);
  }
  .details {
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    font-size: var(--fs-sm);
    max-height: 60vh;
    overflow: auto;
  }
</style>
