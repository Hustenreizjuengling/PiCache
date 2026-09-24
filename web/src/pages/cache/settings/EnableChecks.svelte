<!--
  @component
  What turning on LanCache will do, checked right before: the address
  download hosts will be answered with, the storage (path, file system, free
  space), the download service list and the cache port, with warnings.
  Mounted inside the enable dialog, so it loads when the dialog opens.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatBytes, formatNumber } from '$lib/format'
  import { Chip, KeyValue, Notice, Skeleton } from '$lib/ui'

  const ips = resource((signal) => api.dns.cacheIps({ signal }))
  const store = resource((signal) => api.cache.state({ signal }))
  const targets = resource((signal) => api.storage.targets({ signal }))
  const caps = resource((signal) => api.storage.capabilities({ signal }))
  const source = resource((signal) => api.lancache.source({ signal }))
  const info = resource((signal) => api.system.info({ signal }))

  // While LanCache is off the server reports only "LanCache is disabled";
  // other reasons (e.g. an auto-detected public address) are real warnings.
  const DISABLED_REASON = 'LanCache is disabled'

  const target = $derived(targets.data?.find((x) => x.active))
  const cacheBound = $derived(info.data?.listeners.bound?.cache ?? [])
  const cacheFailed = $derived(info.data?.listeners.failed?.cache)
  const loading = $derived(!ips.loaded || !store.loaded || !targets.loaded || !source.loaded || !info.loaded)
  const firstError = $derived(ips.error ?? store.error ?? targets.error ?? source.error ?? info.error)
</script>

{#if firstError && loading}
  <Notice tone="fail">{errorText(firstError)}</Notice>
{:else if loading}
  <div class="stack"><Skeleton height="72px" /><Skeleton height="72px" /><Skeleton height="48px" /></div>
{:else}
  <div class="stack">
    <p>{t('cache.enable.intro')}</p>

    <section class="stack-sm">
      <h3>{t('cache.enable.addressTitle')}</h3>
      {#if ips.data && ips.data.ipv4.length > 0}
        <p>
          {t('cache.enable.addressText')}
          {#each ips.data.ipv4 as ip (ip)}<span class="ip mono">{ip}</span>{/each}
          {#each ips.data.ipv6 as ip (ip)}<span class="ip mono">{ip}</span>{/each}
        </p>
        <p class="muted small">{ips.data.auto ? t('cache.enable.addressAuto') : t('cache.enable.addressManual')}</p>
      {:else}
        <Notice tone="fail" title={t('cache.enable.noAddress')}>{ips.data?.reason ?? ''}</Notice>
      {/if}
      {#if ips.data?.reason && ips.data.reason !== DISABLED_REASON && ips.data.ipv4.length > 0}
        <Notice tone="warn">{ips.data.reason}</Notice>
      {/if}
      {#if caps.data?.dockerMode === 'bridge' && ips.data?.auto}
        <Notice tone="warn">{t('cache.caps.bridgeWarning')}</Notice>
      {/if}
    </section>

    <section class="stack-sm">
      <h3>{t('cache.enable.storageTitle')}</h3>
      <KeyValue
        items={[
          { label: t('cache.target.location'), value: target?.name ?? store.data?.targetId },
          { label: t('cache.store.path'), value: target?.status.storeRoot || target?.path, mono: true },
          { label: t('cache.store.fileSystem'), value: target?.status.fsType },
          {
            label: t('cache.store.free'),
            value:
              store.data && store.data.totalBytes > 0
                ? t('cache.target.spaceValue', { free: formatBytes(store.data.freeBytes), total: formatBytes(store.data.totalBytes) })
                : t('cache.enable.freeUnknown'),
          },
        ]}
      />
      {#if store.data && !store.data.online}
        <Notice tone="warn" title={t('cache.store.offline')}>
          {store.data.reason ?? ''}<br />{t('cache.enable.offlineText')}
        </Notice>
      {/if}
      {#if store.data?.sdCard}
        <Notice tone="warn" title={t('cache.store.sdTitle')}>{t('cache.store.sdText')}</Notice>
      {/if}
      {#if store.data?.lowSpace}
        <Notice tone="warn" title={t('cache.store.lowTitle')}>
          {t('cache.store.lowText', { size: formatBytes(store.data.minFreeBytes) })}
        </Notice>
      {/if}
      {#if target?.status.sameFsAsData}
        <Notice tone="info">{t('cache.store.sameFs')}</Notice>
      {/if}
    </section>

    <section class="stack-sm">
      <h3>{t('cache.enable.servicesTitle')}</h3>
      {#if source.data?.ready}
        <p>{t('cache.enable.servicesText', { services: formatNumber(source.data.serviceCount), hosts: formatNumber(source.data.domainCount) })}</p>
      {:else}
        <Notice tone="warn" title={t('cache.source.notReadyTitle')}>{source.data?.error ?? t('cache.source.notReadyText')}</Notice>
      {/if}
    </section>

    <section class="stack-sm">
      <h3>{t('cache.enable.portTitle')}</h3>
      {#if cacheFailed}
        <Notice tone="fail" title={t('cache.enable.portFailed')}>{cacheFailed}<br />{t('cache.enable.portFailedText')}</Notice>
      {:else if cacheBound.length > 0}
        <p class="row">
          <Chip size="sm" tone="ok" label={t('cache.enable.portOk')} />
          {#each cacheBound as a (a)}<span class="mono small">{a}</span>{/each}
        </p>
      {:else}
        <Notice tone="warn">{t('cache.enable.portUnknown')}</Notice>
      {/if}
    </section>
  </div>
{/if}

<style>
  h3 {
    font-size: var(--fs-md);
    font-weight: 600;
  }
  .ip {
    display: inline-block;
    margin-left: var(--sp-2);
    font-weight: 600;
  }
</style>
