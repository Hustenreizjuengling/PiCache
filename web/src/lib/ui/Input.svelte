<!--
  @component
  Text input (inside a Field it is labelled automatically).
  <Input bind:value={domain} mono placeholder="example.com" />
  <Input type="search" icon="search" bind:value={q} aria-label="Search" />
-->
<script lang="ts">
  import type { HTMLInputAttributes } from 'svelte/elements'
  import type { IconName } from '../icons'
  import { getFieldContext } from './field'
  import Icon from './Icon.svelte'

  interface Props extends Omit<HTMLInputAttributes, 'size'> {
    value?: string | number | null
    /** Machine values: monospace. */
    mono?: boolean
    invalid?: boolean
    /** Leading icon. */
    icon?: IconName
    size?: 'sm' | 'md'
    /** Bindable element reference (focus()). */
    ref?: HTMLInputElement
  }

  let {
    value = $bindable(''),
    type = 'text',
    mono = false,
    invalid,
    icon,
    size = 'md',
    id,
    ref = $bindable(),
    class: cls,
    ...rest
  }: Props = $props()

  const field = getFieldContext()
  const isInvalid = $derived(invalid ?? field?.invalid ?? false)
</script>

<span class={['wrap', size, icon && 'has-icon', cls]}>
  {#if icon}<span class="lead"><Icon name={icon} size={size === 'sm' ? 16 : 18} /></span>{/if}
  <input
    bind:this={ref}
    bind:value
    {type}
    id={id ?? field?.id}
    class={{ mono }}
    aria-invalid={isInvalid || undefined}
    aria-describedby={field?.describedBy}
    required={field?.required || undefined}
    spellcheck={mono ? false : undefined}
    autocapitalize={mono ? 'off' : undefined}
    {...rest}
  />
</span>

<style>
  .wrap {
    position: relative;
    display: flex;
    align-items: center;
    min-width: 0;
  }
  input {
    width: 100%;
    min-width: 0;
    height: var(--control-h);
    padding: 0 var(--sp-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    font-size: var(--fs-md);
  }
  .sm input {
    height: var(--control-h-sm);
    font-size: var(--fs-sm);
    padding: 0 var(--sp-2);
  }
  .has-icon input {
    padding-left: 34px;
  }
  .sm.has-icon input {
    padding-left: 30px;
  }
  .lead {
    position: absolute;
    left: 10px;
    display: flex;
    color: var(--text-3);
    pointer-events: none;
  }
  .sm .lead {
    left: 8px;
  }
  input::placeholder {
    color: var(--text-3);
  }
  input:disabled {
    background: var(--surface-2);
    color: var(--text-2);
  }
  input[aria-invalid='true'] {
    border-color: var(--danger);
  }
  input:focus-visible {
    outline: 2px solid var(--focus);
    outline-offset: 1px;
  }
  input[type='search']::-webkit-search-cancel-button {
    cursor: pointer;
  }
</style>
