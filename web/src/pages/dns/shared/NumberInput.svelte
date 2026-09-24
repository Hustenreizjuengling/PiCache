<!--
  @component
  A whole-number input bound to a number. Incomplete input is flagged and
  never written back (so a half-typed value is never saved as 0 or null);
  on blur it snaps back to the last valid number. Optional unit after it.

  <Field label="Cache size" error={form.error('cacheSize')}>
    <NumberInput bind:value={form.draft.cacheSize} min={0} max={10000000} unit="entries" />
  </Field>
-->
<script lang="ts">
  import { untrack } from 'svelte'
  import { Input } from '$lib/ui'

  interface Props {
    value?: number
    min?: number
    max?: number
    step?: number
    /** Unit shown after the field ("ms", "seconds"). */
    unit?: string
    disabled?: boolean
    invalid?: boolean
    placeholder?: string
    ariaLabel?: string
  }

  let {
    value = $bindable(0),
    min,
    max,
    step = 1,
    unit,
    disabled,
    invalid,
    placeholder,
    ariaLabel,
  }: Props = $props()

  let text = $state(untrack(() => String(value ?? '')))

  function parse(s: string): number | undefined {
    const v = s.trim()
    if (!/^-?\d+$/.test(v)) return undefined
    const n = Number(v)
    if (!Number.isSafeInteger(n)) return undefined
    if (min !== undefined && n < min) return undefined
    if (max !== undefined && n > max) return undefined
    return n
  }

  const bad = $derived(parse(text) === undefined)

  $effect(() => {
    const v = value
    untrack(() => {
      if (parse(text) !== v) text = String(v ?? '')
    })
  })

  function oninput(e: Event & { currentTarget: HTMLInputElement }) {
    const n = parse(e.currentTarget.value)
    if (n !== undefined && n !== value) value = n
  }

  function onblur() {
    if (parse(text) === undefined) text = String(value ?? '')
  }
</script>

<span class="num-input">
  <Input
    bind:value={text}
    inputmode="numeric"
    {min}
    {max}
    {step}
    {disabled}
    {placeholder}
    invalid={invalid || bad || undefined}
    aria-label={ariaLabel}
    autocomplete="off"
    {oninput}
    {onblur}
  />
  {#if unit}<span class="unit">{unit}</span>{/if}
</span>

<style>
  .num-input {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    max-width: 280px;
  }
  .num-input > :global(.wrap) {
    flex: 1;
  }
  .unit {
    flex: none;
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
</style>
