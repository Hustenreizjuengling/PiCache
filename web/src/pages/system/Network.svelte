<!--
  @component
  System › Network: the listeners (bound now, saved for the next start,
  defaults; changed with the password and applied by a restart; read-only
  in Docker), the outbound proxy for the chosen downloads, and the NTP
  server (its switch and stratum, the state of its listener and the
  open-server warning).
  Query: ?section=listeners|proxy|ntp (scrolls there)
-->
<script lang="ts">
  import { tick, untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Notice } from '$lib/ui'
  import ListenersPanel from './network/ListenersPanel.svelte'
  import NtpPanel from './network/NtpPanel.svelte'
  import ProxyPanel from './network/ProxyPanel.svelte'

  const listeners = resource((signal) => api.system.listeners({ signal }), { interval: 60_000 })
  // The NTP panel: the clock warning (health check "ntp") and a bind failure of its listener.
  const health = resource((signal) => api.system.health({ signal }), { interval: 60_000 })
  const info = resource((signal) => api.system.info({ signal }), { interval: 60_000 })

  /** After a restart everything on the page describes the new process. */
  function restarted() {
    void health.refresh()
    void info.refresh()
  }

  const SECTIONS = ['listeners', 'proxy', 'ntp']
  const section = $derived(router.param('section'))
  $effect(() => {
    const s = section
    if (!listeners.loaded || !SECTIONS.includes(s)) return
    untrack(() => void tick().then(() => document.getElementById(`net-${s}`)?.scrollIntoView({ block: 'start' })))
  })
</script>

<div class="page">
  {#if !session.canOperate}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  <ListenersPanel config={listeners} onrestarted={restarted} />

  <div class="cols-2">
    <ProxyPanel />
    <NtpPanel ntpRole={listeners.data?.roles.find((r) => r.role === 'ntp')} {health} bindError={info.data?.listeners.failed?.ntp} />
  </div>
</div>

<style>
  .cols-2 {
    align-items: start;
  }
  .page :global(section[id^='net-']) {
    scroll-margin-top: var(--sp-4);
  }
</style>
