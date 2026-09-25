<!--
  @component
  Cache › Cache settings: turning the download cache on (after a check of address,
  storage, services and port) and off; the cache address and DNS answer
  lifetime; clients allowed to force a refresh; retention and free space;
  slices and parallel downloads; the download service list. Edits of both
  sections (downloadCache, cache) are saved together from the bar at the bottom.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { appStatus } from '$lib/status.svelte'
  import {
    Button,
    Chip,
    Dialog,
    Field,
    Input,
    Notice,
    Panel,
    Select,
    Skeleton,
    Toggle,
    confirm,
    toast,
    type SelectOption,
  } from '$lib/ui'
  import EnableChecks from './settings/EnableChecks.svelte'
  import ListField from './settings/ListField.svelte'
  import { formatBinary, isIPOrCIDR, isRFC1918, isULA } from './shared/util'

  const dl = settingsForm('downloadCache')
  const cache = settingsForm('cache')
  const ips = resource((signal) => api.dns.cacheIps({ signal }), { interval: 30_000 })

  const readOnly = $derived(!session.isAdmin)
  const loaded = $derived(!!dl.draft && !!cache.draft)
  const loadError = $derived(dl.loadError ?? cache.loadError)
  const dirty = $derived(dl.dirty || cache.dirty)
  const saving = $derived(dl.saving || cache.saving)
  const enabled = $derived(!!dl.saved?.enabled)
  const activeSliceSize = $derived(appStatus.overview.data?.store.sliceSize ?? 0)

  const GB = 1e9
  const SLICE_SIZES = [18, 19, 20, 21, 22, 23, 24, 25, 26].map((e) => 2 ** e) // 256 KiB … 64 MiB
  const sliceOptions: SelectOption[] = SLICE_SIZES.map((b) => ({ value: String(b), label: formatBinary(b) }))

  // ---- number helpers for bind:value getter/setter pairs

  function toNumber(v: unknown): number {
    const n = typeof v === 'number' ? v : Number(v)
    return Number.isFinite(n) ? n : 0
  }

  function toGB(bytes: number): number {
    return Math.round((bytes / GB) * 100) / 100
  }

  function fromGB(v: unknown): number {
    return Math.max(0, Math.round(toNumber(v) * GB))
  }

  function between(v: number, min: number, max: number): boolean {
    return Number.isInteger(v) && v >= min && v <= max
  }

  // ---- client-side checks (the server validates again)

  const problems = $derived.by(() => {
    const p: Record<string, string | undefined> = {}
    const l = dl.draft
    const c = cache.draft
    if (l) {
      const bad4 = l.cacheIpv4.filter((x) => !isRFC1918(x))
      if (bad4.length) p.cacheIpv4 = t('cache.settings.ipv4Invalid', { list: bad4.join(', ') })
      const bad6 = l.cacheIpv6.filter((x) => !isULA(x))
      if (bad6.length) p.cacheIpv6 = t('cache.settings.ipv6Invalid', { list: bad6.join(', ') })
      const badClients = l.nocacheClients.filter((x) => !isIPOrCIDR(x))
      if (badClients.length) p.nocacheClients = t('cache.settings.clientsInvalid', { list: badClients.join(', ') })
      if (!between(l.dnsTtl, 1, 86400)) p.dnsTtl = t('cache.settings.range', { min: '1', max: '86400' })
      if (!/^https:\/\/[^\s/]+/i.test(l.domainsSource.trim())) p.domainsSource = t('cache.settings.httpsUrl')
      if (!between(l.updateIntervalHours, 0, 720)) p.updateIntervalHours = t('cache.settings.range', { min: '0', max: '720' })
    }
    if (c) {
      if (!between(c.maxAgeDays, 1, 3650)) p.maxAgeDays = t('cache.settings.range', { min: '1', max: '3650' })
      if (c.maxSizeBytes < 0) p.maxSizeBytes = t('cache.settings.notNegative')
      if (c.minFreeBytes < 0) p.minFreeBytes = t('cache.settings.notNegative')
      if (!between(c.readAheadSlices, 0, 16)) p.readAheadSlices = t('cache.settings.range', { min: '0', max: '16' })
      if (!between(c.maxConcurrentFills, 1, 1024)) p.maxConcurrentFills = t('cache.settings.range', { min: '1', max: '1024' })
      else if (c.maxConcurrentFills * c.sliceSizeBytes > 2 ** 30) p.maxConcurrentFills = t('cache.settings.fillsMemory')
      if (!between(c.maxFillsPerClient, 1, Math.max(1, c.maxConcurrentFills)))
        p.maxFillsPerClient = t('cache.settings.perClientRange', { max: String(c.maxConcurrentFills) })
    }
    return p
  })
  const valid = $derived(Object.values(problems).every((v) => !v))

  function dlErr(key: string): string | undefined {
    return problems[key] ?? dl.error(key)
  }
  function cacheErr(key: string): string | undefined {
    return problems[key] ?? cache.error(key)
  }

  // ---- save bar

  async function save() {
    const okDl = await dl.save()
    const okCache = okDl && (await cache.save())
    if (okDl && okCache) {
      toast.success(t('common.state.saved'))
      void ips.refresh()
    }
  }

  function discard() {
    dl.revert()
    cache.revert()
  }

  const saveError = $derived(dl.errorMessage ?? cache.errorMessage)

  // ---- turning the download cache on and off

  let enableOpen = $state(false)
  let switching = $state(false)

  async function setEnabled(on: boolean): Promise<boolean> {
    if (!dl.draft || dl.dirty) return false
    switching = true
    try {
      dl.draft.enabled = on
      if (!(await dl.save())) {
        const message = dl.errorMessage ?? t('common.error.generic')
        dl.revert() // nothing else was edited (the switch is disabled while there are changes)
        toast.error(message)
        return false
      }
      toast.success(on ? t('cache.settings.turnedOn') : t('cache.settings.turnedOff'))
      void ips.refresh()
      return true
    } finally {
      switching = false
    }
  }

  async function turnOff() {
    const ok = await confirm({
      title: t('cache.settings.turnOffTitle'),
      message: t('cache.settings.turnOffText'),
      confirmLabel: t('cache.settings.turnOff'),
    })
    if (ok) await setEnabled(false)
  }

  async function turnOn() {
    if (await setEnabled(true)) enableOpen = false
  }

  const autoAddress = $derived(ips.data?.auto ? ips.data.ipv4.join(', ') : '')
