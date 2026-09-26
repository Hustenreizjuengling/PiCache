<!--
  @component
  One notification channel in a side panel: send a test message (the result
  stays next to the button), its settings, the events it gets, edit and
  delete. The secret is only reported as stored or not.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { NotifyChannel, NotifyEvent } from '$lib/api'
  import { formatDateTime } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, KeyValue, Notice, SidePanel } from '$lib/ui'
  import { displayUrl, eventText, kindLabel, passes, severityLabel, severityTone, type TestState } from './channels'
  import TestResult from './TestResult.svelte'

  interface Props {
    /** The selected channel; undefined when the id in the URL is unknown. */
    channel: NotifyChannel | undefined
    /** The list has loaded (so an unknown id means the channel is gone). */
    loaded: boolean
    catalog: readonly NotifyEvent[] | undefined
    test: TestState | undefined
    ontest: (c: NotifyChannel) => void
    ondismisstest: (c: NotifyChannel) => void
    onedit: (c: NotifyChannel) => void
    ondelete: (c: NotifyChannel) => void
    onclose: () => void
  }

  let { channel, loaded, catalog, test, ontest, ondismisstest, onedit, ondelete, onclose }: Props = $props()

  /** Events of the channel, or undefined for all. */
  const selectedEvents = $derived(channel && channel.events.length > 0 ? channel.events : undefined)

  function severityOf(key: string) {
    return catalog?.find((e) => e.key === key)?.severity
  }
</script>

<SidePanel
  open
  title={channel?.name ?? t('system.notifications.channels.title')}
  subtitle={channel ? kindLabel(channel.kind) : undefined}
  {onclose}
>
  {#if channel}
    {@const c = channel}
    <div class="stack">
      {#if !c.enabled}
        <Notice tone="info">{t('system.notifications.off')}</Notice>
      {/if}

      <section class="stack-sm" aria-label={t('system.notifications.test')}>
        <div class="row">
          <Button variant="primary" icon="send" loading={test?.running} disabled={!session.canOperate} onclick={() => ontest(c)}>
            {t('system.notifications.test')}
          </Button>
        </div>
        <p class="small muted">{t('system.notifications.detail.test')}</p>
        {#if test}
          <TestResult name={c.name} {test} ondismiss={test.running ? undefined : () => ondismisstest(c)} />
        {/if}
      </section>

      <KeyValue
        items={[
          { label: t('common.label.type'), value: kindLabel(c.kind) },
          { label: t('system.notifications.col.address'), value: displayUrl(c.url), mono: true },
          {
            label: t(`system.notifications.secret.${c.kind}`),
            value: c.hasSecret ? t('system.notifications.secretStored') : t('common.state.none'),
          },
          { label: t('common.label.status'), value: c.enabled ? t('common.state.enabled') : t('common.state.disabled') },
          { label: t('system.notifications.col.sends'), value: t(`system.notifications.min.${c.minSeverity}`) },
          { label: t('common.label.created'), value: formatDateTime(c.createdAt) },
          { label: t('common.label.updated'), value: formatDateTime(c.updatedAt) },
        ]}
      />

      <section class="stack-sm">
        <h3>{t('system.notifications.col.events')}</h3>
        {#if !selectedEvents}
          <p class="small muted">{t('system.notifications.detail.allEvents')}</p>
        {:else}
          <ul class="events">
            {#each selectedEvents as key (key)}
              {@const sev = severityOf(key)}
              {@const below = !!sev && !passes(c.minSeverity, sev)}
              <li>
                <span class="ev">
                  <span class={{ muted: below }}>{eventText(key, catalog).title}</span>
                  {#if below}<span class="xsmall muted">{t('system.notifications.detail.below')}</span>{/if}
                </span>
                {#if sev}<Chip size="sm" tone={severityTone(sev)} label={severityLabel(sev)} />{/if}
              </li>
            {/each}
          </ul>
        {/if}
      </section>
    </div>
  {:else if loaded}
    <p class="small muted">{t('system.notifications.gone')}</p>
  {/if}
  {#snippet actions()}
    {#if channel}
      {@const c = channel}
      <Button variant="danger" icon="trash" disabled={!session.isAdmin} onclick={() => ondelete(c)}>
        {t('system.notifications.delete')}
      </Button>
      <Button icon="edit" disabled={!session.isAdmin} onclick={() => onedit(c)}>{t('common.action.edit')}</Button>
    {/if}
  {/snippet}
</SidePanel>

<style>
  h3 {
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .events {
    margin: 0;
    padding: 0;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    font-size: var(--fs-sm);
  }
  .events li {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--sp-3);
  }
  .ev {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
  }
</style>
