<!--
  @component
  Set-up of the DHCP server (settings section "dhcp"): the interface whose
  network is served (with PiCache's address on it and a warning when that
  address comes from DHCP), the address range (prefilled with a free-looking
  part of the network when an interface is chosen), the lease time (presets
  or seconds), router, DNS server and domain (empty = detected values, shown
  as placeholders) and DNS names for devices. Saved with the page's save bar.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t, tn } from '$i18n/index.svelte'
  import type { ApiError, DhcpInterface, DhcpSettings, DhcpStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDuration } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Button, Field, Input, Notice, Panel, Select, Toggle, type SelectOption } from '$lib/ui'
  import NumberInput from '../shared/NumberInput.svelte'
  import { interfaceSubnet, intToIp4, privateAddresses, rangeSize, subnetOf, subnetText, suggestRange, inSubnet } from './net'

  interface Props {
    form: SettingsForm<'dhcp'>
    status: DhcpStatus | undefined
    interfaces: readonly DhcpInterface[] | undefined
    interfacesError?: ApiError
    onretryinterfaces: () => void
    /** The default gateway of this machine, if known. */
    gateway?: string
    /** dns.localDomain */
    localDomain?: string
  }

  let { form, status, interfaces, interfacesError, onretryinterfaces, gateway, localDomain }: Props = $props()

  const d = $derived(form.draft as DhcpSettings)
  const readOnly = $derived(!session.isAdmin)

  // ---- interface

  const selected = $derived(interfaces?.find((i) => i.name === d.interface))
  const ownCidr = $derived.by(() => {
    if (selected) return privateAddresses(selected).length === 1 ? privateAddresses(selected)[0] : undefined
    const s = status?.interface
    return s && s.name === d.interface ? `${s.ipv4}/${s.prefixLen}` : undefined
  })
  const ownAddress = $derived(ownCidr?.split('/')[0])
  const subnet = $derived(selected ? interfaceSubnet(selected) : subnetOf(ownCidr))
  const dynamic = $derived(selected ? selected.dynamic4 : status?.interface?.name === d.interface && !!status.interface.dynamic)

  const interfaceOptions = $derived.by((): SelectOption[] => {
    if (!interfaces) return d.interface ? [{ value: d.interface, label: d.interface }] : []
    const list = interfaces.filter((i) => !i.virtual || i.name === d.interface)
    const opts: SelectOption[] = list.map((i) => {
      const p = privateAddresses(i)
      if (i.virtual) return { value: i.name, label: t('dns.dhcp.setup.virtual', { name: i.name }), disabled: true }
      if (p.length === 0) return { value: i.name, label: t('dns.dhcp.setup.noPrivate', { name: i.name }), disabled: true }
      if (p.length > 1) return { value: i.name, label: t('dns.dhcp.setup.severalPrivate', { name: i.name }), disabled: true }
      return {
        value: i.name,
        label: i.dynamic4
          ? t('dns.dhcp.setup.optionDynamic', { name: i.name, address: p[0] })
          : t('dns.dhcp.setup.option', { name: i.name, address: p[0] }),
      }
    })
    if (d.interface && !list.some((i) => i.name === d.interface)) {
      opts.push({ value: d.interface, label: t('dns.dhcp.setup.notFound', { name: d.interface }) })
    }
    return opts
  })

  /** A new interface: suggest a range in its network unless the current one fits. */
  function chooseInterface(name: string) {
    d.interface = name
    const sn = interfaceSubnet(interfaces?.find((i) => i.name === name))
    if (!sn || (inSubnet(d.rangeStart, sn) && inSubnet(d.rangeEnd, sn))) return
    const s = suggestRange(sn, [intToIp4(sn.own), gateway])
    if (s) {
      d.rangeStart = s.start
      d.rangeEnd = s.end
    }
  }

  // ---- range

  const size = $derived(rangeSize(d.rangeStart, d.rangeEnd))
  const rangeHelp = $derived(
    [
      size ? tn('dns.dhcp.setup.rangeCount', size) : '',
      subnet ? t('dns.dhcp.setup.rangeHelp', { subnet: subnetText(subnet) }) : t('dns.dhcp.setup.rangeHelpNoSubnet'),
    ]
      .filter(Boolean)
      .join(' · '),
  )
  const placeholderRange = $derived(subnet ? suggestRange(subnet, [intToIp4(subnet.own), gateway]) : undefined)

  // ---- lease time

  const PRESETS = [3600, 43200, 86400, 604800]
  let custom = $state(untrack(() => !PRESETS.includes(d.leaseSeconds)))

  // Revert or a server value outside the presets.
  $effect(() => {
    const v = d.leaseSeconds
    untrack(() => {
      if (!PRESETS.includes(v)) custom = true
    })
  })

  const leaseOptions = $derived<SelectOption[]>([
    { value: '3600', label: t('dns.dhcp.setup.lease.hour') },
    { value: '43200', label: t('dns.dhcp.setup.lease.halfDay') },
    { value: '86400', label: t('dns.dhcp.setup.lease.day') },
    { value: '604800', label: t('dns.dhcp.setup.lease.week') },
    { value: 'custom', label: t('dns.dhcp.setup.lease.custom') },
  ])

  function chooseLease(v: string) {
    if (v === 'custom') {
      custom = true
      return
    }
    custom = false
    d.leaseSeconds = Number(v)
  }

  // ---- defaults shown as placeholders

  const domainExample = $derived(d.domain.trim() || localDomain || 'lan')
