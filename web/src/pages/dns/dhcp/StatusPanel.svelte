<!--
  @component
  The state of the DHCP server in one sentence (off, waiting, serving,
  error, not available) and what it hands out: the interface with PiCache's
  address, the range and how much of it is in use, router, DNS server,
  domain and lease time. While it waits, every blocker in plain words with
  what to do; while it is not available, how to allow it. Admins switch the
  server on and off here (not while the form below has unsaved changes).
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { DhcpBlocker, DhcpSettings, DhcpState, DhcpStatus } from '$lib/api'
  import { formatDuration, formatNumber } from '$lib/format'
  import type { IconName } from '$lib/icons'
  import { session } from '$lib/session.svelte'
  import { Button, Chip, Icon, Meter, Notice, Panel, Trans, type Tone } from '$lib/ui'
  import AllowHowTo from './AllowHowTo.svelte'
  import OtherServerList from './OtherServerList.svelte'
  import { subnetOf, subnetText } from './net'

  interface Props {
    status: DhcpStatus
    /** Saved settings (undefined until loaded). */
    settings: DhcpSettings | undefined
    /** The settings below have unsaved changes. */
    dirty: boolean
    switching: boolean
    probing: boolean
    onswitchon: () => void
    onswitchoff: () => void
    onprobe: () => void
    /** Scrolls to the other DHCP servers ("serve anyway" is there). */
    onshowothers: () => void
    /** Scrolls to the set-up. */
    onshowsetup: () => void
  }

  let { status, settings, dirty, switching, probing, onswitchon, onswitchoff, onprobe, onshowothers, onshowsetup }: Props = $props()

  const VIEW: Record<DhcpState, { tone: Tone; icon: IconName }> = {
    unavailable: { tone: 'neutral', icon: 'info' },
    off: { tone: 'neutral', icon: 'power' },
    blocked: { tone: 'warn', icon: 'alert' },
    serving: { tone: 'ok', icon: 'success' },
    error: { tone: 'fail', icon: 'error' },
  }

  const st = $derived<DhcpState>(VIEW[status.state] ? status.state : 'error')
  // Right after switching on or a restart the server searches for other
  // DHCP servers (about 5 s) and reports that as the other-server blocker
  // without any server found yet.
  const searching = $derived(
    st === 'blocked' &&
      !status.error &&
      (status.blockers ?? []).length === 1 &&
      status.blockers?.[0] === 'other-server' &&
      (status.otherServers ?? []).length === 0,
  )
  const view = $derived(searching ? { tone: 'info' as Tone, icon: 'search' as IconName } : VIEW[st])
  const enabled = $derived(!!settings?.enabled)
  const iface = $derived(status.interface)
  const configured = $derived(!!settings?.interface && !!settings.rangeStart && !!settings.rangeEnd)
  const canSwitchOn = $derived(status.available && configured && !dirty)
  const others = $derived(status.otherServers ?? [])

  const title = $derived(
    searching
      ? t('dns.dhcp.title.searching')
      : {
      unavailable: t('dns.dhcp.title.unavailable'),
      off: t('dns.dhcp.title.off'),
      blocked: t('dns.dhcp.title.blocked'),
      serving: t('dns.dhcp.title.serving'),
      error: t('dns.dhcp.title.error'),
    }[st],
  )

  const text = $derived(
    searching
      ? t('dns.dhcp.text.searching')
      : {
      unavailable: t('dns.dhcp.text.unavailable'),
      off: t('dns.dhcp.text.off'),
      blocked: t('dns.dhcp.text.blocked'),
      serving: t('dns.dhcp.text.serving', { interface: iface?.name ?? settings?.interface ?? '–' }),
      error: t('dns.dhcp.text.error'),
    }[st],
  )

  /** Why the switch is disabled (admins only). */
  const hint = $derived.by(() => {
    if (!session.isAdmin || !settings) return undefined
    if (enabled) return dirty ? t('dns.dhcp.hint.saveFirst') : undefined
    if (!status.available) return t('dns.dhcp.hint.allowFirst')
    if (dirty) return t('dns.dhcp.hint.saveFirst')
    if (!configured) return t('dns.dhcp.hint.setupFirst')
    return undefined
  })

  // ---- facts (while switched on)

  const showFacts = $derived(enabled && st !== 'unavailable')
  const pool = $derived(status.pool)
  const range = $derived(
    pool ? `${pool.start} – ${pool.end}` : settings?.rangeStart ? `${settings.rangeStart} – ${settings.rangeEnd}` : undefined,
  )
  const dnsText = $derived(
    status.dnsServer
      ? status.dnsServer === iface?.ipv4
        ? t('dns.dhcp.fact.picache', { address: status.dnsServer })
        : status.dnsServer
      : settings?.dnsServer || undefined,
  )
  const poolTone = $derived<Tone>(pool && pool.size > 0 && pool.used / pool.size >= 0.9 ? 'warn' : 'info')

  // ---- blockers

  const blockers = $derived(st === 'blocked' && !searching ? (status.blockers ?? []) : [])
  const subnet = $derived(iface ? subnetOf(`${iface.ipv4}/${iface.prefixLen}`) : undefined)

  function blockerTitle(b: DhcpBlocker): string {
    switch (b) {
      case 'dynamic-address':
        return t('dns.dhcp.blocker.dynamic-address.title')
      case 'other-server':
        return t('dns.dhcp.blocker.other-server.title')
      case 'no-interface':
        return t('dns.dhcp.blocker.no-interface.title')
      case 'range':
        return t('dns.dhcp.blocker.range.title')
    }
    return t('dns.dhcp.blocker.other', { blocker: String(b) })
  }

  const counterRows = $derived(
    status.counters
      ? [
          { label: t('dns.dhcp.counters.received'), value: status.counters.received },
          { label: t('dns.dhcp.counters.offers'), value: status.counters.offers },
          { label: t('dns.dhcp.counters.acks'), value: status.counters.acks },
          { label: t('dns.dhcp.counters.naks'), value: status.counters.naks },
          { label: t('dns.dhcp.counters.declines'), value: status.counters.declines },
          { label: t('dns.dhcp.counters.releases'), value: status.counters.releases },
          { label: t('dns.dhcp.counters.informs'), value: status.counters.informs },
          { label: t('dns.dhcp.counters.dropped'), value: status.counters.dropped },
        ]
      : [],
  )
