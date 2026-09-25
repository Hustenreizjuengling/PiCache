<!--
  @component
  IPv6 answers: "do not answer IPv6 addresses" for networks where IPv6 is
  broken, and DNS64 (IPv6 addresses built from IPv4 ones with a /96 NAT64
  prefix) for IPv6-only networks with a NAT64 gateway. The two exclude each
  other, here as on the server.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DnsSettings } from '$lib/api'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Button, Field, Input, Panel, Toggle } from '$lib/ui'

  let { form }: { form: SettingsForm<'dns'> } = $props()

  const d = $derived(form.draft as DnsSettings)
  const defaultPrefix = $derived(form.defaults?.dns64.prefix ?? '64:ff9b::/96')

  const aaaaError = $derived(form.error('disableAAAA'))
  // "dns.dns64" or "dns.dns64.enabled"; the prefix field shows its own error.
  const prefixError = $derived(form.error('dns64.prefix'))
  const dns64Error = $derived(prefixError ? undefined : form.error('dns64'))
</script>

<Panel id="dns-set-ipv6" title={t('dns.settings.ipv6.title')} description={t('dns.settings.ipv6.description')}>
  <div class="stack">
    <div class="stack-sm">
      <Toggle
        bind:checked={d.disableAAAA}
        disabled={d.dns64.enabled}
        label={t('dns.settings.ipv6.disableAaaa')}
        description={t('dns.settings.ipv6.disableAaaaHelp')}
      />
      {#if d.dns64.enabled}<p class="sub small muted">{t('dns.settings.ipv6.exclusiveAaaa')}</p>{/if}
      {#if aaaaError}<p class="sub err">{aaaaError}</p>{/if}
    </div>

    <div class="stack-sm">
      <Toggle
        bind:checked={d.dns64.enabled}
        disabled={d.disableAAAA}
        label={t('dns.settings.ipv6.dns64')}
        description={t('dns.settings.ipv6.dns64Help')}
      />
      {#if d.disableAAAA}<p class="sub small muted">{t('dns.settings.ipv6.exclusiveDns64')}</p>{/if}
      {#if dns64Error}<p class="sub err">{dns64Error}</p>{/if}
    </div>

    {#if d.dns64.enabled}
      <div class="grid">
        <Field label={t('dns.settings.ipv6.prefix')} help={t('dns.settings.ipv6.prefixHelp')} error={prefixError}>
          <Input bind:value={d.dns64.prefix} mono placeholder={defaultPrefix} maxlength={64} autocomplete="off" />
        </Field>
      </div>
      {#if d.dns64.prefix.trim() !== defaultPrefix}
        <div class="row">
          <Button size="sm" variant="ghost" onclick={() => (d.dns64.prefix = defaultPrefix)}>
            {t('dns.settings.ipv6.usePrefix', { prefix: defaultPrefix })}
          </Button>
        </div>
      {/if}
    {/if}
  </div>
</Panel>

<style>
  /* Aligned with the toggle's text (track 36 px + gap). */
  .sub {
    padding-left: calc(36px + var(--sp-3));
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
