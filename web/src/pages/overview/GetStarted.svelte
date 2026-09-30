<!--
  @component
  Getting-started checklist of the Overview (admins, while
  web.onboardingDone is false): choose upstreams and blocklists, a fixed
  address for this machine, the router pointed at PiCache and a test from a
  device. Steps 2–4 come from /network/check (self.dynamic4, the DHCP
  server, the queries of the last 24 hours, the router checks and the
  requester of this request); step 1 and the steps that cannot be checked
  are ticked by hand in this browser only (onboarding.ticks). Step 1 names
  the DNSSEC mode and links to it. It never changes the host or the
  network; "Hide checklist" stores web.onboardingDone (System › Health &
  about shows it again).
-->
<script lang="ts">
  import { t, tn } from '../../i18n/index.svelte'
  import { api, resource, type DnssecMode, type NetworkCheck, type Settings } from '../../lib/api'
  import { formatNumber } from '../../lib/format'
  import { href } from '../../lib/router.svelte'
  import { loadPref, savePref } from '../../lib/storage'
  import { Badge, Button, Checkbox, Icon, Notice, Panel, Spinner, toast, type Tone } from '../../lib/ui'
  import { dnssecModeLabel } from '../dns/shared/dnssec'

  interface Props {
    /** dns.dnssecMode (GET /settings; undefined while loading). */
    dnssecMode?: DnssecMode
    onhidden: (all: Settings) => void
  }

  let { dnssecMode, onhidden }: Props = $props()

  const modeLabel = $derived(dnssecMode ? dnssecModeLabel(dnssecMode) : undefined)

  const net = resource((signal) => api.network.check({ signal }), { interval: 60_000 })

  type StepId = 'upstreams' | 'address' | 'router' | 'device'
  type StepState = 'done' | 'open' | 'unknown'

  interface Step {
    id: StepId
    title: string
    /** What the check says; a manual tick makes an open or unknown step done. */
    state: StepState
    text: string
    links: { label: string; href: string; icon?: 'sliders' | 'shield' | 'network' | 'document' }[]
    /** Label of the manual tick, where the step offers one. */
    tick?: string
    /** Waiting for the network check. */
    pending?: boolean
    /** A setting the step names, with a link to it. */
    setting?: { label: string; href: string }
  }

  // ---- manual ticks: a per-browser convenience, never sent to the server

  const TICKS = 'onboarding.ticks'
  const IDS: readonly StepId[] = ['upstreams', 'address', 'router', 'device']

  function readTicks(): StepId[] {
    try {
      const v: unknown = JSON.parse(loadPref(TICKS) ?? '[]')
      return Array.isArray(v) ? IDS.filter((id) => v.includes(id)) : []
    } catch {
      return []
    }
  }

  let ticks = $state<StepId[]>(readTicks())

  function setTick(id: StepId, on: boolean) {
    ticks = IDS.filter((x) => (x === id ? on : ticks.includes(x)))
    try {
      savePref(TICKS, JSON.stringify(ticks))
    } catch {
      /* the tick lasts for this page load */
    }
  }

  // ---- steps

  const data = $derived(net.data)
  const unavailable = $derived(!data && !!net.error)
  const ipv4 = $derived(data?.self.ipv4[0])

  function routerStep(n: NetworkCheck): Pick<Step, 'state' | 'text'> {
    if (n.dhcp?.serving) return { state: 'done', text: t('overview.start.router.doneDhcp') }
    const warns = (id: string) => (n.checks ?? []).some((c) => c.id === id && c.status === 'warn')
    // container-nat is the forwarding check of a container bridge network.
    const nat = warns('container-nat')
    const warned = nat || warns('router-forwarding') || warns('ipv6-dns')
    if (n.queries24h.total > 0 && !warned) return { state: 'done', text: t('overview.start.router.done') }
    return {
      state: 'open',
      text: nat
        ? t('overview.start.router.fixContainer')
        : warned
          ? t('overview.start.router.fix')
          : n.mode === 'bridge'
            ? // The container's own address is not the one devices reach.
              t('overview.start.router.openBridge')
            : t('overview.start.router.open', { address: n.self.ipv4[0] ?? '–' }),
    }
  }

  function deviceStep(n: NetworkCheck): Pick<Step, 'state' | 'text' | 'tick'> {
    const tick = t('overview.start.device.tick')
    const r = n.requester
    if (n.mode === 'bridge') return { state: 'unknown', text: t('overview.start.device.bridge'), tick }
    if (!r) return { state: 'unknown', text: t('overview.start.device.unknown'), tick }
    if (r.local) return { state: 'open', text: t('overview.start.device.local') }
    if (r.privacy) return { state: 'unknown', text: t('overview.start.device.privacy'), tick }
    const q = r.queries24h ?? 0
    if (q > 0) {
      return {
        state: 'done',
        text: tn('overview.start.device.done', q, { count: formatNumber(q), device: r.name || r.address }),
      }
    }
    const server = n.self.ipv4[0] ?? n.self.ula[0] ?? '–'
    return { state: 'open', text: t('overview.start.device.open', { address: r.address, server }) }
  }

  const steps = $derived.by((): Step[] => {
    const netLinks: Step['links'] = [{ label: t('overview.start.router.link'), href: href('/dns/network'), icon: 'network' }]
    const out: Step[] = [
      {
        id: 'upstreams',
        title: t('overview.start.upstreams.title'),
        state: 'open',
        text: t('overview.start.upstreams.text'),
        links: [
          { label: t('overview.start.upstreams.dns'), href: href('/dns/settings', { section: 'upstreams' }), icon: 'sliders' },
          { label: t('overview.start.upstreams.lists'), href: href('/dns/filtering'), icon: 'shield' },
        ],
        tick: t('overview.start.upstreams.tick'),
        setting: modeLabel
          ? { label: t('overview.start.upstreams.dnssec', { mode: modeLabel }), href: href('/dns/settings', { section: 'dnssec' }) }
          : undefined,
      },
    ]
    const pending = !data && !net.error
    const waiting = (id: StepId, title: string, tick?: string): Step => ({
      id,
      title,
      state: 'unknown',
      text: pending ? t('overview.start.checking') : t('overview.start.unavailableStep'),
      links: [],
      tick,
      pending,
    })
    if (!data) {
      out.push(
        waiting('address', t('overview.start.address.title'), t('overview.start.address.tick')),
        waiting('router', t('overview.start.router.title')),
        waiting('device', t('overview.start.device.title'), t('overview.start.device.tick')),
      )
      return out
    }
    const dyn = data.self.dynamic4
    out.push({
      id: 'address',
      title: t('overview.start.address.title'),
      ...(dyn === false
        ? { state: 'done' as const, text: t('overview.start.address.static', { address: ipv4 ?? '–' }) }
        : dyn === true
          ? { state: 'open' as const, text: t('overview.start.address.dynamic', { address: ipv4 ?? '–' }) }
          : { state: 'unknown' as const, text: t('overview.start.address.unknown') }),
      links: [],
      tick: dyn === false ? undefined : t('overview.start.address.tick'),
    })
    const router = routerStep(data)
    out.push({ id: 'router', title: t('overview.start.router.title'), ...router, links: router.state === 'done' ? [] : netLinks })
    const device = deviceStep(data)
    out.push({
      id: 'device',
      title: t('overview.start.device.title'),
      ...device,
      links: device.state === 'done' ? [] : [{ label: t('overview.start.device.guides'), href: href('/dns/network/devices'), icon: 'document' }],
    })
    return out
  })

  /** A tick counts only where the step offers one and the check has not settled it. */
  function ticked(s: Step): boolean {
    return !!s.tick && s.state !== 'done' && ticks.includes(s.id)
  }

  function effective(s: Step): StepState {
    return ticked(s) ? 'done' : s.state
  }

  const done = $derived(steps.filter((s) => effective(s) === 'done').length)
  const allDone = $derived(done === steps.length)

  const TONE: Record<StepState, Tone> = { done: 'ok', open: 'warn', unknown: 'neutral' }

  // ---- hide

  let hiding = $state(false)

  async function hide() {
    hiding = true
    try {
      const all = await api.settings.patch('web', { onboardingDone: true })
      toast.success(t('overview.start.hidden'))
      onhidden(all)
    } catch (err) {
      toast.error(err)
    } finally {
      hiding = false
    }
  }
