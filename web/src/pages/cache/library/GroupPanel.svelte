<!--
  @component
  Details of one cached content group in a side panel: size and retention,
  the clients that downloaded it, its files, and the admin actions rename,
  pin and purge. Mounted while a group is selected (?group=&groupService=).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type CacheObject, type GroupClient, type ObjectSort } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatBytes, formatDate, formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import {
    Button,
    Chip,
    Field,
    Input,
    KeyValue,
    Menu,
    Notice,
    Pager,
    SidePanel,
    Skeleton,
    Table,
    Toggle,
    confirm,
    toast,
    type Column,
    type SortState,
  } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'

  interface Props {
    service: string
    groupKey: string
    /** Title until the details are loaded (the label from the list). */
    fallbackTitle: string
    catalog: ServiceCatalog
    /** Something changed (label, pin, purge): reload the list. */
    onchanged: () => void
    onclose: () => void
  }

  let { service, groupKey, fallbackTitle, catalog, onchanged, onclose }: Props = $props()

  const OBJECT_LIMIT = 25
  const MAX_LABEL = 128

  const detail = resource((signal) => api.cache.groupDetail(service, groupKey, { signal }))
  const g = $derived(detail.data?.group)

  let objSort = $state<SortState>({ key: 'lastAccess', desc: true })
  let objOffset = $state(0)
  const objects = resource((signal) =>
    api.cache.objects(
      {
        service,
        group: groupKey,
        sort: objSort.key as ObjectSort,
        desc: objSort.desc,
        limit: OBJECT_LIMIT,
        offset: objOffset,
      },
      { signal },
    ),
  )

  // ---- label

  let label = $state('')
  let labelLoaded = false
  let labelSaving = $state(false)
  let labelError = $state<unknown>(undefined)
  $effect(() => {
    const l = g?.label
    if (l !== undefined && !untrack(() => labelLoaded)) {
      label = l
      labelLoaded = true
    }
  })

  async function saveLabel(value: string) {
    labelSaving = true
    labelError = undefined
    try {
      await api.lancache.setLabel(groupKey, value)
      toast.success(value ? t('cache.library.labelSaved') : t('cache.library.labelReset'))
      labelLoaded = false
      await detail.refresh()
      onchanged()
    } catch (err) {
      labelError = err
    } finally {
      labelSaving = false
    }
  }

  // ---- pin, purge

  let pinning = $state(false)
  async function setPinned(on: boolean) {
    pinning = true
    try {
      await api.cache.pinGroup(service, groupKey, on)
      toast.success(on ? t('cache.library.pinned') : t('cache.library.unpinned'))
      await detail.refresh()
      void objects.refresh()
      onchanged()
    } catch (err) {
      toast.error(err)
      void detail.refresh()
    } finally {
      pinning = false
    }
  }

  async function purge() {
    const name = g?.label || fallbackTitle
    let freed = 0
    const ok = await confirm({
      title: t('cache.library.purgeTitle', { name }),
      message: tn('cache.library.purgeText', g?.objects ?? 0, { size: formatBytes(g?.cachedBytes), count: formatNumber(g?.objects ?? 0) }),
      confirmLabel: t('cache.library.purge'),
      action: async () => {
        freed = (await api.cache.deleteGroup(service, groupKey)).bytesFreed
      },
    })
    if (!ok) return
    toast.success(t('cache.library.purged', { name, size: formatBytes(freed) }))
    onchanged()
    onclose()
  }

  // ---- files

  async function pinObject(o: CacheObject, on: boolean) {
    try {
      await api.cache.pinObject(o.id, on)
      toast.success(on ? t('cache.library.filePinned') : t('cache.library.fileUnpinned'))
      void objects.refresh()
    } catch (err) {
      toast.error(err)
    }
  }

  async function deleteObject(o: CacheObject) {
    const ok = await confirm({
      title: t('cache.library.deleteFileTitle'),
      message: `${o.host}${o.path}`,
      confirmLabel: t('cache.library.deleteFile'),
      action: () => api.cache.deleteObject(o.id),
    })
    if (!ok) return
    toast.success(t('cache.library.fileDeleted'))
    void objects.refresh()
    void detail.refresh()
    onchanged()
  }

  const completeness = $derived(g && g.totalBytes > 0 ? Math.min(1, g.cachedBytes / g.totalBytes) : null)

  const clientColumns: Column<GroupClient>[] = $derived([
    { key: 'client', label: t('common.label.client'), cell: groupClientCell },
    { key: 'sessions', label: t('cache.col.sessions'), align: 'right', format: (c) => formatNumber(c.sessions) },
    { key: 'sent', label: t('cache.col.downloaded'), align: 'right', format: (c) => formatBytes(c.bytesSent) },
    { key: 'last', label: t('common.label.lastSeen'), align: 'right', format: (c) => formatRelative(c.lastSeen) },
  ])

  const objectColumns: Column<CacheObject>[] = $derived([
    { key: 'path', label: t('cache.col.file'), sortable: true, cell: pathCell },
    { key: 'size', label: t('common.label.size'), align: 'right', sortable: true, format: (o) => formatBytes(o.total) },
    { key: 'cached', label: t('cache.col.cached'), align: 'right', cell: cachedCell },
    { key: 'lastAccess', label: t('cache.col.lastAccess'), align: 'right', sortable: true, cell: accessCell },
    { key: 'hits', label: t('cache.col.hits'), align: 'right', format: (o) => formatNumber(o.hits) },
    { key: 'actions', label: t('common.label.actions'), align: 'right', cell: objectActions },
  ])
