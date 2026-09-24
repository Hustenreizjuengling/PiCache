<!--
  @component
  Button with a verb label ("Add blocklist"). Renders a link when `href` is set.
  <Button variant="primary" icon="plus" onclick={add}>Add blocklist</Button>
  Variants: primary (one per view), secondary (default), ghost, danger. Sizes: sm, md.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import type { HTMLButtonAttributes } from 'svelte/elements'
  import type { IconName } from '../icons'
  import Icon from './Icon.svelte'
  import Spinner from './Spinner.svelte'

  interface Props extends HTMLButtonAttributes {
    variant?: 'primary' | 'secondary' | 'ghost' | 'danger'
    size?: 'sm' | 'md'
    icon?: IconName
    /** Shows a spinner and disables the button. */
    loading?: boolean
    /** Renders an <a> instead (navigation, downloads). */
    href?: string
    /** With href: download attribute. */
    download?: string | boolean
    children?: Snippet
  }

  let {
    variant = 'secondary',
    size = 'md',
    icon,
    loading = false,
    href,
    download,
    type = 'button',
    disabled,
    children,
    class: cls,
    ...rest
  }: Props = $props()

  const iconSize = $derived(size === 'sm' ? 16 : 18)
</script>

{#if href}
  <a
    class={['btn', variant, size, !children && 'icon-only', cls]}
    href={disabled ? undefined : href}
    download={download === true ? '' : download || undefined}
    aria-disabled={disabled || undefined}
  >
    {#if icon}<Icon name={icon} size={iconSize} />{/if}
    {#if children}<span class="label">{@render children()}</span>{/if}
  </a>
{:else}
  <button
    {type}
    class={['btn', variant, size, !children && 'icon-only', cls]}
    disabled={disabled || loading}
    aria-busy={loading || undefined}
    {...rest}
  >
    {#if loading}<Spinner size={iconSize - 2} />{:else if icon}<Icon name={icon} size={iconSize} />{/if}
    {#if children}<span class="label">{@render children()}</span>{/if}
  </button>
{/if}

<style>
  .btn {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: var(--sp-2);
    height: var(--control-h);
    padding: 0 var(--sp-4);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    font-size: var(--fs-md);
    font-weight: 600;
    line-height: 1;
    white-space: nowrap;
    text-decoration: none;
    cursor: pointer;
    user-select: none;
    transition:
      background-color var(--dur-fast),
      border-color var(--dur-fast);
  }
  .btn:hover:not(:disabled):not([aria-disabled='true']) {
    background: var(--surface-2);
  }
  .btn:disabled,
  .btn[aria-disabled='true'] {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .sm {
    height: var(--control-h-sm);
    padding: 0 var(--sp-3);
    font-size: var(--fs-sm);
    gap: 6px;
  }
  .icon-only {
    padding: 0;
    width: var(--control-h);
  }
  .icon-only.sm {
    width: var(--control-h-sm);
  }
  .primary {
    background: var(--text);
    border-color: var(--text);
    color: var(--surface);
  }
  .primary:hover:not(:disabled):not([aria-disabled='true']) {
    background: color-mix(in srgb, var(--text) 86%, var(--surface));
  }
  .ghost {
    background: transparent;
    border-color: transparent;
    color: var(--text);
  }
  .ghost:hover:not(:disabled):not([aria-disabled='true']) {
    background: var(--surface-2);
  }
  .danger {
    background: var(--danger);
    border-color: var(--danger);
    color: var(--on-accent);
  }
  .danger:hover:not(:disabled):not([aria-disabled='true']) {
    background: color-mix(in srgb, var(--danger) 88%, var(--text));
  }
  .label {
    overflow: hidden;
    text-overflow: ellipsis;
  }
</style>
