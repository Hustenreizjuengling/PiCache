<!--
  @component
  Scheduled backups: the state (last and next backup), "Back up now", the
  settings (settings.backups: on/off applies at once, the schedule is saved
  with "Save changes") and the stored backups with download and delete.
  Read-only principals see the state and the stored backups, no controls.
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import {
    api,
    isApiError,
    resource,
    toApiError,
    type ApiError,
    type BackupSchedule,
    type Resource,
    type ScheduledBackups,
    type SystemInfo,
  } from '$lib/api'
  import { errorText } from '$lib/errors'
  import { session } from '$lib/session.svelte'
  import { settingsForm } from '$lib/settings.svelte'
  import { Button, Checkbox, Field, Input, Notice, Panel, Select, Skeleton, Toggle, toast } from '$lib/ui'
  import { rangeError, RANGES } from '../forms'
  import { masterKeyKind } from '../health/about'
  import ScheduledFiles from './ScheduledFiles.svelte'
  import ScheduledStatus from './ScheduledStatus.svelte'
  import { LOCAL, TIME_RE, destinationLabel, destinations, weekdayOptions, withServerPath } from './schedule'

  let { info }: { info?: SystemInfo } = $props()

  const form = settingsForm('backups')
  const targets = resource((signal) => api.storage.targets({ signal }), { interval: 30_000 })

  // A backup this page started (or found running on a 409) is followed until
  // the server no longer reports it running, to say how it went: the run is
  // over when `last` changed or the server was seen running it (two runs
  // within one second share a time). While any backup runs, the status is
  // polled every 2 s.
  let tracking = $state<{ baseline: string | undefined; since: number; seen: boolean } | undefined>(undefined)
  const status: Resource<ScheduledBackups> = resource((signal) => api.backups.scheduled({ signal }), {
    interval: () => (tracking || status.data?.running ? 2_000 : 30_000),
  })

  const TRACK_MAX_MS = 10 * 60_000

  let starting = $state(false)
  let runError = $state.raw<ApiError | undefined>(undefined)

  $effect(() => {
    const tr = tracking
    const data = status.data
    if (!tr || !data) return
    const last = data.last
    const busy = data.running
    untrack(() => {
      if (busy) {
        tr.seen = true
      } else if (last && (tr.seen || last.time !== tr.baseline)) {
        tracking = undefined
        runError = undefined
        if (last.ok) toast.success(t('system.backup.scheduled.runDone'))
        else toast.error(t('system.backup.scheduled.runFailed'))
      } else if (Date.now() - tr.since > TRACK_MAX_MS) {
        tracking = undefined
      }
    })
  })

  // ---- back up now

  async function runNow() {
    starting = true
    runError = undefined
    try {
      await status.refresh() // a fresh baseline: the run is over when `last` changes
      const baseline = status.data?.last?.time
      try {
        await api.backups.run()
        tracking = { baseline, since: Date.now(), seen: false }
      } catch (err) {
        const e = toApiError(err)
        runError = e
        // 409: one is being written right now; follow it as well.
        if (isApiError(e, 'conflict')) tracking = { baseline, since: Date.now(), seen: true }
      }
      void status.refresh()
    } finally {
      starting = false
    }
  }

  // ---- on/off applies immediately; the rest is saved with the form

  let switching = $state(false)

  async function setEnabled(on: boolean) {
    switching = true
    try {
      const all = await api.settings.patch('backups', { enabled: on })
      form.saved = all.backups
      if (form.draft) form.draft.enabled = all.backups.enabled
      toast.success(on ? t('system.backup.scheduled.turnedOn') : t('system.backup.scheduled.turnedOff'))
      void status.refresh()
    } catch (err) {
      if (form.draft) form.draft.enabled = !on
      toast.error(err)
    } finally {
      switching = false
    }
  }

  // ---- form

  const dests = $derived(
    withServerPath(destinations(targets.data, info?.dataDir), status.data?.settings.destination, status.data?.destinationPath),
  )
  const savedDest = $derived(form.saved?.destination)
  const draftDest = $derived(form.draft?.destination)
  const dest = $derived(dests.find((d) => d.id === draftDest))
  const offline = $derived(dests.filter((d) => !d.online))

  const destOptions = $derived.by(() => {
    const opts = dests.map((d) => ({
      value: d.id,
      label: d.online
        ? destinationLabel(d)
        : d.reason
          ? t('system.backup.scheduled.destOffline', { name: d.name, reason: d.reason })
          : t('system.backup.scheduled.destOfflineShort', { name: d.name }),
      // The saved one stays selectable, so switching away and back works.
      disabled: !d.online && d.id !== savedDest,
    }))
    const cur = draftDest
    if (cur && !dests.some((d) => d.id === cur)) {
      opts.push({ value: cur, label: targets.data ? t('system.backup.scheduled.destUnknown') : t('common.state.loading'), disabled: true })
    }
    return opts
  })

  const destHelp = $derived.by(() => {
    const parts: string[] = []
    if (dest?.path) parts.push(t('system.backup.scheduled.destPath', { path: dest.path }))
    if (dest?.id === LOCAL) parts.push(t('system.backup.scheduled.destLocalHint'))
    if (offline.some((d) => d.id !== draftDest)) parts.push(t('system.backup.scheduled.destOthers'))
    return parts.join(' ') || undefined
  })

  const repeatOptions = $derived([
    { value: 'daily', label: t('system.backup.scheduled.daily') },
    { value: 'weekly', label: t('system.backup.scheduled.weekly') },
  ])

  const problems = $derived.by(() => {
    const d = form.draft
    const p: Record<string, string | undefined> = {}
    if (!d) return p
    if (!TIME_RE.test(d.time ?? '')) p.time = t('system.backup.scheduled.timeInvalid')
    p.keep = rangeError(d.keep, RANGES.backupKeep)
    if (d.schedule === 'weekly') p.weekday = rangeError(d.weekday, { min: 0, max: 6 })
    return p
  })
  const invalid = $derived(Object.values(problems).some(Boolean))
  const FIELDS = ['enabled', 'schedule', 'time', 'weekday', 'keep', 'destination', 'includeSecrets']
  const general = $derived(
    form.saveError && !FIELDS.some((f) => form.error(f)) ? form.errorMessage : undefined,
  )

  function err(field: string): string | undefined {
    return problems[field] ?? form.error(field)
  }

  async function save(e: SubmitEvent) {
    e.preventDefault()
    if (invalid) return
    if (await form.save()) {
      toast.success(t('common.state.saved'))
      void status.refresh()
    }
  }

  const keyKind = $derived(info ? masterKeyKind(info.masterKeySource) : undefined)
  /** Where the stored files are (the saved destination). */
  const filesPath = $derived(dests.find((d) => d.id === savedDest)?.path)
  const settings = $derived(form.saved ?? status.data?.settings)
  const zone = $derived(status.data?.timeZone)
  const timeHelp = $derived(
    zone === 'UTC'
      ? t('system.backup.scheduled.timeHelpUtc')
      : zone
        ? t('system.backup.scheduled.timeHelpZone', { zone })
        : t('system.backup.scheduled.timeHelp'),
  )
  const running = $derived(!!status.data?.running || (!!tracking && !tracking.seen))

  const formId = $props.id()
