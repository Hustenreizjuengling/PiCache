<!--
  @component
  The storage the cache uses right now: used and free space, the minimum free
  space kept, warnings (offline, full, low on space, SD card) and "remove old
  content now".
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, type ApiError, type StorageTargetWithStatus, type StoreState } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatNumber } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, KeyValue, Meter, Notice, Panel, Skeleton, toast } from '$lib/ui'
  import { formatBinary } from '../shared/util'

  interface Props {
    store: StoreState | undefined
    error: ApiError | undefined
    /** The active target (for its name, path and file system). */
    target: StorageTargetWithStatus | undefined
    onchanged: () => void
  }

  let { store, error, target, onchanged }: Props = $props()

  const cached = $derived(store?.usage?.cachedBytes ?? 0)
  const used = $derived(store ? Math.max(0, store.totalBytes - store.freeBytes) : 0)
  const other = $derived(Math.max(0, used - cached))

  let evicting = $state(false)
  async function evict() {
    evicting = true
    try {
      const r = await api.cache.evict()
      if (r.objects > 0) {
        toast.success(t('cache.store.evicted', { count: formatNumber(r.objects), size: formatBytes(r.bytes) }))
      } else {
        toast.info(t('cache.store.evictedNothing'))
      }
      if (r.full) toast.info(t('cache.store.stillFull'))
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      evicting = false
    }
  }
</script>

<Panel title={t('cache.store.title')} description={target ? t('cache.store.description', { name: target.name }) : undefined}>
  {#snippet actions()}
    <Button icon="trash" loading={evicting} disabled={!session.isAdmin || !store?.online} onclick={evict}>{t('cache.store.evict')}</Button>
  {/snippet}

  {#if !store}
    {#if error}
      <Notice tone="fail">{errorText(error)}</Notice>
    {:else}
      <Skeleton height="80px" />
    {/if}
  {:else}
    <div class="stack">
      {#if !store.online}
        <Notice tone="fail" title={t('cache.store.offline')}>
          {store.reason ?? ''}{#if store.hint}<br />{store.hint}{/if}
          <br />{t('cache.store.passThrough')}
        </Notice>
      {:else}
        {#if store.full}
          <Notice tone="warn" title={t('cache.store.fullTitle')}>{t('cache.store.fullText')}</Notice>
        {:else if store.lowSpace}
          <Notice tone="warn" title={t('cache.store.lowTitle')}>{t('cache.store.lowText', { size: formatBytes(store.minFreeBytes) })}</Notice>
        {/if}
        {#if store.sdCard}
          <Notice tone="warn" title={t('cache.store.sdTitle')}>{t('cache.store.sdText')}</Notice>
        {/if}
        {#if store.hint}<Notice tone="info">{store.hint}</Notice>{/if}

        {#if store.totalBytes > 0}
          <p class="summary">
            {t('cache.store.summary', {
              cached: formatBytes(cached),
              free: formatBytes(store.freeBytes),
              total: formatBytes(store.totalBytes),
            })}
          </p>
          <Meter
            label={t('cache.store.title')}
            max={store.totalBytes}
            segments={[
              { label: t('cache.store.cached'), value: cached, text: formatBytes(cached), pair: 'green' },
              { label: t('cache.store.other'), value: other, text: formatBytes(other), tone: 'neutral' },
            ]}
            rest={{ label: t('cache.store.free'), text: formatBytes(store.freeBytes) }}
            marker={store.minFreeBytes > 0 && store.totalBytes > store.minFreeBytes
              ? {
                  value: store.totalBytes - store.minFreeBytes,
                  label: t('cache.store.minFreeMarker', { size: formatBytes(store.minFreeBytes) }),
                }
              : undefined}
          />
        {:else}
          <p class="summary">{t('cache.store.summaryNoSize', { cached: formatBytes(cached) })}</p>
        {/if}
      {/if}

      <KeyValue
        items={[
          { label: t('cache.store.location'), value: target?.name ?? store.targetId },
          { label: t('cache.store.path'), value: target?.status.storeRoot || target?.path, mono: true },
          { label: t('cache.store.fileSystem'), value: target?.status.fsType },
          { label: t('cache.store.cachedFiles'), value: store.usage ? tn('cache.store.files', store.usage.objects) : undefined },
          { label: t('cache.store.cached'), value: formatBytes(cached) },
          { label: t('cache.store.minFree'), value: formatBytes(store.minFreeBytes) },
          { label: t('cache.store.sliceSize'), value: store.sliceSize ? formatBinary(store.sliceSize) : undefined },
          { label: t('cache.store.storeId'), value: store.storeId, mono: true },
        ]}
      />
      <p class="facts">
        {#if store.online}<Chip size="sm" tone="ok" label={t('cache.store.online')} />{/if}
        {#if target?.status.sameFsAsData}
          <span class="muted small">{t('cache.store.sameFs')}</span>
        {/if}
      </p>
    </div>
  {/if}
</Panel>

<style>
  .summary {
    font-weight: 600;
  }
  .facts {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
</style>
