<!--
  @component
  DNS › DHCP: the optional DHCP server. A status header (off; waiting, with
  every blocker and what to do; serving, with what it hands out; error; not
  available, with how to allow it) and the switch; the set-up (settings
  section "dhcp"), other DHCP servers (search, "serve anyway"), IPv6 DNS
  announcements, the handed-out and the reserved addresses. While the server
  is on, the addresses come first; while it is off, the set-up does. Edits
  of the settings are saved from the bar at the bottom. Read-only principals
  see everything without actions.
  Query: ?reservation=<mac> opens the editor of a reserved address.
-->
<script lang="ts">
  import { tick } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import {
    api,
    isApiError,
    resource,
    toApiError,
    type DhcpLease,
    type DhcpProbeResult,
    type DhcpStaticLeaseInput,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDuration } from '$lib/format'
  import { router } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Button, confirm, Notice, Skeleton, toast } from '$lib/ui'
  import EnableDialog from './dhcp/EnableDialog.svelte'
  import Ipv6Panel from './dhcp/Ipv6Panel.svelte'
  import LeasesPanel from './dhcp/LeasesPanel.svelte'
  import { interfaceSubnet, intToIp4, subnetOf, subnetText } from './dhcp/net'
  import OtherServersPanel from './dhcp/OtherServersPanel.svelte'
  import ReservationPanel from './dhcp/ReservationPanel.svelte'
  import ReservedPanel from './dhcp/ReservedPanel.svelte'
  import SetupPanel from './dhcp/SetupPanel.svelte'
  import StatusPanel from './dhcp/StatusPanel.svelte'

  // Right after a change the state moves on within seconds (the server searches for other servers first).
  let fastUntil = 0
  const status = resource((signal) => api.dhcp.status({ signal }), {
    interval: () => (Date.now() < fastUntil ? 2_000 : 10_000),
  })
  const form = settingsForm('dhcp')
  const interfaces = resource((signal) => api.dhcp.interfaces({ signal }))
  const leases = resource((signal) => api.dhcp.leases({ signal }), { interval: 30_000 })
  const reserved = resource((signal) => api.dhcp.static.list({ signal }))
  const clients = resource((signal) => api.clients.list({ signal }))
  const groups = resource((signal) => api.groups.list({ signal }))
  const allSettings = resource((signal) => api.settings.get({ signal }))

  const st = $derived(status.data)
  const on = $derived(!!form.saved?.enabled)

  /** Refreshes what a change affects and follows the state closely for a while. */
  function follow() {
    fastUntil = Date.now() + 30_000
    void status.refresh()
    void leases.refresh()
  }

  // ---- values shown as defaults

  const overviewRouter = $derived(appStatus.overview.data?.router)
  /** The default gateway: the effective router while none is set, else the router resolver's automatic address. */
  const gateway = $derived(
    (form.saved && !form.saved.router ? st?.router : undefined) ||
      (overviewRouter?.mode === 'auto' && overviewRouter.address ? overviewRouter.address : undefined),
  )
  const localDomain = $derived(allSettings.data?.dns.localDomain || (form.saved && !form.saved.domain ? st?.domain : undefined) || undefined)

  const configuredIface = $derived(interfaces.data?.find((i) => i.name === form.saved?.interface))
  const subnet = $derived(
    st?.interface ? subnetOf(`${st.interface.ipv4}/${st.interface.prefixLen}`) : interfaceSubnet(configuredIface),
  )
  const subnetStr = $derived(subnet ? subnetText(subnet) : undefined)
  const example = $derived(subnet && subnet.broadcast - subnet.network > 21 ? intToIp4(subnet.network + 20) : undefined)

  // ---- switching on and off

  let enableOpen = $state(false)
  let switching = $state(false)
  let switchError = $state<string | undefined>(undefined)

  function openEnable() {
    switchError = undefined
    enableOpen = true
  }

  async function switchOn() {
    const draft = form.draft
    if (!draft || form.dirty) return
    switching = true
    switchError = undefined
    draft.enabled = true
    try {
      if (await form.save()) {
        enableOpen = false
        toast.success(t('dns.dhcp.switchedOn'))
        follow()
      } else {
        // Field errors stay visible in the set-up; nothing else was edited.
        switchError = form.errorMessage
        draft.enabled = false
      }
    } finally {
      switching = false
    }
  }

  async function switchOff() {
    const draft = form.draft
    const saved = form.saved
    if (!draft || !saved || form.dirty) return
    const message = [
      t('dns.dhcp.off.text', { duration: formatDuration(saved.leaseSeconds * 1000) }),
      saved.ipv6.routerAdvertisements ? t('dns.dhcp.off.textRa') : '',
    ]
      .filter(Boolean)
      .join(' ')
    const ok = await confirm({
      title: t('dns.dhcp.off.title'),
      message,
      confirmLabel: t('dns.dhcp.switchOff'),
      action: async () => {
        switching = true
        draft.enabled = false
        try {
          if (!(await form.save())) {
            draft.enabled = true
            throw form.saveError ?? new Error(t('common.error.generic'))
          }
        } finally {
          switching = false
        }
      },
    })
    if (!ok) return
    toast.success(t('dns.dhcp.switchedOff'))
    follow()
  }

  // ---- search for other DHCP servers

  let probing = $state(false)
  let probeResult = $state.raw<DhcpProbeResult | undefined>(undefined)
  let probeNote = $state<{ tone: 'info' | 'warn' | 'fail'; text: string } | undefined>(undefined)

  /** Searches for other DHCP servers; from the status header the result is also announced (the panel may be out of view). */
  async function probe(announce: boolean) {
    probing = true
    probeNote = undefined
    try {
      const r = await api.dhcp.probe()
      probeResult = r
      if (announce) {
        const n = r.servers?.length ?? 0
      if (n === 0) toast.success(t('dns.dhcp.probe.none'))
        else toast.info(tn('dns.dhcp.probe.found', n))
      }
    } catch (err) {
      const e = toApiError(err)
      if (isApiError(e, 'too_many_requests')) probeNote = { tone: 'info', text: t('dns.dhcp.probe.tooSoon') }
      else if (isApiError(e, 'unavailable')) probeNote = { tone: 'warn', text: t('dns.dhcp.probe.unavailable', { reason: errorText(e) }) }
      else probeNote = { tone: 'fail', text: `${t('dns.dhcp.probe.failed')}: ${errorText(e)}` }
      if (announce) toast.error(probeNote.text)
    } finally {
      probing = false
      follow()
    }
  }

  // ---- reserved addresses

  let addOpen = $state(false)
  let addPreset = $state.raw<Partial<DhcpStaticLeaseInput>>({})
  const selMac = $derived(router.param('reservation').trim().toLowerCase())
  const editing = $derived(selMac ? reserved.data?.find((s) => s.mac === selMac) : undefined)

  function addReservation() {
    addPreset = {}
    addOpen = true
  }

  function reserve(l: DhcpLease) {
    addPreset = { mac: l.mac, ip: l.ip, hostname: l.hostname ?? '' }
    addOpen = true
  }

  function reservationsChanged() {
    void reserved.refresh()
    void leases.refresh()
    void status.refresh()
  }

  // ---- save bar

  let root: HTMLElement | undefined = $state()

  async function save() {
    if (await form.save()) {
      toast.success(t('common.state.saved'))
      follow()
      return
    }
    await tick()
    root?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus()
  }

  function show(id: string) {
    document.getElementById(id)?.scrollIntoView({ block: 'start', behavior: 'smooth' })
  }
