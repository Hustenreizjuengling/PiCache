<!--
  @component
  Centered layout for the sign-in and setup screens, with language and theme
  switchers and a pointer to HTTPS when the page was opened over plain HTTP.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { LOCALES, i18n, setLocale, t } from '../../i18n/index.svelte'
  import { theme, type ThemeChoice } from '../../lib/theme.svelte'
  import { Menu, type MenuItem } from '../../lib/ui'
  import Brand from '../../shell/Brand.svelte'
  import HttpsNotice from './HttpsNotice.svelte'

  let { title, intro, children }: { title: string; intro?: string; children: Snippet } = $props()

  const langItems = $derived(
    LOCALES.map((l): MenuItem => ({ label: l.label, checked: i18n.locale === l.id, onselect: () => setLocale(l.id) })),
  )
  const themeItems = $derived(
    (['system', 'light', 'dark'] as ThemeChoice[]).map(
      (c): MenuItem => ({ label: t(`common.theme.${c}`), checked: theme.choice === c, onselect: () => theme.set(c) }),
    ),
  )
  const themeIcon = $derived(theme.choice === 'light' ? 'sun' : theme.choice === 'dark' ? 'moon' : 'monitor')
</script>

<div class="strip" aria-hidden="true">
  <span style:background="var(--blue)"></span><span style:background="var(--orange)"></span><span
    style:background="var(--green)"
  ></span><span style:background="var(--brown)"></span>
</div>
<main class="screen">
  <div class="col">
    <Brand size="lg" />
    <section class="card" aria-labelledby="auth-title">
      <h1 id="auth-title">{title}</h1>
      {#if intro}<p class="intro">{intro}</p>{/if}
      <HttpsNotice />
      {@render children()}
    </section>
    <div class="switchers">
      <Menu label={t('common.language.label')} icon="globe" size="sm" variant="ghost" align="start" items={langItems} />
      <Menu label={t('common.theme.label')} icon={themeIcon} size="sm" variant="ghost" align="start" items={themeItems} />
    </div>
  </div>
</main>

<style>
  .strip {
    display: flex;
    height: 6px;
  }
  .strip span {
    flex: 1;
  }
  .screen {
    display: flex;
    justify-content: center;
    min-height: calc(100vh - 6px);
    padding: var(--sp-7) var(--sp-4) var(--sp-5);
  }
  .col {
    display: flex;
    flex-direction: column;
    gap: var(--sp-5);
    width: min(440px, 100%);
  }
  .card {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
    padding: var(--sp-5);
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
  }
  h1 {
    font-size: var(--fs-xl);
  }
  .intro {
    color: var(--text-2);
  }
  .switchers {
    display: flex;
    gap: var(--sp-2);
  }
  @media (max-width: 480px) {
    .screen {
      padding-top: var(--sp-5);
    }
    .card {
      padding: var(--sp-4);
    }
  }
</style>
