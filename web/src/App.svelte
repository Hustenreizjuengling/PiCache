<!--
  @component
  Root: picks the screen from the session (setup, login, the app) and hosts
  toasts and confirmation dialogs.
-->
<script lang="ts">
  import { t } from './i18n/index.svelte'
  import { errorText } from './lib/errors'
  import { session } from './lib/session.svelte'
  import { Button, ConfirmHost, Notice, Spinner, Toasts } from './lib/ui'
  import Login from './pages/Login.svelte'
  import Setup from './pages/Setup.svelte'
  import Brand from './shell/Brand.svelte'
  import Shell from './shell/Shell.svelte'

  void session.load()
</script>

{#if session.phase === 'ready'}
  <Shell />
{:else if session.phase === 'login'}
  <Login />
{:else if session.phase === 'setup'}
  <Setup />
{:else}
  <div class="screen">
    <div class="box">
      <Brand size="lg" />
      {#if session.phase === 'loading'}
        <p class="row muted"><Spinner size={18} />{t('common.state.loading')}</p>
      {:else if session.phase === 'misdirected'}
        <Notice tone="fail" title={t('common.misdirected.title')}>
          <p>{t('common.misdirected.text', { host: location.hostname })}</p>
        </Notice>
      {:else}
        <Notice tone="fail" title={t('common.error.startTitle')}>
          {errorText(session.error)}
          {#snippet actions()}
            <Button size="sm" icon="refresh" onclick={() => session.load()}>{t('common.action.retry')}</Button>
          {/snippet}
        </Notice>
      {/if}
    </div>
  </div>
{/if}

<Toasts />
<ConfirmHost />

<style>
  .screen {
    display: flex;
    align-items: center;
    justify-content: center;
    min-height: 100vh;
    padding: var(--sp-4);
  }
  .box {
    display: flex;
    flex-direction: column;
    gap: var(--sp-5);
    width: min(480px, 100%);
  }
</style>
