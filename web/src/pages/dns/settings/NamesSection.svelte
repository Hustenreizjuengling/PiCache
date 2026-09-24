<!--
  @component
  Local names: the local domain (never sent to public upstreams), the names
  PiCache answers with its own addresses and the router resolver (auto =
  default gateway, a fixed address, or off).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import type { DnsSettings, RouterStatus } from '$lib/api'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Field, Input, Panel, Select } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import LinesInput from '../shared/LinesInput.svelte'

  interface Props {
    form: SettingsForm<'dns'>
    /** Current router resolver state (from the status poll). */
    status: RouterStatus | undefined
  }

  let { form, status }: Props = $props()

  const d = $derived(form.draft as DnsSettings)

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
  </div>
</Panel>

<style>
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
