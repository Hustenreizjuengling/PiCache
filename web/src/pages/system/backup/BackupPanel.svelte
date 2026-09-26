<!--
  @component
  Download a backup of the configuration database (a plain link: the
  browser sends the session cookie). Explains what is and is not included,
  in particular that the master key never is.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type SystemInfo } from '$lib/api'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Checkbox, CopyButton, Notice, Panel } from '$lib/ui'
  import { masterKeyKind } from '../health/about'

  let { info }: { info?: SystemInfo } = $props()

  let includeSecrets = $state(false)
  const url = $derived(api.system.backupUrl(includeSecrets))
  const curl = $derived(`curl -fsS -H "Authorization: Bearer $PICACHE_TOKEN" -o "picache-$(date +%F).db" "${url}"`)
  const keyKind = $derived(info ? masterKeyKind(info.masterKeySource) : undefined)
</script>

<Panel title={t('system.backup.download.title')} description={t('system.backup.download.description')}>
  <div class="stack">
    <Checkbox
      bind:checked={includeSecrets}
      label={t('system.backup.download.secrets')}
      description={t('system.backup.download.secretsHelp')}
      disabled={!session.canOperate}
    />
    <Notice tone="info" icon="key" title={t('system.backup.download.keyTitle')}>
      <p>{t('system.backup.download.keyText')}</p>
      {#if info && keyKind}
        <p class="key">
          <span>{t(`system.health.keySource.${keyKind}`)}:</span>
          <code class="mono">{info.masterKeySource}</code>
        </p>
      {/if}
    </Notice>
    <div class="row">
      <Button variant="primary" icon="download" href={url} download disabled={!session.canOperate}>
        {t('system.backup.download.button')}
      </Button>
    </div>
    <details class="auto">
      <summary class="small">{t('system.backup.download.autoTitle')}</summary>
      <div class="stack-sm">
        <p class="small muted">
          {t('system.backup.download.autoText')}
          <a href={href('/system/tokens')}>{t('system.backup.download.autoLink')}</a>
        </p>
        <div class="cmd">
          <code class="mono">{curl}</code>
          <CopyButton text={curl} />
        </div>
      </div>
    </details>
  </div>
</Panel>

<style>
  .key {
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-2);
    margin-top: var(--sp-1);
  }
  .auto summary {
    cursor: pointer;
    color: var(--text-2);
    width: fit-content;
  }
  .auto[open] summary {
    margin-bottom: var(--sp-2);
  }
  .cmd {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    min-width: 0;
  }
  .cmd code {
    flex: 1;
    min-width: 0;
    font-size: var(--fs-sm);
  }
</style>
