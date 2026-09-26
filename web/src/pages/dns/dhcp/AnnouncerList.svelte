<!--
  @component
  Other IPv6 announcers PiCache has seen on the interface: a router
  advertisement or a DHCPv6 server that answered PiCache's search, with its
  address, the DNS servers it announces (the addresses of this machine
  marked as PiCache's),
  whether it offers DHCPv6 (M/O flags) or is the default router, and when it
  was seen. Records that announce a DNS server other than PiCache are
  marked.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DhcpAnnouncer } from '$lib/api'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { Badge, Chip } from '$lib/ui'

  interface Props {
    announcers: readonly DhcpAnnouncer[]
  }

  let { announcers }: Props = $props()

  function kindLabel(a: DhcpAnnouncer): string {
    if (a.kind === 'ra') return t('dns.dhcp.ipv6.others.ra')
    if (a.kind === 'dhcpv6') return t('dns.dhcp.ipv6.others.dhcpv6')
    return a.kind
  }

  function dnsText(a: DhcpAnnouncer): string {
    const list = a.dns ?? []
    if (list.length === 0) return t('dns.dhcp.ipv6.others.noDns')
    const own = a.ownDns ?? []
    const shown = list.map((d) => (own.includes(d) ? t('dns.dhcp.fact.picache', { address: d }) : d))
    return t('dns.dhcp.ipv6.others.dns', { addresses: shown.join(', ') })
  }
</script>

<ul class="list">
  {#each announcers as a (`${a.kind}|${a.address}|${a.interface}|${a.serverId ?? ''}`)}
    <li class={[a.conflict && 'conflict']}>
      <span class="head">
        <Badge tone={a.conflict ? 'warn' : 'neutral'}>{kindLabel(a)}</Badge>
        <span class="mono addr">{a.address}</span>
        {#if a.interface}<span class="small muted">{t('dns.dhcp.ipv6.others.on', { interface: a.interface })}</span>{/if}
        {#if a.conflict}<Chip size="sm" tone="warn" label={t('dns.dhcp.ipv6.others.conflict')} />{/if}
      </span>
      <span class="facts small muted">
        <span class="mono dns">{dnsText(a)}</span>
        {#if a.managed || a.other}<span>{t('dns.dhcp.ipv6.others.offersDhcpv6')}</span>{/if}
        {#if (a.routerLifetime ?? 0) > 0}<span>{t('dns.dhcp.ipv6.others.defaultRouter')}</span>{/if}
        {#if a.serverId}<span class="mono id">{t('dns.dhcp.ipv6.others.serverId', { id: a.serverId })}</span>{/if}
        <span title={formatDateTime(a.lastSeen)}>{t('dns.dhcp.ipv6.others.seen', { when: formatRelative(a.lastSeen) })}</span>
      </span>
    </li>
  {/each}
</ul>

<style>
  .list {
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
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  li.conflict {
    border-left: 4px solid var(--warn);
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
    min-width: 0;
  }
  .addr {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
  .facts {
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-3);
    min-width: 0;
  }
  .dns,
  .id {
    overflow-wrap: anywhere;
  }
</style>