</script>

{#snippet groupClientCell(c: GroupClient)}
  <a href={href('/cache/downloads', { client: c.clientIp, group: groupKey, service, range: '90d' })}>
    {#if c.clientName}{c.clientName} <span class="subtle mono">{c.clientIp}</span>{:else}<span class="mono">{c.clientIp}</span>{/if}
  </a>
{/snippet}

{#snippet pathCell(o: CacheObject)}
  <span class="file">
    <span class="mono path" title={o.path}>{o.path}</span>
    <span class="subtle xsmall mono">{o.host}{#if o.pinned} · {t('cache.library.pinnedShort')}{/if}</span>
  </span>
{/snippet}

{#snippet cachedCell(o: CacheObject)}
  <span title={tn('cache.library.slices', o.slicesTotal, { cached: formatNumber(o.sliceCount), count: formatNumber(o.slicesTotal) })}>
    {formatBytes(o.cachedBytes)}
  </span>
{/snippet}

{#snippet accessCell(o: CacheObject)}
  <span title={formatDateTime(o.lastAccess)}>{formatRelative(o.lastAccess)}</span>
{/snippet}

{#snippet objectActions(o: CacheObject)}
  <Menu
    iconOnly
    icon="more"
    variant="ghost"
    size="sm"
    label={t('common.action.more')}
    disabled={!session.isAdmin}
    items={[
      o.pinned
        ? { label: t('cache.library.unpinFile'), icon: 'pin', onselect: () => pinObject(o, false) }
        : { label: t('cache.library.pinFile'), icon: 'pin', onselect: () => pinObject(o, true) },
      { separator: true },
      { label: t('cache.library.deleteFile'), icon: 'trash', danger: true, onselect: () => deleteObject(o) },
    ]}
  />
{/snippet}

<SidePanel
  size="lg"
  bind:open={() => true, (v) => !v && onclose()}
  title={g?.label || fallbackTitle}
  subtitle={`${catalog.name(service)} · ${groupKey}`}
>
  <div class="stack">
    {#if detail.error && !g}
      <Notice tone="fail" title={t('cache.library.detailError')}>{errorText(detail.error)}</Notice>
    {:else if !g}
      <Skeleton height="120px" />
    {:else}
      <div class="row">
        {#if g.pinned}<Chip tone="info" icon="pin" label={t('cache.library.pinnedChip')} />{/if}
        <Chip
          tone={completeness !== null && completeness >= 0.999 ? 'ok' : 'neutral'}
          label={completeness !== null && completeness >= 0.999
            ? t('cache.library.complete')
            : t('cache.library.partly', { share: formatPercent(completeness) })}
        />
      </div>

      <KeyValue
        items={[
          { label: t('common.label.service'), value: catalog.name(g.service) },
          { label: t('cache.detail.groupKey'), value: g.groupKey, mono: true },
          { label: t('cache.col.onDisk'), value: formatBytes(g.cachedBytes) },
          { label: t('cache.detail.fullSize'), value: formatBytes(g.totalBytes) },
          { label: t('cache.col.objects'), value: formatNumber(g.objects) },
          { label: t('cache.col.firstCached'), value: formatDateTime(g.firstCached) },
          { label: t('cache.col.lastAccess'), value: `${formatDateTime(g.lastAccess)} (${formatRelative(g.lastAccess)})` },
          {
            label: t('cache.col.expires'),
            value: g.pinned ? t('cache.library.neverPinned') : g.expiresAt ? formatDate(g.expiresAt) : t('common.state.never'),
          },
          { label: t('cache.col.served'), value: formatBytes(g.bytesServed) },
          { label: t('cache.col.hits'), value: formatNumber(g.hits) },
          { label: t('cache.col.clients'), value: formatNumber(g.clients) },
        ]}
      />

      <section class="stack-sm">
        <h3>{t('cache.library.labelTitle')}</h3>
        <form
          class="label-form"
          onsubmit={(e) => {
            e.preventDefault()
            void saveLabel(label.trim())
          }}
        >
          <Field
            label={t('cache.library.labelField')}
            help={t('cache.library.labelHelp')}
            error={labelError ? (fieldError(labelError, 'label') ?? errorText(labelError)) : undefined}
          >
            <Input bind:value={label} maxlength={MAX_LABEL} disabled={!session.isAdmin} />
          </Field>
          <div class="row">
            <Button type="submit" variant="primary" size="sm" loading={labelSaving} disabled={!session.isAdmin || !label.trim() || label.trim() === g.label}>
              {t('cache.library.saveLabel')}
            </Button>
            <!-- Only a name set by a user can be reset (older servers do not say: always offered). -->
            {#if g.userLabel ?? true}
              <Button size="sm" variant="ghost" disabled={!session.isAdmin || labelSaving} onclick={() => saveLabel('')}>
                {t('cache.library.resetLabel')}
              </Button>
            {/if}
          </div>
        </form>
      </section>

      <section class="stack-sm">
        <h3>{t('cache.library.keepTitle')}</h3>
        <Toggle
          label={t('cache.library.pinLabel')}
          description={t('cache.library.pinHelp')}
          checked={g.pinned}
          disabled={!session.isAdmin || pinning}
          onchange={setPinned}
        />
      </section>

      <section class="stack-sm">
        <h3>{t('cache.library.clientsTitle')}</h3>
        <div class="box">
          <Table
            compact
            caption={t('cache.library.clientsTitle')}
            rows={detail.data?.clients}
            key={(c) => c.clientIp}
            columns={clientColumns}
            emptyText={t('cache.library.noClients')}
          />
        </div>
      </section>

      <section class="stack-sm">
        <h3>{t('cache.library.filesTitle')}</h3>
        <div class="box">
          <Table
            compact
            caption={t('cache.library.filesTitle')}
            rows={objects.data?.items}
            key={(o) => o.id}
            columns={objectColumns}
            loading={objects.loading && !objects.loaded}
            error={objects.error ? errorText(objects.error) : undefined}
            onretry={() => objects.refresh()}
            skeletonRows={3}
            sort={objSort}
            onsort={(s) => {
              objSort = s
              objOffset = 0
            }}
            emptyText={t('cache.library.noFiles')}
          />
          {#if objects.data && objects.data.total > OBJECT_LIMIT}
            <Pager total={objects.data.total} limit={OBJECT_LIMIT} offset={objOffset} onchange={(o) => (objOffset = o)} />
          {/if}
        </div>
      </section>
    {/if}
  </div>

  {#snippet actions()}
    <Button icon="download" href={href('/cache/downloads', { group: groupKey, service, range: '90d' })}>
      {t('cache.library.showDownloads')}
    </Button>
    <Button variant="danger" icon="trash" disabled={!session.isAdmin || !g} onclick={purge}>{t('cache.library.purge')}</Button>
  {/snippet}
</SidePanel>

<style>
  h3 {
    font-size: var(--fs-md);
    font-weight: 600;
  }
  .label-form {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    max-width: 480px;
  }
  .box {
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    overflow: hidden;
  }
  .file {
    display: flex;
    flex-direction: column;
    min-width: 0;
    max-width: 44ch;
  }
  .path {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
</style>
