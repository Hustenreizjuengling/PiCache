<!--
  @component
  System › Sync: this PiCache as a read-only follower of another one (the
  primary). The status card (GET /system/sync) shows the primary, the
  synced sections, the last run, its error and the next run, with "Sync
  now" for admins (followed until the run ends). Admins set the follower
  up: the primary's address, a sync token of the primary (write-only), its
  CA certificate, the interval and the sections (with the dependency rule).
  Viewers see the status only.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type Resource, type SyncStatus } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Notice } from '$lib/ui'
  import FollowerPanel from './sync/FollowerPanel.svelte'
  import StatusPanel from './sync/StatusPanel.svelte'

  // Every 2 s while a run is going (started here or on schedule), else every 30 s.
  const status: Resource<SyncStatus> = resource((signal) => api.system.sync({ signal }), {
    interval: () => (status.data?.running ? 2_000 : 30_000),
  })

  function saved() {
    void status.refresh()
    // The synced sections are read-only everywhere: the pages learn it from /auth/status.
    void session.refresh()
  }
</script>

<div class="page">
  {#if !session.canOperate}
    <Notice tone="info">{t('system.sync.viewerNote')}</Notice>
  {/if}

  {#if session.canOperate}
    <div class="layout">
      <StatusPanel {status} />
      <FollowerPanel onsaved={saved} />
    </div>
  {:else}
    <StatusPanel {status} />
  {/if}
</div>

<style>
  .layout {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: var(--sp-4);
    align-items: start;
  }
  @media (max-width: 1000px) {
    .layout {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
