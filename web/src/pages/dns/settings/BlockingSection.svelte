<!--
  @component
  How blocked names are answered (blocking mode, custom addresses, TTL),
  CNAME inspection, the list update interval and the special domains that
  keep browsers and Apple devices on PiCache. Edits the filter section
  (only changed members are saved, so the blocking pause is never touched).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { BlockingMode, FilterSettings } from '$lib/api'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Field, Input, Panel, Select, Toggle } from '$lib/ui'
  import NumberInput from '../shared/NumberInput.svelte'

  let { form }: { form: SettingsForm<'filter'> } = $props()

  const d = $derived(form.draft as FilterSettings)
  const MODES: BlockingMode[] = ['null', 'nxdomain', 'nodata', 'refused', 'custom_ip']

  const modeOptions = $derived([
    { value: 'null', label: t('dns.settings.blocking.mode.null') },
    { value: 'nxdomain', label: t('dns.settings.blocking.mode.nxdomain') },
    { value: 'nodata', label: t('dns.settings.blocking.mode.nodata') },
    { value: 'refused', label: t('dns.settings.blocking.mode.refused') },
    { value: 'custom_ip', label: t('dns.settings.blocking.mode.custom_ip') },
  ])
  const modeHelp = $derived(
    {
      null: t('dns.settings.blocking.modeHelp.null'),
      nxdomain: t('dns.settings.blocking.modeHelp.nxdomain'),
      nodata: t('dns.settings.blocking.modeHelp.nodata'),
      refused: t('dns.settings.blocking.modeHelp.refused'),
      custom_ip: t('dns.settings.blocking.modeHelp.custom_ip'),
    }[d.blockingMode],
  )
</script>

<Panel id="dns-set-blocking" title={t('dns.settings.blocking.title')} description={t('dns.settings.blocking.description')}>
  <div class="stack">
    <div class="grid">
      <Field label={t('dns.settings.blocking.mode')} help={modeHelp} error={form.error('blockingMode')}>
        <Select
          bind:value={() => d.blockingMode, (v) => (d.blockingMode = MODES.includes(v as BlockingMode) ? (v as BlockingMode) : 'null')}
          options={modeOptions}
        />
      </Field>
      <Field label={t('dns.settings.blocking.ttl')} help={t('dns.settings.blocking.ttlHelp')} error={form.error('blockedTtl')}>
        <NumberInput bind:value={d.blockedTtl} min={0} max={86400} unit={t('dns.shared.unit.seconds')} />
      </Field>
    </div>
    {#if d.blockingMode === 'custom_ip'}
      <div class="grid">
        <Field label={t('dns.settings.blocking.ipv4')} required error={form.error('blockingIpv4')}>
          <Input bind:value={d.blockingIpv4} mono placeholder="192.168.1.2" maxlength={64} />
        </Field>
        <Field label={t('dns.settings.blocking.ipv6')} optional help={t('dns.settings.blocking.ipv6Help')} error={form.error('blockingIpv6')}>
          <Input bind:value={d.blockingIpv6} mono placeholder="fd00::2" maxlength={64} />
        </Field>
      </div>
    {/if}
    <Toggle bind:checked={d.cnameInspection} label={t('dns.settings.blocking.cname')} description={t('dns.settings.blocking.cnameHelp')} />
    <Field label={t('dns.settings.blocking.interval')} help={t('dns.settings.blocking.intervalHelp')} error={form.error('updateIntervalHours')}>
      <NumberInput bind:value={d.updateIntervalHours} min={0} max={720} unit={t('dns.shared.unit.hours')} />
    </Field>

    <h3>{t('dns.settings.special.title')}</h3>
    <Toggle
      bind:checked={d.blockMozillaCanary}
      label={t('dns.settings.special.canary')}
      description={t('dns.settings.special.canaryHelp')}
    />
    <Toggle
      bind:checked={d.blockIcloudPrivateRelay}
      label={t('dns.settings.special.relay')}
      description={t('dns.settings.special.relayHelp')}
    />
  </div>
</Panel>

<style>
  h3 {
    font-size: var(--fs-md);
    padding-top: var(--sp-2);
    border-top: 1px solid var(--line);
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
