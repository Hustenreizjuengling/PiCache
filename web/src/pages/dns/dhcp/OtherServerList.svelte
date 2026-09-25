<!--
  @component
  Other DHCP servers PiCache has seen: the address (and the server ID when it
  differs), how it was seen (it answered PiCache's search, or a device took
  an address from it) and when.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DhcpOtherServer } from '$lib/api'
  import { formatDateTime, formatRelative } from '$lib/format'

  let { servers }: { servers: readonly DhcpOtherServer[] } = $props()
</script>

<ul class="servers">
  {#each servers as s (`${s.address}|${s.source}`)}
    <li>
      <span class="addr">
        <span class="mono">{s.address}</span>
        {#if s.serverId && s.serverId !== s.address}
          <span class="small muted">{t('dns.dhcp.other.serverId', { id: s.serverId })}</span>
        {/if}
      </span>
      <span class="small muted" title={formatDateTime(s.lastSeen)}>
        {s.source === 'request' ? t('dns.dhcp.other.request') : t('dns.dhcp.other.probe')} · {formatRelative(s.lastSeen)}
      </span>
    </li>
  {/each}
</ul>

<style>
  .servers {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-sm);
  }
  li {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 2px var(--sp-3);
    min-width: 0;
  }
  .addr {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0 var(--sp-2);
  }
  .addr .mono {
    font-weight: 600;
  }
</style>
