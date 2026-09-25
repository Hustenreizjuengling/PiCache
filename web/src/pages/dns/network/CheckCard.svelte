<!--
  @component
  One check that needs attention (warn or info): what was found, with the
  numbers, and the steps that fix it for a FRITZ!Box or any other router,
  PiCache's addresses filled in with copy buttons. Refused sources list the
  addresses; for addresses in a network this machine is connected to, admins
  can allow those networks right here (a DNS setting), for other global IPv6
  ones the card offers their /64 network for the allowed networks. The IPv6
  DNS check notes when this machine ignores router advertisements.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, type NetworkCheck, type NetworkCheckItem, type RefusedSource } from '$lib/api'
  import { formatDateTime, formatNumber, formatPercent, formatRelative } from '$lib/format'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, Chip, Icon, Notice, Table, toast, Toggle, Trans, type Column } from '$lib/ui'
  import { REPO } from '../../system/health/about'
  import { globalPrefixes, isFritz, stepsFor } from './checks'
  import CopyValue from './CopyValue.svelte'

  interface Props {
    check: NetworkCheckItem
    net: NetworkCheck
    /** Shows the devices that do not use PiCache (devices check). */
    onshowdevices?: () => void
  }

  let { check, net, onshowdevices }: Props = $props()

  const auto = $props.id()
  const steps = $derived(stepsFor(check, net))
  // Without an IPv6 listener the IPv6 steps are the same for every router.
  const fritzSteps = $derived(isFritz(net) && !(check.id === 'ipv6-dns' && !net.self.dnsIpv6))
  const warn = $derived(check.status === 'warn')

  const title = $derived.by(() => {
    const c = check
    switch (c.id) {
      case 'router-forwarding':
        return warn ? t('dns.network.check.routerForwarding.warnTitle') : t('dns.network.check.routerForwarding.infoTitle')
      case 'container-nat':
        return warn ? t('dns.network.check.containerNat.warnTitle') : t('dns.network.check.containerNat.infoTitle')
      case 'ipv6-dns':
        return t('dns.network.check.ipv6Dns.title')
      case 'ipv6-address':
        return warn ? t('dns.network.check.ipv6Address.warnTitle') : t('dns.network.check.ipv6Address.infoTitle')
      case 'refused':
        return t('dns.network.check.refused.title')
      case 'devices':
        return tn('dns.network.check.devices.title', c.data.inactive + c.data.never)
    }
    return ''
  })

  /** The finding in one or two sentences (numbers from the server). */
  const text = $derived.by((): string[] => {
    const c = check
    switch (c.id) {
      case 'router-forwarding':
      case 'container-nat': {
        const d = c.data
        const params = {
          share: formatPercent(d.totalQueries > 0 ? d.routerQueries / d.totalQueries : 0),
          total: formatNumber(d.totalQueries),
          addresses: (d.routerAddresses ?? []).join(', ') || '–',
        }
        return [
          c.id === 'router-forwarding'
            ? t('dns.network.check.routerForwarding.text', params)
            : t('dns.network.check.containerNat.text', params),
        ]
      }
      case 'ipv6-dns':
        return [t('dns.network.check.ipv6Dns.text')]
      case 'ipv6-address':
        return [
          warn
            ? t('dns.network.check.ipv6Address.warnText')
            : t('dns.network.check.ipv6Address.infoText', { addresses: (c.data.global ?? []).join(', ') || '–' }),
        ]
      case 'refused':
        return [t('dns.network.check.refused.text', { since: formatDateTime(c.data.since) })]
      case 'devices': {
        const d = c.data
        return [
          ...(d.inactive > 0 ? [tn('dns.network.check.devices.inactive', d.inactive)] : []),
          ...(d.never > 0 ? [tn('dns.network.check.devices.never', d.never)] : []),
          t('dns.network.check.devices.text'),
        ]
      }
    }
    return []
  })

  const refused = $derived(check.id === 'refused' ? (check.data.sources ?? []) : [])
  // Addresses in a connected network are covered by trusting those networks; the /64 offer is for the others.
  const onLink = $derived(refused.filter((s) => s.onLink))
  const prefixes = $derived(globalPrefixes(refused.filter((s) => !s.onLink).map((s) => s.address)))
  const trustOn = $derived(check.id === 'refused' && !!check.data.trustConnectedNetworks)

  // ---- allow the networks this machine is connected to (saved right away)

  let trustSwitch = $state(false)
  let trusting = $state(false)
  let trusted = $state(false)

  async function trust(on: boolean) {
    if (!on || trusting) return
    trusting = true
    try {
      await api.settings.patch('dns', { trustConnectedNetworks: true })
      trusted = true
      toast.success(t('dns.network.check.refused.trustedToast'))
    } catch (e) {
      trustSwitch = false
      toast.error(e)
    } finally {
      trusting = false
    }
  }

  const refusedColumns: Column<RefusedSource>[] = $derived([
    { key: 'address', label: t('dns.network.check.refused.address'), value: (s) => s.address, cell: addressCell },
    { key: 'count', label: t('dns.network.check.refused.count'), align: 'right', value: (s) => s.count, format: (s) => formatNumber(s.count) },
    { key: 'last', label: t('dns.network.check.refused.last'), value: (s) => s.last, cell: lastCell },
  ])
