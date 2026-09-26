<!--
  @component
  Recent exchanges of the DHCP server (GET /dhcp/log: the last 200 handled
  requests, newest first, in memory on the server): when, DHCPv4, DHCPv6 or
  a router solicitation, the device (host name, MAC address or DUID) with
  its address, the messages (request → answer), the result and why.
  Unknown result and reason codes are shown as they are.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DhcpLogEntry, Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatDateTimeShort, formatTime, sameDay } from '$lib/format'
  import { Chip, EmptyState, IconButton, Panel, Table, type Column, type Tone } from '$lib/ui'

  interface Props {
    log: Resource<DhcpLogEntry[]>
  }

  let { log }: Props = $props()

  // Several exchanges can share a timestamp: keys include the position.
  type Row = DhcpLogEntry & { i: number }
  const rows = $derived<Row[] | undefined>(log.data?.map((e, i) => ({ ...e, i })))

  const RESULT: Record<string, { tone: Tone; label: () => string }> = {
    answered: { tone: 'ok', label: () => t('dns.dhcp.log.result.answered') },
    nak: { tone: 'warn', label: () => t('dns.dhcp.log.result.nak') },
    processed: { tone: 'info', label: () => t('dns.dhcp.log.result.processed') },
    ignored: { tone: 'neutral', label: () => t('dns.dhcp.log.result.ignored') },
  }

  const REASON: Record<string, () => string> = {
    'rapid-commit': () => t('dns.dhcp.log.reason.rapid-commit'),
    'not-reserved': () => t('dns.dhcp.log.reason.not-reserved'),
    'other-server': () => t('dns.dhcp.log.reason.other-server'),
    'pool-exhausted': () => t('dns.dhcp.log.reason.pool-exhausted'),
    'address-unavailable': () => t('dns.dhcp.log.reason.address-unavailable'),
    'move-to-reservation': () => t('dns.dhcp.log.reason.move-to-reservation'),
    'client-id-conflict': () => t('dns.dhcp.log.reason.client-id-conflict'),
  }

  function kindLabel(k: string): string {
    switch (k) {
      case 'dhcpv4':
        return t('dns.dhcp.log.kind.dhcpv4')
      case 'dhcpv6':
        return t('dns.dhcp.log.kind.dhcpv6')
      case 'ra':
        return t('dns.dhcp.log.kind.ra')
    }
    return k
  }

  const now = $derived(log.data ? Date.now() : 0)

  const columns = $derived<Column<Row>[]>([
    { key: 'time', label: t('common.label.time'), width: '1%', value: (e) => e.time, cell: timeCell },
    { key: 'kind', label: t('dns.dhcp.log.kind'), width: '1%', value: (e) => e.kind, format: (e) => kindLabel(e.kind) },
    { key: 'device', label: t('dns.dhcp.leases.device'), value: (e) => e.hostname || e.mac || e.duid || '', cell: deviceCell },
    { key: 'messages', label: t('dns.dhcp.log.messages'), width: '1%', value: (e) => e.in, cell: messagesCell },
    { key: 'result', label: t('dns.dhcp.log.result'), value: (e) => e.result, cell: resultCell },
  ])
</script>

{#snippet timeCell(e: Row)}
  <span class="nowrap" title={formatDateTime(e.time, true)}>
    {sameDay(e.time, now) ? formatTime(e.time, true) : formatDateTimeShort(e.time, true)}
  </span>
{/snippet}

{#snippet deviceCell(e: Row)}
  <span class="two">
    {#if e.hostname}
      <span class="host">{e.hostname}</span>
      {#if e.mac || e.duid}<span class="sub mono">{e.mac ?? e.duid}</span>{/if}
    {:else if e.mac || e.duid}
      <span class="mono id">{e.mac ?? e.duid}</span>
    {:else}
      <span class="subtle">–</span>
    {/if}
    {#if e.address}<span class="sub mono">{e.address}</span>{/if}
  </span>
{/snippet}

{#snippet messagesCell(e: Row)}
  <span class="mono nowrap">{e.in}{#if e.out}<span class="arrow"> → </span>{e.out}{/if}</span>
{/snippet}

{#snippet resultCell(e: Row)}
  <span class="result">
    <Chip size="sm" tone={RESULT[e.result]?.tone ?? 'neutral'} label={RESULT[e.result]?.label() ?? e.result} />
    {#if e.reason}<span class="small muted">{REASON[e.reason]?.() ?? e.reason}</span>{/if}
  </span>
{/snippet}

<Panel id="dhcp-log" flush title={t('dns.dhcp.log.title')} description={t('dns.dhcp.log.description')}>
  {#snippet actions()}
    <IconButton icon="refresh" label={t('common.action.refresh')} loading={log.loading} onclick={() => log.refresh()} />
  {/snippet}
  <Table
    {columns}
    {rows}
    key={(e) => `${e.i}|${e.time}`}
    compact
    loading={log.loading && !log.loaded}
    error={log.error && !log.data ? errorText(log.error) : undefined}
    onretry={() => log.refresh()}
    caption={t('dns.dhcp.log.title')}
    maxHeight="28rem"
    skeletonRows={3}
  >
    {#snippet empty()}
      <EmptyState compact icon="activity" title={t('dns.dhcp.log.empty')} text={t('dns.dhcp.log.emptyText')} />
    {/snippet}
  </Table>
</Panel>

<style>
  .two {
    display: flex;
    flex-direction: column;
    gap: 1px;
    min-width: 0;
    padding: 2px 0;
    line-height: 1.3;
  }
  .host,
  .id {
    white-space: nowrap;
  }
  .sub {
    color: var(--text-3);
    font-size: var(--fs-xs);
    white-space: nowrap;
  }
  .arrow {
    color: var(--text-3);
  }
  .result {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px var(--sp-2);
    min-width: 14ch;
  }
</style>
