<!--
  @component
  Local names: the local domain (never sent to public upstreams), the names
  PiCache answers with its own addresses and, optionally, which addresses
  those are (with this machine's interface addresses as help), the router
  resolver (auto = default gateway, a fixed address, or off), single-label
  names kept local and extra networks whose reverse lookups stay local
  (with a warning for public ranges).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, resource, type DnsSettings, type RouterStatus } from '$lib/api'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Field, Input, Notice, Panel, Select, Toggle } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import LinesInput from '../shared/LinesInput.svelte'
  import { isPublicNetwork } from './ranges'

  interface Props {
    form: SettingsForm<'dns'>
    /** Current router resolver state (from the status poll). */
    status: RouterStatus | undefined
  }

  let { form, status }: Props = $props()

  const d = $derived(form.draft as DnsSettings)

  // This machine's addresses, as help for the server-name addresses (link-local ones are never answered).
  const interfaces = resource((signal) => api.dhcp.interfaces({ signal }))
  const ownV4 = $derived(
    (interfaces.data ?? []).flatMap((i) => i.ipv4.map((a) => `${a.split('/')[0]} (${i.name})`)),
  )
  const ownV6 = $derived(
    (interfaces.data ?? []).flatMap((i) =>
      i.ipv6.filter((a) => a.kind !== 'link-local' && !a.temporary).map((a) => `${a.address} (${i.name})`),
    ),
  )

  type Mode = 'auto' | 'off' | 'manual'

  function modeOf(v: string): Mode {
    return v === 'auto' ? 'auto' : v === '' ? 'off' : 'manual'
  }

  let mode = $state<Mode>(untrack(() => modeOf(d.routerResolver)))
  let address = $state(untrack(() => (modeOf(d.routerResolver) === 'manual' ? d.routerResolver : '')))

  function value(): string {
    return mode === 'auto' ? 'auto' : mode === 'off' ? '' : address.trim()
  }

  // Revert or "reset to default" changed the setting.
  $effect(() => {
    const v = d.routerResolver
    untrack(() => {
      if (v === value()) return
      mode = modeOf(v)
      if (mode === 'manual') address = v
    })
  })

  function apply() {
    d.routerResolver = value()
  }

  const modeOptions = $derived([
    { value: 'auto', label: t('dns.router.mode.auto') },
    { value: 'manual', label: t('dns.router.mode.manual') },
    { value: 'off', label: t('dns.router.mode.off') },
  ])

  // Entries outside the private ranges take PTR queries away from the public upstreams.
  const publicNetworks = $derived(d.privateReverseNetworks.filter(isPublicNetwork))

  const statusText = $derived.by(() => {
    if (!status || status.mode === 'off') return undefined
    if (!status.address) return t('dns.settings.names.routerNone')
    return status.answers
      ? t('dns.settings.names.routerOk', { address: status.address })
      : t('dns.settings.names.routerBad', { address: status.address || '–' })
  })
</script>

<Panel id="dns-set-names" title={t('dns.settings.names.title')} description={t('dns.settings.names.description')}>
  <div class="stack">
    <div class="grid">
      <Field label={t('dns.settings.names.localDomain')} optional help={t('dns.settings.names.localDomainHelp')} error={form.error('localDomain')}>
        <Input bind:value={d.localDomain} mono placeholder="lan" maxlength={253} autocomplete="off" />
      </Field>
      <Field label={t('dns.settings.names.serverNames')} optional help={t('dns.settings.names.serverNamesHelp')} error={lineError(form.saveError, 'dns.serverNames')}>
        <LinesInput bind:value={d.serverNames} rows={2} placeholder="picache" />
      </Field>
    </div>
    <section class="stack-sm sub" aria-labelledby="dns-names-addr">
      <h3 id="dns-names-addr">{t('dns.settings.names.addresses')}</h3>
      <p class="small muted">{t('dns.settings.names.addressesHelp')}</p>
      <div class="grid">
        <Field
          label={t('dns.settings.names.addressesV4')}
          optional
          help={ownV4.length > 0 ? t('dns.settings.names.ownAddresses', { addresses: ownV4.join(', ') }) : undefined}
          error={lineError(form.saveError, 'dns.serverNameAddresses.ipv4')}
        >
          <LinesInput bind:value={d.serverNameAddresses.ipv4} rows={2} placeholder={t('dns.settings.names.automatic')} />
        </Field>
        <Field
          label={t('dns.settings.names.addressesV6')}
          optional
          help={ownV6.length > 0 ? t('dns.settings.names.ownAddresses', { addresses: ownV6.join(', ') }) : undefined}
          error={lineError(form.saveError, 'dns.serverNameAddresses.ipv6')}
        >
          <LinesInput bind:value={d.serverNameAddresses.ipv6} rows={2} placeholder={t('dns.settings.names.automatic')} />
        </Field>
      </div>
    </section>
    <div class="grid">
      <Field label={t('dns.settings.names.router')} help={statusText ?? t('dns.settings.names.routerHelp')} error={form.error('routerResolver')}>
        <Select
          bind:value={() => mode, (v) => {
            mode = v === 'manual' || v === 'off' ? v : 'auto'
            apply()
          }}
          options={modeOptions}
        />
      </Field>
      {#if mode === 'manual'}
        <Field label={t('dns.router.address')} required help={address.trim() ? undefined : t('dns.settings.names.routerAddressHelp')}>
          <Input bind:value={address} mono placeholder="192.168.1.1" maxlength={64} oninput={apply} />
        </Field>
      {/if}
    </div>
    <div class="stack-sm">
      <Toggle
        bind:checked={d.domainNeeded}
        label={t('dns.settings.names.domainNeeded')}
        description={d.localDomain.trim()
          ? t('dns.settings.names.domainNeededHelp', { example: `nas.${d.localDomain.trim()}` })
          : t('dns.settings.names.domainNeededHelpNoDomain')}
      />
      {#if form.error('domainNeeded')}<p class="err">{form.error('domainNeeded')}</p>{/if}
    </div>
    <Field
      label={t('dns.settings.names.reverse')}
      optional
      help={t('dns.settings.names.reverseHelp')}
      error={lineError(form.saveError, 'dns.privateReverseNetworks')}
    >
      <LinesInput bind:value={d.privateReverseNetworks} rows={3} placeholder="10.8.0.0/16" />
    </Field>
    {#if publicNetworks.length > 0}
      <Notice tone="warn">
        {#each publicNetworks as n (n)}
          <p>{t('dns.settings.names.reversePublic', { network: n })}</p>
        {/each}
      </Notice>
    {/if}
  </div>
</Panel>

<style>
  .sub {
    padding-top: var(--sp-3);
    border-top: 1px solid var(--line);
  }
  h3 {
    font-size: var(--fs-md);
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
  }
  @media (max-width: 700px) {
    .grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
