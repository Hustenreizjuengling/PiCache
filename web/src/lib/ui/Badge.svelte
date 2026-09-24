<!--
  @component
  Small count or tag, e.g. in tabs or next to names ("Default", "Custom", "3").
  <Badge>Default</Badge> · <Badge tone="warn">2</Badge>
-->
<script lang="ts">
  import type { Snippet } from 'svelte'
  import type { Tone } from './types'

  let { tone = 'neutral', title, children }: { tone?: Tone; title?: string; children: Snippet } = $props()
</script>

<span class={['badge', tone]} {title}>{@render children()}</span>

<style>
  .badge {
    display: inline-flex;
    align-items: center;
    min-width: 20px;
    height: 20px;
    padding: 0 6px;
    justify-content: center;
    border-radius: 4px;
    background: var(--surface-3);
    color: var(--text-2);
    font-size: var(--fs-xs);
    font-weight: 600;
    line-height: 1;
    white-space: nowrap;
    vertical-align: middle;
  }
  .info {
    background: color-mix(in srgb, var(--focus) 16%, var(--surface));
    color: var(--text);
  }
  .ok {
    background: color-mix(in srgb, var(--ok) 16%, var(--surface));
    color: var(--text);
  }
  .warn {
    background: color-mix(in srgb, var(--warn) 20%, var(--surface));
    color: var(--text);
  }
  .fail {
    background: color-mix(in srgb, var(--fail) 16%, var(--surface));
    color: var(--text);
  }
</style>
