<!--
  @component
  The prefill service does not run: what it is, and how to start it (the
  systemd unit, or the second container in Docker). An installed service
  that does not answer shows why.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { PrefillState } from '$lib/api'
  import { Button, CopyButton, Notice, Panel } from '$lib/ui'

  let { state, onretry }: { state: PrefillState; onretry: () => void } = $props()

  const COMMAND = 'sudo picache prefill enable'
</script>

<Panel title={t('cache.prefill.title')} description={t('cache.prefill.description')}>
  {#snippet actions()}
    <Button icon="refresh" onclick={onretry}>{t('cache.prefill.setup.check')}</Button>
  {/snippet}
  <div class="stack">
    <Notice tone={state.installed ? 'warn' : 'info'} title={state.installed ? t('cache.prefill.setup.notAnswering') : t('cache.prefill.setup.notRunning')}>
      {#if state.installed && state.error}{state.error}{:else}{t('cache.prefill.setup.why')}{/if}
    </Notice>
    <p>{t('cache.prefill.setup.systemd')}</p>
    <div class="code">
      <pre class="mono">{COMMAND}</pre>
      <span class="copy"><CopyButton text={COMMAND} label={t('cache.prefill.setup.copy')} /></span>
    </div>
    <p class="muted small">{t('cache.prefill.setup.docker')}</p>
    <p class="muted small">{t('cache.prefill.setup.socket', { socket: state.socket || '/run/picache-prefill/prefill.sock' })}</p>
  </div>
</Panel>

<style>
  .code {
    position: relative;
  }
  pre {
    margin: 0;
    padding: var(--sp-3) var(--sp-7) var(--sp-3) var(--sp-3);
    overflow: auto;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    font-size: var(--fs-sm);
    white-space: pre;
  }
  .copy {
    position: absolute;
    top: var(--sp-2);
    right: var(--sp-2);
  }
</style>
