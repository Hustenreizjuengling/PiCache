<!--
  @component
  Why the DHCP server is not available and what to do, chosen by the
  status's reasonCode: PICACHE_DHCP=off (remove the line), not Linux, a
  container network (host networking), the ports could not be opened (the
  reason; stop the other DHCP server or switch PiCache's off) or they open
  only at start (restart, admins get the button; while the marker file for
  the next start cannot be written, that a restart will not help yet and
  why). Without a known code the server's reason is shown as it is.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DhcpStatus } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Icon, Notice } from '$lib/ui'
  import RestartButton from '../../system/RestartButton.svelte'
  import { REPO } from '../../system/health/about'

  interface Props {
    status: DhcpStatus
    /** PiCache answers again after a restart. */
    onrestarted?: () => void
  }

  let { status, onrestarted }: Props = $props()

  const code = $derived(status.reasonCode ?? '')
</script>

{#snippet docs()}
  <p>
    <a href="{REPO}/blob/main/docs/DEPLOYMENT.md#dhcp-server" target="_blank" rel="noopener noreferrer">
      {t('dns.dhcp.unavailable.docs')}<Icon name="external" size={14} />
    </a>
  </p>
{/snippet}

{#snippet markerError()}
  {#if status.markerError}
    <Notice tone="fail"><span class="reason">{t('dns.dhcp.unavailable.markerError', { reason: status.markerError })}</span></Notice>
  {/if}
{/snippet}

{#if code === 'opt-out'}
  <Notice>
    <p>{t('dns.dhcp.unavailable.opt-out')}</p>
    {@render docs()}
  </Notice>
{:else if code === 'not-linux'}
  <Notice>{t('dns.dhcp.unavailable.not-linux')}</Notice>
{:else if code === 'bridge'}
  <Notice>
    <p>{t('dns.dhcp.unavailable.bridge')}</p>
    {@render docs()}
  </Notice>
{:else if code === 'socket'}
  <Notice tone="fail">
    {#if status.reason}<p class="reason">{status.reason}</p>{/if}
    <p>{t('dns.dhcp.unavailable.socket')}</p>
  </Notice>
{:else if code === 'restart-required' && session.canOperate}
  <Notice tone="warn">
    {t('dns.dhcp.unavailable.restart')}
    {#snippet actions()}
      <RestartButton variant="primary" size="sm" message={t('dns.dhcp.unavailable.restartConfirm')} ondone={onrestarted} />
    {/snippet}
  </Notice>
  {@render markerError()}
{:else if code === 'restart-required'}
  <Notice tone="warn">{t('dns.dhcp.unavailable.restart')}</Notice>
  {@render markerError()}
{:else if status.reason}
  <Notice>{t('dns.dhcp.text.reason', { reason: status.reason })}</Notice>
{/if}

<style>
  .reason {
    overflow-wrap: anywhere;
  }
  a :global(.icon) {
    margin-left: 4px;
    vertical-align: -2px;
  }
</style>
