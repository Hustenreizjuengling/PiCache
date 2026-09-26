<!--
  @component
  "Pause filtering" of one group (PUT /parental/groups/{id}/pause): 5 min,
  1 h, until the next 06:00 on the host's clock, or a custom time up to 7
  days. The menu and the custom dialog say what stays on. Pausing the
  Default group asks first, because it covers every device without its own
  client entry. A new pause replaces the running one. Admins only.
-->
<script lang="ts">
  import { t, tn } from '$i18n/index.svelte'
  import { api, DEFAULT_GROUP_ID, type GroupControls, type PauseInput } from '$lib/api'
  import { hostDayTime, nextHostTime } from '$lib/hostclock.svelte'
  import { confirm, DurationDialog, Menu, Notice, toast, type MenuItem } from '$lib/ui'

  interface Props {
    groupId: number
    groupName: string
    /** Clients assigned to the group. */
    clientCount: number
    /** Trigger text (default "Pause filtering"). */
    label?: string
    size?: 'sm' | 'md'
    align?: 'start' | 'end'
    disabled?: boolean
    onpaused: (g: GroupControls) => void
  }

  let { groupId, groupName, clientCount, label, size = 'sm', align = 'end', disabled = false, onpaused }: Props = $props()

  /** "Until 06:00": minutes after midnight on the host's clock. */
  const MORNING = 6 * 60

  let busy = $state(false)
  let customOpen = $state(false)
  const isDefault = $derived(groupId === DEFAULT_GROUP_ID)

  // The clock ticks so "Until <day> 06:00" follows the time while the page
  // stays open; the pick sends the time its label shows.
  let now = $state(new Date())
  $effect(() => {
    const id = setInterval(() => (now = new Date()), 30_000)
    return () => clearInterval(id)
  })
  const morning = $derived(nextHostTime(MORNING, now))

  async function run(body: PauseInput) {
    const g = await api.parental.pause(groupId, body)
    onpaused(g)
    const until = g.state.pausedUntil
    toast.success(t('dns.pause.pausedToast', { group: g.groupName, when: until ? hostDayTime(new Date(until)) : '' }))
  }

  async function pick(body: PauseInput) {
    if (isDefault) {
      await confirm({
        title: t('dns.pause.defaultTitle'),
        message: `${tn('dns.pause.defaultText', clientCount)} ${t('dns.pause.keeps')}`,
        confirmLabel: t('dns.pause.submit'),
        danger: false,
        action: () => run(body),
      })
      return
    }
    busy = true
    try {
      await run(body)
    } catch (e) {
      toast.error(e)
    } finally {
      busy = false
    }
  }

  const items = $derived.by((): MenuItem[] => [
    { label: t('dns.parental.forMinutes', { count: 5 }), onselect: () => pick({ minutes: 5 }) },
    { label: tn('dns.parental.forHours', 1), onselect: () => pick({ minutes: 60 }) },
    {
      label: t('dns.pause.until', { when: hostDayTime(morning) }),
      icon: 'clock',
      // Between 06:00 and the next tick the label's time has just passed: take the next one.
      onselect: () => pick({ until: (morning.getTime() > Date.now() ? morning : nextHostTime(MORNING)).toISOString() }),
    },
    { label: t('dns.pause.custom'), icon: 'clock', onselect: () => (customOpen = true) },
    { separator: true },
    { note: t('dns.pause.keeps') },
  ])
</script>

<Menu label={label ?? t('dns.pause.button')} icon="pause" {items} {size} {align} disabled={disabled || busy} />

<DurationDialog
  bind:open={customOpen}
  title={t('dns.pause.title', { group: groupName })}
  submitLabel={t('dns.pause.submit')}
  onsubmit={(minutes) => run({ minutes })}
>
  {#if isDefault}
    <Notice tone="warn">{tn('dns.pause.defaultText', clientCount)}</Notice>
  {/if}
  <p>{t('dns.pause.keeps')}</p>
</DurationDialog>
