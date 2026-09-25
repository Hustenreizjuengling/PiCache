<!--
  @component
  One storage speed test run. While it runs: the phase, a progress bar and
  Cancel (admins). Afterwards: the result, or why it stopped and what was
  measured until then. The caller polls GET /storage/benchmark about every
  second while it runs.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import { api, type BenchmarkRun } from '$lib/api'
  import { formatDuration, formatPercent } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { Button, Notice, Spinner, toast } from '$lib/ui'
  import { spanMs } from '../shared/util'
  import SpeedResult from './SpeedResult.svelte'
  import { formatMiB, sentence } from './speed'

  interface Props {
    run: BenchmarkRun
    /** Reload the speed test state (after cancelling). */
    onchanged: () => Promise<void> | void
    /** Level of the sub-headings: 3 inside a page panel, 4 inside a side panel section. */
    level?: 3 | 4
  }

  let { run, onchanged, level = 3 }: Props = $props()

  const progress = $derived(Math.max(0, Math.min(1, Number.isFinite(run.progress) ? run.progress : 0)))
  const percent = $derived(formatPercent(progress, 0))
  const phase = $derived(t(`cache.speed.phase.${run.phase}`))
  // Re-evaluated with every poll (a new run object each second).
  const elapsed = $derived(spanMs(run.startedAt, new Date().toISOString()))

  let cancelling = $state(false)
  async function cancel() {
    cancelling = true
    try {
      await api.storage.cancelBenchmark()
      toast.success(t('cache.speed.cancelled'))
      await onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      cancelling = false
    }
  }
</script>

{#if run.state === 'running'}
  <div class="running">
    <div class="now">
      <span class="spin" aria-hidden="true"><Spinner size={18} /></span>
      <!-- Announces phase changes, not every percent. -->
      <p class="phase" role="status">{phase}</p>
      <span class="pct" aria-hidden="true">{percent}</span>
    </div>
    <div
      class="bar"
      role="progressbar"
      aria-label={t('cache.speed.progressLabel')}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(progress * 100)}
      aria-valuetext={t('cache.speed.progressText', { percent, phase })}
    >
      <span class="fill" style:width="{progress * 100}%"></span>
    </div>
    <p class="small muted">
      {elapsed !== null
        ? t('cache.speed.elapsed', { size: formatMiB(run.sizeMiB), time: formatDuration(elapsed) })
        : formatMiB(run.sizeMiB)}
    </p>
    <p class="small muted">{t('cache.speed.keepsRunning')}</p>
    {#if session.isAdmin}
      <div>
        <Button icon="close" loading={cancelling} onclick={cancel}>{t('cache.speed.cancel')}</Button>
      </div>
    {/if}
  </div>
{:else}
  <div class="stack">
    {#if run.state === 'failed'}
      <Notice tone="fail" title={t('cache.speed.failedTitle')}>{sentence(run.error ?? '')}</Notice>
    {:else if run.state === 'cancelled'}
      <Notice tone="info" title={t('cache.speed.cancelledTitle')} />
    {/if}
    {#if run.result}
      {#if run.state !== 'done'}
        <svelte:element this={`h${level}`} class="sub">{t('cache.speed.partial')}</svelte:element>
      {/if}
      <SpeedResult result={run.result} {run} {level} />
    {/if}
  </div>
{/if}

<style>
  .running {
    display: flex;
    flex-direction: column;
    gap: var(--sp-2);
  }
  .now {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
  }
  .spin {
    display: inline-flex;
  }
  .phase {
    flex: 1;
    min-width: 0;
    font-weight: 600;
  }
  .pct {
    font-weight: 600;
    font-variant-numeric: tabular-nums;
  }
  .bar {
    height: 8px;
    overflow: hidden;
    border-radius: var(--r-pill);
    background: var(--surface-3);
  }
  .fill {
    display: block;
    height: 100%;
    background: var(--focus);
  }
  .running > div:last-child {
    margin-top: var(--sp-2);
  }
  .sub {
    font-size: var(--fs-sm);
    font-weight: 600;
  }
</style>
