<!--
  @component
  The delivery log: the last delivery attempts (in memory on the server,
  newest first) with the channel, the message, its severity, whether it was
  delivered and the error of a failed attempt.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, NotifyDelivery, NotifyEvent } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatDateTimeShort } from '$lib/format'
  import { Chip, EmptyState, IconButton, Panel, Table, type Column } from '$lib/ui'
  import { eventText, severityLabel, severityTone } from './channels'

  interface Props {
    entries: readonly NotifyDelivery[] | undefined
    loading: boolean
    error: ApiError | undefined
    catalog: readonly NotifyEvent[] | undefined
    onrefresh: () => void
  }

  let { entries, loading, error, catalog, onrefresh }: Props = $props()

  function key(e: NotifyDelivery): string {
    return `${e.time}|${e.channelId}|${e.event}|${e.attempt}|${e.title}`
  }

  // Server order (newest first); several attempts can share a timestamp, so
  // make the keys unique by position as well.
  const rows = $derived(entries?.map((e, i) => ({ ...e, i })))
  type Row = NotifyDelivery & { i: number }

  const columns = $derived<Column<Row>[]>([
    { key: 'time', label: t('common.label.time'), width: '1%', cell: timeCell, value: (e) => e.time },
    { key: 'channel', label: t('system.notifications.log.channel'), width: '1%', cell: channelCell, value: (e) => e.channelName },
    { key: 'message', label: t('system.notifications.log.message'), cell: messageCell, value: (e) => e.title },
    { key: 'severity', label: t('system.notifications.log.severity'), width: '1%', cell: severityCell, value: (e) => e.severity },
    { key: 'result', label: t('system.notifications.log.result'), width: '1%', cell: resultCell, value: (e) => (e.ok ? 1 : 0) },
    { key: 'error', label: t('system.notifications.log.error'), cell: errorCell, value: (e) => e.error },
  ])
</script>

{#snippet timeCell(e: Row)}
  <span class="nowrap" title={formatDateTime(e.time, true)}>{formatDateTimeShort(e.time, true)}</span>
{/snippet}

{#snippet channelCell(e: Row)}
  <span class="channel">{e.channelName || e.channelId}</span>
{/snippet}

{#snippet messageCell(e: Row)}
  <span class="two">
    <span class="msg">{e.title || '–'}</span>
    <span class="sub">{eventText(e.event, catalog).title}</span>
  </span>
{/snippet}

{#snippet severityCell(e: Row)}
  <Chip size="sm" tone={severityTone(e.severity)} label={severityLabel(e.severity)} />
{/snippet}

{#snippet resultCell(e: Row)}
  <span class="two">
    {#if e.ok}
      <Chip size="sm" tone="ok" label={t('system.notifications.log.delivered')} />
    {:else}
      <Chip size="sm" tone="fail" label={t('system.notifications.log.failed')} />
    {/if}
    {#if e.attempt > 1}<span class="sub nowrap">{t('system.notifications.log.attempt', { n: e.attempt })}</span>{/if}
  </span>
{/snippet}

{#snippet errorCell(e: Row)}
  {#if e.error}<span class="error">{e.error}</span>{:else}<span class="subtle">–</span>{/if}
{/snippet}

<Panel title={t('system.notifications.log.title')} description={t('system.notifications.log.description')} flush>
  {#snippet actions()}
    <IconButton icon="refresh" label={t('common.action.refresh')} loading={loading} onclick={onrefresh} />
  {/snippet}
  <Table
    {columns}
    {rows}
    key={(e) => `${e.i}|${key(e)}`}
    loading={loading && !entries}
    error={error && !entries ? errorText(error) : undefined}
    onretry={onrefresh}
    caption={t('system.notifications.log.title')}
    maxHeight="36rem"
    skeletonRows={3}
  >
    {#snippet empty()}
      <EmptyState compact icon="document" title={t('system.notifications.log.emptyTitle')} text={t('system.notifications.log.emptyText')} />
    {/snippet}
  </Table>
</Panel>

<style>
  .two {
    display: flex;
    flex-direction: column;
    align-items: flex-start;
    gap: 2px;
    min-width: 0;
    padding: var(--sp-1) 0;
  }
  .channel {
    display: block;
    min-width: 10ch;
    max-width: 24ch;
    overflow-wrap: anywhere;
  }
  .msg {
    min-width: 12ch;
    overflow-wrap: anywhere;
  }
  .sub {
    color: var(--text-3);
    font-size: var(--fs-xs);
  }
  .error {
    display: block;
    min-width: 18ch;
    max-width: 48ch;
    padding: var(--sp-1) 0;
    color: var(--text-2);
    overflow-wrap: anywhere;
  }
</style>