</script>

<div class="page">
  {#if readOnly}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  {#if loadError && !loaded}
    <Notice tone="fail" title={t('cache.settings.loadError')}>
      {errorText(loadError)}
      {#snippet actions()}
        <Button
          size="sm"
          icon="refresh"
          onclick={() => {
            void dl.load()
            void cache.load()
          }}>{t('common.action.retry')}</Button
        >
      {/snippet}
    </Notice>
  {:else if !dl.draft || !cache.draft}
    <Skeleton height="120px" />
    <Skeleton height="200px" />
  {:else}
    {@const l = dl.draft}
    {@const c = cache.draft}

    <Panel title={t('cache.settings.downloadCacheTitle')}>
      {#snippet actions()}
        {#if enabled}
          <Button icon="power" loading={switching} disabled={readOnly || dl.dirty} onclick={turnOff}>{t('cache.settings.turnOff')}</Button>
        {:else}
          <Button variant="primary" icon="power" disabled={readOnly || dl.dirty} onclick={() => (enableOpen = true)}>
            {t('cache.settings.turnOn')}
          </Button>
        {/if}
      {/snippet}
      <div class="stack">
        <p class="row">
          {#if enabled}
            <Chip tone="ok" label={t('cache.settings.isOn')} />
          {:else}
            <Chip tone="neutral" label={t('cache.settings.isOff')} />
          {/if}
          <span class="muted">{enabled ? t('cache.settings.onText') : t('cache.settings.offText')}</span>
        </p>
        {#if enabled && ips.data && !ips.data.ready}
          <Notice tone="fail" title={t('cache.settings.notReady')}>{ips.data.reason ?? ''}</Notice>
        {:else if enabled && ips.data?.ready}
          <p class="small">
            {t('cache.settings.answering')}
            {#each ips.data.ipv4 as ip (ip)}<span class="ip mono">{ip}</span>{/each}
            {#if ips.data.warning ?? ips.data.reason}<span class="warn"> · {ips.data.warning ?? ips.data.reason}</span>{/if}
          </p>
        {/if}
        {#if dl.dirty}<p class="subtle small">{t('cache.settings.saveFirst')}</p>{/if}
      </div>
    </Panel>

    <Panel title={t('cache.settings.addressTitle')} description={t('cache.settings.addressText')}>
      <div class="form">
        <ListField
          bind:values={l.cacheIpv4}
          label={t('cache.settings.ipv4')}
          help={autoAddress ? t('cache.settings.ipv4HelpAuto', { ip: autoAddress }) : t('cache.settings.ipv4Help')}
          error={dlErr('cacheIpv4')}
          placeholder="192.168.1.10"
          rows={2}
          disabled={readOnly}
        />
        <ListField
          bind:values={l.cacheIpv6}
          label={t('cache.settings.ipv6')}
          help={t('cache.settings.ipv6Help')}
          error={dlErr('cacheIpv6')}
          placeholder="fd00::10"
          rows={2}
          disabled={readOnly}
        />
        <Field label={t('cache.settings.dnsTtl')} help={t('cache.settings.dnsTtlHelp')} error={dlErr('dnsTtl')}>
          <div class="num-field">
            <Input type="number" min={1} max={86400} disabled={readOnly} bind:value={() => l.dnsTtl, (v) => (l.dnsTtl = toNumber(v))} />
            <span class="unit">{t('cache.settings.seconds')}</span>
          </div>
        </Field>
      </div>
    </Panel>

    <Panel title={t('cache.settings.clientsTitle')}>
      <div class="form">
        <ListField
          bind:values={l.nocacheClients}
          label={t('cache.settings.nocache')}
          help={t('cache.settings.nocacheHelp')}
          error={dlErr('nocacheClients')}
          placeholder="192.168.1.50"
          disabled={readOnly}
        />
        <Toggle
          label={t('cache.settings.privateUpstreams')}
          description={t('cache.settings.privateUpstreamsHelp')}
          disabled={readOnly}
          bind:checked={l.allowPrivateUpstreams}
        />
        {#if l.allowPrivateUpstreams}
          <Notice tone="fail" title={t('cache.settings.privateUpstreamsWarnTitle')}>{t('cache.settings.privateUpstreamsWarn')}</Notice>
        {/if}
      </div>
    </Panel>

    <Panel title={t('cache.settings.retentionTitle')} description={t('cache.settings.retentionText')}>
      <div class="form">
        <Field label={t('cache.settings.maxAge')} help={t('cache.settings.maxAgeHelp')} error={cacheErr('maxAgeDays')}>
          <div class="num-field">
            <Input type="number" min={1} max={3650} disabled={readOnly} bind:value={() => c.maxAgeDays, (v) => (c.maxAgeDays = toNumber(v))} />
            <span class="unit">{t('cache.settings.days')}</span>
          </div>
        </Field>
        <Field label={t('cache.settings.maxSize')} help={t('cache.settings.maxSizeHelp')} error={cacheErr('maxSizeBytes')}>
          <div class="num-field">
            <Input
              type="number"
              min={0}
              step="any"
              disabled={readOnly}
              bind:value={() => toGB(c.maxSizeBytes), (v) => (c.maxSizeBytes = fromGB(v))}
            />
            <span class="unit">GB</span>
          </div>
        </Field>
        <Field
          label={t('cache.settings.minFree')}
          help={t('cache.settings.minFreeHelp', { size: formatBytes(appStatus.overview.data?.store.minFreeBytes) })}
          error={cacheErr('minFreeBytes')}
        >
          <div class="num-field">
            <Input
              type="number"
              min={0}
              step="any"
              disabled={readOnly}
              bind:value={() => toGB(c.minFreeBytes), (v) => (c.minFreeBytes = fromGB(v))}
            />
            <span class="unit">GB</span>
          </div>
        </Field>
      </div>
    </Panel>

    <Panel title={t('cache.settings.performanceTitle')} description={t('cache.settings.performanceText')}>
      <div class="form">
        <Field
          label={t('cache.settings.sliceSize')}
          help={activeSliceSize
            ? t('cache.settings.sliceSizeHelpActive', { size: formatBinary(activeSliceSize) })
            : t('cache.settings.sliceSizeHelp')}
          error={cacheErr('sliceSizeBytes')}
        >
          <div class="num-field">
            <Select
              options={sliceOptions}
              disabled={readOnly}
              bind:value={() => String(c.sliceSizeBytes), (v) => (c.sliceSizeBytes = toNumber(v))}
            />
          </div>
        </Field>
        <Field
          label={t('cache.settings.fills')}
          help={t('cache.settings.fillsHelp', { memory: formatBinary(Math.max(1, c.maxConcurrentFills) * c.sliceSizeBytes) })}
          error={cacheErr('maxConcurrentFills')}
        >
          <div class="num-field">
            <Input
              type="number"
              min={1}
              max={1024}
              disabled={readOnly}
              bind:value={() => c.maxConcurrentFills, (v) => (c.maxConcurrentFills = toNumber(v))}
            />
          </div>
        </Field>
        <Field label={t('cache.settings.fillsPerClient')} help={t('cache.settings.fillsPerClientHelp')} error={cacheErr('maxFillsPerClient')}>
          <div class="num-field">
            <Input
              type="number"
              min={1}
              max={1024}
              disabled={readOnly}
              bind:value={() => c.maxFillsPerClient, (v) => (c.maxFillsPerClient = toNumber(v))}
            />
          </div>
        </Field>
        <Field label={t('cache.settings.readAhead')} help={t('cache.settings.readAheadHelp')} error={cacheErr('readAheadSlices')}>
          <div class="num-field">
            <Input
              type="number"
              min={0}
              max={16}
              disabled={readOnly}
              bind:value={() => c.readAheadSlices, (v) => (c.readAheadSlices = toNumber(v))}
            />
          </div>
        </Field>
      </div>
    </Panel>

    <Panel title={t('cache.settings.sourceTitle')} description={t('cache.settings.sourceText')}>
      <div class="form">
        <Field label={t('cache.source.url')} help={t('cache.settings.sourceUrlHelp')} error={dlErr('domainsSource')}>
          <Input type="url" mono disabled={readOnly} bind:value={l.domainsSource} />
        </Field>
        <Field label={t('cache.settings.updateInterval')} help={t('cache.settings.updateIntervalHelp')} error={dlErr('updateIntervalHours')}>
          <div class="num-field">
            <Input
              type="number"
              min={0}
              max={720}
              disabled={readOnly}
              bind:value={() => l.updateIntervalHours, (v) => (l.updateIntervalHours = toNumber(v))}
            />
            <span class="unit">{t('cache.settings.hours')}</span>
          </div>
        </Field>
        <p class="small"><a href={href('/cache/services')}>{t('cache.settings.servicesLink')}</a></p>
      </div>
    </Panel>

    {#if dirty || saveError}
      <div class="savebar" role="region" aria-label={t('cache.settings.saveBar')}>
        <div class="savebar-inner">
          {#if saveError}
            <span class="err" role="alert">{saveError}</span>
          {:else}
            <span>{t('cache.settings.unsaved')}</span>
          {/if}
          <span class="spacer"></span>
          <Button variant="ghost" disabled={saving || !dirty} onclick={discard}>{t('cache.settings.discard')}</Button>
          <Button variant="primary" loading={saving} disabled={readOnly || !dirty || !valid} onclick={save}>{t('common.action.save')}</Button>
        </div>
      </div>
    {/if}
  {/if}
</div>

<Dialog bind:open={enableOpen} size="md" dismissible={!switching} title={t('cache.enable.title')}>
  <EnableChecks />
  {#snippet actions()}
    <Button variant="ghost" disabled={switching} onclick={() => (enableOpen = false)}>{t('common.action.cancel')}</Button>
    <Button variant="primary" icon="power" loading={switching} disabled={readOnly} onclick={turnOn}>{t('cache.settings.turnOn')}</Button>
  {/snippet}
</Dialog>

<style>
  .form {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    max-width: 560px;
  }
  .num-field {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    max-width: 240px;
  }
  .unit {
    color: var(--text-2);
    font-size: var(--fs-sm);
    white-space: nowrap;
  }
  .ip {
    margin-left: var(--sp-2);
    font-weight: 600;
  }
  .warn {
    color: var(--warning);
  }
  .savebar {
    position: sticky;
    bottom: 0;
    z-index: 2;
    margin: 0 calc(-1 * var(--sp-2));
    padding: var(--sp-2);
    background: var(--bg);
  }
  .savebar-inner {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-panel);
    background: var(--surface);
    font-weight: 600;
  }
  .err {
    color: var(--danger);
  }
</style>
