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
    /** Id of the group (focused by the settings search). */
    id?: string
    /** Id of a text describing the group (e.g. the help of the selected option). */
    describedby?: string
  }

  let { value, options, label, onchange, id, describedby }: Props = $props()

  let group: HTMLDivElement
  const selected = $derived(options.findIndex((o) => o.value === value))

  function select(v: string) {
    if (v !== value) onchange(v)
  }

  function onKey(e: KeyboardEvent) {
    let step = 0
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') step = 1
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') step = -1
    if (step === 0) return
    e.preventDefault()
    // The next option that can be chosen: disabled ones are skipped, and a
    // group inside a disabled fieldset (a read-only principal) has none
    // (`:disabled` matches its buttons too).
    const buttons = group.querySelectorAll<HTMLButtonElement>('[role="radio"]')
    let n = Math.max(0, selected)
    for (let k = 0; k < options.length; k++) {
      n = (n + step + options.length) % options.length
      const btn = buttons[n]
      if (!btn || btn.matches(':disabled')) continue
      select(options[n].value)
      btn.focus()
      return
    }
  }
</script>

<div bind:this={group} {id} class="seg" role="radiogroup" aria-label={label} aria-describedby={describedby} tabindex="-1" onkeydown={onKey}>
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