</script>

{#snippet fritzPath()}<span class="path">{t('dns.network.fritz.ipv4Path')}</span>{/snippet}
{#snippet fritzField()}<strong>{t('dns.dhcp.fritz.dhcpServer')}</strong>{/snippet}
{#snippet othersLink()}<button type="button" class="link" onclick={onshowothers}>{t('dns.dhcp.probe.title')}</button>{/snippet}
{#snippet setupLink()}<button type="button" class="link" onclick={onshowsetup}>{t('dns.dhcp.setup.title')}</button>{/snippet}

<Panel>
  <div class="summary">
    <div class="head">
      <div class={['verdict', view.tone]}>
        <span class="ic"><Icon name={view.icon} size={24} /></span>
        <div class="vtext">
          <div class="titleline">
            <h2>{title}</h2>
            <Chip size="sm" tone={view.tone} label={searching ? t('dns.dhcp.state.searching') : t(`dns.dhcp.state.${st}`)} />
          </div>
          <p class="muted">{text}</p>
          {#if st === 'unavailable' && status.reason}
            <p class="small muted">{t('dns.dhcp.text.reason', { reason: status.reason })}</p>
          {/if}
        </div>
      </div>
      {#if session.isAdmin && settings}
        <div class="switch">
          {#if enabled}
            <Button icon="power" loading={switching} disabled={dirty} onclick={onswitchoff}>{t('dns.dhcp.switchOff')}</Button>
          {:else}
            <Button variant="primary" icon="power" loading={switching} disabled={!canSwitchOn} onclick={onswitchon}>
              {t('dns.dhcp.switchOn')}
            </Button>
          {/if}
        </div>
      {/if}
    </div>
    {#if hint}<p class="hint small subtle">{hint}</p>{/if}

    {#if st === 'error' && status.error}
      <Notice tone="fail">{status.error}</Notice>
    {/if}

    {#if st === 'unavailable'}
      <AllowHowTo />
    {/if}

    {#if blockers.length > 0}
      <ul class="blockers">
        {#each blockers as b (b)}
          <li class="blocker">
            <h3>{blockerTitle(b)}</h3>
            {#if b === 'dynamic-address'}
              <p>
                {t('dns.dhcp.blocker.dynamic-address.text', { address: iface?.ipv4 ?? '–', interface: iface?.name ?? settings?.interface ?? '–' })}
              </p>
              <p class="step">
                <Icon name="chevron-right" size={16} />
                <span>
                  {t('dns.dhcp.blocker.dynamic-address.step', {
                    address: iface?.ipv4 ?? '–',
                    prefix: iface?.prefixLen ?? 24,
                  })}
                </span>
              </p>
            {:else if b === 'other-server'}
              <p>{t('dns.dhcp.blocker.other-server.text')}</p>
              {#if others.length > 0}<OtherServerList servers={others} />{/if}
              <p class="step">
                <Icon name="chevron-right" size={16} />
                <span>{t('dns.dhcp.blocker.other-server.step')}</span>
              </p>
              <p class="step">
                <Icon name="chevron-right" size={16} />
                <span><Trans key="dns.dhcp.fritz.step" path={fritzPath} field={fritzField} /></span>
              </p>
              {#if session.isAdmin}
                <div class="row">
                  <Button size="sm" icon="search" loading={probing} onclick={onprobe}>{t('dns.dhcp.probe.again')}</Button>
                </div>
                <p class="small muted"><Trans key="dns.dhcp.blocker.other-server.ignore" link={othersLink} /></p>
              {/if}
            {:else if b === 'no-interface'}
              <p>
                {settings?.interface
                  ? t('dns.dhcp.blocker.no-interface.text', { interface: settings.interface })
                  : t('dns.dhcp.blocker.no-interface.none')}
              </p>
              <p class="step">
                <Icon name="chevron-right" size={16} />
                <span><Trans key="dns.dhcp.blocker.no-interface.step" link={setupLink} /></span>
              </p>
            {:else if b === 'range'}
              <p>
                {subnet && settings?.rangeStart
                  ? t('dns.dhcp.blocker.range.text', { start: settings.rangeStart, end: settings.rangeEnd, subnet: subnetText(subnet) })
                  : t('dns.dhcp.blocker.range.textShort')}
              </p>
              <p class="step">
                <Icon name="chevron-right" size={16} />
                <span><Trans key="dns.dhcp.blocker.range.step" link={setupLink} /></span>
              </p>
            {/if}
          </li>
        {/each}
      </ul>
    {/if}

    {#if showFacts}
      <dl class="facts">
        <dt>{t('dns.dhcp.fact.interface')}</dt>
        <dd>
          {#if iface}
            <span>{iface.name}</span>
            <span class="mono">{iface.ipv4}/{iface.prefixLen}</span>
            <Chip
              size="sm"
              tone={iface.dynamic ? 'warn' : 'neutral'}
              label={iface.dynamic ? t('dns.dhcp.fact.dynamic') : t('dns.dhcp.fact.fixed')}
            />
          {:else}
            <span>{settings?.interface || '–'}</span>
          {/if}
        </dd>
        {#if pool && pool.size > 0}
          <dt>{t('dns.dhcp.fact.addresses')}</dt>
          <dd class="pool">
            <Meter
              label={t('dns.dhcp.pool.label')}
              max={pool.size}
              segments={[{ label: t('dns.dhcp.pool.used'), value: pool.used, text: formatNumber(pool.used), tone: poolTone }]}
              rest={{ label: t('dns.dhcp.pool.free'), text: formatNumber(Math.max(0, pool.size - pool.used)) }}
            />
            {#if pool.static > 0}<span class="small muted">{tn('dns.dhcp.pool.reserved', pool.static)}</span>{/if}
          </dd>
        {/if}
        <dt>{t('dns.dhcp.fact.range')}</dt>
        <dd><span class="mono">{range ?? '–'}</span></dd>
        <dt>{t('dns.dhcp.fact.router')}</dt>
        <dd><span class="mono">{status.router || settings?.router || '–'}</span></dd>
        <dt>{t('dns.dhcp.fact.dnsServer')}</dt>
        <dd><span class="mono">{dnsText ?? '–'}</span></dd>
        <dt>{t('dns.dhcp.fact.domain')}</dt>
        <dd><span class="mono">{status.domain || settings?.domain || '–'}</span></dd>
        <dt>{t('dns.dhcp.fact.leaseTime')}</dt>
        <dd>{settings ? formatDuration(settings.leaseSeconds * 1000) : '–'}</dd>
      </dl>
    {/if}

    {#if st === 'serving' && others.length > 0}
      <Notice tone="warn" title={t('dns.dhcp.other.serving')}>
        <p>{t('dns.dhcp.other.servingText')}</p>
        <div class="others"><OtherServerList servers={others} /></div>
      </Notice>
    {/if}

    {#if showFacts && status.counters && status.counters.received > 0}
      <details class="counters">
        <summary class="small">{t('dns.dhcp.counters.title')}</summary>
        <dl>
          {#each counterRows as c (c.label)}
            <div class="counter">
              <dt>{c.label}</dt>
              <dd>{formatNumber(c.value)}</dd>
            </div>
          {/each}
        </dl>
      </details>
    {/if}
  </div>
</Panel>

<style>
  .summary {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    justify-content: space-between;
    gap: var(--sp-3) var(--sp-4);
  }
  .verdict {
    --c: var(--text-3);
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    flex: 1 1 320px;
    min-width: 0;
  }
  .verdict.ok {
    --c: var(--ok);
  }
  .verdict.warn {
    --c: var(--warn);
  }
  .verdict.fail {
    --c: var(--fail);
  }
  .ic {
    display: flex;
    color: var(--c);
  }
  .vtext {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    min-width: 0;
    max-width: 90ch;
  }
  .titleline {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
  }
  h2 {
    font-size: var(--fs-xl);
    overflow-wrap: anywhere;
  }
  .switch {
    display: flex;
    flex: none;
  }
  .hint {
    margin-top: calc(-1 * var(--sp-2));
    text-align: right;
  }
  .blockers {
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .blocker {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    min-width: 0;
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--line);
    border-left: 4px solid var(--warn);
    border-radius: var(--r-control);
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
  .blocker h3 {
    color: var(--text);
    overflow-wrap: anywhere;
  }
  .blocker > p {
    max-width: 90ch;
    overflow-wrap: anywhere;
  }
  .step {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    color: var(--text);
  }
  .step :global(.icon) {
    flex: none;
    margin-top: 1px;
    color: var(--text-3);
  }
  .path {
    font-style: italic;
  }
  .link {
    padding: 0;
    border: 0;
    background: none;
    color: var(--link);
    font: inherit;
    text-decoration: underline;
    text-decoration-thickness: 1px;
    text-underline-offset: 2px;
    cursor: pointer;
  }
  .link:hover {
    text-decoration-thickness: 2px;
  }
  .facts {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: var(--sp-2) var(--sp-4);
    margin: 0;
    font-size: var(--fs-sm);
  }
  dt {
    color: var(--text-2);
  }
  .facts dd {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
    margin: 0;
    min-width: 0;
  }
  .facts .pool {
    display: flex;
    flex-direction: column;
    align-items: stretch;
    max-width: 420px;
  }
  .others {
    margin-top: var(--sp-2);
  }
  .counters summary {
    width: fit-content;
    color: var(--text-2);
    cursor: pointer;
  }
  .counters dl {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
    gap: var(--sp-2) var(--sp-4);
    margin: var(--sp-3) 0 0;
    font-size: var(--fs-sm);
  }
  .counter {
    display: flex;
    flex-direction: column;
    min-width: 0;
  }
  .counter dd {
    margin: 0;
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }
  @media (max-width: 600px) {
    .hint {
      text-align: left;
    }
  }
  @media (max-width: 480px) {
    .facts {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
    .facts dd {
      margin-bottom: var(--sp-2);
    }
    h2 {
      font-size: var(--fs-lg);
    }
    .blocker {
      padding: var(--sp-3);
    }
  }
</style>
