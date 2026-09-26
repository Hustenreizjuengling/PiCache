<!--
  @component
  Storage speed test panel: the running or most recent test of any storage
  location with its result, and "Test speed" for admins. A test goes on when
  the page is left and is picked up again here (the page polls the state).
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import type { ApiError, BenchmarkStatus, StorageTargetWithStatus } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { Button, Notice, Panel, Skeleton } from '$lib/ui'
  import SpeedRun from './SpeedRun.svelte'

  interface Props {
    status: BenchmarkStatus | undefined
    error: ApiError | undefined
    targets: StorageTargetWithStatus[] | undefined
    /** Opens the speed test dialog (the storage is chosen there). */
    onstart: () => void
    /** Reload the speed test state. */
    onchanged: () => Promise<void> | void
  }

  let { status, error, targets, onstart, onchanged }: Props = $props()

  const run = $derived(status?.run)
  const running = $derived(run?.state === 'running')
  const anyOnline = $derived(!!targets?.some((x) => x.status.online))
  const name = $derived(run ? (targets?.find((x) => x.id === run.targetId)?.name ?? run.targetId) : '')
</script>

<Panel title={t('cache.speed.title')} description={t('cache.speed.description')}>
  {#snippet actions()}
    {#if session.canOperate && anyOnline}
      <Button icon="overview" disabled={running} onclick={onstart}>{t('cache.speed.test')}</Button>
    {/if}
  {/snippet}

  {#if !status}
    {#if error}
      <Notice tone="fail">{errorText(error)}</Notice>
    {:else}
      <Skeleton height="40px" />
    {/if}
  {:else if run}
    <div class="stack">
      <p class="target">{name}</p>
      <SpeedRun {run} {onchanged} />
    </div>
  {:else}
    <div class="stack-sm">
      <p class="muted small">{t('cache.speed.none')}</p>
      {#if session.canOperate && targets && !anyOnline}<p class="muted small">{t('cache.speed.noOnline')}</p>{/if}
    </div>
  {/if}
</Panel>

<style>
  .target {
    font-weight: 600;
    overflow-wrap: anywhere;
  }
</style>