</script>

{#snippet formFooter()}
  <div class="row">
    <Button
      type="submit"
      form="scheduled-{formId}"
      variant="primary"
      loading={form.saving}
      disabled={!form.dirty || invalid || switching}
    >
      {t('common.action.save')}
    </Button>
    <Button variant="ghost" disabled={!form.dirty || form.saving} onclick={() => form.revert()}>
      {t('system.form.discard')}
    </Button>
  </div>
{/snippet}

<Panel
  title={t('system.backup.scheduled.title')}
  description={t('system.backup.scheduled.description')}
  footer={session.isAdmin && form.draft ? formFooter : undefined}
>
  {#snippet actions()}
    {#if session.isAdmin && settings}
      <Button
        icon="archive"
        loading={starting || running}
        title={form.dirty ? t('system.backup.scheduled.runSaved') : undefined}
        onclick={runNow}
      >
        {t('system.backup.scheduled.run')}
      </Button>
    {/if}
  {/snippet}

  {#if form.loadError && !form.draft}
    <Notice tone="fail" title={t('system.backup.scheduled.loadError')}>{errorText(form.loadError)}</Notice>
  {:else if !settings || !form.draft}
    <Skeleton height="220px" />
  {:else}
    {@const draft = form.draft}
    <div class="layout">
      <section class="state stack">
        {#if runError}
          <Notice
            tone={isApiError(runError, 'conflict') ? 'warn' : 'fail'}
            title={t('system.backup.scheduled.runRefused')}
            ondismiss={() => (runError = undefined)}
          >
            {isApiError(runError, 'conflict') ? t('system.backup.scheduled.runBusy') : errorText(runError)}
          </Notice>
        {/if}
        <ScheduledStatus {settings} data={status.data} {running} {dests} />
        {#if status.error && !status.data}
          <Notice tone="fail">
            {errorText(status.error)}
            {#snippet actions()}
              <Button size="sm" icon="refresh" onclick={() => status.refresh()}>{t('common.action.retry')}</Button>
            {/snippet}
          </Notice>
        {/if}
        {#if !session.isAdmin}
          <p class="small muted">{t('system.backup.scheduled.readOnly')}</p>
        {/if}
      </section>

      {#if session.isAdmin}
        <form id="scheduled-{formId}" class="settings stack" onsubmit={save} novalidate>
          {#if general}<Notice tone="fail">{general}</Notice>{/if}
          <Toggle
            bind:checked={draft.enabled}
            label={t('system.backup.scheduled.enable')}
            description={t('system.backup.scheduled.enableHelp')}
            disabled={switching || form.saving}
            onchange={setEnabled}
          />

          <div class="grid">
            <Field label={t('system.backup.scheduled.repeat')} error={err('schedule')}>
              <Select
                options={repeatOptions}
                bind:value={() => draft.schedule, (v) => (draft.schedule = (v === 'weekly' ? 'weekly' : 'daily') as BackupSchedule)}
              />
            </Field>
            {#if draft.schedule === 'weekly'}
              <Field label={t('system.backup.scheduled.weekday')} error={err('weekday')}>
                <Select
                  options={weekdayOptions()}
                  bind:value={() => String(draft.weekday), (v) => (draft.weekday = Number(v))}
                />
              </Field>
            {/if}
            <Field label={t('system.backup.scheduled.time')} error={err('time')} help={timeHelp}>
              <Input type="time" bind:value={draft.time} step={60} required />
            </Field>
            <Field label={t('system.backup.scheduled.keep')} error={err('keep')}>
              <Input
                type="number"
                bind:value={draft.keep}
                min={RANGES.backupKeep.min}
                max={RANGES.backupKeep.max}
                step={1}
                inputmode="numeric"
              />
            </Field>
          </div>
          <p class="small muted keep-help">{t('system.backup.scheduled.keepHelp')}</p>

          <div class="stack-sm">
            <Field label={t('system.backup.scheduled.destination')} error={err('destination')} help={destHelp}>
              <Select options={destOptions} bind:value={draft.destination} />
            </Field>
            {#if targets.data && draftDest && draftDest !== LOCAL}
              {#if !dest}
                <Notice tone="warn">{t('system.backup.scheduled.destMissing')}</Notice>
              {:else if !dest.online}
                <Notice tone="warn" title={t('system.backup.scheduled.destIsOffline', { name: dest.name })}>
                  {dest.reason ?? ''}
                </Notice>
              {/if}
            {/if}
          </div>

          <div class="stack-sm">
            <Checkbox
              bind:checked={draft.includeSecrets}
              label={t('system.backup.scheduled.secrets')}
              description={t('system.backup.scheduled.secretsHelp')}
            />
            {#if draft.includeSecrets}
              <Notice tone="info" icon="key" title={t('system.backup.scheduled.secretsNoteTitle')}>
                <p>{t('system.backup.scheduled.secretsNoteText')}</p>
                {#if info && keyKind}
                  <p class="key">
                    <span>{t(`system.health.keySource.${keyKind}`)}:</span>
                    <code class="mono">{info.masterKeySource}</code>
                  </p>
                {/if}
              </Notice>
            {/if}
          </div>
        </form>
      {/if}
    </div>
  {/if}

</Panel>

{#if form.saved}
  <ScheduledFiles
    files={status.data?.files}
    loading={status.loading}
    error={status.error}
    filesError={status.data?.filesError}
    path={filesPath}
    onretry={() => status.refresh()}
    onchanged={() => status.refresh()}
  />
{/if}

<style>
  .layout {
    display: grid;
    grid-template-columns: minmax(0, 5fr) minmax(0, 6fr);
    gap: var(--sp-5) var(--sp-6);
    align-items: start;
  }
  .state,
  .settings {
    min-width: 0;
  }
  .settings {
    padding-left: var(--sp-6);
    border-left: 1px solid var(--line);
  }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 160px), 1fr));
    gap: var(--sp-4);
  }
  .keep-help {
    margin-top: calc(-1 * var(--sp-2));
  }
  .key {
    display: flex;
    flex-wrap: wrap;
    gap: 0 var(--sp-2);
    margin-top: var(--sp-1);
  }
  @media (max-width: 1100px) {
    .layout {
      grid-template-columns: minmax(0, 1fr);
    }
    .settings {
      padding-left: 0;
      padding-top: var(--sp-5);
      border-left: 0;
      border-top: 1px solid var(--line);
    }
  }
</style>
