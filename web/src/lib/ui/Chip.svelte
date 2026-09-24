<!--
  @component
  Fully rounded status label. Pair colours carry traffic meaning only
  (blue answered, orange blocked, green cache hit, brown WAN); `striped` marks
  a secondary state of the same meaning (e.g. cached vs forwarded). Tones are
  for health/status. Chips always carry text: colour is never the only signal.
  <Chip pair="orange">Blocked</Chip> · <Chip pair="blue" striped>Cached</Chip> · <Chip tone="warn">Paused</Chip>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import type { IconName } from '../icons'
  import Icon from './Icon.svelte'
  import type { Pair, Tone } from './types'

  interface Props {
    pair?: Pair
    striped?: boolean
    tone?: Tone
    size?: 'sm' | 'md'
    icon?: IconName
    title?: string
    label?: string
    children?: Snippet
  }

  let { pair, striped = false, tone, size = 'md', icon, title, label, children }: Props = $props()

  const color = $derived(
    pair
      ? `var(--${pair})`
      : tone === 'ok'
        ? 'var(--ok)'
        : tone === 'warn'
          ? 'var(--warn)'
          : tone === 'fail'
            ? 'var(--fail)'
            : tone === 'info'
              ? 'var(--focus)'
              : 'var(--text-3)',
  )
  const dot = $derived(!!pair || (!!tone && tone !== 'neutral'))
</script>

<span class={['chip', size, striped && 'striped', !dot && 'plain']} style:--c={color} {title}>
  {#if icon}<Icon name={icon} size={14} />{:else if dot}<span class="dot" aria-hidden="true"></span>{/if}
  {#if label}{label}{/if}{@render children?.()}
</span>

<style>
  .chip {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    max-width: 100%;
    height: 24px;
    padding: 0 10px 0 8px;
    border: 1px solid color-mix(in srgb, var(--c) 45%, var(--surface));
    border-radius: var(--r-pill);
    background: color-mix(in srgb, var(--c) 12%, var(--surface));
    color: var(--text);
    font-size: var(--fs-sm);
    font-weight: 600;
    line-height: 1;
    white-space: nowrap;
    vertical-align: middle;
  }
  .sm {
    height: 20px;
    padding: 0 8px 0 6px;
    font-size: var(--fs-xs);
    gap: 5px;
  }
  .plain {
    padding: 0 10px;
    border-color: var(--line);
    background: var(--surface-2);
  }
  .striped {
    background: repeating-linear-gradient(
      -45deg,
      color-mix(in srgb, var(--c) 18%, var(--surface)) 0 4px,
      color-mix(in srgb, var(--c) 5%, var(--surface)) 4px 8px
    );
  }
  .dot {
    flex: none;
    width: 8px;
    height: 8px;
    border-radius: 50%;
    background: var(--c);
  }
  .striped .dot {
    background: linear-gradient(-45deg, var(--c) 0 38%, var(--stripe) 38% 62%, var(--c) 62%);
    box-shadow: 0 0 0 1px var(--c);
  }
  .chip :global(.icon) {
    color: var(--c);
  }
</style>
