<!--
  @component
  Blocking status in the top bar with the pause menu (30 s / 5 min / 1 h /
  until the next 06:00 on the host's clock / a custom time up to 7 days /
  until re-enabled / resume). The menu says what a pause keeps in force.
  Pauses longer than a day show their end instead of a countdown.
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { api, type BlockingStatus } from '../lib/api'
  import { formatDateTimeShort } from '../lib/format'
  import { hostDayTime, nextHostTime, setHostClock, toHost } from '../lib/hostclock.svelte'
  import { session } from '../lib/session.svelte'
  import { appStatus } from '../lib/status.svelte'
  import { DurationDialog, Icon, Menu, toast, type MenuItem } from '../lib/ui'

  /** "Until 06:00": minutes after midnight on the host's clock. */
  const MORNING = 6 * 60
  const DAY_MS = 86_400_000

  const blocking = $derived(appStatus.overview.data?.blocking)
  // A string: effects below rerun only when the pause changes, not on every poll.
  const pausedUntil = $derived(blocking?.pausedUntil)
  let now = $state(Date.now())
  let busy = $state(false)
  let customOpen = $state(false)

  // Pauses "until 06:00" and their end times use the host's clock.
  $effect(() => setHostClock(blocking))

  // Tick while a timed pause runs; refresh when it ends (the server re-enables).
  $effect(() => {
    const until = pausedUntil
    if (!until) return
    now = Date.now()
    const id = setInterval(() => {
      now = Date.now()
      if (now >= Date.parse(until)) {
        clearInterval(id)
        void appStatus.overview.refresh()
      }
    }, 1000)
    return () => clearInterval(id)
  })

  const remaining = $derived(pausedUntil ? Math.max(0, Date.parse(pausedUntil) - now) : 0)
  const mode = $derived<'unknown' | 'on' | 'paused' | 'off'>(
    !blocking ? 'unknown' : blocking.enabled ? 'on' : blocking.pausedUntil && !blocking.permanent ? 'paused' : 'off',
  )

  function countdown(ms: number): string {
    const s = Math.ceil(ms / 1000)
    const h = Math.floor(s / 3600)
    const m = Math.floor((s % 3600) / 60)
    const sec = String(s % 60).padStart(2, '0')
    return h > 0 ? `${h}:${String(m).padStart(2, '0')}:${sec}` : `${m}:${sec}`
  }

  const text = $derived(
    mode === 'on'
      ? t('common.blocking.on')
      : mode === 'paused'
        ? remaining > DAY_MS && pausedUntil
          ? t('common.blocking.pausedUntil', { when: formatDateTimeShort(toHost(new Date(pausedUntil))) })
          : t('common.blocking.pausedFor', { time: countdown(remaining) })
        : mode === 'off'
          ? t('common.blocking.off')
          : t('common.blocking.unknown'),
  )

  function show(st: BlockingStatus) {
    const ov = appStatus.overview.data
    if (ov) appStatus.overview.set({ ...ov, blocking: st })
    void appStatus.overview.refresh()
  }

  async function apply(enabled: boolean, pauseSeconds: number | undefined, message: string) {
    busy = true
    try {
      show(await api.dns.setBlocking(enabled, pauseSeconds))
      toast.success(message)
    } catch (err) {
      toast.error(err)
    } finally {
      busy = false
    }
  }

  function untilMorning() {
    const until = nextHostTime(MORNING)
    const seconds = Math.ceil((until.getTime() - Date.now()) / 1000)
    void apply(false, seconds, t('common.blocking.pausedToastUntil', { when: hostDayTime(until) }))
  }

  // The dialog shows errors itself: let them through.
  async function pauseCustom(minutes: number) {
    const st = await api.dns.setBlocking(false, minutes * 60)
    show(st)
    toast.success(t('common.blocking.pausedToastUntil', { when: hostDayTime(new Date(Date.now() + minutes * 60_000)) }))
  }

  const items = $derived.by((): MenuItem[] => {
    // Read the status so the "until" label follows the clock with every poll.
    void blocking
    const pause: MenuItem[] = [
      { label: t('common.blocking.pause30s'), icon: 'pause', onselect: () => apply(false, 30, t('common.blocking.pausedToast30s')) },
      { label: t('common.blocking.pause5m'), icon: 'pause', onselect: () => apply(false, 300, t('common.blocking.pausedToast5m')) },
      { label: t('common.blocking.pause1h'), icon: 'pause', onselect: () => apply(false, 3600, t('common.blocking.pausedToast1h')) },
      { label: t('common.blocking.pauseUntil', { when: hostDayTime(nextHostTime(MORNING)) }), icon: 'clock', onselect: untilMorning },
      { label: t('common.blocking.pauseCustom'), icon: 'clock', onselect: () => (customOpen = true) },
      { label: t('common.blocking.disable'), icon: 'shield-off', onselect: () => apply(false, undefined, t('common.blocking.disabledToast')) },
      { separator: true },
      { note: t('common.blocking.note') },
    ]
    if (mode === 'on') return pause
    return [
      { label: t('common.blocking.resume'), icon: 'play', onselect: () => apply(true, undefined, t('common.blocking.enabledToast')) },
      { separator: true },
      ...pause,
    ]
  })
</script>

<Menu label={t('common.blocking.menu')} {items} align="end" disabled={!blocking || busy || !session.isAdmin}>
  {#snippet trigger()}
    <span class={['state', mode]}>
      <Icon name={mode === 'on' ? 'shield-check' : mode === 'unknown' ? 'shield' : 'shield-off'} size={18} />
      <span class="txt">{text}</span>
      <Icon name="chevron-down" size={16} />
    </span>
  {/snippet}
</Menu>

{#if session.isAdmin}
  <DurationDialog
    bind:open={customOpen}
    title={t('common.blocking.customTitle')}
    submitLabel={t('common.blocking.customSubmit')}
    onsubmit={pauseCustom}
  >
    <p>{t('common.blocking.note')}</p>
  </DurationDialog>
{/if}

<style>
  .state {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: var(--fs-sm);
    font-weight: 600;
  }
  .state :global(.icon:first-child) {
    color: var(--text-3);
  }
  .on :global(.icon:first-child) {
    color: var(--ok);
  }
  .paused :global(.icon:first-child),
  .off :global(.icon:first-child) {
    color: var(--warn);
  }
  .txt {
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
</style>
