<!--
  @component
  One storage target in a side panel: its state and why, all settings, and
  the steps to use it: test, set up (initialise) or adopt an existing cache,
  mount (host-apply), switch the cache to it; its speed test; configuration
  snippets; edit and delete.
-->
<script lang="ts">
  import { t } from '$i18n/index.svelte'
  import {
    api,
    toApiError,
    type BenchmarkStatus,
    type StorageCapabilities,
    type StorageTargetWithStatus,
    type StorageTestResult,
  } from '$lib/api'
  import { formatBytes, formatDateTime, formatNumber, formatRelative } from '$lib/format'
  import { session } from '$lib/session.svelte'
  import { appStatus } from '$lib/status.svelte'
  import { Button, Chip, Icon, KeyValue, Notice, SidePanel, Spinner, confirm, toast, type KeyValueItem } from '$lib/ui'
  import Snippets from './Snippets.svelte'
  import SpeedResult from './SpeedResult.svelte'
  import SpeedRun from './SpeedRun.svelte'
  import { lastLine } from './speed'
  import { STATE_TONES, targetActions, targetState } from './status'

  interface Props {
    target: StorageTargetWithStatus
    caps: StorageCapabilities | undefined
    /** Run a test right away (after adding the target). */
    autoTest?: boolean
    /** Speed test state: the running or most recent run and the last result per target. */
    speed: BenchmarkStatus | undefined
    /** Name of a storage target by id (the target of a running speed test). */
    nameOf: (id: string) => string
    /** State changed on the server: reload targets and the store state. */
    onchanged: () => Promise<void> | void
    /** Opens the speed test dialog for this target. */
    onspeedtest: () => void
    /** Reload the speed test state. */
    onspeedchanged: () => Promise<void> | void
    onedit: () => void
    onclose: () => void
  }

  let { target, caps, autoTest = false, speed, nameOf, onchanged, onspeedtest, onspeedchanged, onedit, onclose }: Props = $props()

  const st = $derived(target.status)
  const current = $derived(targetState(target))
  const can = $derived(targetActions(target, caps))

  // ---- speed test: this target's run (running or its most recent), else its last result
  const speedRun = $derived(speed?.run)
  const speedRunning = $derived(speedRun?.state === 'running')
  const ownRun = $derived(speedRun?.targetId === target.id ? speedRun : undefined)
  const lastSpeed = $derived(speed?.last?.[target.id])

  // ---- test

  let testing = $state(false)
  let test = $state.raw<StorageTestResult | undefined>(undefined)
  async function runTest() {
    testing = true
    try {
      test = await api.storage.test(target.id)
      if (test.ok) toast.success(t('cache.target.testOk'))
      await onchanged()
    } catch (err) {
      toast.error(err)
    } finally {
      testing = false
    }
  }

  let autoTested = false
  $effect(() => {
    if (autoTest && !autoTested) {
      autoTested = true
      void runTest()
    }
  })

  // ---- set up, adopt, activate, apply, delete

  let busy = $state('')

  /**
   * Runs an action. When the answer is lost (timeout, connection dropped) the
   * server may still have finished: reload the targets and let `happened`
   * decide from the new state instead of reporting a failure.
   */
  async function run(what: string, fn: () => Promise<unknown>, done: string, happened?: () => boolean) {
    busy = what
    try {
      await fn()
      toast.success(done)
      await onchanged()
      void appStatus.overview.refresh()
    } catch (err) {
      if (toApiError(err).code !== 'network' || !happened) {
        toast.error(err)
        return
      }
      try {
        await onchanged()
      } catch {
        /* still unreachable: report the original error */
      }
      void appStatus.overview.refresh()
      if (happened()) toast.success(done)
      else toast.error(err)
    } finally {
      busy = ''
    }
  }

  async function init() {
    const ok = await confirm({
      title: t('cache.target.initTitle', { name: target.name }),
      message: t('cache.target.initText', { path: st.storeRoot || target.path }),
      confirmLabel: t('cache.target.init'),
      danger: false,
    })
    if (!ok) return
    const before = target.storeId
    await run('init', () => api.storage.init(target.id, false), t('cache.target.initDone', { name: target.name }), () => !!target.storeId && target.storeId !== before)
  }

  async function adopt() {
    const ok = await confirm({
      title: t('cache.target.adoptTitle', { name: target.name }),
      message: t('cache.target.adoptText', { store: st.storeId ?? '' }),
      confirmLabel: t('cache.target.adopt'),
      danger: false,
    })
    if (!ok) return
    const found = st.storeId
    await run('adopt', () => api.storage.init(target.id, true), t('cache.target.adoptDone', { name: target.name }), () => !!found && target.storeId === found)
  }

  async function activate() {
    const ok = await confirm({
      title: t('cache.target.activateTitle', { name: target.name }),
      message: t('cache.target.activateText', { name: target.name }),
      confirmLabel: t('cache.target.activate'),
      danger: false,
    })
    if (ok) await run('activate', () => api.storage.activate(target.id), t('cache.target.activated', { name: target.name }), () => target.active)
  }

  async function apply() {
    await run('apply', () => api.storage.apply(target.id), t('cache.target.applyQueued'))
  }

  async function remove() {
    const ok = await confirm({
      title: t('cache.target.deleteTitle', { name: target.name }),
      message: t('cache.target.deleteText'),
      confirmLabel: t('cache.target.delete'),
      action: () => api.storage.remove(target.id),
    })
    if (!ok) return
    toast.success(t('cache.target.deleted', { name: target.name }))
    await onchanged()
    onclose()
  }

  // ---- details

  const kindText = $derived(t(`cache.target.kind.${target.kind}`))
  const details = $derived.by((): KeyValueItem[] => {
    const tg = target
    const items: KeyValueItem[] = [
      { label: t('common.label.type'), value: kindText },
      { label: t('cache.target.mode'), value: t(`cache.target.modeName.${tg.mode}`) },
      { label: t('cache.target.path'), value: tg.path, mono: true },
    ]
    if (tg.kind !== 'local') items.push({ label: t('cache.target.server'), value: tg.server, mono: true })
    if (tg.kind === 'smb') {
      items.push(
        { label: t('cache.target.share'), value: tg.share, mono: true },
        { label: t('cache.target.username'), value: tg.username || t('cache.target.guest'), mono: !!tg.username },
        { label: t('cache.target.domain'), value: tg.domain, mono: true },
        { label: t('cache.target.password'), value: tg.hasPassword ? t('cache.target.passwordSaved') : t('common.state.none') },
        { label: t('cache.target.smbVersion'), value: tg.smbVersion },
        { label: t('cache.target.smbSeal'), value: tg.smbSeal ? t('common.state.on') : t('common.state.off') },
      )
    }
    if (tg.kind === 'nfs') {
      items.push(
        { label: t('cache.target.export'), value: tg.export, mono: true },
        { label: t('cache.target.nfsVersion'), value: tg.nfsVersion },
        { label: t('cache.target.nconnect'), value: formatNumber(tg.nfsNconnect) },
      )
    }
    items.push(
      { label: t('cache.target.subdir'), value: tg.subdir, mono: true },
      { label: t('cache.target.storeRoot'), value: st.storeRoot, mono: true },
      { label: t('cache.target.storeId'), value: tg.storeId || t('cache.target.noStore'), mono: !!tg.storeId },
      { label: t('cache.store.fileSystem'), value: st.fsType },
      { label: t('cache.target.device'), value: st.device, mono: true },
      {
        label: t('cache.target.space'),
        value: st.totalBytes > 0 ? t('cache.target.spaceValue', { free: formatBytes(st.freeBytes), total: formatBytes(st.totalBytes) }) : undefined,
      },
      { label: t('cache.target.latency'), value: st.latencyMs > 0 ? `${formatNumber(st.latencyMs, 2)} ms` : undefined },
      { label: t('cache.target.checked'), value: st.checkedAt ? `${formatDateTime(st.checkedAt, true)} (${formatRelative(st.checkedAt)})` : undefined },
    )
    return items
  })

  const applyFailure = $derived(st.applyState?.startsWith('failed') ? st.applyState.replace(/^failed:?\s*/, '') : '')
