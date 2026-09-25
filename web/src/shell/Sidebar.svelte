<!--
  @component
  Main navigation: Overview, then the DNS, Cache and System sections. A dot
  marks "Updates" while a new PiCache release is available.
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { Icon } from '../lib/ui'
  import { router } from '../lib/router.svelte'
  import { appStatus } from '../lib/status.svelte'
  import { routes, sections, type RouteDef } from '../routes'

  let { onnavigate }: { onnavigate?: () => void } = $props()

  function active(r: RouteDef): boolean {
    const p = router.path
    return r.path === '/' ? p === '/' : p === r.path || p.startsWith(r.path + '/')
  }

  const top = routes.filter((r) => !r.section)

  /** Accessible text of a navigation item's dot, or undefined for no dot. */
  function marker(r: RouteDef): string | undefined {
    const u = appStatus.update.data
    if (r.path === '/system/updates' && u?.updateAvailable && u.status?.state !== 'running') {
      return t('common.nav.updateAvailable')
    }
    return undefined
  }
</script>

<nav class="nav" aria-label={t('common.nav.label')}>
  <ul>
    {#each top as r (r.path)}
      <li>
        <a href="#{r.path}" aria-current={active(r) ? 'page' : undefined} onclick={() => onnavigate?.()}>
          <Icon name={r.icon} size={18} /><span>{t(r.title)}</span>
        </a>
      </li>
    {/each}
  </ul>
  {#each sections as s (s.id)}
    <p class="heading" id="nav-{s.id}">{t(s.title)}</p>
    <ul aria-labelledby="nav-{s.id}">
      {#each routes.filter((r) => r.section === s.id) as r (r.path)}
        {@const mark = marker(r)}
        <li>
          <a href="#{r.path}" aria-current={active(r) ? 'page' : undefined} onclick={() => onnavigate?.()}>
            <Icon name={r.icon} size={18} /><span>{t(r.title)}</span>
            {#if mark}<span class="mark" title={mark}><span class="visually-hidden">({mark})</span></span>{/if}
          </a>
        </li>
      {/each}
    </ul>
  {/each}
</nav>

<style>
  .nav {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    padding: var(--sp-2) var(--sp-3) var(--sp-5);
  }
  ul {
    margin: 0;
    padding: 0;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .heading {
    margin: var(--sp-4) var(--sp-2) var(--sp-1);
    font-size: var(--fs-sm);
    font-weight: 600;
    color: var(--text-3);
  }
  a {
    display: flex;
    align-items: center;
    gap: var(--sp-3);
    min-height: 36px;
    padding: 6px var(--sp-2);
    border-radius: var(--r-control);
    color: var(--text-2);
    font-size: var(--fs-md);
    text-decoration: none;
  }
  a:hover {
    background: var(--surface-2);
    color: var(--text);
  }
  a[aria-current='page'] {
    background: var(--surface-3);
    color: var(--text);
    font-weight: 600;
    box-shadow: inset 3px 0 0 var(--text);
  }
  a :global(.icon) {
    color: var(--text-3);
  }
  a[aria-current='page'] :global(.icon) {
    color: var(--text);
  }
  .mark {
    flex: none;
    width: 8px;
    height: 8px;
    margin-left: auto;
    margin-right: var(--sp-1);
    border-radius: 50%;
    background: var(--focus);
    box-shadow: 0 0 0 2px color-mix(in srgb, var(--focus) 25%, transparent);
  }
</style>
