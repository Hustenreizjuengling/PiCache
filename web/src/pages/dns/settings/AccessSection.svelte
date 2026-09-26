<!--
  @component
  Who may use this DNS server: extra client networks beyond the private
  ones, trusting every network this machine is connected to (public IPv6
  prefixes too, followed when the provider changes them), the dangerous
  "answer everyone" switch (behind a confirmation), refusing ANY queries,
  blocked clients (DNS only) and trusted forwarders whose EDNS options
  identify the devices behind them.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { DnsSettings, DnsStats } from '$lib/api'
  import { formatNumber } from '$lib/format'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { confirm, Field, Notice, Panel, Toggle } from '$lib/ui'
  import { lineError } from '../shared/errors'
  import LinesInput from '../shared/LinesInput.svelte'

  let { form, stats }: { form: SettingsForm<'dns'>; stats: DnsStats | undefined } = $props()

  const d = $derived(form.draft as DnsSettings)

  // The options a trusted forwarder must set (dnsmasq), shown verbatim.
  const STRIP_FLAGS = '--strip-subnet --strip-mac --add-subnet=32,128 --add-mac'

  // Opening the resolver to everyone needs an explicit confirmation.
  async function setAllowAll(on: boolean) {
    if (!on) {
      d.allowAllNetworks = false
      return
    }
    const ok = await confirm({
      title: t('dns.settings.access.allowAllTitle'),
      message: t('dns.settings.access.allowAllText'),
      confirmLabel: t('dns.settings.access.allowAllConfirm'),
    })
    if (ok && form.draft) form.draft.allowAllNetworks = true
  }
</script>

<Panel id="dns-set-access" title={t('dns.settings.access.title')} description={t('dns.settings.access.description')}>
  <div class="stack">
    <div class="stack-sm">
      <Toggle
        bind:checked={d.trustConnectedNetworks}
        label={t('dns.settings.access.trustConnected')}
        description={t('dns.settings.access.trustConnectedHelp')}
      />
      {#if form.error('trustConnectedNetworks')}<p class="err">{form.error('trustConnectedNetworks')}</p>{/if}
    </div>
    <Field
      label={t('dns.settings.access.networks')}
      optional
      help={t('dns.settings.access.networksHelp')}
      error={lineError(form.saveError, 'dns.allowedNetworks')}
    >
      <LinesInput bind:value={d.allowedNetworks} rows={3} placeholder="100.64.0.0/10" />
    </Field>
    <div class={['danger', d.allowAllNetworks && 'on']}>
      <Toggle
        bind:checked={() => d.allowAllNetworks, setAllowAll}
        label={t('dns.settings.access.allowAll')}
        description={t('dns.settings.access.allowAllHelp')}
      />
    </div>
    {#if d.allowAllNetworks}
      <Notice tone="fail" title={t('dns.settings.access.openTitle')}>{t('dns.settings.access.openText')}</Notice>
    {/if}
    <Toggle bind:checked={d.refuseAny} label={t('dns.settings.access.refuseAny')} description={t('dns.settings.access.refuseAnyHelp')} />

    <div class="grid">
      <div class="stack-sm">
        <Field
          label={t('dns.settings.access.blocked')}
          optional
          help={t('dns.settings.access.blockedHelp')}
          error={lineError(form.saveError, 'dns.blockedClients')}
        >
          <LinesInput bind:value={d.blockedClients} rows={4} placeholder={'192.168.1.66\naa:bb:cc:dd:ee:ff'} />
        </Field>
        {#if stats && stats.blockedClients > 0}
          <p class="small muted">{tn('dns.settings.access.blockedCount', stats.blockedClients, { count: formatNumber(stats.blockedClients) })}</p>
        {/if}
      </div>
      <Field
        label={t('dns.settings.access.trusted')}
        optional
        help={t('dns.settings.access.trustedHelp')}
        error={lineError(form.saveError, 'dns.ednsClientTrusted')}
      >
        <LinesInput bind:value={d.ednsClientTrusted} rows={4} placeholder="192.168.1.1" />
      </Field>
    </div>
    {#if d.ednsClientTrusted.length > 0}
      <Notice tone="warn" title={t('dns.settings.access.trustedWarnTitle')}>
        <p>{t('dns.settings.access.trustedWarn')}</p>
        <p class="flags"><code class="mono">{STRIP_FLAGS}</code></p>
      </Notice>
    {/if}
  </div>
</Panel>

<style>
  .danger {
    padding: var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
  }
  .danger.on {
    border-color: var(--danger);
  }
  .err {
    color: var(--danger);
    font-size: var(--fs-sm);
  }
  .flags {
    margin-top: var(--sp-1);
    overflow-wrap: anywhere;
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
