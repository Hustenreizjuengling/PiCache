<!--
  @component
  "Device names" (settings.clients.nameSources): where the names of
  addresses come from after a configured client name, first hit wins: DHCP
  lease names, reverse lookups (PTR), this machine's hosts file (in Docker
  the container's own file: the switch says it is not meaningful there) and
  the WHOIS network owner of public addresses (sends data out; explained
  next to the switch). Switching applies immediately; read-only principals
  see the switches disabled.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ClientsSettings } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Notice, Panel, Skeleton, Toggle, toast } from '$lib/ui'

  const form = settingsForm('clients')

  type Source = keyof ClientsSettings['nameSources']
  // The order in which the server asks them.
  const SOURCES: Source[] = ['dhcp', 'ptr', 'hostsFile', 'whois']

  const docker = $derived(appStatus.update.data?.mode === 'docker')

  async function save() {
    if (await form.save()) toast.success(t('dns.names.saved'))
    else {
      toast.error(form.errorMessage ?? t('common.error.generic'))
      form.revert()
    }
  }
</script>

<Panel id="device-names" title={t('dns.names.title')} description={t('dns.names.description')}>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('dns.names.loadError')}>{errorText(form.loadError)}</Notice>
  {:else if !form.draft}
    <Skeleton height="200px" />
  {:else}
    {@const ns = form.draft.nameSources}
    <div class="stack">
      {#each SOURCES as s (s)}
        <div class="stack-sm">
          <Toggle
            bind:checked={ns[s]}
            label={t(`dns.names.${s}`)}
            description={t(`dns.names.${s}Help`)}
            disabled={!session.isAdmin || form.saving}
            onchange={save}
          />
          {#if s === 'hostsFile' && docker}
            <p class="note small muted">{t('dns.names.hostsFileContainer')}</p>
          {/if}
        </div>
      {/each}
    </div>
  {/if}
</Panel>

<style>
  .note {
    padding-left: calc(36px + var(--sp-3));
  }
</style>
