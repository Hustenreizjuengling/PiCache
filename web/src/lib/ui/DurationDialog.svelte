<!--
  @component
  Asks for a duration in minutes or hours (1 minute up to `maxMinutes`),
  says when it ends on the PiCache host's clock and hands the minutes to
  `onsubmit`. If that throws, the error is shown and the dialog stays open.
  Used for "Pause … for a custom time".
  <DurationDialog bind:open title="Pause blocking" submitLabel="Pause blocking"
    onsubmit={(minutes) => api.dns.setBlocking(false, minutes * 60)}>
    <p>What stays on …</p>
  </DurationDialog>
-->
<script lang="ts">
  import { untrack, type Snippet } from 'svelte'
  import { t } from '../../i18n/index.svelte'
  import { toApiError, type ApiError } from '../api/client'
  import { errorText } from '../errors'
  import { formatDateTimeShort } from '../format'
  import { toHost } from '../hostclock.svelte'
  import Button from './Button.svelte'
  import Dialog from './Dialog.svelte'
  import Field from './Field.svelte'
  import Input from './Input.svelte'
  import Notice from './Notice.svelte'
  import Select from './Select.svelte'

  interface Props {
    open?: boolean
    title: string
    submitLabel: string
    /** Longest duration in minutes (default 7 days). */
    maxMinutes?: number
    /** Explanation above the fields. */
    children?: Snippet
    onsubmit: (minutes: number) => unknown
  }

  let { open = $bindable(false), title, submitLabel, maxMinutes = 7 * 24 * 60, children, onsubmit }: Props = $props()

  const auto = $props.id()
  // A number input binds a number (null while it is empty or incomplete).
  let amount = $state<string | number | null>(30)
  let unit = $state<'minutes' | 'hours'>('minutes')
  let saving = $state(false)
  let submitted = $state(false)
  let err = $state.raw<ApiError | undefined>(undefined)
  let now = $state(Date.now())

  $effect.pre(() => {
    if (!open) return
    untrack(() => {
      amount = 30
      unit = 'minutes'
      submitted = false
      err = undefined
      now = Date.now()
    })
  })

  const raw = $derived(String(amount ?? '').trim())
  const count = $derived(/^\d+$/.test(raw) ? Number(raw) : NaN)
  const minutes = $derived(Number.isSafeInteger(count) ? count * (unit === 'hours' ? 60 : 1) : NaN)
  const problem = $derived(
    !Number.isSafeInteger(count) || count < 1
      ? t('common.duration.invalid')
      : minutes > maxMinutes
        ? t('common.duration.tooLong', { days: Math.floor(maxMinutes / 1440), hours: Math.floor(maxMinutes / 60) })
        : undefined,
  )
  const amountError = $derived(
    (err?.field ? errorText(err) : undefined) ?? (submitted || raw !== '' ? problem : undefined),
  )
  const ends = $derived(problem ? undefined : formatDateTimeShort(toHost(new Date(now + minutes * 60_000))))

  async function submit(e: SubmitEvent) {
    e.preventDefault()
    submitted = true
    err = undefined
    if (problem || saving) return
    saving = true
    try {
      await onsubmit(minutes)
      open = false
    } catch (x) {
      err = toApiError(x)
    } finally {
      saving = false
    }
  }
</script>

<Dialog bind:open {title} size="sm" dismissible={!saving}>
  <form id="duration-{auto}" class="stack" onsubmit={submit} novalidate>
    {#if children}<div class="stack-sm small">{@render children()}</div>{/if}
    {#if err && !err.field}<Notice tone="fail">{errorText(err)}</Notice>{/if}
    <div class="row-fields">
      <div class="amount">
        <Field label={t('common.duration.label')} error={amountError} help={ends ? t('common.duration.ends', { when: ends }) : undefined}>
          <Input bind:value={amount} type="number" inputmode="numeric" min={1} step={1} required autocomplete="off" />
        </Field>
      </div>
      <div class="unit">
        <Field label={t('common.duration.unit')}>
          <Select
            bind:value={() => unit, (v) => (unit = v === 'hours' ? 'hours' : 'minutes')}
            options={[
              { value: 'minutes', label: t('common.duration.minutes') },
              { value: 'hours', label: t('common.duration.hours') },
            ]}
          />
        </Field>
      </div>
    </div>
  </form>
  {#snippet actions()}
    <Button variant="ghost" disabled={saving} onclick={() => (open = false)}>{t('common.action.cancel')}</Button>
    <Button type="submit" form="duration-{auto}" variant="primary" icon="pause" loading={saving}>{submitLabel}</Button>
  {/snippet}
</Dialog>

<style>
  .row-fields {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    gap: var(--sp-3);
  }
  .amount {
    flex: 1 1 140px;
  }
  .unit {
    flex: 1 1 140px;
  }
</style>
