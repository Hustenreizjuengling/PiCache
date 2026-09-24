<!--
  @component
  Dropdown menu (popover, keyboard: arrows, Home/End, Esc). Items run
  `onselect` or navigate to `href`; `checked` renders radio-style items.
  <Menu label="Pause blocking" icon="pause" items={[
    { label: 'For 5 minutes', onselect: () => pause(300) },
    { separator: true },
    { label: 'Delete', icon: 'trash', danger: true, onselect: remove },
  ]} />
  Icon-only trigger: iconOnly (label becomes the accessible name). Custom trigger content: the `trigger` snippet.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import type { IconName } from '../icons'
  import Icon from './Icon.svelte'
  import { placeMenu } from './position'
  import type { MenuItem } from './types'

  interface Props {
    items: MenuItem[]
    /** Trigger text, or its accessible name with iconOnly/trigger. */
    label: string
    icon?: IconName
    iconOnly?: boolean
    variant?: 'secondary' | 'ghost'
    size?: 'sm' | 'md'
    align?: 'start' | 'end'
    disabled?: boolean
    /** Custom trigger content (replaces icon + label). */
    trigger?: Snippet
  }

  let {
    items,
    label,
    icon,
    iconOnly = false,
    variant = 'secondary',
    size = 'md',
    align = 'end',
    disabled = false,
    trigger,
  }: Props = $props()

  const auto = $props.id()
  const menuId = `menu-${auto}`
  let btn: HTMLButtonElement
  let pop: HTMLDivElement
  let open = $state(false)
  let restoreFocus = false

  function entries(): HTMLElement[] {
    return [...pop.querySelectorAll<HTMLElement>('.item:not(:disabled)')]
  }

  function focusAt(i: number) {
    const list = entries()
    if (list.length === 0) return
    list[(i + list.length) % list.length].focus()
  }

  function reposition() {
    if (open) placeMenu(btn, pop, align)
  }

  $effect(() => {
    const before = (e: Event) => {
      const te = e as ToggleEvent
      if (te.newState === 'open') placeMenu(btn, pop, align)
      else restoreFocus = pop.contains(document.activeElement)
    }
    const toggled = (e: Event) => {
      open = (e as ToggleEvent).newState === 'open'
      if (open) {
        const list = entries()
        const checked = list.findIndex((el) => el.getAttribute('aria-checked') === 'true')
        focusAt(checked >= 0 ? checked : 0)
      } else if (restoreFocus) {
        btn.focus()
      }
    }
    pop.addEventListener('beforetoggle', before)
    pop.addEventListener('toggle', toggled)
    return () => {
      pop.removeEventListener('beforetoggle', before)
      pop.removeEventListener('toggle', toggled)
    }
  })

  function onKey(e: KeyboardEvent) {
    const list = entries()
    const i = list.indexOf(document.activeElement as HTMLElement)
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault()
        focusAt(i + 1)
        break
      case 'ArrowUp':
        e.preventDefault()
        focusAt(i - 1)
        break
      case 'Home':
        e.preventDefault()
        focusAt(0)
        break
      case 'End':
        e.preventDefault()
        focusAt(list.length - 1)
        break
      case 'Tab':
        pop.hidePopover()
        break
    }
  }

  function onTriggerKey(e: KeyboardEvent) {
    if ((e.key === 'ArrowDown' || e.key === 'ArrowUp') && !open) {
      e.preventDefault()
      pop.showPopover()
    }
  }

  function select(onselect?: () => void) {
    pop.hidePopover()
    onselect?.()
  }

  const radio = $derived(items.some((it) => 'label' in it && it.checked !== undefined))
</script>

<svelte:window onresize={reposition} />

<button
  bind:this={btn}
  type="button"
  class={['trigger', variant, size, (iconOnly || trigger) && 'compact']}
  popovertarget={menuId}
  aria-haspopup="menu"
  aria-expanded={open}
  aria-label={iconOnly || trigger ? label : undefined}
  title={iconOnly ? label : undefined}
  {disabled}
  onkeydown={onTriggerKey}
