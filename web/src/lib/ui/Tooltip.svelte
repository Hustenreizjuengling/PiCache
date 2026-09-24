<!--
  @component
  Short explanation shown on hover and keyboard focus (never essential
  information). Wraps non-interactive content; buttons use their `title`/label.
  <Tooltip text="Answered from the DNS cache">
    <Chip pair="blue" striped>Cached</Chip>
  </Tooltip>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import { placeTooltip } from './position'

  interface Props {
    text: string
    /** Make the wrapper focusable so keyboard users can read the tooltip (default true). */
    focusable?: boolean
    children: Snippet
  }

  let { text, focusable = true, children }: Props = $props()

  const auto = $props.id()
  const tipId = `tip-${auto}`
  let anchor: HTMLSpanElement
  let tip: HTMLDivElement
  let timer: ReturnType<typeof setTimeout> | undefined

  function show() {
    clearTimeout(timer)
    timer = setTimeout(() => {
      if (!text || !tip.isConnected) return
      tip.showPopover()
      placeTooltip(anchor, tip)
    }, 250)
  }

  function hide() {
    clearTimeout(timer)
    if (tip.matches(':popover-open')) tip.hidePopover()
  }

  $effect(() => () => clearTimeout(timer))
</script>

<!-- Focusable so keyboard users can reach the description; it has no action. -->
<!-- svelte-ignore a11y_no_noninteractive_tabindex, a11y_no_static_element_interactions -->
<span
  bind:this={anchor}
  class="anchor"
  tabindex={focusable ? 0 : undefined}
  aria-describedby={tipId}
  onpointerenter={show}
  onpointerleave={hide}
  onfocusin={show}
  onfocusout={hide}
  onkeydown={(e) => e.key === 'Escape' && hide()}
>
  {@render children()}
</span>
<div bind:this={tip} id={tipId} popover="manual" role="tooltip" class="tip">{text}</div>

<style>
  .anchor {
    display: inline-flex;
    max-width: 100%;
    border-radius: 4px;
  }
  .tip {
    padding: 6px 10px;
    border: 1px solid var(--line);
    border-radius: var(--r-control);
    background: var(--text);
    color: var(--surface);
    font-size: var(--fs-sm);
    line-height: 1.35;
    box-shadow: var(--shadow-float);
    pointer-events: none;
    overflow-wrap: anywhere;
  }
</style>
