<!--
  @component
  Square button with only an icon; `label` is the accessible name and tooltip.
  <IconButton icon="trash" label="Delete record" onclick={remove} />
-->
<script lang="ts">
  import type { HTMLButtonAttributes } from 'svelte/elements'
  import type { IconName } from '../icons'
  import Icon from './Icon.svelte'
  import Spinner from './Spinner.svelte'

  interface Props extends HTMLButtonAttributes {
    icon: IconName
    label: string
    variant?: 'ghost' | 'secondary' | 'danger'
    size?: 'sm' | 'md'
    loading?: boolean
    /** Toggle buttons: aria-pressed. */
    pressed?: boolean
    href?: string
  }

  let {
    icon,
    label,
    variant = 'ghost',
    size = 'md',
    loading = false,
    pressed,
    href,
    type = 'button',
    disabled,
    class: cls,
    ...rest
  }: Props = $props()

  const iconSize = $derived(size === 'sm' ? 16 : 20)
</script>

{#if href}
  <a class={['ib', variant, size, cls]} {href} aria-label={label} title={label}>
    <Icon name={icon} size={iconSize} />
  </a>
{:else}
  <button
    {type}
    class={['ib', variant, size, cls]}
    aria-label={label}
    title={label}
    aria-pressed={pressed}
    disabled={disabled || loading}
    aria-busy={loading || undefined}
    {...rest}
  >
    {#if loading}<Spinner size={iconSize - 4} />{:else}<Icon name={icon} size={iconSize} />{/if}
  </button>
{/if}

<style>
  .ib {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    flex: none;
    width: var(--control-h);
    height: var(--control-h);
    padding: 0;
    border: 1px solid transparent;
    border-radius: var(--r-control);
    background: transparent;
    color: var(--text-2);
    cursor: pointer;
    transition: background-color var(--dur-fast);
  }
  .ib:hover:not(:disabled) {
    background: var(--surface-2);
    color: var(--text);
  }
  .ib:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .ib[aria-pressed='true'] {
    background: var(--surface-3);
    color: var(--text);
  }
  .sm {
    width: var(--control-h-sm);
    height: var(--control-h-sm);
  }
  .secondary {
    border-color: var(--line-strong);
    background: var(--surface);
    color: var(--text);
  }
  .danger {
    color: var(--danger);
  }
  .danger:hover:not(:disabled) {
    color: var(--danger);
    background: color-mix(in srgb, var(--danger) 12%, transparent);
  }
</style>
