<!--
  @component
  Details of one query-log entry: answer, reason, timing, the upstream's
  extended error and the client's subnet, plus actions: block or allow the
  domain (creates a rule), explain the filter decision, narrow the log to
  this domain or client, open the client, show the client's cache traffic
  around that moment and block the device (admins, not while client
  addresses are anonymised, nor for an entry whose address was). Answers blocked by the upstream say why a rule
  cannot help; answers blocked by rebinding protection offer to allow the
  name; safe-search answers point to parental controls. After "Explain",
  a client in exactly one group other than Default can have that group's
  filtering paused (admins, not while addresses are anonymised). The
  explanation names the enabled privacy lists that contain the domain as
  "Known tracker" (PiCache has no tracker database of its own). The
  upstream's own answer is shown where it differs from the final one.
  Entries recorded while domains were hidden offer no domain actions.
  "Only for this device" creates the rule for a group that holds only the
  client (admins, not while addresses are anonymised). Answers blocked for
  an address in them link the list or IP rule and explain that an allow
  rule for the name lifts the check.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import {
    api,
    DEFAULT_GROUP_ID,
    toApiError,
    type ApiError,
    type BlockClientResult,
    type ClientGroup,
    type DeviceRuleResult,
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
  import PauseMenu from '../shared/PauseMenu.svelte'
  import { edeText } from './ede'
  import { looksAnonymised } from './filters'

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
  /** The query name of entries recorded while logs.hideDomains was on. */
  const HIDDEN = 'hidden'

  let explain = $state.raw<ExplainResult | undefined>(undefined)
  let explainErr = $state.raw<ApiError | undefined>(undefined)
  let explaining = $state(false)
  let ruleOpen = $state(false)
  let rulePreset = $state.raw<Partial<FilterRuleInput>>({})
  /** The rule panel is in device mode ("Only for this device"). */
  let ruleDevice = $state.raw<{ ip: string; name?: string } | undefined>(undefined)
  let deviceResult = $state.raw<DeviceRuleResult | undefined>(undefined)
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
      deviceResult = undefined
    })
  })
  $effect(() => () => ctrl?.abort())

  const blocked = $derived(!!event && isBlockedStatus(event.status))
  /** The domain was not recorded (privacy setting "Hide domains"). */
  const hidden = $derived(event?.qname === HIDDEN)
  // An allow rule cannot lift an upstream's block: the upstream sent no usable answer.
  // (For rebinding the allow list is the first choice; an allow rule exempts the name too.)
  const ruleAction = $derived(!event || hidden || event.status === 'blocked-upstream' ? undefined : blocked ? 'allow' : 'block')

  /**
   * Enabled privacy lists blocking the domain: PiCache's substitute for a tracker database.
   * An exception of a list (action allow) says the opposite and does not count.
   */
  const trackers = $derived([
    ...new Set(
      (explain?.matches ?? [])
        .filter((m) => m.source === 'list' && m.action === 'block' && m.category === 'privacy')
        .map((m) => m.name),
    ),
  ])
  const listName = $derived(event?.listId ? (lists?.find((l) => l.id === event.listId)?.name ?? `#${event.listId}`) : '')

  const cacheHref = $derived.by(() => {
    if (!event) return ''
    const at = Math.floor(new Date(event.time).getTime() / 1000)
    return href('/cache/downloads', { client: event.clientIp, from: at - CACHE_WINDOW, to: at + CACHE_WINDOW })
  })

  // Anonymised addresses (logs.anonymizeClientIps, also of entries logged while it was on) name a
  // whole network, not the device.
  const canBlockDevice = $derived(
    session.isAdmin && !!settings && !settings.logs.anonymizeClientIps && !!event && !looksAnonymised(event.clientIp),
  )
  /** "Only for this device": next to the rule action, for an entry with a client address. */
  const canDeviceRule = $derived(!!ruleAction && canBlockDevice && !!event?.clientIp)
  /** The IP rule that blocked an answer (ruleId of blocked-ip entries names an IP rule). */
  const ipRuleId = $derived(event?.status === 'blocked-ip' ? event.ruleId : undefined)

  // Pausing a group from here is offered only when the client is in exactly one
  // group and that is not Default (which covers every unknown device).
  const pauseGroup = $derived.by(() => {
    const ids = explain?.groupIds ?? []
    if (!canBlockDevice || ids.length !== 1 || ids[0] === DEFAULT_GROUP_ID) return undefined
    return groups?.find((g) => g.id === ids[0])
  })

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
      explain = await api.filter.explain(e.qname, e.clientIp, e.qtype, { signal: c.signal })
    } catch (err) {
      const ae = toApiError(err)
      if (ae.code !== 'aborted') explainErr = ae
    } finally {
      if (ctrl === c) explaining = false
    }
  }

  function createRule(forDevice = false) {
    if (!event || !ruleAction) return
    rulePreset = {
      action: ruleAction,
      type: 'exact',
      pattern: event.qname,
      groupIds: [DEFAULT_GROUP_ID],
    }
    ruleDevice = forDevice ? { ip: event.clientIp, name: event.clientName } : undefined
    ruleOpen = true
  }

  function deviceDone(res: DeviceRuleResult) {
    deviceResult = res
    if (explain) void runExplain()
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

<SidePanel
  bind:open
  title={hidden ? t('dns.queryLog.hiddenShort') : (event?.qname ?? '')}
  subtitle={event ? formatDateTime(event.time, true) : undefined}
  size="lg"
>
  {#if event}
    <div class="stack">
      <div class="headline">
        <QueryStatusChip status={event.status} size="md" />
        <span class="muted small">{event.rcode} · {formatMicros(event.durationUs)}</span>
        <span class="spacer"></span>
        {#if !hidden}<CopyButton text={event.qname} label={t('dns.queryLog.copyDomain')} />{/if}
      </div>

      {#if hidden}<p class="small muted">{t('dns.queryLog.hiddenDomain')}</p>{/if}

      <KeyValue
        items={[
          { label: t('common.label.domain'), value: hidden ? t('dns.queryLog.hiddenShort') : event.qname, mono: !hidden },
          { label: t('common.label.type'), value: event.qtype },
          { label: t('common.label.client'), value: event.clientName ? `${event.clientName} (${event.clientIp})` : event.clientIp },
          { label: t('dns.queryLog.answer'), value: event.answer, mono: true },
          ...(event.upstreamAnswer ? [{ label: t('dns.queryLog.upstreamAnswer'), value: event.upstreamAnswer, mono: true }] : []),
          { label: t('dns.queryLog.reason'), value: event.reason },
        ]}
      >
        {#if event.listId}
          <dt>{t('dns.queryLog.blockedByList')}</dt>
          <dd><a href={href('/dns/filtering', { tab: 'lists', sel: event.listId })}>{listName}</a></dd>
        {/if}
        {#if ipRuleId}
          <dt>{t('dns.queryLog.blockedByIpRule')}</dt>
          <dd><a href={href('/dns/filtering', { tab: 'rules', view: 'ip', sel: ipRuleId })}>{t('dns.queryLog.ipRuleNumber', { id: ipRuleId })}</a></dd>
        {:else if event.ruleId}
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
            {#if session.isAdmin && settings && !rebindAllowedBy && !hidden}
              <span class="long">
                <Button size="sm" variant="primary" icon="shield-off" loading={allowing} onclick={allowRebinding}>
                  {t('dns.queryLog.allowRebinding', { name: rebindName })}
                </Button>
              </span>
            {/if}
            <Button size="sm" variant="ghost" href={href('/dns/settings', { section: 'protection' })}>{t('dns.queryLog.rebindSetting')}</Button>
          {/snippet}
        </Notice>
      {:else if event.status === 'blocked-ip'}
        <Notice tone="info" title={t('dns.queryLog.ipBlockTitle')}>{t('dns.queryLog.ipBlockText')}</Notice>
      {:else if event.status === 'safesearch'}
        <Notice tone="info" title={t('dns.queryLog.safeSearchTitle')}>
          {t('dns.queryLog.safeSearchText')}
          {#snippet actions()}
            <Button size="sm" icon="family" href={href('/dns/parental')}>{t('dns.queryLog.openParental')}</Button>
          {/snippet}
        </Notice>
      {/if}

      <div class="row">
        {#if ruleAction}
          <Button
            variant={ruleAction === 'allow' && event.status !== 'blocked-rebind' ? 'primary' : 'secondary'}
            icon={ruleAction === 'allow' ? 'shield-off' : 'shield'}
            disabled={!session.isAdmin}
            onclick={() => createRule()}
          >
            {ruleAction === 'allow' ? t('dns.queryLog.allowDomain') : t('dns.queryLog.blockDomain')}
          </Button>
        {/if}
        {#if canDeviceRule}
          <Button icon="user" title={t('dns.queryLog.onlyDeviceHelp')} onclick={() => createRule(true)}>
            {ruleAction === 'allow' ? t('dns.queryLog.allowForDevice') : t('dns.queryLog.blockForDevice')}
          </Button>
        {/if}
        {#if !hidden}
          <Button icon="info" loading={explaining} onclick={runExplain}>{t('dns.queryLog.explain')}</Button>
        {/if}
        <Button icon="user" href={href('/dns/clients', { ip: event.clientIp })}>{t('dns.queryLog.openClient')}</Button>
        <Button icon="download" href={cacheHref}>{t('dns.queryLog.cacheTraffic')}</Button>
        {#if canBlockDevice && !blockResult}
          <Button icon="ban" onclick={blockDevice}>{t('dns.shared.blockDevice')}</Button>
        {/if}
      </div>
      <div class="row">
        {#if !hidden}
          <Button size="sm" variant="ghost" icon="filter" onclick={() => filterBy({ domain: `"${event.qname}"` })}>
            {t('dns.queryLog.onlyDomain')}
          </Button>
        {/if}
        <Button size="sm" variant="ghost" icon="filter" onclick={() => filterBy({ client: event.clientIp })}>
          {t('dns.queryLog.onlyClient')}
        </Button>
      </div>

      {#if deviceResult}
        {@const r = deviceResult}
        <Notice tone="ok" title={t('dns.queryLog.deviceRuleTitle', { client: r.client.name, group: r.group.name })}>
          <ul class="done small">
            <li>{r.createdClient ? t('dns.queryLog.deviceClientCreated', { name: r.client.name }) : t('dns.queryLog.deviceClientUsed', { name: r.client.name })}</li>
            <li>{r.createdGroup ? t('dns.queryLog.deviceGroupCreated', { name: r.group.name }) : t('dns.queryLog.deviceGroupUsed', { name: r.group.name })}</li>
            <li>{r.createdRule ? t('dns.queryLog.deviceRuleCreated', { pattern: r.rule.pattern }) : t('dns.queryLog.deviceRuleShared', { pattern: r.rule.pattern })}</li>
          </ul>
          {#snippet actions()}
            <Button size="sm" variant="ghost" href={href('/dns/filtering', { tab: 'rules', sel: r.rule.id })}>{t('dns.queryLog.openRule')}</Button>
            <Button size="sm" variant="ghost" href={href('/dns/clients', { tab: 'groups', sel: r.group.id })}>{t('dns.queryLog.openGroup')}</Button>
          {/snippet}
        </Notice>
      {/if}

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
          {#if trackers.length > 0}
            <Notice tone="info" title={t('dns.queryLog.tracker', { lists: trackers.join(', ') })}>
              {t('dns.queryLog.trackerHelp')}
            </Notice>
          {/if}
          <MatchList matches={explain.matches} {groups} groupIds={explain.groupIds} />
          {#if pauseGroup}
            <div class="row">
              <PauseMenu
                groupId={pauseGroup.id}
                groupName={pauseGroup.name}
                clientCount={pauseGroup.clientCount}
                label={t('dns.pause.buttonNamed', { group: pauseGroup.name, devices: tn('dns.parental.devices', pauseGroup.clientCount) })}
                size="md"
                align="start"
                onpaused={() => undefined}
              />
            </div>
          {/if}
          <p class="small">
            <a href={href('/dns/filtering', { tab: 'test', domain: event.qname, client: event.clientIp, qtype: event.qtype === 'A' ? undefined : event.qtype })}>
              {t('dns.queryLog.openTester')}
            </a>
          </p>
        </section>
      {/if}
    </div>
  {/if}
</SidePanel>

<RulePanel
  bind:open={ruleOpen}
  preset={rulePreset}
  device={ruleDevice}
  {groups}
  onsaved={() => explain && runExplain()}
  ondevice={deviceDone}
/>

<style>
  .done {
    margin: 0;
    padding-left: var(--sp-4);
    overflow-wrap: anywhere;
  }
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