</script>

<Panel id="dhcp-setup" title={t('dns.dhcp.setup.title')} description={t('dns.dhcp.setup.description')}>
  <fieldset class="plain" disabled={readOnly}>
    <div class="stack">
      <div class="grid">
        <div class="span stack-sm">
          <Field label={t('dns.dhcp.setup.interface')} help={t('dns.dhcp.setup.interfaceHelp')} error={form.error('interface')}>
            <Select
              bind:value={() => d.interface, chooseInterface}
              options={interfaceOptions}
              placeholder={t('dns.dhcp.setup.interfacePlaceholder')}
            />
          </Field>
          {#if interfacesError && !interfaces}
            <Notice tone="warn">
              {t('dns.dhcp.setup.interfacesError', { error: errorText(interfacesError) })}
              {#snippet actions()}
                <Button size="sm" icon="refresh" onclick={onretryinterfaces}>{t('common.action.retry')}</Button>
              {/snippet}
            </Notice>
          {/if}
          {#if ownCidr && dynamic}
            <Notice tone="warn" title={t('dns.dhcp.setup.dynamicTitle', { address: ownCidr })}>{t('dns.dhcp.setup.dynamicText')}</Notice>
          {:else if ownCidr}
            <p class="small muted">{t('dns.dhcp.setup.address', { address: ownCidr })}</p>
          {/if}
        </div>

        <Field label={t('dns.dhcp.setup.rangeStart')} error={form.error('rangeStart')}>
          <Input bind:value={d.rangeStart} mono placeholder={placeholderRange?.start ?? '192.168.1.100'} maxlength={15} autocomplete="off" />
        </Field>
        <Field label={t('dns.dhcp.setup.rangeEnd')} error={form.error('rangeEnd')}>
          <Input bind:value={d.rangeEnd} mono placeholder={placeholderRange?.end ?? '192.168.1.199'} maxlength={15} autocomplete="off" />
        </Field>
        <p class="span small muted range-help">{rangeHelp}</p>

        <div class="stack-sm">
          <Field label={t('dns.dhcp.setup.leaseTime')} help={custom ? undefined : t('dns.dhcp.setup.leaseHelp')} error={form.error('leaseSeconds')}>
            <Select bind:value={() => (custom ? 'custom' : String(d.leaseSeconds)), chooseLease} options={leaseOptions} />
          </Field>
          {#if custom}
            <Field label={t('dns.dhcp.setup.leaseSeconds')} hideLabel help={t('dns.dhcp.setup.leaseCustomHelp', { duration: formatDuration(d.leaseSeconds * 1000) })}>
              <NumberInput
                bind:value={d.leaseSeconds}
                min={300}
                max={604800}
                unit={t('dns.shared.unit.seconds')}
                ariaLabel={t('dns.dhcp.setup.leaseSeconds')}
              />
            </Field>
          {/if}
        </div>
        <Field
          label={t('dns.dhcp.setup.router')}
          optional
          help={gateway ? t('dns.dhcp.setup.routerHelpAuto', { address: gateway }) : t('dns.dhcp.setup.routerHelp')}
          error={form.error('router')}
        >
          <Input bind:value={d.router} mono placeholder={gateway ?? '192.168.1.1'} maxlength={15} autocomplete="off" />
        </Field>
        <Field
          label={t('dns.dhcp.setup.dnsServer')}
          optional
          help={ownAddress ? t('dns.dhcp.setup.dnsHelpAuto', { address: ownAddress }) : t('dns.dhcp.setup.dnsHelp')}
          error={form.error('dnsServer')}
        >
          <Input bind:value={d.dnsServer} mono placeholder={ownAddress ?? ''} maxlength={15} autocomplete="off" />
        </Field>
        <Field
          label={t('dns.dhcp.setup.domain')}
          optional
          help={localDomain ? t('dns.dhcp.setup.domainHelpAuto', { domain: localDomain }) : t('dns.dhcp.setup.domainHelp')}
          error={form.error('domain')}
        >
          <Input bind:value={d.domain} mono placeholder={localDomain ?? 'lan'} maxlength={253} autocomplete="off" />
        </Field>
      </div>

      <div class="stack-sm">
        <Toggle
          bind:checked={d.registerHostnames}
          label={t('dns.dhcp.setup.register')}
          description={t('dns.dhcp.setup.registerHelp', { domain: domainExample })}
        />
        {#if form.error('registerHostnames')}<p class="sub err">{form.error('registerHostnames')}</p>{/if}
      </div>
    </div>
  </fieldset>
</Panel>

<style>
  .plain {
    min-width: 0;
    margin: 0;
    padding: 0;
    border: 0;
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  .span {
    grid-column: 1 / -1;
    max-width: calc(50% - var(--sp-4) / 2);
  }
  .range-help {
    margin-top: calc(-1 * var(--sp-2));
    max-width: none;
  }
  /* Aligned with the toggle's text (track 36 px + gap). */
  .sub {
    padding-left: calc(36px + var(--sp-3));
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  @media (max-width: 700px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
    .span {
      max-width: none;
    }
  }
</style>