</script>

<SidePanel size="lg" bind:open={() => true, (v) => !v && onclose()} title={target.name} subtitle={kindText}>
  <div class="stack">
    <p class="row">
      <Chip tone={STATE_TONES[current]} label={t(`cache.target.state.${current}`)} />
      {#if target.active && current !== 'active'}<Chip tone="info" label={t('cache.target.inUse')} />{/if}
    </p>

    <!-- After a test its result explains the problem; do not repeat it here. -->
    {#if !st.online && st.reason && !test && !testing}
      <Notice tone={current === 'notSetUp' || current === 'existing' ? 'info' : 'fail'} title={st.reason}>
        {st.hint ?? ''}
        {#if target.active}<br />{t('cache.store.passThrough')}{/if}
      </Notice>
    {/if}
    {#if st.online && st.hint}<Notice tone="info">{st.hint}</Notice>{/if}
    {#if st.sdCard}<Notice tone="warn" title={t('cache.store.sdTitle')}>{t('cache.store.sdText')}</Notice>{/if}
    {#if st.sameFsAsData}<Notice tone="info">{t('cache.target.sameFs')}</Notice>{/if}
    {#if st.applyState === 'queued'}
      <Notice tone="info" title={t('cache.target.applyQueuedTitle')}>{t('cache.target.applyQueuedText')}</Notice>
    {:else if st.applyState === 'applied'}
      <Notice tone="ok">{t('cache.target.applyDone')}</Notice>
    {:else if applyFailure}
      <Notice tone="fail" title={t('cache.target.applyFailed')}>{applyFailure}</Notice>
    {/if}

    <section class="stack-sm">
      <h3>{t('cache.target.nextSteps')}</h3>
      <div class="row">
        <Button icon="activity" loading={testing} disabled={!session.isAdmin} onclick={runTest}>{t('cache.target.test')}</Button>
        {#if can.apply}
          <Button icon="upload" loading={busy === 'apply'} disabled={!session.isAdmin || !!busy} onclick={apply}>{t('cache.target.apply')}</Button>
        {/if}
        {#if can.init}
          <Button icon="plus" loading={busy === 'init'} disabled={!session.isAdmin || !!busy} onclick={init}>{t('cache.target.init')}</Button>
        {/if}
        {#if can.adopt}
          <Button icon="check" loading={busy === 'adopt'} disabled={!session.isAdmin || !!busy} onclick={adopt}>{t('cache.target.adopt')}</Button>
        {/if}
        {#if can.activate}
          <Button variant="primary" icon="power" loading={busy === 'activate'} disabled={!session.isAdmin || !!busy} onclick={activate}>
            {t('cache.target.activate')}
          </Button>
        {/if}
        {#if session.isAdmin && st.online}
          <Button icon="overview" disabled={speedRunning || testing || !!busy} onclick={onspeedtest}>{t('cache.speed.test')}</Button>
        {/if}
      </div>
      {#if !target.active && !can.activate && target.storeId && !st.online}
        <p class="muted small">{t('cache.target.activateNeedsOnline')}</p>
      {/if}
      {#if session.isAdmin && st.online && speedRunning && speedRun && speedRun.targetId !== target.id}
        <p class="muted small">{t('cache.speed.busyOther', { name: nameOf(speedRun.targetId) })}</p>
      {/if}
    </section>

    {#if testing || test}
      <section class="stack-sm" aria-live="polite">
        <h3>{t('cache.target.testTitle')}</h3>
        {#if testing}
          <p class="row"><Spinner size={18} /><span>{t('cache.target.testing')}</span></p>
        {:else if test}
          <ol class="steps">
            {#each test.steps as step, i (i)}
              {@const problem = step.startsWith('Problem:')}
              <li class={{ problem }}>
                <Icon name={problem ? 'error' : 'check'} size={16} />
                <span>{step}</span>
              </li>
            {/each}
          </ol>
          {#if test.ok}
            <Notice tone="ok">{t('cache.target.testOkText')}</Notice>
          {:else}
            <Notice tone="fail" title={test.error ?? t('cache.target.testFailed')}>{test.hint ?? ''}</Notice>
          {/if}
        {/if}
      </section>
    {/if}

    <section class="stack-sm">
      <h3>{t('cache.speed.title')}</h3>
      {#if ownRun}
        <SpeedRun run={ownRun} onchanged={onspeedchanged} level={4} />
      {:else if lastSpeed}
        <p>{lastLine(lastSpeed)}</p>
        <details class="speed-details">
          <summary>{t('common.label.details')}</summary>
          <SpeedResult result={lastSpeed} level={4} />
        </details>
      {:else}
        <p class="muted small">{t('cache.speed.noneTarget')}</p>
      {/if}
    </section>

    <section class="stack-sm">
      <h3>{t('cache.target.settingsTitle')}</h3>
      <KeyValue items={details} />
    </section>

    {#if session.isAdmin && target.id !== 'local'}
      <section class="stack-sm">
        <h3>{t('cache.snippets.title')}</h3>
        <p class="muted small">{t('cache.snippets.intro')}</p>
        <Snippets id={target.id} />
      </section>
    {/if}
  </div>

  {#snippet actions()}
    <Button icon="edit" disabled={!session.isAdmin} onclick={onedit}>{t('cache.target.edit')}</Button>
    {#if can.remove}
      <Button variant="danger" icon="trash" disabled={!session.isAdmin} onclick={remove}>{t('cache.target.delete')}</Button>
    {/if}
  {/snippet}
</SidePanel>

<style>
  h3 {
    font-size: var(--fs-md);
    font-weight: 600;
  }
  .steps {
    margin: 0;
    padding: 0;
    list-style: none;
    display: flex;
    flex-direction: column;
    gap: var(--sp-1);
    font-size: var(--fs-sm);
  }
  .steps li {
    display: flex;
    align-items: flex-start;
    gap: var(--sp-2);
    color: var(--text-2);
  }
  .steps li :global(.icon) {
    flex: none;
    margin-top: 2px;
    color: var(--ok);
  }
  .steps li.problem {
    color: var(--text);
  }
  .steps li.problem :global(.icon) {
    color: var(--fail);
  }
  .speed-details > summary {
    width: fit-content;
    margin-bottom: var(--sp-3);
    font-size: var(--fs-sm);
    font-weight: 600;
    cursor: pointer;
  }
  .speed-details:not([open]) > summary {
    margin-bottom: 0;
  }
</style>
