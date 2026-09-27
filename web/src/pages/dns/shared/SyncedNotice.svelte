<!--
  @component
  The banner of a page whose section this follower syncs from its primary
  (session.syncedSections): "Synced from <origin>: change it on the
  primary", with a link to System › Sync. Renders nothing while the section
  is not synced. The origin comes from GET /system/sync.

  <SyncedNotice section="local-dns" />
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, resource, type SyncSection } from '$lib/api'
  import { href } from '$lib/router.svelte'
  import { session } from '$lib/session.svelte'
  import { Button, Notice } from '$lib/ui'

  interface Props {
    section: SyncSection
    /** An extra sentence (e.g. which members stay this PiCache's own). */
    note?: string
  }

  let { section, note }: Props = $props()

  const synced = $derived(session.isSynced(section))
  // Loaded once per page, only on a follower.
  const status = resource((signal) => (synced ? api.system.sync({ signal }) : Promise.resolve(undefined)))
  const origin = $derived(status.data?.source)
</script>

{#if synced}
  <Notice tone="info" icon="sync" title={origin ? t('common.synced.title', { origin }) : t('common.synced.titleNoOrigin')}>
    {#if note}<p>{note}</p>{/if}
    {#snippet actions()}
      <Button size="sm" variant="ghost" href={href('/system/sync')}>{t('common.synced.status')}</Button>
    {/snippet}
  </Notice>
{/if}
