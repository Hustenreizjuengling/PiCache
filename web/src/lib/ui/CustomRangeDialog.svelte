<!--
  @component
  Picks a custom time window (local date and time): start before end, at
  most 400 days, not in the future and not before `earliest` (what the page's
  retention keeps).
  <CustomRangeDialog bind:open value={custom} earliest={now - retention} onapply={(r) => setRange(r)} />
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { t } from '../../i18n/index.svelte'
  import { formatDateTime } from '../format'
  import { DAY, MAX_CUSTOM_DAYS, type CustomRange } from '../range'
  import Button from './Button.svelte'
  import Dialog from './Dialog.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'

  interface Props {
    open?: boolean
    /** The window shown when the dialog opens (default: the last 24 hours). */
    value?: CustomRange | null
    /** Earliest start in unix seconds (the page's retention); none: no limit. */
    earliest?: number
    onapply: (range: CustomRange) => void
  }

  let { open = $bindable(false), value = null, earliest, onapply }: Props = $props()

  const auto = $props.id()
  const formId = `range-${auto}`
  const SLACK = 60 // seconds: "now" may move on while the dialog is open

  let fromText = $state('')
  let toText = $state('')
  let submitted = $state(false)

  function pad(n: number): string {
    return String(n).padStart(2, '0')
  }

  /** Unix seconds as the value of a datetime-local input (local time, minutes). */
  function toInput(sec: number): string {
    const d = new Date(sec * 1000)
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`
  }

  /** A datetime-local value as unix seconds (undefined when empty or invalid). */
  function fromInput(s: string): number | undefined {
    if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}/.test(s)) return undefined
    const ms = new Date(s).getTime()
    return Number.isFinite(ms) ? Math.floor(ms / 1000) : undefined
  }

  $effect(() => {
    if (!open) return
    untrack(() => {
      const now = Math.floor(Date.now() / 1000)
      const r = value ?? { from: Math.floor((now - DAY) / 3600) * 3600, to: now }
      fromText = toInput(r.from)
      toText = toInput(r.to)
      submitted = false
    })
  })

  const from = $derived(fromInput(fromText))
  const to = $derived(fromInput(toText))

  const fromError = $derived.by(() => {
    if (from === undefined) return t('common.customRange.invalid')
    if (earliest !== undefined && from < earliest - SLACK) {
      return t('common.customRange.beforeRetention', { date: formatDateTime(earliest * 1000) })
    }
    return undefined
  })
  const toError = $derived.by(() => {
    if (to === undefined) return t('common.customRange.invalid')
    if (to > Date.now() / 1000 + SLACK) return t('common.customRange.future')
    if (from !== undefined && to <= from) return t('common.customRange.order')
    if (from !== undefined && to - from > MAX_CUSTOM_DAYS * DAY) return t('common.customRange.tooLong', { days: MAX_CUSTOM_DAYS })
    return undefined
  })

  function apply(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    if (fromError || toError || from === undefined || to === undefined) return
    open = false
    onapply({ from, to: Math.min(to, Math.floor(Date.now() / 1000)) })
  }

  const nowInput = $derived(open ? toInput(Math.floor(Date.now() / 1000)) : '')
</script>

<Dialog bind:open title={t('common.customRange.title')} size="sm">
  <form id={formId} class="stack" onsubmit={apply} novalidate>
    <Field label={t('common.customRange.from')} error={submitted ? fromError : undefined}>
      <Input type="datetime-local" bind:value={fromText} min={earliest !== undefined ? toInput(earliest) : undefined} max={nowInput} />
    </Field>
    <Field label={t('common.customRange.to')} error={submitted ? toError : undefined}>
      <Input type="datetime-local" bind:value={toText} max={nowInput} />
    </Field>
    <p class="small muted">
      {earliest !== undefined
        ? t('common.customRange.helpRetention', { days: MAX_CUSTOM_DAYS, date: formatDateTime(earliest * 1000) })
        : t('common.customRange.help', { days: MAX_CUSTOM_DAYS })}
    </p>
  </form>
  {#snippet actions()}
    <Button variant="ghost" onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button variant="primary" type="submit" form={formId}>{t('common.customRange.apply')}</Button>
  {/snippet}
</Dialog>
