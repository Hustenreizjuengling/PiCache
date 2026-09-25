<!--
  @component
  Check the cache files (verify) or check and repair them (rebuild the index,
  delete damaged files). Runs in the background; progress is polled every
  2 seconds while it runs.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, isApiError, resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatDateTime, formatDuration, formatNumber, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, KeyValue, Notice, Panel, Spinner, confirm, toast } from '$lib/ui'
  import { spanMs } from '../shared/util'

  let { online }: { online: boolean } = $props()

  // Only while a store is online: the endpoint answers 503 otherwise.
  const verify = resource((signal) => (online ? api.cache.verifyState({ signal }) : Promise.resolve(undefined)), {
    interval: 30_000,
  })
  const v = $derived(verify.data)
  const running = $derived(!!v?.running)

  $effect(() => {
    if (!running) return
    const id = setInterval(() => void verify.refresh(), 2000)
    return () => clearInterval(id)
  })

  let starting = $state(false)
  async function start(repair: boolean) {
    if (repair) {
      const ok = await confirm({
        title: t('cache.verify.repairTitle'),
        message: t('cache.verify.repairText'),
        confirmLabel: t('cache.verify.repair'),
      })
      if (!ok) return
    }
    starting = true
    try {
      verify.set(await api.cache.verify(repair))
      toast.success(repair ? t('cache.verify.repairStarted') : t('cache.verify.started'))
    } catch (err) {
      toast.error(err)
    } finally {
      starting = false
    }
  }

  const unavailable = $derived(isApiError(verify.error, 'unavailable'))
</script>

<Panel title={t('cache.verify.title')} description={t('cache.verify.description')}>
  {#snippet actions()}
    <Button icon="check" loading={starting} disabled={!session.isAdmin || running || !online || unavailable} onclick={() => start(false)}>
      {t('cache.verify.check')}
    </Button>
    <Button icon="refresh" disabled={!session.isAdmin || running || starting || !online || unavailable} onclick={() => start(true)}>
      {t('cache.verify.repair')}
    </Button>
  {/snippet}

  {#if unavailable || !online}
    <p class="muted small">{t('cache.verify.unavailable')}</p>
  {:else if verify.error && !v}
    <Notice tone="fail">{errorText(verify.error)}</Notice>
  {:else if v}
    <div class="stack">
      {#if v.running}
        <p class="row status" role="status">
          <Spinner size={18} />
          <span>
            {v.repair ? t('cache.verify.repairing') : t('cache.verify.checking')}
            {tn('cache.verify.progress', v.progress.filesScanned, { count: formatNumber(v.progress.filesScanned), size: formatBytes(v.progress.bytesScanned) })}
          </span>
        </p>
      {:else if v.finishedAt}
        <p class="status">
          {t('cache.verify.last', { time: formatRelative(v.finishedAt) })}
        </p>
      {:else}
        <p class="muted small">{t('cache.verify.never')}</p>
      {/if}
      {#if v.error}<Notice tone="fail" title={t('cache.verify.failed')}>{v.error}</Notice>{/if}
      {#if v.startedAt}
        <KeyValue
          items={[
            { label: t('cache.verify.mode'), value: v.repair ? t('cache.verify.modeRepair') : t('cache.verify.modeCheck') },
            { label: t('cache.verify.startedAt'), value: formatDateTime(v.startedAt) },
            {
              label: t('cache.verify.duration'),
              value: formatDuration(spanMs(v.startedAt, v.finishedAt ?? new Date().toISOString())),
            },
            { label: t('cache.verify.files'), value: formatNumber(v.progress.filesScanned) },
            { label: t('cache.verify.bytes'), value: formatBytes(v.progress.bytesScanned) },
            { label: t('cache.verify.corrupt'), value: formatNumber(v.progress.corrupt) },
            { label: t('cache.verify.added'), value: formatNumber(v.progress.added) },
            { label: t('cache.verify.removed'), value: formatNumber(v.progress.removed) },
          ]}
        />
      {/if}
      {#if !v.running && v.finishedAt && v.progress.corrupt > 0 && !v.repair}
        <Notice tone="warn">{t('cache.verify.corruptHint')}</Notice>
      {/if}
    </div>
  {/if}
</Panel>

<style>
  .status {
    font-weight: 600;
  }
</style>
