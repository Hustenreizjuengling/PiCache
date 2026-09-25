<!--
  @component
  Reference of the events PiCache notifies about (GET /notifications/events,
  readable by everyone): title, key, default severity and when it is sent.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, NotifyEvent } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { Chip, Panel, Table, type Column } from '$lib/ui'
  import { eventText, severityLabel, severityTone } from './channels'

  interface Props {
    events: readonly NotifyEvent[] | undefined
    loading: boolean
    error: ApiError | undefined
    onretry: () => void
  }

  let { events, loading, error, onretry }: Props = $props()

  const columns = $derived<Column<NotifyEvent>[]>([
    { key: 'event', label: t('system.notifications.events.event'), width: '1%', cell: eventCell, value: (e) => e.key },
    { key: 'severity', label: t('system.notifications.events.severity'), width: '1%', cell: severityCell, value: (e) => e.severity },
    { key: 'description', label: t('common.label.details'), cell: descriptionCell },
  ])
</script>

{#snippet eventCell(e: NotifyEvent)}
  <span class="two">
    <span class="strong">{eventText(e.key, events).title}</span>
    <span class="mono sub">{e.key}</span>
  </span>
{/snippet}

{#snippet severityCell(e: NotifyEvent)}
  <Chip size="sm" tone={severityTone(e.severity)} label={severityLabel(e.severity)} />
{/snippet}

{#snippet descriptionCell(e: NotifyEvent)}
  <span class="desc">{eventText(e.key, events).description || '–'}</span>
{/snippet}

<Panel title={t('system.notifications.events.title')} description={t('system.notifications.events.description')} flush>
  <Table
    {columns}
    rows={events}
    key={(e) => e.key}
    loading={loading && !events}
    error={error && !events ? errorText(error) : undefined}
    {onretry}
    caption={t('system.notifications.events.title')}
    skeletonRows={4}
  />
</Panel>

<style>
  .two {
    display: flex;
    flex-direction: column;
    gap: 2px;
    padding: var(--sp-1) 0;
  }
  .strong {
    font-weight: 600;
    white-space: nowrap;
  }
  .sub {
    color: var(--text-3);
    font-size: var(--fs-xs);
    white-space: nowrap;
  }
  .desc {
    display: block;
    min-width: 24ch;
    padding: var(--sp-1) 0;
    color: var(--text-2);
  }
</style>
