<!--
  @component
  The Prometheus endpoint (settings.web.metricsEnabled, off by default).
  Switching applies immediately; when on, shows the URL and a scrape config
  for this address. Scraping needs an admin API token.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { CopyButton, Notice, Panel, Skeleton, Toggle, toast } from '$lib/ui'

  const form = settingsForm('web')

  const url = new URL('metrics', document.baseURI)
  const https = url.protocol === 'https:'
  const scrapeConfig = [
    'scrape_configs:',
    '  - job_name: picache',
    `    scheme: ${https ? 'https' : 'http'}`,
    `    metrics_path: ${url.pathname}`,
    '    authorization:',
    '      type: Bearer',
    '      credentials_file: /etc/prometheus/picache-token',
    ...(https ? ['    tls_config:', '      ca_file: /etc/prometheus/picache-cert.pem'] : []),
    '    static_configs:',
    `      - targets: ['${url.host}']`,
  ].join('\n')

  async function toggle(on: boolean) {
    if (await form.save()) {
      toast.success(on ? t('system.metrics.enabled') : t('system.metrics.disabled'))
    } else {
      toast.error(form.errorMessage ?? t('common.error.generic'))
      form.revert()
    }
  }
</script>

<Panel title={t('system.metrics.title')} description={t('system.metrics.description')}>
  {#if form.loadError && !form.draft}
    <Notice tone="fail">{form.loadError.message}</Notice>
  {:else if !form.draft}
    <Skeleton height="48px" />
  {:else}
    <div class="stack">
      <Toggle
        bind:checked={form.draft.metricsEnabled}
        label={t('system.metrics.toggle')}
        description={t('system.metrics.toggleHelp')}
        disabled={!session.isAdmin || form.saving}
        onchange={toggle}
      />
      {#if form.saved?.metricsEnabled}
        <div class="stack-sm">
          <p class="small">{t('system.metrics.url')}</p>
          <div class="box">
            <code class="mono">{url.href}</code>
            <CopyButton text={url.href} />
          </div>
        </div>
        <div class="stack-sm">
          <p class="small">{t('system.metrics.scrape')}</p>
          <div class="box top">
            <pre class="mono">{scrapeConfig}</pre>
            <CopyButton text={scrapeConfig} />
          </div>
          <p class="small muted">
            {https ? t('system.metrics.scrapeHelpTls') : t('system.metrics.scrapeHelp')}
          </p>
        </div>
      {/if}
    </div>
  {/if}
</Panel>

<style>
  .box {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    min-width: 0;
  }
  .box.top {
    align-items: flex-start;
  }
  .box code,
  .box pre {
    flex: 1;
    min-width: 0;
    font-size: var(--fs-sm);
  }
  pre {
    overflow-x: auto;
    white-space: pre;
  }
</style>
