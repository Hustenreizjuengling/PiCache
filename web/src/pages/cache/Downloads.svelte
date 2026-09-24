<!--
  @component
  Cache › Downloads: what is downloading right now, download sessions per
  client and content, the raw request log, HTTPS pass-through connections
  and removed content. Tab and filters are in the URL; incoming links use
  ?client=<ip>&active=true.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { href, router } from '$lib/router.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Button, Notice, Tabs, type TabItem } from '$lib/ui'
  import Evictions from './downloads/Evictions.svelte'
  import LiveNow from './downloads/LiveNow.svelte'
  import PassThrough from './downloads/PassThrough.svelte'
  import Requests from './downloads/Requests.svelte'
  import Sessions from './downloads/Sessions.svelte'
  import { serviceCatalog } from './shared/catalog.svelte'
  import { pick } from './shared/util'

  const TABS = ['sessions', 'requests', 'passthrough', 'evictions'] as const
  type Tab = (typeof TABS)[number]

  const tab = $derived<Tab>(pick(router.param('tab'), TABS, 'sessions'))
  const catalog = serviceCatalog()
  const live = resource((signal) => api.cache.live({ signal }), { interval: 3000 })

  const tabs: TabItem[] = $derived([
    { id: 'sessions', label: t('cache.downloads.tabSessions'), icon: 'download' },
    { id: 'requests', label: t('cache.downloads.tabRequests'), icon: 'list' },
    { id: 'passthrough', label: t('cache.downloads.tabPassThrough'), icon: 'lock' },
    { id: 'evictions', label: t('cache.downloads.tabEvictions'), icon: 'trash' },
  ])

  /** Switching tabs keeps client, service and search; tab-specific state is dropped. */
  function selectTab(id: string) {
    router.setQuery({
      tab: id === 'sessions' ? null : id,
      range: null,
      status: null,
      active: null,
      group: null,
      offset: null,
      live: null,
      session: null,
      request: null,
      connection: null,
      eviction: null,
    })
  }

  const lancacheOff = $derived(appStatus.overview.data?.lancacheEnabled === false)
</script>

<div class="page">
  {#if lancacheOff}
    <Notice tone="info" title={t('cache.off.title')}>
      {t('cache.off.text')}
      {#snippet actions()}
        <Button size="sm" icon="sliders" href={href('/cache/settings')}>{t('cache.off.open')}</Button>
      {/snippet}
    </Notice>
  {/if}

  <LiveNow rows={live.data} loading={live.loading} error={live.error} onretry={() => live.refresh()} {catalog} />

  <Tabs label={t('cache.downloads.tabsLabel')} {tabs} bind:active={() => tab, selectTab}>
    {#snippet children(active)}
      {#if active === 'requests'}
        <Requests {catalog} />
      {:else if active === 'passthrough'}
        <PassThrough {catalog} />
      {:else if active === 'evictions'}
        <Evictions {catalog} />
      {:else}
        <Sessions {catalog} live={live.data} />
      {/if}
    {/snippet}
  </Tabs>
</div>
