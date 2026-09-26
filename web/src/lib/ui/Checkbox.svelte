<!--
  @component
  Checkbox with label (groups, multi-select filters).
  <Checkbox bind:checked={selected} label={group.name} />
-->
<script lang="ts">
  interface Props {
    checked?: boolean
    indeterminate?: boolean
    label?: string
    description?: string
    disabled?: boolean
    /**
     * Accessible name: when there is no visible label, or to give a visible
     * label its context (then it contains the label, e.g. "IPv4 address: …").
     */
    ariaLabel?: string
    name?: string
    value?: string
    onchange?: (checked: boolean) => void
  }

  let {
    checked = $bindable(false),
    indeterminate = $bindable(false),
    label,
    description,
    disabled = false,
    ariaLabel,
    name,
    value,
    onchange,
  }: Props = $props()
</script>

<label class={['cb', disabled && 'disabled']}>
  <input
    type="checkbox"
    bind:checked
    bind:indeterminate
    {disabled}
    {name}
    {value}
    aria-label={ariaLabel}
    onchange={() => onchange?.(checked)}
  />
  {#if label}
    <span class="text">
      <span>{label}</span>
      {#if description}<span class="desc">{description}</span>{/if}
    </span>
  {/if}
</label>

<style>
  .cb {
    display: inline-flex;
    align-items: flex-start;
    gap: var(--sp-2);
    cursor: pointer;
    min-width: 0;
  }
  .disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  input {
    flex: none;
    width: 16px;
    height: 16px;
    margin: 3px 0 0;
    accent-color: var(--text);
    cursor: inherit;
  }
  .text {
    display: flex;
    flex-direction: column;
    gap: 2px;
  }
  .desc {
    font-size: var(--fs-sm);
    color: var(--text-2);
  }
</style>
