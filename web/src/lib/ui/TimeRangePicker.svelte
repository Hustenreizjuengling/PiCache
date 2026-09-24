<!--
  @component
  Segmented control for chart and list ranges (independent of retention).
  <TimeRangePicker bind:value={range} onchange={(r) => router.setQuery({ range: r })} />
-->
<script lang="ts">
  import { t } from '../../i18n/index.svelte'
  import type { RangePreset } from '../api/types'

  interface Props {
    /** null: no preset is selected (the page shows an explicit time window instead). */
    value?: RangePreset | null
    options?: RangePreset[]
    /** Accessible name (default "Time range"). */
    label?: string
    onchange?: (value: RangePreset) => void
  }

  let { value = $bindable('24h'), options = ['15m', '1h', '24h', '7d', '30d'], label, onchange }: Props = $props()

  let group: HTMLDivElement
  /** Index of the selected option (-1: none, e.g. an explicit window). */
  const selected = $derived(value ? options.indexOf(value) : -1)

  function select(v: RangePreset) {
    if (v === value) return
    value = v
    onchange?.(v)
  }

  function onKey(e: KeyboardEvent) {
    const i = selected
    let n = -1
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') n = (i + 1) % options.length
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') n = i < 0 ? options.length - 1 : (i - 1 + options.length) % options.length
    if (n < 0) return
    e.preventDefault()
    select(options[n])
    group.querySelectorAll<HTMLElement>('[role="radio"]')[n]?.focus()
  }
</script>

<div bind:this={group} class="seg" role="radiogroup" aria-label={label ?? t('common.range.label')} tabindex="-1" onkeydown={onKey}>
  {#each options as o, i (o)}
    <button
      type="button"
      role="radio"
      aria-checked={o === value}
      tabindex={i === Math.max(0, selected) ? 0 : -1}
      title={t(`common.range.long.${o}`)}
      onclick={() => select(o)}>{t(`common.range.short.${o}`)}</button
    >
  {/each}
</div>

<style>
  .seg {
    display: inline-flex;
    padding: 2px;
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
  }
  button {
    height: calc(var(--control-h-sm) - 2px);
    min-width: 44px;
    padding: 0 var(--sp-2);
    border: 0;
    border-radius: 4px;
    background: none;
    color: var(--text-2);
    font-size: var(--fs-sm);
    font-weight: 600;
    cursor: pointer;
  }
  button:hover {
    color: var(--text);
  }
  button[aria-checked='true'] {
    background: var(--text);
    color: var(--surface);
  }
</style>
