<!--
  @component
  Blocking status in the top bar with the pause menu
  (30 s / 5 min / 1 h / until re-enabled / resume).
-->
<script lang="ts">
  import { t } from '../i18n/index.svelte'
  import { api, type BlockingStatus } from '../lib/api'
  import { session } from '../lib/session.svelte'
  import { appStatus } from '../lib/status.svelte'
  import { Icon, Menu, toast, type MenuItem } from '../lib/ui'

  const blocking = $derived(appStatus.overview.data?.blocking)
  // A string: effects below rerun only when the pause changes, not on every poll.
  const pausedUntil = $derived(blocking?.pausedUntil)
  let now = $state(Date.now())
  let busy = $state(false)

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
        ? t('common.blocking.pausedFor', { time: countdown(remaining) })
        : mode === 'off'
          ? t('common.blocking.off')
          : t('common.blocking.unknown'),
  )

  async function apply(enabled: boolean, pauseSeconds: number | undefined, message: string) {
    busy = true
    try {
      const st: BlockingStatus = await api.dns.setBlocking(enabled, pauseSeconds)
      const ov = appStatus.overview.data
      if (ov) appStatus.overview.set({ ...ov, blocking: st })
      toast.success(message)
      void appStatus.overview.refresh()
    } catch (err) {
      toast.error(err)
    } finally {
      busy = false
    }
  }

  const items = $derived.by((): MenuItem[] => {
    const pause: MenuItem[] = [
      { label: t('common.blocking.pause30s'), icon: 'pause', onselect: () => apply(false, 30, t('common.blocking.pausedToast30s')) },
      { label: t('common.blocking.pause5m'), icon: 'pause', onselect: () => apply(false, 300, t('common.blocking.pausedToast5m')) },
      { label: t('common.blocking.pause1h'), icon: 'pause', onselect: () => apply(false, 3600, t('common.blocking.pausedToast1h')) },
      { label: t('common.blocking.disable'), icon: 'shield-off', onselect: () => apply(false, undefined, t('common.blocking.disabledToast')) },
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
