<!--
  @component
  Native select with options.
  <Select bind:value={mode} options={[{ value: 'null', label: 'Null IP' }, …]} />
-->
<script lang="ts">
  import type { HTMLSelectAttributes } from 'svelte/elements'
  import { getFieldContext } from './field'
  import Icon from './Icon.svelte'
  import type { SelectOption } from './types'

  interface Props extends Omit<HTMLSelectAttributes, 'size' | 'value'> {
    value?: string
    options: SelectOption[]
    /** Adds a first, empty option with this text. */
    placeholder?: string
    invalid?: boolean
    size?: 'sm' | 'md'
  }

  let {
    value = $bindable(''),
    options,
    placeholder,
    invalid,
    size = 'md',
    id,
    class: cls,
    ...rest
  }: Props = $props()

  const field = getFieldContext()
  const isInvalid = $derived(invalid ?? field?.invalid ?? false)
</script>

<span class={['wrap', size, cls]}>
  <select
    bind:value
    id={id ?? field?.id}
    aria-invalid={isInvalid || undefined}
    aria-describedby={field?.describedBy}
    required={field?.required || undefined}
    {...rest}
  >
    {#if placeholder !== undefined}<option value="">{placeholder}</option>{/if}
    {#each options as o (o.value)}
      <option value={o.value} disabled={o.disabled}>{o.label}</option>
    {/each}
  </select>
  <span class="chev"><Icon name="chevron-down" size={16} /></span>
</span>

<style>
  .wrap {
    position: relative;
    display: flex;
    align-items: center;
    min-width: 0;
  }
  select {
    appearance: none;
    width: 100%;
    min-width: 0;
    height: var(--control-h);
    padding: 0 34px 0 var(--sp-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    font-size: var(--fs-md);
    cursor: pointer;
  }
  .sm select {
    height: var(--control-h-sm);
    font-size: var(--fs-sm);
    padding: 0 28px 0 var(--sp-2);
  }
  select:disabled {
    background: var(--surface-2);
    color: var(--text-2);
    cursor: not-allowed;
  }
  select[aria-invalid='true'] {
    border-color: var(--danger);
  }
  select:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 1px;
  }
  .chev {
    position: absolute;
    right: 10px;
    display: flex;
    color: var(--text-2);
    pointer-events: none;
  }
  .sm .chev {
    right: 8px;
  }
</style>