</script>

<Panel title={t('overview.start.title')} description={t('overview.start.description')}>
  {#snippet actions()}
    <span class="small muted">{t('overview.start.progress', { done, total: steps.length })}</span>
    <Button size="sm" variant="ghost" icon="eye-off" loading={hiding} onclick={hide}>{t('overview.start.hide')}</Button>
  {/snippet}

  <div class="stack">
    {#if allDone}
      <Notice tone="ok">{t('overview.start.allDone')}</Notice>
    {/if}
    {#if unavailable}
      <Notice tone="warn">
        {t('overview.start.unavailable')}
        {#snippet actions()}
          <Button size="sm" icon="refresh" loading={net.loading} onclick={() => net.refresh()}>{t('common.action.retry')}</Button>
        {/snippet}
      </Notice>
    {/if}

    <ol class="steps">
      {#each steps as s, i (s.id)}
        {@const state = effective(s)}
        <li class={['step', state]}>
          <span class="mark" aria-hidden="true">
            {#if state === 'done'}<Icon name="check" size={16} />{:else}{i + 1}{/if}
          </span>
          <div class="body">
            <div class="head">
              <h3>{s.title}</h3>
              {#if s.pending}
                <Spinner size={14} />
              {:else}
                <Badge tone={TONE[state]}>{t(`overview.start.state.${state}`)}</Badge>
              {/if}
            </div>
            <p class="text small">{s.text}</p>
            {#if s.setting}<p class="small"><a href={s.setting.href}>{s.setting.label}</a></p>{/if}
            {#if ticked(s)}<p class="small muted">{t('overview.start.tickedNote')}</p>{/if}
            {#if s.links.length > 0 || (s.tick && s.state !== 'done')}
              <div class="acts">
                {#each s.links as l (l.href)}
                  <Button size="sm" icon={l.icon} href={l.href}>{l.label}</Button>
                {/each}
                {#if s.tick && s.state !== 'done'}
                  <Checkbox checked={ticks.includes(s.id)} label={s.tick} onchange={(c) => setTick(s.id, c)} />
                {/if}
              </div>
            {/if}
          </div>
        </li>
      {/each}
    </ol>
  </div>
</Panel>

<style>
  .steps {
    display: flex;
    flex-direction: column;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  .step {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
    padding: var(--sp-3) 0;
    border-top: 1px solid var(--line);
  }
  .step:first-child {
    border-top: 0;
    padding-top: 0;
  }
  .step:last-child {
    padding-bottom: 0;
  }
  .mark {
    --c: var(--text-3);
    display: inline-flex;
    flex: none;
    align-items: center;
    justify-content: center;
    width: 26px;
    height: 26px;
    border: 1.5px solid var(--c);
    border-radius: 50%;
    color: var(--c);
    font-size: var(--fs-sm);
    font-weight: 700;
  }
  .done .mark {
    --c: var(--ok);
    background: color-mix(in srgb, var(--ok) 14%, var(--surface));
  }
  .open .mark {
    --c: var(--text);
  }
  .body {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    flex: 1;
    min-width: 0;
  }
  .head {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-1) var(--sp-2);
    min-height: 26px;
  }
  .text {
    color: var(--text-2);
    max-width: 90ch;
    overflow-wrap: anywhere;
  }
  .done .text {
    color: var(--text-3);
  }
  .acts {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
    margin-top: var(--sp-1);
  }
</style>
