<!--
  @component
  How to trust the local CA on each kind of device (collapsible per
  platform) and what trusting it means: the CA can only vouch for
  PiCache's own names and addresses, and whoever can read PiCache's data
  directory can use its key.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import { Notice, Panel } from '$lib/ui'

  const PLATFORMS = ['windows', 'macos', 'ios', 'android', 'firefox', 'linux'] as const
  const LINUX_CMD = 'sudo cp picache-ca.crt /usr/local/share/ca-certificates/ && sudo update-ca-certificates'
</script>

<Panel title={t('system.https.trust.title')} description={t('system.https.trust.description')}>
  <div class="stack">
    <div class="platforms">
      {#each PLATFORMS as p (p)}
        <details>
          <summary>{t(`system.https.trust.${p}.title` as MessageKey)}</summary>
          <div class="body small muted">
            <p>{t(`system.https.trust.${p}.text` as MessageKey)}</p>
            {#if p === 'linux'}<p><code class="mono">{LINUX_CMD}</code></p>{/if}
          </div>
        </details>
      {/each}
    </div>
    <Notice tone="info" icon="shield" title={t('system.https.trust.riskTitle')}>{t('system.https.trust.riskText')}</Notice>
  </div>
</Panel>

<style>
  .platforms {
    display: flex;
    flex-direction: column;
  }
  details {
    border-top: 1px solid var(--line);
  }
  details:first-child {
    border-top: 0;
  }
  summary {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) 0;
    font-size: var(--fs-sm);
    font-weight: 600;
    list-style: none;
    cursor: pointer;
  }
  summary::-webkit-details-marker {
    display: none;
  }
  summary::before {
    content: '';
    flex: none;
    width: 6px;
    height: 6px;
    margin: 0 4px 0 2px;
    border-right: 1.5px solid var(--text-2);
    border-bottom: 1.5px solid var(--text-2);
    transform: rotate(-45deg);
    transition: transform var(--dur-fast);
  }
  details[open] > summary::before {
    transform: rotate(45deg);
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: 0 0 var(--sp-3) var(--sp-5);
    max-width: 80ch;
  }
  .body code {
    overflow-wrap: anywhere;
  }
</style>
