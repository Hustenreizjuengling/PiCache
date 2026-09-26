<!--
  @component
  The signed-in app: pair strip, sidebar (a top drawer below 900 px), top
  bar, the restrictions the host sets for admins (configuration lock,
  destructive actions off) and the current page (loaded on demand).
-->
<script lang="ts">
  import type { Component } from 'svelte'
  import { t } from '../i18n/index.svelte'
  import { router, setLeavePrompt } from '../lib/router.svelte'
  import { session } from '../lib/session.svelte'
  import { appStatus, startAppStatus } from '../lib/status.svelte'
  import { Button, EmptyState, Icon, IconButton, Notice, PairStrip, Skeleton, confirm } from '../lib/ui'
  import { resolve } from '../routes'
  import Brand from './Brand.svelte'
  import Sidebar from './Sidebar.svelte'
  import TopBar from './TopBar.svelte'

  $effect(() => startAppStatus())

  // Leaving a page with unsaved settings asks first (lib/router.svelte.ts).
  setLeavePrompt(() =>
    confirm({
      title: t('common.unsaved.title'),
      message: t('common.unsaved.text'),
      confirmLabel: t('common.unsaved.discard'),
      cancelLabel: t('common.unsaved.stay'),
    }),
  )

  let navOpen = $state(false)
  let main: HTMLElement
  let firstRender = true

  // A primitive: effects that read it rerun only when the path changes, not
  // for every query change (search boxes, row selection, panels), which must
  // keep focus and scroll position.
  const path = $derived(router.path)
  const match = $derived(resolve(path))

  // Pages load on demand (one chunk each); the latest navigation wins.
  let loaded = $state<{ path: string; component: Component<any> } | null>(null)
  let loadError = $state(false)
  let loadSeq = 0

  $effect(() => {
    const route = match?.route
    const seq = ++loadSeq
    loadError = false
    if (!route) return
    route
      .load()
      .then((mod) => {
        if (seq === loadSeq) loaded = { path: route.path, component: mod.default }
      })
      .catch(() => {
        if (seq === loadSeq) loadError = true
      })
  })

  const Page = $derived(loaded && match && loaded.path === match.route.path ? loaded.component : null)

  // Title, focus and scroll follow the route (not on first render).
  $effect(() => {
    const title = match ? t(match.route.title) : t('common.notFound.title')
    document.title = `${title} · PiCache`
  })

  $effect(() => {
    void path
    navOpen = false
    if (firstRender) {
      firstRender = false
      return
    }
    window.scrollTo(0, 0)
    main?.focus({ preventScroll: true })
  })

  const strip = $derived(appStatus.strip.data)

  function onKey(e: KeyboardEvent) {
    if (e.key === 'Escape' && navOpen) navOpen = false
  }
</script>

<svelte:window onkeydown={onKey} />

