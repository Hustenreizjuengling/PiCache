<!--
  @component
  Confirms switching the DHCP server on: what PiCache will hand out, that
  the router's DHCP server has to be switched off (FRITZ!Box menu path) and
  that PiCache needs a fixed address of its own (with the state of its
  current one) and, in a container, that PiCache restarts once (the ports
  open only at start). Errors of the save are shown in the dialog.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DhcpInterface, DhcpSettings } from '$lib/api'
  import { Button, Dialog, Icon, Notice, Trans } from '$lib/ui'
  import { privateAddresses } from './net'

  interface Props {
    open?: boolean
    settings: DhcpSettings | undefined
    /** The configured interface (from /dhcp/interfaces), if found. */
    iface: DhcpInterface | undefined
    /** How PiCache runs (DhcpStatus.deployment). */
    deployment?: string
    switching: boolean
    error?: string
    onconfirm: () => void
  }

  let { open = $bindable(false), settings, iface, deployment, switching, error, onconfirm }: Props = $props()

  const address = $derived(iface ? privateAddresses(iface)[0]?.split('/')[0] : undefined)
</script>

{#snippet fritzPath()}<span class="path">{t('dns.network.fritz.ipv4Path')}</span>{/snippet}
{#snippet fritzField()}<strong>{t('dns.dhcp.fritz.dhcpServer')}</strong>{/snippet}

<Dialog bind:open title={t('dns.dhcp.on.title')} dismissible={!switching}>
  <div class="stack">
    {#if settings}
      <p>
        {t('dns.dhcp.on.summary', { start: settings.rangeStart, end: settings.rangeEnd, interface: settings.interface })}
      </p>
    {/if}

    <section class="point">
      <h3><span class="num" aria-hidden="true">1</span>{t('dns.dhcp.on.routerTitle')}</h3>
      <p class="small muted">{t('dns.dhcp.on.routerText')}</p>
      <ul class="small">
        <li><Trans key="dns.dhcp.fritz.step" path={fritzPath} field={fritzField} /></li>
        <li>{t('dns.dhcp.on.generic')}</li>
      </ul>
    </section>

    <section class="point">
      <h3><span class="num" aria-hidden="true">2</span>{t('dns.dhcp.on.fixedTitle')}</h3>
      {#if iface && address && iface.dynamic4}
        <Notice tone="warn">{t('dns.dhcp.on.fixedBad', { address, interface: iface.name })}</Notice>
      {:else if iface && address}
        <p class="small ok"><Icon name="success" size={16} /><span>{t('dns.dhcp.on.fixedOk', { address, interface: iface.name })}</span></p>
      {:else}
        <p class="small muted">{t('dns.dhcp.on.fixedUnknown')}</p>
      {/if}
    </section>

    {#if deployment === 'docker'}
      <section class="point">
        <h3><span class="num" aria-hidden="true">3</span>{t('dns.dhcp.on.restartTitle')}</h3>
        <p class="small muted">{t('dns.dhcp.on.restartText')}</p>
      </section>
    {/if}

    <p class="small muted">{t('dns.dhcp.on.devices')}</p>

    {#if error}<Notice tone="fail">{error}</Notice>{/if}
  </div>

  {#snippet actions()}
    <Button variant="ghost" disabled={switching} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button variant="primary" icon="power" loading={switching} onclick={onconfirm}>{t('dns.dhcp.switchOn')}</Button>
  {/snippet}
</Dialog>

<style>
  .point {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
  }
  h3 {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .num {
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    width: 22px;
    height: 22px;
    border-radius: 50%;
    background: var(--surface-3);
    font-size: var(--fs-xs);
    font-weight: 700;
  }
  ul {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    margin: 0;
    padding-left: var(--sp-5);
    overflow-wrap: anywhere;
  }
  .path {
    font-style: italic;
  }
  .ok {
    display: flex;
    align-items: flex-start;
    gap: 6px;
  }
  .ok :global(.icon) {
    flex: none;
    margin-top: 1px;
    color: var(--ok);
  }
</style>
