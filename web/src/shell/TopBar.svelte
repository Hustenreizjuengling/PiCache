<!--
  @component
  Page title, live status (DNS, blocking + pause menu, cache store, the
  unacknowledged warnings linking to the warning history), global search,
  theme and language switchers and the account menu.
-->
<script lang="ts">
  import { LOCALES, i18n, setLocale, t, tn } from '../i18n/index.svelte'
  import { href } from '../lib/router.svelte'
  import { session } from '../lib/session.svelte'
  import { appStatus } from '../lib/status.svelte'
  import { theme, type ThemeChoice } from '../lib/theme.svelte'
  import { Icon, IconButton, Menu, type MenuItem, type Tone } from '../lib/ui'
  import BlockingControl from './BlockingControl.svelte'
  import GlobalSearch from './GlobalSearch.svelte'

  interface Props {
    title: string
    menuOpen: boolean
    onmenu: () => void
  }

  let { title, menuOpen, onmenu }: Props = $props()

  const ov = $derived(appStatus.overview.data)

  const dns = $derived.by((): { tone: Tone; text: string } => {
    if (!ov) return { tone: 'neutral', text: t('common.status.dns') }
    const ups = ov.upstreams ?? []
    if (ups.length > 0 && ups.every((u) => !u.healthy)) return { tone: 'fail', text: t('common.status.dnsDown') }
    if (ov.clockGuard) return { tone: 'warn', text: t('common.status.dnsClock') }
    return { tone: 'ok', text: t('common.status.dns') }
  })

  const cache = $derived.by((): { tone: Tone; text: string; link: string } => {
    if (!ov) return { tone: 'neutral', text: t('common.status.cache'), link: href('/cache/storage') }
    if (!ov.downloadCacheEnabled) return { tone: 'neutral', text: t('common.status.cacheOff'), link: href('/cache/settings') }
    const s = ov.store
    if (!s.online || s.passThrough) return { tone: 'fail', text: t('common.status.cacheOffline'), link: href('/cache/storage') }
    if (s.full) return { tone: 'warn', text: t('common.status.cacheFull'), link: href('/cache/storage') }
    if (s.lowSpace) return { tone: 'warn', text: t('common.status.cacheLow'), link: href('/cache/storage') }
    return { tone: 'ok', text: t('common.status.cache'), link: href('/cache/storage') }
  })

  /** Unacknowledged warnings and errors of the warning history (0 from servers without it). */
  const warnings = $derived(ov?.events?.unacknowledged ?? 0)

  const themeIcon = $derived(theme.choice === 'light' ? 'sun' : theme.choice === 'dark' ? 'moon' : 'monitor')
  const themeItems = $derived(
    (['system', 'light', 'dark'] as ThemeChoice[]).map(
      (c): MenuItem => ({ label: t(`common.theme.${c}`), checked: theme.choice === c, onselect: () => theme.set(c) }),
    ),
  )
  const langItems = $derived(
    LOCALES.map((l): MenuItem => ({ label: l.label, checked: i18n.locale === l.id, onselect: () => setLocale(l.id) })),
  )
  const accountItems = $derived<MenuItem[]>([
    { label: t('common.nav.account'), icon: 'user', href: href('/system/account') },
    { separator: true },
    { label: t('common.account.logout'), icon: 'logout', onselect: () => void session.logout() },
  ])
</script>

<header class="topbar">
  <div class="title-row">
    <span class="menu-btn">
      <IconButton
        icon={menuOpen ? 'close' : 'menu'}
        label={menuOpen ? t('common.nav.close') : t('common.nav.open')}
        aria-expanded={menuOpen}
        aria-controls="app-nav"
        onclick={onmenu}
      />
    </span>
    <h1>{title}</h1>
  </div>

  <div class="status" role="group" aria-label={t('common.status.label')}>
    <a class={['ind', dns.tone]} href={href('/dns/settings')}><span class="dot" aria-hidden="true"></span>{dns.text}</a>
    <BlockingControl />
    <a class={['ind', cache.tone]} href={cache.link}><span class="dot" aria-hidden="true"></span>{cache.text}</a>
    {#if warnings > 0}
      {@const label = tn('common.status.warnings', warnings)}
      <a class="ind warn badge" href={href('/system/health', { section: 'warnings', warnings: 'open' })} title={label} aria-label={label}>
        <Icon name="bell" size={16} /><span class="count">{warnings > 99 ? '99+' : warnings}</span>
      </a>
    {/if}
  </div>

  <div class="tools">
    <GlobalSearch />
    <Menu label={t('common.theme.label')} icon={themeIcon} iconOnly variant="ghost" items={themeItems} />
    <Menu label={t('common.language.label')} icon="globe" iconOnly variant="ghost" items={langItems} />
    <span class="account">
      <Menu label={session.user?.username ?? t('common.account.label')} icon="user" variant="ghost" items={accountItems} />
    </span>
  </div>
</header>

<style>
  .topbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-4);
    min-height: 60px;
    padding: var(--sp-2) var(--sp-5);
    border-bottom: 1px solid var(--line);
    background: var(--surface);
  }
  .title-row {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    flex: 1 1 auto;
    min-width: 0;
  }
  h1 {
    font-size: var(--fs-lg);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .menu-btn {
    display: none;
    margin-left: calc(-1 * var(--sp-2));
  }
  .status,
  .tools {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2);
  }
  .ind {
    --c: var(--text-3);
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: var(--control-h);
    padding: 0 var(--sp-2);
    border-radius: var(--r-control);
    color: var(--text);
    font-size: var(--fs-sm);
    font-weight: 600;
    text-decoration: none;
    white-space: nowrap;
  }
  .ind:hover {
    background: var(--surface-2);
  }
  .dot {
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--c);
  }
  .ok {
    --c: var(--ok);
  }
  .warn {
    --c: var(--warn);
  }
  .fail {
    --c: var(--fail);
  }
  .badge :global(.icon) {
    color: var(--c);
  }
  .count {
    min-width: 18px;
    padding: 0 5px;
    border-radius: var(--r-pill);
    background: var(--c);
    color: var(--on-accent);
    font-size: var(--fs-xs);
    line-height: 18px;
    text-align: center;
  }
  .neutral .dot {
    background: transparent;
    box-shadow: inset 0 0 0 1.5px var(--text-3);
  }
  @media (max-width: 900px) {
    .menu-btn {
      display: inline-flex;
    }
    .topbar {
      padding: var(--sp-2) var(--sp-4);
    }
  }
  @media (max-width: 640px) {
    .status {
      order: 3;
      width: 100%;
    }
    .tools {
      width: 100%;
      flex-wrap: nowrap;
    }
    .tools > :global(.search) {
      flex: 1 1 auto;
      min-width: 0;
    }
    /* Only the user icon on phones; the name stays readable for screen readers. */
    .account :global(.lbl) {
      position: absolute;
      width: 1px;
      height: 1px;
      overflow: hidden;
      clip: rect(0 0 0 0);
      white-space: nowrap;
    }
    .account :global(.icon:last-child) {
      display: none;
    }
  }
</style>