<div class="app">
  <div class="strip">
    <PairStrip
      caption={t('common.strip.caption')}
      allowed={strip ? strip.dnsQueries - strip.dnsBlocked : 0}
      blocked={strip?.dnsBlocked ?? 0}
      hit={strip?.cacheBytesHit ?? 0}
      wan={strip?.cacheBytesWan ?? 0}
    />
  </div>

  <button class="skip" type="button" onclick={() => main.focus()}>{t('common.nav.skip')}</button>

  <aside class={['sidebar', navOpen && 'open']} id="app-nav">
    <div class="inner">
      <div class="brand">
        <a href="#/" aria-label={t('common.nav.home')}><Brand /></a>
        <span class="close"><IconButton icon="close" label={t('common.nav.close')} onclick={() => (navOpen = false)} /></span>
      </div>
      <Sidebar onnavigate={() => (navOpen = false)} />
    </div>
  </aside>
  {#if navOpen}
    <button class="scrim" type="button" tabindex="-1" aria-label={t('common.nav.close')} onclick={() => (navOpen = false)}></button>
  {/if}

  <div class="column">
    <TopBar
      title={match ? t(match.route.title) : t('common.notFound.title')}
      menuOpen={navOpen}
      onmenu={() => (navOpen = !navOpen)}
    />
    <main bind:this={main} tabindex="-1">
      <!-- Set on the host, so not dismissible: why admin actions are missing or disabled. -->
      {#if session.canOperate && (session.configLocked || !session.destructiveApi)}
        <div class="host">
          {#if session.configLocked}
            <p class="host-note"><Icon name="lock" size={16} />{t('common.host.configLocked')}</p>
          {/if}
          {#if !session.destructiveApi}
            <p class="host-note"><Icon name="ban" size={16} />{t('common.host.destructiveOff')}</p>
          {/if}
        </div>
      {/if}
      {#if !match}
        <div class="page">
          <EmptyState icon="search" title={t('common.notFound.title')} text={t('common.notFound.text')}>
            <Button href="#/" icon="overview">{t('common.notFound.home')}</Button>
          </EmptyState>
        </div>
      {:else if loadError}
        <Notice tone="fail" title={t('common.error.pageLoad')}>
          {t('common.error.pageLoadText')}
          {#snippet actions()}
            <Button size="sm" icon="refresh" onclick={() => location.reload()}>{t('common.action.reload')}</Button>
          {/snippet}
        </Notice>
      {:else if Page}
        <Page params={match.params} />
      {:else}
        <div class="page" aria-busy="true">
          <Skeleton height="28px" width="40%" />
          <Skeleton height="220px" />
        </div>
      {/if}
    </main>
  </div>
</div>

<style>
  .app {
    display: grid;
    grid-template-columns: var(--sidebar-w) minmax(0, 1fr);
    grid-template-rows: auto 1fr;
    grid-template-areas:
      'strip strip'
      'side column';
    min-height: 100vh;
  }
  .strip {
    grid-area: strip;
    position: sticky;
    top: 0;
    z-index: 30;
  }
  .sidebar {
    grid-area: side;
    border-right: 1px solid var(--line);
    background: var(--surface);
  }
  .inner {
    position: sticky;
    top: 6px;
    max-height: calc(100vh - 6px);
    overflow-y: auto;
    overscroll-behavior: contain;
  }
  .brand {
    display: flex;
    align-items: center;
    height: 60px;
    padding: 0 var(--sp-5);
    border-bottom: 1px solid var(--line);
  }
  .brand .close {
    display: none;
    margin-left: auto;
  }
  .brand a {
    text-decoration: none;
    border-radius: var(--r-control);
  }
  .column {
    grid-area: column;
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .column > :global(.topbar) {
    position: sticky;
    top: 6px;
    z-index: 20;
  }
  main {
    flex: 1;
    width: 100%;
    max-width: calc(var(--content-max) + 2 * var(--sp-5));
    padding: var(--sp-5);
    outline: none;
  }
  .host {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin-bottom: var(--sp-4);
  }
  .host-note {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border-left: 3px solid var(--focus);
    border-radius: var(--r-control);
    background: color-mix(in srgb, var(--focus) 8%, var(--surface));
    color: var(--text-2);
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
  }
  .host-note :global(.icon) {
    flex: none;
    margin-top: 2px;
    color: var(--focus);
  }
  .skip {
    position: absolute;
    left: var(--sp-2);
    top: var(--sp-2);
    z-index: 50;
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    transform: translateY(-200%);
  }
  .skip:focus-visible {
    transform: none;
  }
  .scrim {
    display: none;
  }
  @media (max-width: 900px) {
    .app {
      grid-template-columns: minmax(0, 1fr);
      grid-template-areas:
        'strip'
        'column';
    }
    .sidebar {
      display: none;
      position: fixed;
      top: 6px;
      left: 0;
      right: 0;
      z-index: 40;
      max-height: calc(100vh - 6px);
      overflow-y: auto;
      border-right: 0;
      border-bottom: 1px solid var(--line);
      box-shadow: var(--shadow-float);
    }
    .inner {
      position: static;
      max-height: none;
    }
    .sidebar .close {
      display: inline-flex;
    }
    .sidebar.open {
      display: block;
      animation: drawer-down var(--dur-fast) ease-out;
    }
    .scrim {
      display: block;
      position: fixed;
      inset: 0;
      z-index: 35;
      border: 0;
      background: rgb(10 14 20 / 0.35);
    }
    main {
      padding: var(--sp-4);
    }
  }
  @keyframes drawer-down {
    from {
      transform: translateY(-8px);
      opacity: 0;
    }
  }
</style>
