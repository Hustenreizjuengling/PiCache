<!--
  @component
  Details of one download service in a side panel: on/off, notes and
  warnings, traffic of the last 24 hours, all host names, the editor for
  extra host names (built-in services) and edit/delete for custom services.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import { api, resource, type LanCacheService, type ServiceStat } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { formatBytes, formatNumber } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import {
    Button,
    Field,
    Input,
    KeyValue,
    Notice,
    SidePanel,
    Textarea,
    Toggle,
    confirm,
    toast,
  } from '$lib/ui'
  import { formatHitRatio, mostlyHttps, parseList, sameList } from '../shared/util'
  import CustomServiceDialog from './CustomServiceDialog.svelte'

  interface Props {
    /** The row from the list (domains trimmed) until the full service is loaded. */
    row: LanCacheService
    stat: ServiceStat | undefined
    onchanged: (s: LanCacheService) => void
    ondeleted: (id: string) => void
    onclose: () => void
  }

  let { row, stat, onchanged, ondeleted, onclose }: Props = $props()

  const DOMAIN_FILTER_FROM = 30

  // The parent re-creates this panel for another service ({#key}), so the id is fixed.
  const id = untrack(() => row.id)
  const full = resource((signal) => api.lancache.service(id, { signal }))
  const s = $derived(full.data ?? row)

  // ---- on/off

  let toggling = $state(false)
  async function setEnabled(on: boolean) {
    toggling = true
    try {
      const updated = await api.lancache.setEnabled(s.id, on)
      full.set(updated)
      onchanged(updated)
      toast.success(on ? t('cache.services.enabled', { name: s.name }) : t('cache.services.disabled', { name: s.name }))
    } catch (err) {
      toast.error(err)
    } finally {
      toggling = false
    }
  }

  // ---- host names

  const sourceDomains = $derived(s.custom ? [] : s.domains.filter((d) => !s.extraDomains.includes(d)))
  let domainFilter = $state('')
  const shownDomains = $derived.by(() => {
    const q = domainFilter.trim().toLowerCase()
    const list = s.custom ? s.domains : sourceDomains
    return q ? list.filter((d) => d.includes(q)) : list
  })
  const trimmed = $derived(!full.data && row.domainCount > row.domains.length)

  let extraText = $state('')
  let extraLoaded = false
  let extraSaving = $state(false)
  let extraError = $state<unknown>(undefined)
  $effect(() => {
    const d = full.data
    if (d && !untrack(() => extraLoaded)) {
      extraText = d.extraDomains.join('\n')
      extraLoaded = true
    }
  })
  const extraList = $derived(parseList(extraText))
  const extraDirty = $derived(!!full.data && !sameList(extraList, full.data.extraDomains))

  async function saveExtra() {
    extraSaving = true
    extraError = undefined
    try {
      const updated = await api.lancache.setExtraDomains(s.id, extraList)
      full.set(updated)
      extraText = updated.extraDomains.join('\n')
      onchanged(updated)
      toast.success(t('cache.services.extraSaved'))
    } catch (err) {
      extraError = err
    } finally {
      extraSaving = false
    }
  }

  // ---- custom services

  let editing = $state(false)

  async function remove() {
    const ok = await confirm({
      title: t('cache.services.deleteTitle', { name: s.name }),
      message: t('cache.services.deleteText'),
      confirmLabel: t('cache.services.delete'),
      action: () => api.lancache.deleteService(s.id),
    })
    if (!ok) return
    toast.success(t('cache.services.deleted', { name: s.name }))
    ondeleted(s.id)
  }

  const https = $derived(stat ? mostlyHttps(stat.bytesSent, stat.sniBytes) : false)
</script>

<SidePanel
  size="lg"
  bind:open={() => true, (v) => !v && onclose()}
  title={s.name}
  subtitle={s.custom ? t('cache.services.customSubtitle', { id: s.id }) : s.id}
