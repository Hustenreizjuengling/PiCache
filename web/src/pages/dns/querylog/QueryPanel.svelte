<!--
  @component
  Details of one query-log entry: answer, reason and timing, plus actions:
  block or allow the domain (creates a rule), explain the filter decision,
  narrow the log to this domain or client, open the client and show the
  client's cache traffic around that moment.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type ClientGroup,
    type ExplainResult,
    type FilterList,
    type FilterRuleInput,
    type QueryEvent,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatMicros } from '$lib/format'
  import { href, type QueryPatch } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { isBlockedStatus } from '$lib/traffic'
  import { Button, CopyButton, KeyValue, Notice, QueryStatusChip, SidePanel, Spinner } from '$lib/ui'
  import RulePanel from '../filtering/RulePanel.svelte'
  import MatchList from '../shared/MatchList.svelte'

  interface Props {
    open?: boolean
    event: QueryEvent | undefined
    groups: readonly ClientGroup[] | undefined
    lists: readonly FilterList[] | undefined
    onfilter: (patch: QueryPatch) => void
  }

  let { open = $bindable(false), event, groups, lists, onfilter }: Props = $props()

  const CACHE_WINDOW = 5 * 60 // seconds before and after the query

  let explain = $state.raw<ExplainResult | undefined>(undefined)
  let explainErr = $state.raw<ApiError | undefined>(undefined)
  let explaining = $state(false)
  let ruleOpen = $state(false)
  let rulePreset = $state.raw<Partial<FilterRuleInput>>({})
  let ctrl: AbortController | undefined

  // Another entry: forget the previous explanation.
  $effect(() => {
    void event
    untrack(() => {
      ctrl?.abort()
      explain = undefined
      explainErr = undefined
      explaining = false
    })
  })
  $effect(() => () => ctrl?.abort())

  const blocked = $derived(!!event && isBlockedStatus(event.status))
  const listName = $derived(event?.listId ? (lists?.find((l) => l.id === event.listId)?.name ?? `#${event.listId}`) : '')

  const cacheHref = $derived.by(() => {
    if (!event) return ''
    const at = Math.floor(new Date(event.time).getTime() / 1000)
    return href('/cache/downloads', { client: event.clientIp, from: at - CACHE_WINDOW, to: at + CACHE_WINDOW })
  })

  async function runExplain() {
    const e = event
    if (!e) return
    ctrl?.abort()
    const c = new AbortController()
    ctrl = c
    explaining = true
    explainErr = undefined
    try {
      explain = await api.filter.explain(e.qname, e.clientIp, { signal: c.signal })
    } catch (err) {
      const ae = toApiError(err)
      if (ae.code !== 'aborted') explainErr = ae
    } finally {
      if (ctrl === c) explaining = false
    }
  }

  function createRule() {
    if (!event) return
    rulePreset = {
      action: blocked ? 'allow' : 'block',
      type: 'exact',
      pattern: event.qname,
      groupIds: [DEFAULT_GROUP_ID],
    }
    ruleOpen = true
  }

  function filterBy(patch: QueryPatch) {
    open = false
    onfilter(patch)
  }
</script>

<SidePanel bind:open title={event?.qname ?? ''} subtitle={event ? formatDateTime(event.time, true) : undefined} size="lg">
  {#if event}
    <div class="stack">
      <div class="headline">
        <QueryStatusChip status={event.status} size="md" />
        <span class="muted small">{event.rcode} · {formatMicros(event.durationUs)}</span>
        <span class="spacer"></span>
        <CopyButton text={event.qname} label={t('dns.queryLog.copyDomain')} />
      </div>

      <KeyValue
        items={[
          { label: t('common.label.domain'), value: event.qname, mono: true },
          { label: t('common.label.type'), value: event.qtype },
          { label: t('common.label.client'), value: event.clientName ? `${event.clientName} (${event.clientIp})` : event.clientIp },
          { label: t('dns.queryLog.answer'), value: event.answer, mono: true },
          { label: t('dns.queryLog.reason'), value: event.reason },
        ]}
      >
        {#if event.listId}
          <dt>{t('dns.queryLog.blockedByList')}</dt>
          <dd><a href={href('/dns/filtering', { tab: 'lists', sel: event.listId })}>{listName}</a></dd>
        {/if}
        {#if event.ruleId}
          <dt>{t('dns.queryLog.blockedByRule')}</dt>
          <dd><a href={href('/dns/filtering', { tab: 'rules', sel: event.ruleId })}>{t('dns.queryLog.ruleNumber', { id: event.ruleId })}</a></dd>
        {/if}
        {#if event.service}
          <dt>{t('dns.queryLog.service')}</dt>
          <dd>{event.service}</dd>
        {/if}
        <dt>{t('dns.queryLog.upstream')}</dt>
        <dd class="mono">{event.upstream || '–'}</dd>
        <dt>{t('dns.queryLog.duration')}</dt>
        <dd>{formatMicros(event.durationUs)}</dd>
        <dt>{t('dns.queryLog.protocol')}</dt>
        <dd>{event.protocol.toUpperCase()}{event.dnssec ? ` · ${t('dns.queryLog.dnssec')}` : ''}</dd>
      </KeyValue>

      <div class="row">
        <Button
          variant={blocked ? 'primary' : 'secondary'}
          icon={blocked ? 'shield-off' : 'shield'}
          disabled={!session.isAdmin}
          onclick={createRule}
        >
          {blocked ? t('dns.queryLog.allowDomain') : t('dns.queryLog.blockDomain')}
        </Button>
        <Button icon="info" loading={explaining} onclick={runExplain}>{t('dns.queryLog.explain')}</Button>
        <Button icon="user" href={href('/dns/clients', { ip: event.clientIp })}>{t('dns.queryLog.openClient')}</Button>
        <Button icon="download" href={cacheHref}>{t('dns.queryLog.cacheTraffic')}</Button>
      </div>
      <div class="row">
        <Button size="sm" variant="ghost" icon="filter" onclick={() => filterBy({ domain: `"${event.qname}"` })}>
          {t('dns.queryLog.onlyDomain')}
        </Button>
        <Button size="sm" variant="ghost" icon="filter" onclick={() => filterBy({ client: event.clientIp })}>
          {t('dns.queryLog.onlyClient')}
        </Button>
      </div>

      {#if explaining && !explain}
        <p class="row muted small"><Spinner size={16} />{t('dns.queryLog.explaining')}</p>
      {/if}
      {#if explainErr}
        <Notice tone="fail">{errorText(explainErr)}</Notice>
      {/if}
      {#if explain}
        <section class="stack-sm" aria-labelledby="explain-title">
          <h3 id="explain-title">{t('dns.queryLog.explainTitle')}</h3>
          <MatchList matches={explain.matches} {groups} groupIds={explain.groupIds} />
          <p class="small">
            <a href={href('/dns/filtering', { tab: 'test', domain: event.qname, client: event.clientIp })}>
              {t('dns.queryLog.openTester')}
            </a>
          </p>
        </section>
      {/if}
    </div>
  {/if}
</SidePanel>

<RulePanel bind:open={ruleOpen} preset={rulePreset} {groups} onsaved={() => explain && runExplain()} />

<style>
  .headline {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
  h3 {
    font-size: var(--fs-md);
  }
</style>
