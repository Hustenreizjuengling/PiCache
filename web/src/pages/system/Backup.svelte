<!--
  @component
  Backup & restore: download the configuration database, restore one
  (staged, applied by a restart) and the log retention and privacy settings.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Notice } from '$lib/ui'
  import BackupPanel from './backup/BackupPanel.svelte'
  import LogSettingsPanel from './backup/LogSettingsPanel.svelte'
  import RestorePanel from './backup/RestorePanel.svelte'

  const info = resource((signal) => api.system.info({ signal }))
</script>

<div class="page">
  {#if !session.isAdmin}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  <div class="cols-2">
    <BackupPanel info={info.data} />
    <RestorePanel />
  </div>

  <LogSettingsPanel />
</div>

<style>
  .cols-2 {
    align-items: start;
  }
</style>
