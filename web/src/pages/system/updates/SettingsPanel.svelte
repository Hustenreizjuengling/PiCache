<!--
  @component
  Update settings (settings.updates): the daily check and whether
  pre-releases are offered. Switching applies immediately.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Notice, Panel, Skeleton, Toggle, toast } from '$lib/ui'

  let { onsaved }: { onsaved?: () => void } = $props()

  const form = settingsForm('updates')

  async function save(message: string) {
    if (await form.save()) {
      toast.success(message)
      onsaved?.()
    } else {
      toast.error(form.errorMessage ?? t('common.error.generic'))
      form.revert()
    }
  }
</script>

<Panel title={t('system.updates.settings.title')} description={t('system.updates.settings.description')}>
  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.updates.settings.loadError')}>{form.loadError.message}</Notice>
  {:else if !form.draft}
    <Skeleton height="96px" />
  {:else}
    <div class="stack">
      <Toggle
        bind:checked={form.draft.checkEnabled}
        label={t('system.updates.settings.check')}
        description={t('system.updates.settings.checkHelp')}
        disabled={!session.isAdmin || form.saving}
        onchange={(on) => save(on ? t('system.updates.settings.checkOn') : t('system.updates.settings.checkOff'))}
      />
      <Toggle
        bind:checked={form.draft.includePrereleases}
        label={t('system.updates.settings.pre')}
        description={t('system.updates.settings.preHelp')}
        disabled={!session.isAdmin || form.saving}
        onchange={(on) => save(on ? t('system.updates.settings.preOn') : t('system.updates.settings.preOff'))}
      />
    </div>
  {/if}
</Panel>
