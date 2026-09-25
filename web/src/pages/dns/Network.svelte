<!--
  @component
  DNS › Network check: do all devices use PiCache? One verdict sentence,
  the router and PiCache's addresses, a card with concrete steps for every
  check that needs attention (FRITZ!Box or other routers), the passed
  checks, and the devices of the network with a scan and "Add as client".
  Read-only principals see everything without scan and add actions.
  Query: ?show=unused (only devices that do not use PiCache)
-->
<script lang="ts">
  import { tick, untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, isApiError, resource, toApiError, type NetworkCheck, type NetworkCheckItem, type Resource } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { Button, CopyButton, Icon, Notice, Panel, Skeleton, toast } from '$lib/ui'
  import CheckCard from './network/CheckCard.svelte'
  import { sortedChecks, verdict } from './network/checks'
  import DevicesPanel from './network/DevicesPanel.svelte'

  // A scan this page started is followed (every 2 s) until the server reports it done.
  let following = $state(false)
  const check: Resource<NetworkCheck> = resource((signal) => api.network.check({ signal }), {
    interval: () => (following || check.data?.scan.running ? 2_000 : 30_000),
  })
  const groups = resource((signal) => api.groups.list({ signal }))

  const net = $derived(check.data)
  const checks = $derived(net ? sortedChecks(net.checks ?? []) : [])
  const open = $derived(checks.filter((c) => c.status !== 'ok'))
  const passed = $derived(checks.filter((c) => c.status === 'ok'))
  const v = $derived(net ? verdict(net.checks ?? []) : undefined)

  // ---- scan

  const FOLLOW_MAX_MS = 3 * 60_000

  let starting = $state(false)
  let startedWith = $state<number | undefined>(undefined)
  let note = $state<{ tone: 'info' | 'warn' | 'fail'; text: string } | undefined>(undefined)
  /** What the page showed when the scan started: only newer results tell whether it is done. */
  let baseline: { checkedAt?: string; finishedAt?: string; since: number; sawRunning: boolean } | undefined

  function follow() {
    baseline = { checkedAt: net?.checkedAt, finishedAt: net?.scan.finishedAt, since: Date.now(), sawRunning: false }
    following = true
  }

  async function scan() {
    starting = true
    note = undefined
    try {
      const r = await api.network.scan()
      startedWith = r.addresses
      follow()
    } catch (err) {
      const e = toApiError(err)
      if (isApiError(e, 'conflict')) {
        note = { tone: 'info', text: t('dns.network.scan.busy') }
        follow()
      } else if (isApiError(e, 'too_many_requests')) {
        note = { tone: 'warn', text: t('dns.network.scan.tooSoon') }
      } else if (isApiError(e, 'unavailable')) {
        note = { tone: 'warn', text: t('dns.network.scan.unavailable', { reason: errorText(e) }) }
      } else {
        note = { tone: 'fail', text: `${t('dns.network.scan.failed')}: ${errorText(e)}` }
      }
    } finally {
      starting = false
    }
    void check.refresh()
  }

  // The scan is over when a newer result no longer reports it running (it
  // may be done before the first poll: then its finish time changed).
  $effect(() => {
    const d = net
    if (!following || !d) return
    untrack(() => {
      const b = baseline
      if (!b || Date.now() - b.since > FOLLOW_MAX_MS) {
        following = false
        return
      }
      if (d.checkedAt === b.checkedAt && !d.scan.running) return
      if (d.scan.running) {
        b.sawRunning = true
        return
      }
      if (b.sawRunning || d.scan.finishedAt !== b.finishedAt) {
        following = false
        startedWith = undefined
        if (note?.tone === 'info') note = undefined
        toast.success(t('dns.network.scan.done'))
      }
    })
  })

  async function showUnused() {
    router.setQuery({ show: 'unused' })
    await tick()
    document.getElementById('network-devices')?.scrollIntoView({ block: 'start', behavior: 'smooth' })
  }

  function routerName(): string | undefined {
    const r = net?.router
    if (!r) return undefined
    if (r.kind === 'fritzbox') return t('dns.network.router.fritzbox')
    return r.name
  }

  const selfAddresses = $derived(net ? [...net.self.ipv4, ...net.self.ula, ...net.self.global] : [])

  function okText(c: NetworkCheckItem): string | undefined {
    switch (c.id) {
      case 'router-forwarding':
        return t('dns.network.ok.routerForwarding')
      case 'container-nat':
        return t('dns.network.ok.containerNat')
      case 'ipv6-dns':
        if (c.data.lanHasIPv6) return t('dns.network.ok.ipv6Dns')
        // No IPv6 address and router advertisements ignored: "no IPv6" may just be unseen.
        return c.data.hostIgnoresRA ? t('dns.network.ok.ipv6DnsUnknown') : t('dns.network.ok.ipv6DnsNone')
      case 'ipv6-address':
        // Without IPv6 in the network there is nothing to report here (ipv6-dns says so).
        return c.data.ula?.[0] ? t('dns.network.ok.ipv6Address', { address: c.data.ula[0] }) : undefined
      case 'refused':
        return t('dns.network.ok.refused')
      case 'devices':
        return t('dns.network.ok.devices')
    }
    return undefined
  }

  /** A note under a passed check: this machine ignores IPv6 router advertisements. */
  function okNote(c: NetworkCheckItem): string | undefined {
    return c.id === 'ipv6-dns' && c.data.hostIgnoresRA ? t('dns.network.check.ipv6Dns.ignoresRA') : undefined
  }