</script>

{#snippet setupPanel()}
  <SetupPanel
    {form}
    status={st}
    interfaces={interfaces.data}
    interfacesError={interfaces.error}
    onretryinterfaces={() => interfaces.refresh()}
    {gateway}
    {localDomain}
  />
{/snippet}

{#snippet othersPanel()}
  <OtherServersPanel
    {form}
    status={st}
    {probing}
    result={probeResult}
    note={probeNote}
    onprobe={() => probe(false)}
    ondismissnote={() => (probeNote = undefined)}
  />
{/snippet}

{#snippet ipv6Panel()}
  <Ipv6Panel {form} status={st} />
{/snippet}

{#snippet leasesPanel()}
  <LeasesPanel
    {leases}
    clients={clients.data}
    groups={groups.data}
    onreserve={reserve}
    onchanged={() => {
      void leases.refresh()
      void status.refresh()
    }}
    onclientadded={() => {
      void clients.refresh()
      void leases.refresh()
    }}
  />
{/snippet}

{#snippet reservedPanel()}
  <ReservedPanel
    {reserved}
    subnet={subnetStr}
    selected={editing?.mac}
    onadd={addReservation}
    onedit={(s) => router.setQuery({ reservation: s.mac }, { push: true })}
  />
{/snippet}

<div class="page" bind:this={root}>
  <p class="intro muted">{t('dns.dhcp.intro')}</p>

  {#if status.error && !st}
    <Notice tone="fail" title={t('dns.dhcp.loadError')}>
      {errorText(status.error)}
      {#snippet actions()}
        <Button size="sm" icon="refresh" onclick={() => status.refresh()}>{t('common.action.retry')}</Button>
      {/snippet}
    </Notice>
  {:else if !st}
    <Skeleton height="160px" />
    <Skeleton height="280px" />
  {:else}
    {#if !session.isAdmin}<Notice>{t('common.state.readOnly')}</Notice>{/if}

    <StatusPanel
      status={st}
      settings={form.saved}
      dirty={form.dirty && !switching}
      {switching}
      {probing}
      onswitchon={openEnable}
      onswitchoff={switchOff}
      onprobe={() => probe(true)}
      onshowothers={() => show('dhcp-other')}
      onshowsetup={() => show('dhcp-setup')}
    />

    {#if form.loadError && !form.draft}
      <Notice tone="fail" title={t('dns.dhcp.settingsError')}>
        {errorText(form.loadError)}
        {#snippet actions()}
          <Button size="sm" icon="refresh" onclick={() => form.load()}>{t('common.action.retry')}</Button>
        {/snippet}
      </Notice>
    {:else if !form.draft}
      <Skeleton height="280px" />
    {:else if on}
      {@render leasesPanel()}
      {@render reservedPanel()}
      {@render setupPanel()}
      {@render othersPanel()}
      {@render ipv6Panel()}
    {:else}
      {@render setupPanel()}
      {@render othersPanel()}
      {@render ipv6Panel()}
      {@render reservedPanel()}
      {#if (leases.data?.length ?? 0) > 0}{@render leasesPanel()}{/if}
    {/if}

    <!-- The switch saves through the form too: no save bar for that moment. -->
    {#if form.dirty && !switching}
      <!-- Buttons first: toasts appear at the bottom right and must not cover them. -->
      <div class="savebar" role="region" aria-label={t('dns.settings.saveBar')}>
        <Button variant="primary" loading={form.saving} disabled={!session.isAdmin} onclick={save}>{t('common.action.save')}</Button>
        <Button variant="ghost" disabled={form.saving} onclick={() => form.revert()}>{t('dns.settings.discard')}</Button>
        <div class="msg">
          {#if form.errorMessage}
            <p class="err">{form.errorMessage}</p>
          {:else}
            <p>{t('dns.settings.unsaved')}</p>
          {/if}
        </div>
      </div>
    {/if}
  {/if}
</div>

{#if session.isAdmin}
  <EnableDialog
    bind:open={enableOpen}
    settings={form.saved}
    iface={configuredIface}
    {switching}
    error={switchError}
    onconfirm={switchOn}
  />
  <ReservationPanel bind:open={addOpen} preset={addPreset} subnet={subnetStr} {example} onsaved={reservationsChanged} />
{/if}
<ReservationPanel
  bind:open={() => !!editing, (v) => !v && router.setQuery({ reservation: null })}
  lease={editing}
  subnet={subnetStr}
  {example}
  onsaved={reservationsChanged}
  ondeleted={reservationsChanged}
/>

<style>
  .intro {
    max-width: 90ch;
    font-size: var(--fs-sm);
  }
  .page :global(#dhcp-setup),
  .page :global(#dhcp-other),
  .page :global(#dhcp-ipv6) {
    scroll-margin-top: var(--sp-4);
  }
  .savebar {
    position: sticky;
    bottom: 0;
    z-index: 5;
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
    padding: var(--sp-3) var(--sp-4);
    border: 1px solid var(--line);
    border-radius: var(--r-panel);
    background: var(--surface);
    box-shadow: var(--shadow-float);
  }
  .msg {
    flex: 1 1 240px;
    min-width: 0;
    font-size: var(--fs-sm);
  }
  .err {
    color: var(--danger);
    overflow-wrap: anywhere;
  }
</style>
