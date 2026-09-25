<!--
  @component
  Progress of an update run: the helper's steps (download, verify, install,
  restart, health check, and rollback when the new version did not come up),
  "Restarting PiCache…" while it does not answer, and the result. After a
  successful update to another version the page reloads, so the browser
  loads the new version's web interface.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import type { UpdateStep } from '$lib/api'
  import { formatDuration } from '$lib/format'
  import type { IconName } from '$lib/icons'
  import { Button, Icon, Notice, Panel, Spinner } from '$lib/ui'
  import type { UpdateFollower } from './follow.svelte'

  let { follower }: { follower: UpdateFollower } = $props()

  const RELOAD_S = 5
  /** A queued request the helper has not started after this long gets a hint. */
  const QUEUE_HINT_S = 45

  type StepState = 'done' | 'current' | 'pending' | 'failed'
  const ORDER: UpdateStep[] = ['download', 'verify', 'install', 'restart', 'health']

  const run = $derived(follower.run)
  const phase = $derived(follower.phase)
  const rollback = $derived(run?.step === 'rollback' || phase === 'rolled-back')

  const steps = $derived.by((): { step: UpdateStep; state: StepState }[] => {
    const list: UpdateStep[] = rollback ? [...ORDER, 'rollback'] : ORDER
    if (phase === 'succeeded') return list.map((step) => ({ step, state: 'done' }))
    if (!run || phase === 'queued') return list.map((step) => ({ step, state: 'pending' }))
    // While PiCache does not answer it is restarting, even if the last report
    // was still "install".
    let at: UpdateStep = run.step === 'done' ? 'health' : run.step
    if (phase === 'restarting' && ORDER.indexOf(at) >= 0 && ORDER.indexOf(at) < ORDER.indexOf('restart')) at = 'restart'
    if (rollback) {
      return list.map((step) => ({
        step,
        state:
          step === 'health'
            ? 'failed'
            : step === 'rollback'
              ? phase === 'rolled-back'
                ? 'done'
                : 'current'
              : 'done',
      }))
    }
    const idx = list.indexOf(at)
    return list.map((step, i) => ({
      step,
      state: i < idx ? 'done' : i > idx ? 'pending' : phase === 'failed' ? 'failed' : phase === 'timeout' ? 'pending' : 'current',
    }))
  })

  const ICON: Record<Exclude<StepState, 'current'>, IconName> = { done: 'success', pending: 'clock', failed: 'error' }

  const title = $derived.by(() => {
    const version = follower.target
    switch (phase) {
      case 'succeeded':
        return t('system.updates.progress.titleDone', { version })
      case 'failed':
        return t('system.updates.progress.titleFailed', { version })
      case 'rolled-back':
        return t('system.updates.progress.titleRolledBack', { version })
      default:
        return t('system.updates.progress.title', { version })
    }
  })

  // ---- reload after a successful update (new web interface assets)
  const reloads = $derived(phase === 'succeeded' && !!follower.current && follower.current !== follower.from)

  $effect(() => {
    if (!reloads) return
    const timer = setTimeout(() => location.reload(), RELOAD_S * 1000)
    return () => clearTimeout(timer)
  })
</script>

<Panel {title}>
  <div class="stack">
    {#if phase === 'succeeded'}
      <Notice tone="ok" title={t('system.updates.progress.doneTitle', { version: follower.current || follower.target })}>
        {#if reloads}
          <!-- A fixed text: a live countdown would be announced every second. -->
          <p>{tn('system.updates.progress.reloading', RELOAD_S)}</p>
        {:else}
          <p>{t('system.updates.progress.doneText')}</p>
        {/if}
        {#snippet actions()}
          <Button size="sm" variant="primary" icon="refresh" onclick={() => location.reload()}>
            {t('system.updates.progress.reloadNow')}
          </Button>
        {/snippet}
      </Notice>
    {:else if phase === 'failed'}
      <Notice tone="fail" title={t('system.updates.progress.failedTitle')}>
        {#if run?.message}<p class="mono msg">{run.message}</p>{/if}
        <p>
          {follower.current
            ? t('system.updates.progress.failedRunning', { version: follower.current })
            : t('system.updates.progress.failedText')}
        </p>
      </Notice>
    {:else if phase === 'rolled-back'}
      <Notice tone="warn" title={t('system.updates.progress.rolledBackTitle', { version: follower.from })}>
        {#if run?.message}<p class="mono msg">{run.message}</p>{/if}
        <p>{t('system.updates.progress.rolledBackText')}</p>
      </Notice>
    {:else if phase === 'timeout'}
      <Notice tone="warn" title={run ? t('system.updates.progress.timeoutTitle') : t('system.updates.progress.stalledTitle')}>
        <p>{run ? t('system.updates.progress.timeoutText') : t('system.updates.progress.stalledText')}</p>
        {#snippet actions()}
          <Button size="sm" icon="refresh" onclick={() => follower.resume()}>{t('system.updates.progress.keepWaiting')}</Button>
        {/snippet}
      </Notice>
    {:else}
      <div class="now" role="status">
        <Spinner size={20} />
        <div class="stack-sm">
          {#if phase === 'restarting'}
            <p class="strong">{t('system.updates.progress.restarting')}</p>
            <p class="small muted">
              {follower.downFor > 0
                ? t('system.updates.progress.noAnswer', { time: formatDuration(follower.downFor * 1000) })
                : t('system.updates.progress.restartingText')}
            </p>
          {:else if phase === 'queued'}
            <p class="strong">{t('system.updates.progress.queued')}</p>
            {#if follower.queuedFor >= QUEUE_HINT_S}
              <p class="small muted">{t('system.updates.progress.queuedSlow')}</p>
            {/if}
          {:else}
            <p class="strong">
              {t(`system.updates.stepNow.${run?.step ?? 'download'}`, { version: follower.target, from: follower.from })}
            </p>
            <p class="small muted">{t('system.updates.progress.keepOpen')}</p>
          {/if}
        </div>
      </div>
    {/if}

    <ol class="steps" aria-label={t('system.updates.progress.steps')}>
      {#each steps as s (s.step)}
        <li class={['step', s.state]} aria-current={s.state === 'current' ? 'step' : undefined}>
          <span class="ic">
            {#if s.state === 'current'}
              <Spinner size={16} />
            {:else}
              <Icon name={ICON[s.state]} size={18} />
            {/if}
          </span>
          <span>{t(`system.updates.step.${s.step}`, { version: follower.target, from: follower.from })}</span>
          <span class="visually-hidden">({t(`system.updates.stepState.${s.state}`)})</span>
        </li>
      {/each}
    </ol>
  </div>
</Panel>

<style>
  .now {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .strong {
    font-weight: 600;
  }
  .msg {
    font-size: var(--fs-sm);
    color: var(--text);
    overflow-wrap: anywhere;
  }
  .steps {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
    margin: 0;
    padding: 0;
    list-style: none;
    font-size: var(--fs-sm);
  }
  .step {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    color: var(--text-2);
  }
  .ic {
    display: inline-flex;
    justify-content: center;
    width: 20px;
  }
  .step.done .ic {
    color: var(--ok);
  }
  .step.pending {
    color: var(--text-3);
  }
  .step.current {
    color: var(--text);
    font-weight: 600;
  }
  .step.failed {
    color: var(--text);
  }
  .step.failed .ic {
    color: var(--fail);
  }
</style>
