<!--
  @component
  On/off switch (role="switch") for settings that apply immediately.
  <Toggle bind:checked={enabled} label="Enable LanCache" description="…" onchange={save} />
-->
<script lang="ts">
  interface Props {
    checked?: boolean
    label?: string
    description?: string
    disabled?: boolean
    /** Accessible name when there is no visible label (e.g. in a table row). */
    ariaLabel?: string
    id?: string
    onchange?: (checked: boolean) => void
  }

  let { checked = $bindable(false), label, description, disabled = false, ariaLabel, id, onchange }: Props = $props()

  function toggle() {
    checked = !checked
    onchange?.(checked)
  }
</script>

<button
  type="button"
  role="switch"
  class="toggle"
  {id}
  aria-checked={checked}
  aria-label={label ? undefined : ariaLabel}
  {disabled}
  onclick={toggle}
>
  <span class="track" aria-hidden="true"><span class="thumb"></span></span>
  {#if label}
    <span class="text">
      <span class="lbl">{label}</span>
      {#if description}<span class="desc">{description}</span>{/if}
    </span>
  {/if}
</button>

<style>
  .toggle {
    display: inline-flex;
    align-items: flex-start;
    gap: var(--sp-3);
    padding: 0;
    border: 0;
    background: none;
    color: var(--text);
    text-align: left;
    cursor: pointer;
  }
  .toggle:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .track {
    position: relative;
    flex: none;
    width: 36px;
    height: 20px;
    margin-top: 1px;
    border-radius: var(--r-pill);
    background: var(--line-strong);
    transition: background-color var(--dur-fast);
  }
  .thumb {
    position: absolute;
    top: 2px;
    left: 2px;
    width: 16px;
    height: 16px;
    border-radius: 50%;
    background: var(--surface);
    transition: transform var(--dur-fast);
  }
  [aria-checked='true'] .track {
    background: var(--text);
  }
  [aria-checked='true'] .thumb {
    transform: translateX(16px);
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .lbl {
    font-weight: 600;
    font-size: var(--fs-md);
  }
  .desc {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
</style>
