<!--
  @component
  Filter toolbar of the download tabs. Every filter lives in the URL
  (range, service, client, search, status, active, group); changing one goes
  back to the first page. An explicit window (?from=&to=, unix seconds, e.g.
  from the query log) replaces the range until it is cleared or a range is
  picked.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { RangePreset } from '$lib/api'
  import { formatDateTime, formatTime } from '$lib/format'
  import { router, type QueryPatch } from '$lib/router.svelte'
  import { Button, Checkbox, Field, IconButton, Select, TimeRangePicker, type SelectOption } from '$lib/ui'
  import type { ServiceCatalog } from '../shared/catalog.svelte'
  import FilterInput from '../shared/FilterInput.svelte'
  import { pickRange, timeWindow, validClientFilter, validSearch } from '../shared/util'

  interface Props {
    catalog: ServiceCatalog
    ranges: RangePreset[]
    rangeDefault: RangePreset
    /** Show the client filter (not for removed content). */
    client?: boolean
    searchLabel: string
    searchPlaceholder?: string
    /** Options of the status filter (hidden when absent). */
    statuses?: SelectOption[]
    statusLabel?: string
    /** "Only active downloads" (sessions). */
    active?: boolean
    /** The content-group filter (sessions). */
    group?: boolean
    /** Extra controls on the right (live toggle, refresh). */
    actions?: Snippet
  }

  let {
    catalog,
    ranges,
    rangeDefault,
    client = true,
    searchLabel,
    searchPlaceholder,
    statuses,
    statusLabel,
    active = false,
    group = false,
    actions,
  }: Props = $props()

  const range = $derived(pickRange(router.param('range'), ranges, rangeDefault))
  const win = $derived(timeWindow(router.param('from'), router.param('to')))
  const winText = $derived.by(() => {
    if (!win) return ''
    const from = win.from * 1000
    const to = win.to * 1000
    const sameDay = new Date(from).toDateString() === new Date(to).toDateString()
    return t('cache.filters.windowValue', { from: formatDateTime(from), to: sameDay ? formatTime(to) : formatDateTime(to) })
  })
  const groupKey = $derived(group ? router.param('group') : '')
  const filtered = $derived(
    ['client', 'service', 'search', 'status', 'active', 'group', 'from', 'to'].some((k) => router.param(k) !== ''),
  )

  function set(patch: QueryPatch) {
    router.setQuery({ ...patch, offset: null })
  }

  function clear() {
    set({ client: null, service: null, search: null, status: null, active: null, group: null, from: null, to: null })
  }
</script>

<div class="filters">
  <div class="top">
    <TimeRangePicker
      value={win ? null : range}
      options={ranges}
      onchange={(v) => set({ range: v === rangeDefault ? null : v, from: null, to: null })}
    />
    <span class="spacer"></span>
    {#if actions}<div class="actions">{@render actions()}</div>{/if}
  </div>
  <div class="toolbar">
    <div class="select">
      <Field label={t('common.label.service')}>
        <Select
          size="sm"
          value={router.param('service')}
          placeholder={t('cache.filters.allServices')}
          options={catalog.options}
          onchange={(e) => set({ service: e.currentTarget.value })}
        />
      </Field>
    </div>
    {#if client}
      <FilterInput
        label={t('common.label.client')}
        placeholder={t('cache.filters.clientPlaceholder')}
        mono
        icon="user"
        value={router.param('client')}
        valid={validClientFilter}
        invalidText={t('cache.filters.clientInvalid')}
        onchange={(v) => set({ client: v })}
      />
    {/if}
    <FilterInput
      label={searchLabel}
      placeholder={searchPlaceholder}
      value={router.param('search')}
      valid={validSearch}
      invalidText={t('cache.filters.tooShort')}
      onchange={(v) => set({ search: v })}
    />
    {#if statuses}
      <div class="select">
        <Field label={statusLabel ?? t('common.label.status')}>
          <Select
            size="sm"
            value={router.param('status')}
            placeholder={t('cache.filters.all')}
            options={statuses}
            onchange={(e) => set({ status: e.currentTarget.value })}
          />
        </Field>
      </div>
    {/if}
    {#if active}
      <div class="check">
        <Checkbox
          label={t('cache.filters.activeOnly')}
          checked={router.param('active') === 'true'}
          onchange={(on) => set({ active: on })}
        />
      </div>
    {/if}
    {#if filtered}
      <Button size="sm" variant="ghost" icon="close" onclick={clear}>{t('cache.filters.clear')}</Button>
    {/if}
  </div>
  {#if win}
    <p class="group">
      <span>{t('cache.filters.window')}</span>
      <span class="value">{winText}</span>
      <IconButton size="sm" icon="close" label={t('cache.filters.windowClear')} onclick={() => set({ from: null, to: null })} />
    </p>
  {/if}
  {#if groupKey}
    <p class="group">
      <span>{t('cache.filters.group')}</span>
      <span class="mono">{groupKey}</span>
      <IconButton size="sm" icon="close" label={t('cache.filters.groupClear')} onclick={() => set({ group: null })} />
    </p>
  {/if}
</div>

<style>
  .filters {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    min-width: 0;
  }
  .top {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
  .actions {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
  }
  .select {
    width: 180px;
    max-width: 100%;
  }
  .check {
    display: flex;
    align-items: center;
    min-height: var(--control-h-sm);
  }
  .group {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .group .mono,
  .group .value {
    color: var(--text);
  }
</style>
