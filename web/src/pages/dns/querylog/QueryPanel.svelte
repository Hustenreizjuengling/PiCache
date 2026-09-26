<!--
  @component
  Details of one query-log entry: answer, reason, timing, the upstream's
  extended error and the client's subnet, plus actions: block or allow the
  domain (creates a rule), explain the filter decision, narrow the log to
  this domain or client, open the client, show the client's cache traffic
  around that moment and block the device (admins, not while client
  addresses are anonymised). Answers blocked by the upstream say why a rule
  cannot help; answers blocked by rebinding protection offer to allow the
  name.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type BlockClientResult,
    type ClientGroup,
    type ExplainResult,
    type FilterList,
    type FilterRuleInput,
    type QueryEvent,
    type Settings,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatMicros } from '$lib/format'
  import { href, type QueryPatch } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { isBlockedStatus } from '$lib/traffic'
  import { Button, CopyButton, KeyValue, Notice, QueryStatusChip, SidePanel, Spinner, toast } from '$lib/ui'
  import RulePanel from '../filtering/RulePanel.svelte'
  import { confirmBlockDevice } from '../shared/blockClient'
  import MatchList from '../shared/MatchList.svelte'
  import { edeText } from './ede'

  interface Props {
    open?: boolean
    event: QueryEvent | undefined
    groups: readonly ClientGroup[] | undefined
    lists: readonly FilterList[] | undefined
    /** Current settings (rebinding allow list, anonymised client addresses); undefined until loaded. */
    settings: Settings | undefined
    onsettings: (s: Settings) => void
    onfilter: (patch: QueryPatch) => void
  }

  let { open = $bindable(false), event, groups, lists, settings, onsettings, onfilter }: Props = $props()

  const CACHE_WINDOW = 5 * 60 // seconds before and after the query

  let explain = $state.raw<ExplainResult | undefined>(undefined)
  let explainErr = $state.raw<ApiError | undefined>(undefined)
  let explaining = $state(false)
  let ruleOpen = $state(false)
  let rulePreset = $state.raw<Partial<FilterRuleInput>>({})
  let blockResult = $state.raw<BlockClientResult | undefined>(undefined)
  let allowing = $state(false)
  let ctrl: AbortController | undefined

  // Another entry: forget the previous explanation and block result.
  $effect(() => {
    void event
    untrack(() => {
      ctrl?.abort()
      explain = undefined
      explainErr = undefined
      explaining = false
      blockResult = undefined
    })
  })
  $effect(() => () => ctrl?.abort())

  const blocked = $derived(!!event && isBlockedStatus(event.status))
  // An allow rule cannot lift an upstream's block: the upstream sent no usable answer.
  // (For rebinding the allow list is the first choice; an allow rule exempts the name too.)
  const ruleAction = $derived(!event || event.status === 'blocked-upstream' ? undefined : blocked ? 'allow' : 'block')
  const listName = $derived(event?.listId ? (lists?.find((l) => l.id === event.listId)?.name ?? `#${event.listId}`) : '')

  const cacheHref = $derived.by(() => {
    if (!event) return ''
    const at = Math.floor(new Date(event.time).getTime() / 1000)
    return href('/cache/downloads', { client: event.clientIp, from: at - CACHE_WINDOW, to: at + CACHE_WINDOW })
  })

  // Anonymised addresses (logs.anonymizeClientIps) name a whole network, not the device.
  const canBlockDevice = $derived(session.isAdmin && !!settings && !settings.logs.anonymizeClientIps)

  /** The query name as stored in dns.rebindAllow (lower case, no trailing dot). */
  const rebindName = $derived(event ? event.qname.toLowerCase().replace(/\.$/, '') : '')
  /** The address rebinding protection objected to ("rebind: 192.168.1.20"). */
  const rebindAddress = $derived(event?.reason?.startsWith('rebind: ') ? event.reason.slice(8) : '')
  // An entry covers its subdomains.
  const rebindAllowedBy = $derived(
    settings?.dns.rebindAllow.find((d) => rebindName === d || rebindName.endsWith(`.${d}`)),
  )

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
    if (!event || !ruleAction) return
    rulePreset = {
      action: ruleAction,
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

  async function blockDevice() {
    const e = event
    if (!e) return
    const res = await confirmBlockDevice(e.clientIp, e.clientName)
    if (res && event === e) blockResult = res
  }

  // Saves only the allow list; the name and its subdomains may then point into the network.
  // The list is read right before the write: another tab or admin may have changed it
  // since the page loaded, and those changes must not be lost or undone.
  async function allowRebinding() {
    const name = rebindName
    if (!name) return
    allowing = true
    try {
      const cur = await api.settings.get()
      const list = cur.dns.rebindAllow
      const covered = list.some((d) => name === d || name.endsWith(`.${d}`))
      onsettings(covered ? cur : await api.settings.patch('dns', { rebindAllow: [...list, name] }))
      toast.success(t('dns.queryLog.rebindAllowed', { name }))
    } catch (err) {
      toast.error(err)
    } finally {
      allowing = false
    }
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
        {#if event.upstreamEde}
          <dt>{t('dns.queryLog.upstreamEde')}</dt>
          <dd class="ede">{edeText(event.upstreamEde)}</dd>
        {/if}
        {#if event.ecs}
          <dt>{t('dns.queryLog.ecs')}</dt>
          <dd class="mono">{event.ecs}</dd>
        {/if}
        <dt>{t('dns.queryLog.duration')}</dt>
        <dd>{formatMicros(event.durationUs)}</dd>
        <dt>{t('dns.queryLog.protocol')}</dt>
        <dd>{event.protocol.toUpperCase()}{event.dnssec ? ` · ${t('dns.queryLog.dnssec')}` : ''}</dd>
      </KeyValue>

      {#if event.status === 'blocked-upstream'}
        <Notice tone="info" title={t('dns.queryLog.upstreamBlockTitle')}>
          {t('dns.queryLog.upstreamBlockText')}
          {#snippet actions()}
            <Button size="sm" icon="link" href={href('/dns/local', { tab: 'forwarders' })}>{t('dns.queryLog.openForwarders')}</Button>
          {/snippet}
        </Notice>
      {:else if event.status === 'blocked-rebind'}
        <Notice tone="info" title={t('dns.queryLog.rebindTitle')}>
          <p>{rebindAddress ? t('dns.queryLog.rebindText', { address: rebindAddress }) : t('dns.queryLog.rebindTextNoAddress')}</p>
          {#if rebindAllowedBy}
            <p class="small">{t('dns.queryLog.rebindAllowedBy', { entry: rebindAllowedBy })}</p>
          {/if}
          {#snippet actions()}
            {#if session.isAdmin && settings && !rebindAllowedBy}
              <span class="long">
                <Button size="sm" variant="primary" icon="shield-off" loading={allowing} onclick={allowRebinding}>
                  {t('dns.queryLog.allowRebinding', { name: rebindName })}
                </Button>
              </span>
            {/if}
            <Button size="sm" variant="ghost" href={href('/dns/settings', { section: 'protection' })}>{t('dns.queryLog.rebindSetting')}</Button>
          {/snippet}
        </Notice>
      {/if}

      <div class="row">
        {#if ruleAction}
          <Button
            variant={ruleAction === 'allow' && event.status !== 'blocked-rebind' ? 'primary' : 'secondary'}
            icon={ruleAction === 'allow' ? 'shield-off' : 'shield'}
            disabled={!session.isAdmin}
            onclick={createRule}
          >
            {ruleAction === 'allow' ? t('dns.queryLog.allowDomain') : t('dns.queryLog.blockDomain')}
          </Button>
        {/if}
        <Button icon="info" loading={explaining} onclick={runExplain}>{t('dns.queryLog.explain')}</Button>
        <Button icon="user" href={href('/dns/clients', { ip: event.clientIp })}>{t('dns.queryLog.openClient')}</Button>
        <Button icon="download" href={cacheHref}>{t('dns.queryLog.cacheTraffic')}</Button>
        {#if canBlockDevice && !blockResult}
          <Button icon="ban" onclick={blockDevice}>{t('dns.shared.blockDevice')}</Button>
        {/if}
      </div>
      <div class="row">
        <Button size="sm" variant="ghost" icon="filter" onclick={() => filterBy({ domain: `"${event.qname}"` })}>
          {t('dns.queryLog.onlyDomain')}
        </Button>
        <Button size="sm" variant="ghost" icon="filter" onclick={() => filterBy({ client: event.clientIp })}>
          {t('dns.queryLog.onlyClient')}
        </Button>
      </div>

      {#if blockResult}
        <Notice tone={blockResult.added ? 'ok' : 'info'}>
          {blockResult.added ? t('dns.queryLog.blockedNow', { entry: blockResult.entry }) : t('dns.shared.alreadyBlocked', { entry: blockResult.entry })}
          {#snippet actions()}
            <Button size="sm" variant="ghost" href={href('/dns/settings', { section: 'access' })}>{t('dns.queryLog.blockedSetting')}</Button>
          {/snippet}
        </Notice>
      {/if}

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
  .ede {
    overflow-wrap: anywhere;
  }
  /* The button names the query: a long name wraps instead of leaving the panel. */
  .long {
    min-width: 0;
    max-width: 100%;
  }
  .long :global(.btn) {
    max-width: 100%;
    height: auto;
    min-height: var(--control-h-sm);
    padding-block: 6px;
    line-height: 1.3;
    white-space: normal;
    text-align: left;
    overflow-wrap: anywhere;
  }
</style>