>
  {#if trigger}
    {@render trigger()}
  {:else}
    {#if icon}<Icon name={icon} size={size === 'sm' ? 16 : 18} />{/if}
    {#if !iconOnly}<span class="lbl">{label}</span><Icon name="chevron-down" size={16} />{/if}
  {/if}
</button>

<div bind:this={pop} id={menuId} popover="auto" role="menu" aria-label={label} class="menu" tabindex="-1" onkeydown={onKey}>
  {#each items as it, i (i)}
    {#if 'separator' in it}
      <div role="separator" class="sep"></div>
    {:else if it.href}
      <a role="menuitem" class={['item', it.danger && 'danger']} href={it.href} tabindex="-1" onclick={() => select(it.onselect)}>
        {#if it.icon}<Icon name={it.icon} size={18} />{/if}<span>{it.label}</span>
      </a>
    {:else}
      <button
        type="button"
        role={radio ? 'menuitemradio' : 'menuitem'}
        aria-checked={radio ? !!it.checked : undefined}
        class={['item', it.danger && 'danger']}
        tabindex="-1"
        disabled={it.disabled}
        onclick={() => select(it.onselect)}
      >
        {#if radio}
          <span class="check">{#if it.checked}<Icon name="check" size={16} />{/if}</span>
        {:else if it.icon}
          <Icon name={it.icon} size={18} />
        {/if}
        <span>{it.label}</span>
      </button>
    {/if}
  {/each}
</div>

<style>
  .trigger {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    height: var(--control-h);
    padding: 0 var(--sp-2) 0 var(--sp-3);
    border: 1px solid var(--line-strong);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    font-size: var(--fs-md);
    font-weight: 600;
    white-space: nowrap;
    cursor: pointer;
  }
  .trigger:hover:not(:disabled) {
    background: var(--surface-2);
  }
  .trigger:disabled {
    opacity: 0.55;
    cursor: not-allowed;
  }
  .trigger.ghost {
    border-color: transparent;
    background: transparent;
    color: var(--text-2);
  }
  .trigger.ghost:hover:not(:disabled) {
    background: var(--surface-2);
    color: var(--text);
  }
  .trigger.sm {
    height: var(--control-h-sm);
    font-size: var(--fs-sm);
    padding: 0 6px 0 var(--sp-2);
  }
  .trigger.compact {
    padding: 0 var(--sp-2);
    min-width: var(--control-h);
    justify-content: center;
  }
  .trigger.sm.compact {
    min-width: var(--control-h-sm);
  }
  .lbl {
    overflow: hidden;
    text-overflow: ellipsis;
  }
  .menu {
    min-width: 200px;
    padding: var(--sp-1);
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--surface);
    color: var(--text);
    box-shadow: var(--shadow-float);
    overflow: auto;
  }
  .menu:popover-open {
    display: flex;
    flex-direction: column;
  }
  .item {
    display: flex;
    align-items: center;
    gap: var(--sp-2);
    width: 100%;
    min-height: 34px;
    padding: 6px var(--sp-3) 6px var(--sp-2);
    border: 0;
    border-radius: 4px;
    background: none;
    color: var(--text);
    font-size: var(--fs-md);
    text-align: left;
    text-decoration: none;
    cursor: pointer;
  }
  .item:hover:not(:disabled),
  .item:focus-visible {
    background: var(--surface-2);
    outline: none;
  }
  .item:focus-visible {
    box-shadow: inset 0 0 0 2px var(--focus);
  }
  .item:disabled {
    opacity: 0.5;
    cursor: not-allowed;
  }
  .item :global(.icon) {
    color: var(--text-2);
  }
  .danger,
  .danger :global(.icon) {
    color: var(--danger);
  }
  .check {
    display: inline-flex;
    width: 16px;
  }
  .sep {
    height: 1px;
    margin: var(--sp-1) 0;
    background: var(--line);
  }
</style>
