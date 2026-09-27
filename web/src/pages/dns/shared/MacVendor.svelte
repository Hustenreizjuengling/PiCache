<!--
  @component
  A MAC address with its manufacturer below it (or "Private address
  (randomised)" for a locally administered address, explained in its
  tooltip). Used in the tables of seen devices, network devices and DHCP
  leases and reservations.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'

  interface Props {
    mac?: string
    vendor?: string
    macRandomized?: boolean
  }

  let { mac, vendor, macRandomized = false }: Props = $props()
</script>

<span class="mv">
  {#if mac}<span class="mono">{mac}</span>{:else}<span class="subtle">–</span>{/if}
  {#if vendor}
    <span class="vendor">{vendor}</span>
  {:else if macRandomized}
    <span class="vendor" title={t('dns.vendor.privateHelp')}>{t('dns.vendor.private')}</span>
  {/if}
</span>

<style>
  .mv {
    display: inline-flex;
    flex-direction: column;
    padding: 2px 0;
    line-height: 1.3;
  }
  .mono {
    white-space: nowrap;
  }
  /* As wide as the MAC address above it (never wider): long names wrap. */
  .vendor {
    width: 0;
    min-width: 100%;
    color: var(--text-3);
    font-size: var(--fs-xs);
    white-space: normal;
    overflow-wrap: break-word;
  }
</style>
