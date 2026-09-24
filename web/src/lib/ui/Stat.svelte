<!--
  @component
  An inline number that links to the matching filtered page (used in status
  sentences, not as big-number cards).
  <Stat value="42" label="queries/min" href={href('/dns/queries')} />
-->
<script lang="ts">
  import type { Tone } from './types'

  interface Props {
    value: string
    label?: string
    href?: string
    title?: string
    tone?: Tone
  }

  let { value, label, href, title, tone = 'neutral' }: Props = $props()
</script>

{#if href}
  <a class={['stat', tone]} {href} {title}><span class="v">{value}</span>{#if label}&nbsp;<span class="l">{label}</span>{/if}</a>
{:else}
  <span class={['stat', tone]} {title}><span class="v">{value}</span>{#if label}&nbsp;<span class="l">{label}</span>{/if}</span>
{/if}

<style>
  .stat {
    white-space: nowrap;
    color: var(--text);
  }
  a.stat {
    text-decoration-color: color-mix(in srgb, var(--text) 35%, transparent);
  }
  a.stat:hover {
    text-decoration-color: currentColor;
  }
  .v {
    font-weight: 700;
    font-variant-numeric: tabular-nums;
  }
  .warn .v {
    color: var(--warn);
  }
  .fail .v {
    color: var(--fail);
  }
  .ok .v {
    color: var(--ok);
  }
</style>
