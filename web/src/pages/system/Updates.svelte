<!--
  @component
  Updates: the running version, the result of the last check for a new
  release (GitHub, daily or "Check now"), the release notes of an available
  version and how to install it here. With the root helper (mode `helper`)
  admins install it from this page after confirming their password and
  follow its progress through the restart; Docker and manual installations
  get the commands to run. Also the update check settings.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, type ApiError } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { formatDateTime, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Button, Notice, Skeleton, toast } from '$lib/ui'
  import InstallDialog from './updates/InstallDialog.svelte'
  import ProgressPanel from './updates/ProgressPanel.svelte'
  import ReleasePanel from './updates/ReleasePanel.svelte'
  import SettingsPanel from './updates/SettingsPanel.svelte'
  import StatusPanel from './updates/StatusPanel.svelte'
  import { UpdateFollower } from './updates/follow.svelte'

  // The navigation shares this status (its "update available" dot); the
  // page loads it fresh and puts every newer answer back into it.
  const upd = appStatus.update
  const info = $derived(upd.data)

  // (Unless the app is loading it right now, e.g. when it starts on this page.)
  $effect(() => {
    untrack(() => {
      if (!upd.loading) void upd.refresh()
    })
  })

  const follower = new UpdateFollower((i) => upd.set(i))
  $effect(() => () => follower.stop())

  // Follow a run in progress: started here, by another admin, or before the
  // page was reloaded.
  $effect(() => {
    const run = info?.status
    if (run?.state !== 'running' || follower.phase !== 'idle') return
    untrack(() => void follower.follow({ version: run.version, from: run.from }))
  })

  const busy = $derived(follower.active || info?.status?.state === 'running')
  const release = $derived(info?.updateAvailable && info.latest ? info.latest : undefined)
  /** A failed or rolled-back run whose target is still not running (shown until the next run). */
  const lastFailure = $derived.by(() => {
    const run = info?.status
    if (!info || !run || follower.phase !== 'idle') return undefined
    if (run.state !== 'failed' && run.state !== 'rolled-back') return undefined
    return info.current.version !== run.version ? run : undefined
  })

  // ---- check now
  let checking = $state(false)

  async function check() {
    checking = true
    try {
      const i = await api.system.checkUpdate()
      upd.set(i)
      if (i.checkError) toast.error(t('system.updates.checked.error'))
      else if (i.updateAvailable && i.latest) toast.info(t('system.updates.checked.available', { version: i.latest.version }))
      else toast.success(t('system.updates.checked.upToDate'))
    } catch (err) {
      toast.error(err)
    } finally {
      checking = false
    }
  }

  // ---- install (helper mode)
  let installOpen = $state(false)
  let installErr = $state.raw<ApiError | undefined>(undefined)
  /** Frozen when the dialog opens, so a background refresh cannot change what is confirmed. */
  let installTarget = $state.raw<{ version: string; current: string; previousStartedAt?: string } | undefined>(undefined)

  function openInstall() {
    if (!info || !release) return
    installErr = undefined
    installTarget = { version: release.version, current: info.current.version, previousStartedAt: info.status?.startedAt }
    installOpen = true
  }

  function queued() {
    const target = installTarget
    if (!target) return
    void follower.follow({ version: target.version, from: target.current, previousStartedAt: target.previousStartedAt })
  }

  function refused(err: ApiError) {
    installErr = err
    void upd.refresh() // e.g. another admin started it meanwhile: the progress shows up
  }
</script>

<div class="page">
  {#if !session.canOperate}
    <Notice tone="info">{t('common.state.readOnly')}</Notice>
  {/if}

  {#if follower.phase !== 'idle'}
    <ProgressPanel {follower} />
  {/if}

  {#if installErr}
    <Notice tone="fail" title={t('system.updates.install.refused')} ondismiss={() => (installErr = undefined)}>
      {errorText(installErr)}
    </Notice>
  {/if}

  {#if lastFailure}
    <Notice
      tone={lastFailure.state === 'failed' ? 'fail' : 'warn'}
      title={lastFailure.state === 'failed'
        ? t('system.updates.last.failed', { version: lastFailure.version })
        : t('system.updates.last.rolledBack', { version: lastFailure.version })}
    >
      {#if lastFailure.message}<p class="mono msg">{lastFailure.message}</p>{/if}
      <p>
        {lastFailure.state === 'failed' ? t('system.updates.last.failedText') : t('system.updates.last.rolledBackText')}
        {#if lastFailure.finishedAt}
          <span title={formatDateTime(lastFailure.finishedAt, true)}>
            {t('system.updates.last.when', { time: formatRelative(lastFailure.finishedAt) })}</span
          >
        {/if}
      </p>
    </Notice>
  {/if}

  {#if !info}
    {#if upd.error}
      <Notice tone="fail" title={t('system.updates.loadError')}>
        {errorText(upd.error)}
        {#snippet actions()}
          <Button size="sm" icon="refresh" onclick={() => upd.refresh()}>{t('common.action.retry')}</Button>
        {/snippet}
      </Notice>
    {:else}
      <div class="cols-2" aria-busy="true">
        <Skeleton height="260px" />
        <Skeleton height="160px" />
      </div>
    {/if}
  {:else}
    <div class={['layout', release && 'with-release']}>
      <div class="status">
        <StatusPanel {info} {busy} {checking} oncheck={check} />
      </div>
      {#if release}
        <div class="release">
          <ReleasePanel {info} {release} {busy} oninstall={openInstall} />
        </div>
      {/if}
      <div class="settings">
        <SettingsPanel onsaved={() => upd.refresh()} />
      </div>
    </div>
  {/if}
</div>

{#if installOpen && installTarget}
  <InstallDialog
    bind:open={installOpen}
    version={installTarget.version}
    current={installTarget.current}
    onqueued={queued}
    onerror={refused}
  />
{/if}

<style>
  .layout {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    grid-template-areas: 'status settings';
    gap: var(--sp-4);
    align-items: start;
  }
  .layout.with-release {
    grid-template-areas:
      'status release'
      'settings release';
    grid-template-rows: auto 1fr;
  }
  .status {
    grid-area: status;
    min-width: 0;
  }
  .release {
    grid-area: release;
    min-width: 0;
  }
  .settings {
    grid-area: settings;
    min-width: 0;
  }
  @media (max-width: 900px) {
    .layout,
    .layout.with-release {
      grid-template-columns: minmax(0, 1fr);
      grid-template-areas:
        'status'
        'release'
        'settings';
      grid-template-rows: none;
    }
  }
  .msg {
    font-size: var(--fs-sm);
    color: var(--text);
    overflow-wrap: anywhere;
  }
</style>
