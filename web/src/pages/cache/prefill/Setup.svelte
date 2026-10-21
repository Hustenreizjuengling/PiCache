<!--
  @component
  The prefill service does not run: switch it on here (the root helper
  installs the unit when needed, enables and starts it; a start that fails
  shows systemd's view and the service's journal), or, without the helper
  (Debian package, Docker, --without-updater), the command for the host.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type PrefillState } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Button, CopyButton, Notice, Panel, Spinner, toast } from '$lib/ui'

  let { info, onchanged }: { info: PrefillState; onchanged: () => void } = $props()

  const setup = $derived(info.setup)
  const busy = $derived(setup?.state === 'running')
  const failed = $derived(setup?.state === 'failed' && setup.action === 'enable')
  const command = $derived(
    info.mode === 'docker' ? 'docker compose --profile prefill up -d' : 'sudo picache prefill enable',
  )

  let sending = $state(false)
  async function enable() {
    sending = true
    try {
      await api.prefill.setService(true)
      onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      sending = false
    }
  }
</script>

<Panel title={t('cache.prefill.title')} description={t('cache.prefill.description')}>
  {#snippet actions()}
    {#if info.mode === 'helper'}
      <Button icon="power" variant="primary" loading={sending || busy} disabled={!session.isAdmin} onclick={enable}>
        {failed ? t('cache.prefill.setup.retry') : t('cache.prefill.setup.enable')}
      </Button>
    {:else}
      <Button icon="refresh" onclick={onchanged}>{t('cache.prefill.setup.check')}</Button>
    {/if}
  {/snippet}
  <div class="stack">
    {#if busy}
      <p class="row strong" role="status"><Spinner size={18} />{t('cache.prefill.setup.running')}</p>
    {:else if failed && setup}
      <Notice tone="fail" title={t('cache.prefill.setup.failed')}>
        <pre class="mono report">{setup.message}</pre>
      </Notice>
    {:else if info.installed && info.error}
      <Notice tone="warn" title={t('cache.prefill.setup.notAnswering')}>{info.error}</Notice>
    {/if}
    <p>{t('cache.prefill.setup.why')}</p>
    {#if info.mode === 'helper'}
      <p class="muted small">{t('cache.prefill.setup.helper')}</p>
    {:else}
      <p>{info.mode === 'docker' ? t('cache.prefill.setup.docker') : t('cache.prefill.setup.host')}</p>
      <div class="code">
        <pre class="mono">{command}</pre>
        <span class="copy"><CopyButton text={command} label={t('cache.prefill.setup.copy')} /></span>
      </div>
    {/if}
  </div>
</Panel>

<style>
  .strong {
    font-weight: 600;
    gap: var(--sp-2);
  }
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
  pre.report {
    margin-top: var(--sp-2);
    padding: var(--sp-2);
    max-height: 240px;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .copy {
    position: absolute;
    top: var(--sp-2);
    right: var(--sp-2);
  }
</style>
