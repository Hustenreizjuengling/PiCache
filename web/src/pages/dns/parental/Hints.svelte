<!--
  @component
  Short, collapsible hints for parents: devices must be recognisable,
  phones need a fixed private Wi-Fi address, blocking takes a few minutes,
  and encrypted DNS or VPN apps bypass PiCache.
-->
<script lang="ts">
  import { t, type MessageKey } from '$i18n/index.svelte'
  import { href } from '$lib/router.svelte'
  import { Panel, Trans } from '$lib/ui'

  const HINTS = ['identify', 'privateAddress', 'delay', 'bypass'] as const
</script>

{#snippet clientsLink()}<a href={href('/dns/clients')}>{t('common.nav.clients')}</a>{/snippet}
{#snippet filteringLink()}<a href={href('/dns/filtering')}>{t('dns.parental.hint.filteringLink')}</a>{/snippet}

<Panel title={t('dns.parental.hints.title')}>
  <div class="hints">
    {#each HINTS as h (h)}
      <details>
        <summary>{t(`dns.parental.hint.${h}.title` as MessageKey)}</summary>
        <p class="small muted">
          <Trans key={`dns.parental.hint.${h}.text` as MessageKey} clients={clientsLink} filtering={filteringLink} />
        </p>
      </details>
    {/each}
  </div>
</Panel>

<style>
  .hints {
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
  summary:hover {
    color: var(--text);
  }
  details p {
    padding: 0 0 var(--sp-3) var(--sp-5);
    max-width: 75ch;
  }
</style>
