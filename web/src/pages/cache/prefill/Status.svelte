<!--
  @component
  The prefill service: version, the cache it fills (heartbeat), the next
  scheduled run and the running job with its progress; starts a job
  ("Prefill now", or everything again with force) and cancels it.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type PrefillState, type PrefillStatus } from '$lib/api'
  import { formatBytes, formatDateTime, formatPercent, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, KeyValue, Meter, Notice, Panel, confirm, toast } from '$lib/ui'

  let { status, mode, onchanged }: { status: PrefillStatus; mode: PrefillState['mode']; onchanged: () => void } = $props()

  const job = $derived(status.job)
  // Like the service: a store counts with games chosen and, where it needs
  // one, a sign-in.
  const ready = $derived(
    (!!status.steam.account && status.settings.steam.apps.length > 0) ||
      status.settings.battleNet.products.length > 0 ||
      (!!status.epic.account && status.settings.epic.apps.length > 0),
  )
  const canStart = $derived(session.canOperate && !job && ready)

  let busy = $state(false)
  let checking = $state(false)

  async function start(force: boolean) {
    if (force) {
      const ok = await confirm({
        title: t('cache.prefill.status.forceTitle'),
        message: t('cache.prefill.status.forceText'),
        confirmLabel: t('cache.prefill.status.force'),
        danger: false,
      })
      if (!ok) return
    }
    busy = true
    try {
      await api.prefill.startJob(force)
      toast.success(t('cache.prefill.status.started'))
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      busy = false
    }
  }

  async function cancel() {
    busy = true
    try {
      await api.prefill.cancelJob()
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      busy = false
    }
  }

  // Switching the service off through the root helper (the sign-in and
  // the selection stay).
  async function switchOff() {
    const ok = await confirm({
      title: t('cache.prefill.status.offTitle'),
      message: t('cache.prefill.status.offText'),
      confirmLabel: t('cache.prefill.status.off'),
      action: () => api.prefill.setService(false),
    })
    if (ok) onchanged()
  }

  async function checkCache() {
    checking = true
    try {
      await api.prefill.state(true)
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      checking = false
    }
  }

  const cacheValue = $derived(
    status.cache.checkedAt
      ? status.cache.ok
        ? t('cache.prefill.status.cacheOk', { url: status.cache.url, time: formatRelative(status.cache.checkedAt) })
        : status.cache.url
      : t('cache.prefill.status.cacheUnchecked', { url: status.cache.url }),
  )
</script>

<Panel title={t('cache.prefill.title')} description={t('cache.prefill.description')}>
  {#snippet actions()}
    {#if job}
      <Button icon="close" variant="danger" loading={busy} disabled={!session.canOperate} onclick={cancel}>{t('cache.prefill.status.cancel')}</Button>
    {:else}
      <Button icon="download" variant="primary" loading={busy} disabled={!canStart} onclick={() => start(false)}>{t('cache.prefill.status.start')}</Button>
      <Button icon="refresh" disabled={!canStart || busy} onclick={() => start(true)}>{t('cache.prefill.status.force')}</Button>
      {#if mode === 'helper'}
        <Button icon="power" variant="ghost" disabled={!session.isAdmin} onclick={switchOff}>{t('cache.prefill.status.off')}</Button>
      {/if}
    {/if}
  {/snippet}
  <div class="stack">
    <KeyValue
      items={[
        { label: t('cache.prefill.status.service'), value: t('cache.prefill.status.serviceValue', { version: status.version, time: formatRelative(status.started) }) },
        { label: t('cache.prefill.status.cache'), value: cacheValue, mono: !status.cache.checkedAt || !status.cache.ok },
        {
          label: t('cache.prefill.status.next'),
          value: status.nextRun ? formatDateTime(status.nextRun) : t('cache.prefill.status.noSchedule'),
        },
      ]}
    />
    {#if status.cache.checkedAt && !status.cache.ok}
      <Notice tone="fail" title={t('cache.prefill.status.cacheFailed')}>
        {status.cache.error}
        {#snippet actions()}
          <Button size="sm" loading={checking} onclick={checkCache}>{t('cache.prefill.status.checkCache')}</Button>
        {/snippet}
      </Notice>
    {/if}
    {#if !job && !ready}
      <p class="muted small">{t('cache.prefill.status.nothingSelected')}</p>
    {/if}
    {#if job}
      <div class="job" role="status">
        <div class="row">
          <Chip size="sm" tone="info" label={t(`cache.prefill.trigger.${job.trigger}`)} />
          <span class="strong">{job.current || t('cache.prefill.status.preparing')}</span>
        </div>
        {#if job.total > 0}
          <Meter
            label={t('cache.prefill.status.progress')}
            max={Math.max(job.total, job.bytes)}
            segments={[
              { label: t('cache.prefill.status.fromCache'), value: job.fromCache, text: formatBytes(job.fromCache), tone: 'ok' },
              { label: t('cache.prefill.status.fromInternet'), value: Math.max(job.bytes - job.fromCache, 0), text: formatBytes(Math.max(job.bytes - job.fromCache, 0)), pair: 'blue' },
            ]}
            rest={{ label: t('cache.prefill.status.remaining'), text: formatBytes(Math.max(job.total - job.bytes, 0)) }}
          />
          <p class="muted small">
            {t('cache.prefill.status.done', {
              done: formatBytes(job.bytes),
              total: formatBytes(job.total),
              percent: formatPercent(Math.min(job.bytes / job.total, 1), 0),
            })}
          </p>
        {/if}
      </div>
    {/if}
  </div>
</Panel>

<style>
  .job {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .strong {
    font-weight: 600;
  }
</style>
