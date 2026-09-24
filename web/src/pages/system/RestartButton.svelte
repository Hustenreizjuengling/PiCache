<!--
  @component
  "Restart PiCache": asks for confirmation, then shows a dialog until the new
  process answers. After a restore all sessions are revoked, so the app then
  shows the sign-in screen.
  <RestartButton variant="primary" label="Restart now" message="…" />
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { formatDuration } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Button, Dialog, Notice, Spinner, confirm, toast } from '$lib/ui'
  import { RestartWatcher } from './restart.svelte'

  interface Props {
    label?: string
    variant?: 'primary' | 'secondary' | 'danger'
    size?: 'sm' | 'md'
    /** Text of the confirmation (defaults to the general restart explanation). */
    message?: string
  }

  let { label, variant = 'secondary', size = 'md', message }: Props = $props()

  const watcher = new RestartWatcher()
  let open = $state(false)

  $effect(() => () => watcher.stop())

  async function ask() {
    const ok = await confirm({
      title: t('system.restart.confirmTitle'),
      message: message ?? t('system.restart.confirmText'),
      confirmLabel: t('system.restart.confirm'),
      danger: false,
      action: () => watcher.request(),
    })
    if (ok) await follow()
  }

  async function follow() {
    open = true
    const outcome = await watcher.wait()
    if (!outcome) return
    open = false
    if (outcome.signedIn) {
      toast.success(t('system.restart.done'))
      void appStatus.overview.refresh()
    } else {
      await session.load() // the restore revoked this session: back to sign-in
    }
  }
</script>

<Button {variant} {size} icon="power" disabled={!session.isAdmin} onclick={ask}>
  {label ?? t('system.restart.button')}
</Button>

<Dialog bind:open title={t('system.restart.waitTitle')} size="sm" onclose={() => watcher.stop()}>
  {#if watcher.phase === 'timeout'}
    <Notice tone="warn" title={t('system.restart.timeoutTitle')}>{t('system.restart.timeoutText')}</Notice>
  {:else}
    <div class="wait" role="status">
      <Spinner size={24} />
      <div class="stack-sm">
        <p>{t('system.restart.waiting')}</p>
        {#if watcher.elapsed > 0}
          <p class="small muted">{t('system.restart.elapsed', { time: formatDuration(watcher.elapsed * 1000) })}</p>
        {/if}
      </div>
    </div>
  {/if}
  {#snippet actions()}
    <Button onclick={() => (open = false)}>{t('common.action.close')}</Button>
    {#if watcher.phase === 'timeout'}
      <Button variant="primary" icon="refresh" onclick={follow}>{t('system.restart.keepWaiting')}</Button>
    {/if}
  {/snippet}
</Dialog>

<style>
  .wait {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
  }
</style>