>
  <div class="stack">
    <Toggle
      label={t('cache.services.cacheThis')}
      description={s.enabled ? t('cache.services.onText') : t('cache.services.offText')}
      checked={s.enabled}
      disabled={!session.isAdmin || toggling}
      onchange={setEnabled}
    />

    {#if s.description}<p>{s.description}</p>{/if}
    {#if s.notes}
      <Notice tone="info" title={t('cache.services.notesTitle')}>{s.notes}</Notice>
    {/if}
    {#if s.mixedContent}
      <Notice tone="info" title={t('cache.services.mixedTitle')}>{t('cache.services.mixedText')}</Notice>
    {/if}
    {#if https && stat}
      <Notice tone="warn" title={t('cache.services.httpsTitle')}>
        {t('cache.services.httpsText', { https: formatBytes(stat.sniBytes), http: formatBytes(stat.bytesSent) })}
      </Notice>
    {/if}

    <KeyValue
      items={[
        { label: t('cache.services.hostNames'), value: formatNumber(s.domainCount) },
        { label: t('cache.services.yourHostNames'), value: formatNumber(s.extraDomains.length) },
        { label: t('cache.services.downloaded24h'), value: formatBytes(stat?.bytesSent ?? 0) },
        { label: t('cache.col.fromCache'), value: stat ? formatHitRatio(stat.bytesHit, stat.bytesWan) : '–' },
        { label: t('cache.services.https24h'), value: formatBytes(stat?.sniBytes ?? 0) },
      ]}
    />
    <p class="links">
      <a href={href('/cache/library', { service: s.id })}>{t('cache.services.openLibrary')}</a>
      <a href={href('/cache/downloads', { service: s.id })}>{t('cache.services.openDownloads')}</a>
    </p>

    <section class="stack-sm">
      <h3>{s.custom ? t('cache.services.customDomainsTitle') : t('cache.services.sourceDomainsTitle')}</h3>
      {#if !s.custom}<p class="muted small">{t('cache.services.sourceDomainsText')}</p>{/if}
      {#if (s.custom ? s.domains : sourceDomains).length > DOMAIN_FILTER_FROM}
        <div class="filter">
          <Input
            type="search"
            size="sm"
            icon="search"
            mono
            aria-label={t('cache.services.filterDomains')}
            placeholder={t('cache.services.filterDomains')}
            bind:value={domainFilter}
          />
        </div>
      {/if}
      {#if full.error && !full.data}
        <Notice tone="fail">{errorText(full.error)}</Notice>
      {/if}
      <ul class="domains" aria-label={t('cache.services.hostNames')}>
        {#each shownDomains as d (d)}
          <li class="mono">{d}</li>
        {:else}
          <li class="subtle">{t('cache.services.noDomains')}</li>
        {/each}
      </ul>
      {#if trimmed}<p class="subtle small">{t('cache.services.loadingAll')}</p>{/if}
    </section>

    {#if !s.custom}
      <section class="stack-sm">
        <h3>{t('cache.services.extraTitle')}</h3>
        <Field
          label={t('cache.services.extraLabel')}
          help={t('cache.services.extraHelp')}
          error={extraError ? (fieldError(extraError, 'extraDomains') ?? errorText(extraError)) : undefined}
        >
          <Textarea
            bind:value={extraText}
            rows={5}
            mono
            disabled={!session.isAdmin || !full.data}
            placeholder={'dl.example.com\n*.cdn.example.com'}
          />
        </Field>
        <div class="row">
          <Button variant="primary" size="sm" loading={extraSaving} disabled={!session.isAdmin || !extraDirty} onclick={saveExtra}>
            {t('cache.services.saveExtra')}
          </Button>
          {#if extraList.length > 0}
            <span class="subtle small">{tn('cache.services.extraCount', extraList.length)}</span>
          {/if}
        </div>
      </section>
    {/if}
  </div>

  {#snippet actions()}
    {#if s.custom}
      <Button icon="edit" disabled={!session.isAdmin || !full.data} onclick={() => (editing = true)}>
        {t('cache.services.editCustom')}
      </Button>
      <Button variant="danger" icon="trash" disabled={!session.isAdmin} onclick={remove}>{t('cache.services.delete')}</Button>
    {/if}
  {/snippet}
</SidePanel>

{#if s.custom && full.data}
  <CustomServiceDialog
    bind:open={editing}
    service={full.data}
    onsaved={(updated) => {
      full.set(updated)
      onchanged(updated)
    }}
  />
{/if}

<style>
  h3 {
    font-size: var(--fs-md);
    font-weight: 600;
  }
  .links {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2) var(--sp-4);
    font-size: var(--fs-sm);
  }
  .filter {
    max-width: 320px;
  }
  .domains {
    margin: 0;
    padding: var(--sp-2) var(--sp-3);
    list-style: none;
    max-height: 280px;
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    font-size: var(--fs-sm);
    line-height: 1.7;
  }
  .domains li {
    overflow-wrap: anywhere;
  }
</style>