</script>

<div class="page">
  {#if check.error && !net}
    <Notice tone="fail" title={t('dns.network.loadError')}>
      {errorText(check.error)}
      {#snippet actions()}
        <Button size="sm" icon="refresh" onclick={() => check.refresh()}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {:else if !net || !v}
    <Skeleton height="150px" />
    <Skeleton height="220px" />
  {:else}
    <Panel>
      <div class="summary">
        <div class={['verdict', v.tone]}>
          <span class="ic"><Icon name={v.tone === 'ok' ? 'success' : v.tone === 'warn' ? 'alert' : 'info'} size={24} /></span>
          <div class="vtext">
            <h2>{v.title}</h2>
            <p class="muted">{v.text}</p>
          </div>
        </div>

        <dl class="facts">
          <dt>{t('dns.network.router')}</dt>
          <dd>
            {#if net.router?.ipv4}
              <span class="mono">{net.router.ipv4}</span>
              {#if routerName()}<span> — {routerName()}</span>{/if}
              {#if net.router.ipv6.length > 0}
                <span class="also small muted mono">{net.router.ipv6.join(', ')}</span>
              {/if}
            {:else}
              <span class="muted">{net.mode === 'bridge' ? t('dns.network.router.bridge') : t('dns.network.router.none')}</span>
            {/if}
          </dd>
          <dt>{t('dns.network.self')}</dt>
          <dd class="addrs">
            {#each selfAddresses as a (a)}
              <span class="addr"><span class="mono">{a}</span><CopyButton text={a} /></span>
            {:else}
              <span class="muted">–</span>
            {/each}
          </dd>
        </dl>

        <div class="meta small muted">
          <span title={formatDateTime(net.checkedAt)}>
            {Date.now() - new Date(net.checkedAt).getTime() < 45_000
              ? t('dns.network.checkedJustNow')
              : t('dns.network.checked', { when: formatRelative(net.checkedAt) })}
          </span>
          <Button size="sm" variant="ghost" icon="refresh" loading={check.loading} onclick={() => check.refresh()}>{t('dns.network.recheck')}</Button>
        </div>
        {#if !net.statsAvailable}<p class="small muted">{t('dns.network.noStats')}</p>{/if}
        {#if check.error}<Notice tone="warn">{errorText(check.error)}</Notice>{/if}
      </div>
    </Panel>

    {#each open as c (c.id)}
      <CheckCard check={c} {net} onshowdevices={c.id === 'devices' ? showUnused : undefined} />
    {/each}

    {#if passed.length > 0}
      <Panel title={t('dns.network.passed')} level={2}>
        <ul class="passed">
          {#each passed as c (c.id)}
            {@const text = okText(c)}
            {@const note = okNote(c)}
            {#if text}
              <li class={{ unsure: !!note }}>
                <Icon name={note ? 'info' : 'success'} size={18} />
                <span class="ok-text">
                  <span>{text}</span>
                  {#if note}<span class="ok-note small muted">{note}</span>{/if}
                </span>
              </li>
            {/if}
          {/each}
        </ul>
      </Panel>
    {/if}
  {/if}

  {#if !(check.error && !net)}
    <DevicesPanel
      {net}
      loading={check.loading && !check.loaded}
      error={check.error && !net ? errorText(check.error) : undefined}
      onretry={() => check.refresh()}
      groups={groups.data}
      {starting}
      scanning={following}
      scanAddresses={startedWith}
      {note}
      onscan={scan}
      ondismissnote={() => (note = undefined)}
      onchanged={() => check.refresh()}
    />
  {/if}
</div>

<style>
  .summary {
    display: flex;
    flex-direction: column;
    gap: var(--sp-4);
  }
  .verdict {
    --c: var(--ok);
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .verdict.warn {
    --c: var(--warn);
  }
  .verdict.info {
    --c: var(--focus);
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
  }
  h2 {
    font-size: var(--fs-xl);
    overflow-wrap: anywhere;
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
  dd {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    gap: 0 var(--sp-1);
    margin: 0;
    min-width: 0;
  }
  .also {
    flex-basis: 100%;
    overflow-wrap: anywhere;
  }
  .addrs {
    align-items: center;
    gap: var(--sp-1) var(--sp-3);
  }
  .addr {
    display: inline-flex;
    align-items: center;
    gap: 2px;
    min-width: 0;
    overflow-wrap: anywhere;
  }
  .meta {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
  }
  .passed {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-sm);
  }
  .passed li {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
  }
  .passed :global(.icon) {
    flex: none;
    color: var(--ok);
  }
  .passed .unsure :global(.icon) {
    color: var(--focus);
  }
  .ok-text {
    display: flex;
    flex-direction: column;
    gap: 2px;
    min-width: 0;
    max-width: 90ch;
  }
  @media (max-width: 480px) {
    .facts {
      grid-template-columns: minmax(0, 1fr);
      gap: 2px;
    }
    dd {
      margin-bottom: var(--sp-2);
    }
    h2 {
      font-size: var(--fs-lg);
    }
  }
</style>
