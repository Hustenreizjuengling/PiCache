<!--
  @component
  "Until a time…" of the quick actions: blocks the internet of a group or
  lifts its restrictions until a clock time (today, or tomorrow when the
  time has passed today).
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '$i18n/index.svelte'
  import { api, toApiError, type ApiError, type GroupControls, type OverrideMode } from '$lib/api'
  import { errorText, fieldError } from '$lib/errors'
  import { Button, Dialog, Field, Input, Notice } from '$lib/ui'
  import { toHost } from './hostclock.svelte'
  import { clockOf, nextClock, whenText } from './plan'

  interface Props {
    open?: boolean
    group: GroupControls | undefined
    mode: OverrideMode
    onsaved: (g: GroupControls) => void
  }

  let { open = $bindable(false), group, mode, onsaved }: Props = $props()

  const auto = $props.id()
  let time = $state('')
  let saving = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let submitted = $state(false)

  // A useful start: one hour from now, rounded up to the quarter hour.
  $effect.pre(() => {
    if (!open) return
    untrack(() => {
      const d = toHost(new Date(Date.now() + 60 * 60_000))
      time = clockOf(Math.ceil((d.getHours() * 60 + d.getMinutes()) / 15) * 15)
      err = undefined
      submitted = false
    })
  })

  const until = $derived(nextClock(time))
  const timeError = $derived(
    fieldError(err, 'override.until') ?? (submitted && !until ? t('dns.parental.untilInvalid') : undefined),
  )
  const generalError = $derived(err && !fieldError(err, 'override.until') ? errorText(err) : undefined)

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    err = undefined
    const g = group
    const u = until
    if (!g || !u) return
    saving = true
    try {
      const saved = await api.parental.setOverride(g.groupId, { mode, until: u.toISOString() })
      open = false
      onsaved(saved)
    } catch (e) {
      err = toApiError(e)
    } finally {
      saving = false
    }
  }
</script>

<Dialog
  bind:open
  size="sm"
  dismissible={!saving}
  title={group ? t(mode === 'block' ? 'dns.parental.untilBlockTitle' : 'dns.parental.untilLiftTitle', { group: group.groupName }) : ''}
>
  <form id="until-{auto}" class="stack" onsubmit={submit} novalidate>
    {#if generalError}<Notice tone="fail">{generalError}</Notice>{/if}
    <Field
      label={t('dns.parental.untilLabel')}
      error={timeError}
      help={until ? t('dns.parental.untilEnds', { when: whenText(until, 'at') }) : undefined}
    >
      <Input type="time" bind:value={time} step={60} required />
    </Field>
  </form>
  {#snippet actions()}
    <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="until-{auto}" variant="primary" loading={saving}>
      {mode === 'block' ? t('dns.parental.block') : t('dns.parental.lift')}
    </Button>
  {/snippet}
</Dialog>
