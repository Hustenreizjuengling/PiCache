<!--
  @component
  Query log toolbar: time range, client, domain, status, record type and
  upstream. Text filters apply after a short pause in typing (or Enter) once
  they are long enough for the server; everything lives in the URL.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { QueryStatus, RangePreset } from '$lib/api'
  import type { QueryPatch } from '$lib/router.svelte'
  import { Button, Field, Icon, Input, Select, TimeRangePicker } from '$lib/ui'
  import {
    DEFAULT_RANGE,
    hasFilters,
    LOG_RANGES,
    MIN_SEARCH,
    QTYPES,
    validClient,
    validDomain,
    type QueryFilters,
  } from './filters'
  import StatusFilter from './StatusFilter.svelte'

  interface Props {
    filters: QueryFilters
    /** Live mode: the time range does not apply. */
    live: boolean
    /** Upstreams offered by the upstream filter. */
    upstreams: readonly string[]
    /** Validation messages from the server, by filter. */
    errors?: { client?: string; domain?: string }
    onchange: (patch: QueryPatch) => void
  }

  let { filters, live, upstreams, errors = {}, onchange }: Props = $props()

  const DEBOUNCE = 450

  /** Narrow screens: filters are collapsed unless opened (open at first when some are set). */
  let expanded = $state(untrack(() => hasFilters(filters)))
  const active = $derived(
    [filters.client, filters.domain, filters.qtype, filters.upstream].filter(Boolean).length + (filters.status.length > 0 ? 1 : 0),
  )

  let clientText = $state(untrack(() => filters.client))
  let domainText = $state(untrack(() => filters.domain))
  let timer: ReturnType<typeof setTimeout> | undefined

  // Filters changed from outside (links, the details panel, "Clear filters").
  $effect(() => {
    const c = filters.client
    untrack(() => {
      if (c !== clientText.trim()) clientText = c
    })
  })
  $effect(() => {
    const d = filters.domain
    untrack(() => {
      if (d !== domainText.trim()) domainText = d
    })
  })
  $effect(() => () => clearTimeout(timer))

  const clientHint = $derived(validClient(clientText) ? undefined : t('dns.queryLog.filter.tooShort', { min: MIN_SEARCH }))
  const domainHint = $derived(validDomain(domainText) ? undefined : t('dns.queryLog.filter.tooShort', { min: MIN_SEARCH }))

  function applyText() {
    clearTimeout(timer)
    const patch: QueryPatch = {}
    const c = clientText.trim()
    const d = domainText.trim()
    if (validClient(c) && c !== filters.client) patch.client = c
    if (validDomain(d) && d !== filters.domain) patch.domain = d
    if (Object.keys(patch).length > 0) onchange(patch)
  }

  function schedule() {
    clearTimeout(timer)
    timer = setTimeout(applyText, DEBOUNCE)
  }

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Enter') {
      e.preventDefault()
      applyText()
    }
  }

  const typeOptions = $derived([
    { value: '', label: t('dns.queryLog.filter.allTypes') },
    ...[...QTYPES, ...(filters.qtype && !QTYPES.includes(filters.qtype) ? [filters.qtype] : [])].map((q) => ({
      value: q,
      label: q,
    })),
  ])

  const upstreamOptions = $derived([
    { value: '', label: t('dns.queryLog.filter.allUpstreams') },
    ...[...new Set([...upstreams, ...(filters.upstream ? [filters.upstream] : [])])].map((u) => ({ value: u, label: u })),
  ])

  function clearAll() {
    clearTimeout(timer)
    clientText = ''
    domainText = ''
    onchange({ client: null, domain: null, status: null, qtype: null, upstream: null })
  }
</script>

<div class="filters">
  {#if !live}
    <div class="range">
      <TimeRangePicker
        value={filters.range}
        options={LOG_RANGES}
        label={t('dns.queryLog.filter.range')}
        onchange={(r: RangePreset) => onchange({ range: r === DEFAULT_RANGE ? null : r })}
      />
    </div>
  {/if}
  <button type="button" class="toggle" aria-expanded={expanded} aria-controls="ql-filters" onclick={() => (expanded = !expanded)}>
    <Icon name="filter" size={16} />
    {active > 0 ? t('dns.queryLog.filter.toggleCount', { count: active }) : t('dns.queryLog.filter.toggle')}
    <Icon name={expanded ? 'chevron-up' : 'chevron-down'} size={16} />
  </button>
  <div id="ql-filters" class={['toolbar', expanded && 'open']}>
    <div class="f text">
      <Field label={t('common.label.domain')} error={domainHint ?? errors.domain}>
        <Input
          type="search"
          size="sm"
          icon="search"
          bind:value={domainText}
          placeholder={t('dns.queryLog.filter.domainPlaceholder')}
          title={t('dns.queryLog.filter.domainTitle')}
          maxlength={256}
          oninput={schedule}
          onkeydown={onKey}
          onblur={applyText}
        />
      </Field>
    </div>
    <div class="f text">
      <Field label={t('common.label.client')} error={clientHint ?? errors.client}>
        <Input
          type="search"
          size="sm"
          bind:value={clientText}
          placeholder={t('dns.queryLog.filter.clientPlaceholder')}
          maxlength={256}
          oninput={schedule}
          onkeydown={onKey}
          onblur={applyText}
        />
      </Field>
    </div>
    <div class="f">
      <Field label={t('common.label.status')}>
        <StatusFilter value={filters.status} onchange={(s: QueryStatus[]) => onchange({ status: s })} />
      </Field>
    </div>
    <div class="f small-f">
      <Field label={t('common.label.type')}>
        <Select size="sm" value={filters.qtype} options={typeOptions} onchange={(e) => onchange({ qtype: e.currentTarget.value })} />
      </Field>
    </div>
    <div class="f">
      <Field label={t('dns.queryLog.upstream')}>
        <Select
          size="sm"
          value={filters.upstream}
          options={upstreamOptions}
          onchange={(e) => onchange({ upstream: e.currentTarget.value })}
        />
      </Field>
    </div>
    {#if hasFilters(filters)}
      <Button size="sm" variant="ghost" icon="close" onclick={clearAll}>{t('dns.queryLog.filter.clear')}</Button>
    {/if}
  </div>
</div>

<style>
  .filters {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    min-width: 0;
  }
  .range {
    display: flex;
  }
  .f {
    flex: 0 1 180px;
    min-width: 140px;
  }
  .f.text {
    flex: 1 1 200px;
    max-width: 320px;
  }
  .f.small-f {
    flex: 0 1 130px;
    min-width: 110px;
  }
  .toggle {
    display: none;
  }
  @media (max-width: 480px) {
    .toggle {
      display: inline-flex;
      align-items: center;
      gap: var(--sp-2);
      align-self: flex-start;
      padding: 4px 0;
      border: 0;
      background: none;
      color: var(--text);
      font: inherit;
      font-size: var(--fs-sm);
      font-weight: 600;
      cursor: pointer;
    }
    .toggle:focus-visible {
      outline: 2px solid var(--focus);
      outline-offset: 2px;
    }
    .toolbar:not(.open) {
      display: none;
    }
    .f,
    .f.text,
    .f.small-f {
      flex: 1 1 100%;
      max-width: none;
    }
  }
</style>
