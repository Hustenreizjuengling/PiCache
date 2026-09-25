<!--
  @component
  The available release: its notes and how to install it in this
  installation: the "Install update" button (root helper), the Docker
  commands or the command for the host.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { UpdateInfo, UpdateRelease } from '$lib/api'
  import { session } from '$lib/session.svelte'
  import { Badge, Button, CopyButton, Panel } from '$lib/ui'
  import ReleaseNotes from './ReleaseNotes.svelte'

  interface Props {
    info: UpdateInfo
    release: UpdateRelease
    /** An update is being installed. */
    busy: boolean
    oninstall: () => void
  }

  let { info, release, busy, oninstall }: Props = $props()

  const command = $derived(
    (info.mode === 'docker' ? (info.commands?.docker ?? info.commands?.cli) : info.commands?.cli) || 'sudo picache update',
  )
  const notes = $derived(release.notes?.trim() ?? '')
</script>

<Panel title={t('system.updates.release.title', { version: release.version })}>
  {#snippet actions()}
    {#if release.prerelease}<Badge tone="warn">{t('system.updates.release.prerelease')}</Badge>{/if}
  {/snippet}

  <div class="stack">
    {#if notes}
      <ReleaseNotes source={notes} label={t('system.updates.notes.label', { version: release.version })} />
    {:else}
      <p class="small muted">{t('system.updates.notes.none')}</p>
    {/if}
  </div>

  {#snippet footer()}
    {#if info.mode === 'helper'}
      <p class="small muted grow">{t('system.updates.how.helper', { current: info.current.version })}</p>
      {#if session.isAdmin}
        <Button variant="primary" icon="update" disabled={busy} onclick={oninstall}>
          {t('system.updates.install.button', { version: release.version })}
        </Button>
      {:else}
        <p class="small muted">{t('system.updates.how.adminOnly')}</p>
      {/if}
    {:else}
      <div class="how stack-sm">
        <p class="small">{info.mode === 'docker' ? t('system.updates.how.docker') : t('system.updates.how.manual')}</p>
        <div class="cmd">
          <code class="mono">{command}</code>
          <CopyButton text={command} label={t('system.updates.how.copy')} />
        </div>
        {#if info.mode === 'docker'}
          <p class="small muted">{t('system.updates.how.dockerHint')}</p>
        {:else}
          <p class="small muted">{t('system.updates.how.manualHint')}</p>
        {/if}
      </div>
    {/if}
  {/snippet}
</Panel>

<style>
  .grow {
    flex: 1 1 260px;
  }
  .how {
    flex: 1 1 100%;
    min-width: 0;
  }
  .cmd {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    padding: var(--sp-2) var(--sp-3);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface-2);
    min-width: 0;
  }
  .cmd code {
    flex: 1;
    min-width: 0;
    font-size: var(--fs-sm);
    overflow-wrap: anywhere;
    user-select: all;
  }
</style>
