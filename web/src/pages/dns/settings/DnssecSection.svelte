<!--
  @component
  DNSSEC: ask upstreams for signed answers and pass their validation result
  (AD flag) on. PiCache does not validate signatures itself.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { DnsSettings } from '$lib/api'
  import type { SettingsForm } from '$lib/settings.svelte'
  import { Panel, Toggle } from '$lib/ui'

  let { form }: { form: SettingsForm<'dns'> } = $props()

  const d = $derived(form.draft as DnsSettings)
</script>

<Panel id="dns-set-dnssec" title={t('dns.settings.dnssec.title')}>
  <Toggle bind:checked={d.dnssec} label={t('dns.settings.dnssec.label')} description={t('dns.settings.dnssec.help')} />
</Panel>
