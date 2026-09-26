<!--
  @component
  A small segmented switch between views of one page (radio group; arrow
  keys move between the options). For time ranges use TimeRangePicker.
  <Segmented label="Show" value={view} options={[{ value: 'domains', label: 'Domains' }, …]} onchange={(v) => …} />
-->
<script lang="ts">
  import type { SelectOption } from './types'

  interface Props {
    value: string
    options: SelectOption[]
    /** Accessible name of the group. */
    label: string
    onchange: (value: string) => void
  }

  let { value, options, label, onchange }: Props = $props()

  let group: HTMLDivElement
  const selected = $derived(options.findIndex((o) => o.value === value))

  function select(v: string) {
    if (v !== value) onchange(v)
  }

  function onKey(e: KeyboardEvent) {
    const i = Math.max(0, selected)
    let n = -1
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') n = (i + 1) % options.length
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') n = (i - 1 + options.length) % options.length
    if (n < 0) return
    e.preventDefault()
    select(options[n].value)
    group.querySelectorAll<HTMLElement>('[role="radio"]')[n]?.focus()
  }
</script>

<div bind:this={group} class="seg" role="radiogroup" aria-label={label} tabindex="-1" onkeydown={onKey}>
  {#each options as o, i (o.value)}
    <button
      type="button"
      role="radio"
      aria-checked={o.value === value}
      tabindex={i === Math.max(0, selected) ? 0 : -1}
      disabled={o.disabled}
      onclick={() => select(o.value)}>{o.label}</button
    >
  {/each}
</div>

<style>
  .seg {
    display: inline-flex;
    flex-wrap: wrap;
    align-self: flex-start;
    max-width: 100%;
    padding: 2px;
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
  }
  button {
    min-height: calc(var(--control-h-sm) - 2px);
    padding: 2px var(--sp-3);
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
  button:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 1px;
  }
  button[aria-checked='true'] {
    background: var(--text);
    color: var(--surface);
  }
</style>