</script>

{#snippet addressCell(s: RefusedSource)}
  <span class="raddr">
    <span class="mono">{s.address}</span>
    {#if s.onLink}<Badge title={t('dns.network.check.refused.onLinkHelp')}>{t('dns.network.check.refused.onLinkBadge')}</Badge>{/if}
  </span>
{/snippet}

{#snippet lastCell(s: RefusedSource)}
  <span class="nowrap" title={formatDateTime(s.last)}>{formatRelative(s.last)}</span>
{/snippet}

{#snippet settingsLink()}<a href={href('/dns/settings', { section: 'access' })}>{t('dns.network.check.refused.link')}</a>{/snippet}
{#snippet accessLink()}<a href={href('/dns/settings', { section: 'access' })}>{t('dns.network.check.refused.accessLink')}</a>{/snippet}

<section class={['card', check.status]} aria-labelledby="cc-{auto}">
  <header>
    <Chip size="sm" tone={warn ? 'warn' : 'info'} label={warn ? t('dns.network.status.warn') : t('dns.network.status.info')} />
    <h3 id="cc-{auto}">{title}</h3>
  </header>

  <div class="text">
    {#each text as p, i (i)}<p>{p}</p>{/each}
    {#if check.id === 'ipv6-dns' && !net.self.dnsIpv6}
      <p class="note"><Icon name="alert" size={16} /><span>{t('dns.network.check.ipv6Dns.noListener')}</span></p>
    {/if}
    {#if check.id === 'ipv6-dns' && check.data.hostIgnoresRA}
      <p class="note info"><Icon name="info" size={16} /><span>{t('dns.network.check.ipv6Dns.ignoresRA')}</span></p>
    {/if}
  </div>

  {#if steps.length > 0}
    <div class="steps">
      <h4>{fritzSteps ? t('dns.network.steps.fritzbox') : t('dns.network.steps.generic')}</h4>
      <ol>
        {#each steps as step, i (i)}
          {#snippet path()}<span class="path">{step.path}</span>{/snippet}
          {#snippet field()}<strong>{step.field}</strong>{/snippet}
          {#snippet section()}<strong>{step.section}</strong>{/snippet}
          {#snippet value()}<CopyValue value={step.value} missing={step.missing} />{/snippet}
          <li><Trans key={step.key} {path} {field} {section} {value} /></li>
        {/each}
      </ol>
    </div>
  {/if}

  {#if check.id === 'container-nat'}
    <ol class="steps-plain">
      <li>{t('dns.network.check.containerNat.step')}</li>
    </ol>
    <p>
      <a href="{REPO}/blob/main/docs/DEPLOYMENT.md#docker" target="_blank" rel="noopener noreferrer">
        {t('dns.network.check.containerNat.docs')}<Icon name="external" size={14} />
      </a>
    </p>
  {/if}

  {#if check.id === 'refused' && refused.length > 0}
    <div class="refused">
      <Table columns={refusedColumns} rows={refused} key={(s) => s.address} compact caption={t('dns.network.check.refused.caption')} />
    </div>
    {#if onLink.length > 0 && (!trustOn || trusted)}
      <div class="trust stack-sm">
        <p>{t('dns.network.check.refused.onLinkText')}</p>
        {#if trusted}
          <Notice tone="ok">{t('dns.network.check.refused.trusted')}</Notice>
        {:else if session.isAdmin}
          <Toggle
            bind:checked={trustSwitch}
            disabled={trusting}
            label={t('dns.network.check.refused.trust')}
            description={t('dns.network.check.refused.trustHelp')}
            onchange={trust}
          />
        {:else}
          <p class="small muted"><Trans key="dns.network.check.refused.trustReadOnly" link={accessLink} /></p>
        {/if}
      </div>
    {/if}
    {#if prefixes.length > 0}
      <div class="prefixes stack-sm">
        <p><Trans key="dns.network.check.refused.prefixes" link={settingsLink} /></p>
        <ul>
          {#each prefixes as p (p)}<li><CopyValue value={p} /></li>{/each}
        </ul>
        <p class="small muted">{t('dns.network.check.refused.prefixNote')}</p>
        <div class="row">
          <Button size="sm" icon="sliders" href={href('/dns/settings', { section: 'access' })}>{t('dns.network.check.refused.open')}</Button>
        </div>
      </div>
    {/if}
  {/if}

  {#if check.id === 'devices' && onshowdevices}
    <div class="row">
      <Button size="sm" icon="list" onclick={onshowdevices}>{t('dns.network.check.devices.show')}</Button>
    </div>
  {/if}
</section>

<style>
  .card {
    --c: var(--focus);
    display: flex;
    flex-direction: column;
    gap: var(--sp-3);
    min-width: 0;
    padding: var(--sp-4);
    border: 1px solid var(--line);
    border-left: 4px solid var(--c);
    border-radius: var(--r-panel);
    background: var(--surface);
  }
  .card.warn {
    --c: var(--warn);
  }
  header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: var(--sp-2) var(--sp-3);
  }
  h3 {
    font-size: var(--fs-md);
    overflow-wrap: anywhere;
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    max-width: 90ch;
    color: var(--text-2);
    font-size: var(--fs-sm);
  }
  .note {
    display: flex;
    align-items: flex-start;
    gap: 6px;
    color: var(--text);
  }
  .note :global(.icon) {
    flex: none;
    margin-top: 1px;
    color: var(--warn);
  }
  .steps {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    padding: var(--sp-3) var(--sp-4);
    border-radius: var(--r-control);
    background: var(--surface-2);
  }
  h4 {
    font-size: var(--fs-sm);
  }
  ol {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding-left: var(--sp-5);
    font-size: var(--fs-sm);
    max-width: 90ch;
  }
  li {
    overflow-wrap: anywhere;
  }
  .path {
    font-style: italic;
  }
  .steps-plain {
    padding-left: var(--sp-5);
  }
  a :global(.icon) {
    margin-left: 4px;
    vertical-align: -2px;
  }
  .refused {
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .prefixes {
    font-size: var(--fs-sm);
  }
  /* The badge moves below the address when the card is narrow. */
  .raddr {
    display: inline-flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 2px var(--sp-2);
    padding: 3px 0;
  }
  .raddr .mono {
    white-space: nowrap;
  }
  .trust {
    max-width: 90ch;
    font-size: var(--fs-sm);
  }
  .note.info :global(.icon) {
    color: var(--focus);
  }
  .prefixes ul {
    display: flex;
    flex-wrap: wrap;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
  }
  @media (max-width: 480px) {
    .card {
      padding: var(--sp-3);
    }
    .steps {
      padding: var(--sp-3);
    }
  }
</style>
